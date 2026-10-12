package bizplugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/DaWesen/lanmei-dream/internal/database"
	"github.com/DaWesen/lanmei-dream/internal/media"
	"github.com/DaWesen/lanmei-dream/internal/model"
	pluginpkg "github.com/DaWesen/lanmei-dream/internal/plugin"
	"github.com/cloudwego/eino/schema"
	"github.com/zrurf/conduit"
	"go.uber.org/zap"
)

// StickerPlugin 实现自定义表情的收藏与发送：管理员（普通管理员 + 超管）用 /添加表情
// （兼容旧命令 /收表情）收藏消息中的图片（上传 RustFS + 按命令参数人工标签 + 写 sticker_library），
// /删除表情 按精确标签删除并清理未被共享引用的 RustFS 对象，/发表情 按标签检索发送一张
// （无参数时发最新收藏的一张），/表情列表 必须完整匹配且仅管理员可用；
// pick_sticker 工具供 LLM 按语义（情绪/语境）检索表情库，返回可发送的图片 URL。
//
// 插件 ID sticker；依赖 RustFS 对象存储（store 为 nil 时收藏、删除、发送不可用，仍可查询库内记录）
// 与 Postgres（db 由 PluginContext 在 OnInit 注入）；
// 不使用受限 KV 与 StateStore。
//
// 数据来源约定（"不导包"）：
//   - 管理员标记：黑板 "bot.is_super_user"（bot 层注入，普通管理员与超管统一标记）
//   - 消息图片：黑板 "bot.image_urls"（bot 层注入的图片段 url 列表）
type StickerPlugin struct {
	db     *database.DB
	store  *media.ObjectStore
	logger *zap.Logger
}

// NewStickerPlugin 创建表情库插件。store 为 nil 时收藏、删除和发送功能不可用（仍可查询库内记录）。
// db 与 logger 在 OnInit 阶段从 PluginContext 注入。
func NewStickerPlugin(store *media.ObjectStore, logger *zap.Logger) *StickerPlugin {
	return &StickerPlugin{store: store, logger: logger}
}

// Info 返回表情库插件元信息。
func (p *StickerPlugin) Info() pluginpkg.PluginInfo {
	return pluginpkg.PluginInfo{
		ID:          "sticker",
		Name:        "表情库",
		Description: "自定义表情收藏、按标签管理与按语义发送",
		Version:     "1.1.0",
		Commands: []pluginpkg.CommandDef{
			{Name: "添加表情", Description: "收藏消息中的图片为表情（仅管理员），格式：/添加表情 标签1 标签2", Order: 170},
			{Name: "删除表情", Description: "按精确标签删除表情（仅管理员且只接受显式斜杠命令），格式：/删除表情 标签", Order: 180},
			{Name: "发表情", Description: "发送匹配标签的表情；无参数时发送随机表情，格式：/发表情 标签", Order: 80},
			{Name: "表情列表", Description: "查看表情库清单（仅管理员），格式：/表情列表", Order: 90},
		},
		SubtreeID: pluginpkg.SubtreeID("sticker"),
		Tools: []pluginpkg.ToolDef{
			{
				Name:        "pick_sticker",
				Description: "根据情绪/语境从表情库挑选一张表情，返回图片URL；请在回复中把该URL作为唯一内容原样输出，不要用反引号/代码块包裹，也不要编造其他图片URL",
				Parameters: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
					"emotion": {
						Type:     schema.String,
						Desc:     "情绪/语境描述，如\"开心\"\"无语\"\"被坑了\"",
						Required: true,
					},
				}),
				Handler: p.toolPickSticker,
			},
		},
	}
}

// OnInit 初始化表情库插件，注册 Pass、Pipeline 和 Subtree。
func (p *StickerPlugin) OnInit(ctx *pluginpkg.PluginContext) error {
	p.db = ctx.DB
	p.logger = ctx.Logger

	collectPassID := pluginpkg.PassID("sticker", "collect")
	collectPass := &stickerCollectPass{db: p.db, store: p.store, logger: p.logger}
	if err := ctx.Engine.RegisterPass(collectPassID, collectPass); err != nil {
		return fmt.Errorf("register sticker collect pass: %w", err)
	}
	ctx.Registry.TrackPass("sticker", collectPassID)

	deletePassID := pluginpkg.PassID("sticker", "delete")
	deletePass := &stickerDeletePass{db: p.db, store: p.store, logger: p.logger}
	if err := ctx.Engine.RegisterPass(deletePassID, deletePass); err != nil {
		return fmt.Errorf("register sticker delete pass: %w", err)
	}
	ctx.Registry.TrackPass("sticker", deletePassID)

	sendPassID := pluginpkg.PassID("sticker", "send")
	sendPass := &stickerSendPass{db: p.db, store: p.store, logger: p.logger}
	if err := ctx.Engine.RegisterPass(sendPassID, sendPass); err != nil {
		return fmt.Errorf("register sticker send pass: %w", err)
	}
	ctx.Registry.TrackPass("sticker", sendPassID)

	listPassID := pluginpkg.PassID("sticker", "list")
	listPass := &stickerListPass{db: p.db, logger: p.logger}
	if err := ctx.Engine.RegisterPass(listPassID, listPass); err != nil {
		return fmt.Errorf("register sticker list pass: %w", err)
	}
	ctx.Registry.TrackPass("sticker", listPassID)

	collectPipelineID := pluginpkg.PipelineID("sticker", "collect")
	if err := ctx.Engine.RegisterPipeline(conduit.NewPipelineFromIDs(collectPipelineID, collectPassID)); err != nil {
		return fmt.Errorf("register sticker collect pipeline: %w", err)
	}
	ctx.Registry.TrackPipeline("sticker", collectPipelineID)

	deletePipelineID := pluginpkg.PipelineID("sticker", "delete")
	if err := ctx.Engine.RegisterPipeline(conduit.NewPipelineFromIDs(deletePipelineID, deletePassID)); err != nil {
		return fmt.Errorf("register sticker delete pipeline: %w", err)
	}
	ctx.Registry.TrackPipeline("sticker", deletePipelineID)

	sendPipelineID := pluginpkg.PipelineID("sticker", "send")
	if err := ctx.Engine.RegisterPipeline(conduit.NewPipelineFromIDs(sendPipelineID, sendPassID)); err != nil {
		return fmt.Errorf("register sticker send pipeline: %w", err)
	}
	ctx.Registry.TrackPipeline("sticker", sendPipelineID)

	listPipelineID := pluginpkg.PipelineID("sticker", "list")
	if err := ctx.Engine.RegisterPipeline(conduit.NewPipelineFromIDs(listPipelineID, listPassID)); err != nil {
		return fmt.Errorf("register sticker list pipeline: %w", err)
	}
	ctx.Registry.TrackPipeline("sticker", listPipelineID)

	subtree := conduit.NewSelector(
		conduit.NewSequence(
			conduit.NewCondition(isCollectStickerCommand),
			conduit.NewAction(collectPipelineID),
		),
		conduit.NewSequence(
			conduit.NewCondition(isDeleteStickerCommand),
			conduit.NewAction(deletePipelineID),
		),
		conduit.NewSequence(
			conduit.NewCondition(isListStickerCommand),
			conduit.NewAction(listPipelineID),
		),
		conduit.NewSequence(
			conduit.NewCondition(isSendStickerCommand),
			conduit.NewAction(sendPipelineID),
		),
	)
	if err := ctx.Engine.RegisterSubtree(pluginpkg.SubtreeID("sticker"), subtree); err != nil {
		return fmt.Errorf("register sticker subtree: %w", err)
	}

	return nil
}

// OnStart 表情库插件无需后台任务。
func (p *StickerPlugin) OnStart(_ *pluginpkg.PluginContext) error { return nil }

// OnStop 表情库插件无需清理资源。
func (p *StickerPlugin) OnStop(_ *pluginpkg.PluginContext) error { return nil }

// stickerCollectPrefixes 添加表情命令前缀：正式 /添加表情，兼容旧命令 /收表情。
var stickerCollectPrefixes = []string{"/添加表情", "/收表情"}

// hasStickerCollectPrefix 判断消息是否命中添加表情命令前缀。
func hasStickerCollectPrefix(msg string) bool {
	msg = strings.TrimSpace(msg)
	for _, p := range stickerCollectPrefixes {
		if strings.HasPrefix(msg, p) {
			return true
		}
	}
	return false
}

// stripStickerCollectPrefix 去掉添加表情命令前缀，返回剩余参数部分。
func stripStickerCollectPrefix(msg string) string {
	msg = strings.TrimSpace(msg)
	for _, p := range stickerCollectPrefixes {
		if strings.HasPrefix(msg, p) {
			return strings.TrimSpace(strings.TrimPrefix(msg, p))
		}
	}
	return msg
}

// isCollectStickerCommand 判断消息是否为添加表情命令（兼容旧命令 /收表情）。
func isCollectStickerCommand(ctx *conduit.MessageContext) bool {
	return hasStickerCollectPrefix(ctx.RawMsg)
}

// isDeleteStickerCommand 判断消息是否为删除表情命令，并要求命令名边界完整。
func isDeleteStickerCommand(ctx *conduit.MessageContext) bool {
	msg := strings.TrimSpace(ctx.RawMsg)
	return msg == "/删除表情" || strings.HasPrefix(msg, "/删除表情 ") || strings.HasPrefix(msg, "/删除表情\t")
}

// isSendStickerCommand 判断消息是否为发表情命令。
func isSendStickerCommand(ctx *conduit.MessageContext) bool {
	return strings.HasPrefix(strings.TrimSpace(ctx.RawMsg), "/发表情")
}

// isListStickerCommand 判断消息是否为 /表情列表 命令。
// 必须完整匹配 "/表情列表"（trim 后相等），不接受前缀/多余参数，
// 避免 /表情列表XXX 之类的变体误入该分支。
func isListStickerCommand(ctx *conduit.MessageContext) bool {
	return strings.TrimSpace(ctx.RawMsg) == "/表情列表"
}

// 黑板键（bot 层注入，插件按"不导包"约定使用字符串字面量）。
const (
	// 项目会把配置超管与 bot_admin 动态 Bot 管理员合并到同一集合后注入此标记。
	blackboardIsSuperUser    = "bot.is_super_user"
	blackboardImageURLs      = "bot.image_urls"
	blackboardCommandReentry = "bot.command.reentry"
)

// isCommandReentry 判断当前消息是否为插件命令重入（意图路由或斜杠命令触发的插件命令）。
func isCommandReentry(ctx *conduit.MessageContext) bool {
	b, _ := ctx.Extra[blackboardCommandReentry].(bool)
	return b
}

// stickerCollectPass 收藏表情：管理员图片上传 RustFS 后按命令参数中的人工标签写入表情库。
type stickerCollectPass struct {
	db     *database.DB
	store  *media.ObjectStore
	logger *zap.Logger
}

// Execute 收藏表情：校验调用者为管理员后，取黑板 bot.image_urls 的首张图片下载
// （10 秒超时、10MB 上限），上传 RustFS，按命令参数中的人工标签写入表情库。
// 由 plugin.sticker.pipeline.collect 在 isCollectStickerCommand 命中 /添加表情
// （兼容旧命令 /收表情）后调用。
// 失败分支均只回复对应提示且不写库：非管理员、缺标签参数、未附图片、RustFS 未配置、
// 图片下载失败或内容为空、同图已收藏（objectKey 内容寻址幂等）、入库失败。
func (pass *stickerCollectPass) Execute(ctx *conduit.MessageContext) error {
	isSuper, _ := ctx.Extra[blackboardIsSuperUser].(bool)
	if !isSuper {
		conduit.AppendOutput(ctx, &conduit.Message{
			UserID: ctx.UserID, GroupID: ctx.GroupID, IsGroup: ctx.IsGroup,
			Content: "只有管理员能收藏表情哦~",
		})
		return nil
	}

	if pass.store == nil {
		conduit.AppendOutput(ctx, &conduit.Message{
			UserID: ctx.UserID, GroupID: ctx.GroupID, IsGroup: ctx.IsGroup,
			Content: "表情存储未配置（RustFS 不可用），无法收藏",
		})
		return nil
	}

	// 解析标签：/添加表情 标签1 标签2
	raw := stripStickerCollectPrefix(ctx.RawMsg)
	tags := strings.Fields(raw)
	if len(tags) == 0 {
		conduit.AppendOutput(ctx, &conduit.Message{
			UserID: ctx.UserID, GroupID: ctx.GroupID, IsGroup: ctx.IsGroup,
			Content: "格式错误！用法：/添加表情 标签1 标签2（需附带一张图片）",
		})
		return nil
	}

	imageURLs, _ := ctx.Extra[blackboardImageURLs].([]string)
	if len(imageURLs) == 0 {
		conduit.AppendOutput(ctx, &conduit.Message{
			UserID: ctx.UserID, GroupID: ctx.GroupID, IsGroup: ctx.IsGroup,
			Content: "没有检测到图片，请附带一张图片再收藏",
		})
		return nil
	}

	imgURL := imageURLs[0]
	data, mime, err := downloadImage(ctx.Ctx, imgURL)
	if err != nil {
		pass.logger.Warn("sticker: 图片下载失败", zap.String("url", imgURL), zap.Error(err))
		conduit.AppendOutput(ctx, &conduit.Message{
			UserID: ctx.UserID, GroupID: ctx.GroupID, IsGroup: ctx.IsGroup,
			Content: "图片下载失败，请重试",
		})
		return nil
	}
	if len(data) == 0 {
		// llonebot 本地图片（file:// 路径）在容器内不可达时可能返回空内容，
		// 直接拒绝，避免把 0 字节空对象存进表情库（发送时会失败）。
		pass.logger.Warn("sticker: 图片内容为空，拒绝入库",
			zap.String("url", imgURL), zap.String("mime", mime))
		conduit.AppendOutput(ctx, &conduit.Message{
			UserID: ctx.UserID, GroupID: ctx.GroupID, IsGroup: ctx.IsGroup,
			Content: "图片内容为空（本地文件可能无法从服务器访问），请换一张网络图片试试",
		})
		return nil
	}
	pass.logger.Info("sticker: 图片下载完成",
		zap.String("url", imgURL), zap.String("mime", mime), zap.Int("size", len(data)))

	objectKey, err := pass.store.Put(ctx.Ctx, data, mime)
	if err != nil {
		pass.logger.Error("sticker: 上传表情失败", zap.Error(err))
		conduit.AppendOutput(ctx, &conduit.Message{
			UserID: ctx.UserID, GroupID: ctx.GroupID, IsGroup: ctx.IsGroup,
			Content: "表情上传失败，请稍后重试",
		})
		return nil
	}

	// 内容寻址幂等：同图已收藏则提示
	if existing, _ := pass.db.GetStickerByObjectKey(ctx.Ctx, objectKey); existing != nil {
		conduit.AppendOutput(ctx, &conduit.Message{
			UserID: ctx.UserID, GroupID: ctx.GroupID, IsGroup: ctx.IsGroup,
			Content: fmt.Sprintf("这张表情已在表情库啦（ID %d），无需重复收藏", existing.ID),
		})
		return nil
	}

	// 写库（人工标签参数，按空白切分）
	tagsJSON, _ := json.Marshal(tags)
	sticker := &model.StickerLibrary{
		ObjectKey: objectKey,
		FileID:    "",
		Tags:      string(tagsJSON),
		Source:    "manual",
	}
	if err := pass.db.CreateSticker(ctx.Ctx, sticker); err != nil {
		pass.logger.Error("sticker: 写库失败", zap.Error(err))
		conduit.AppendOutput(ctx, &conduit.Message{
			UserID: ctx.UserID, GroupID: ctx.GroupID, IsGroup: ctx.IsGroup,
			Content: "表情入库失败，请稍后重试",
		})
		return nil
	}

	conduit.AppendOutput(ctx, &conduit.Message{
		UserID: ctx.UserID, GroupID: ctx.GroupID, IsGroup: ctx.IsGroup,
		Content: fmt.Sprintf("已收藏表情 ✅ 标签：%s", strings.Join(tags, " / ")),
	})
	return nil
}

// stickerDeletePass 仅允许 Bot 管理员或超级管理员通过显式斜杠命令，
// 按一个完整标签删除所有匹配表情，并清理未被媒体缓存共享引用的 RustFS 对象。
type stickerDeletePass struct {
	db     *database.DB
	store  *media.ObjectStore
	logger *zap.Logger
}

// Execute 按单个精确标签删除表情：删除数据库中匹配标签的全部记录，并逐个清理
// 未被媒体缓存共享引用（IsMediaObjectReferenced）的 RustFS 对象；
// 共享引用检查失败的条目保留对象不删，删除结果以「已删除标签……张表情」回复。
// 由 plugin.sticker.pipeline.delete 在 isDeleteStickerCommand 命中 /删除表情 后调用。
// 删除属于破坏性管理员操作，命中命令重入标记 bot.command.reentry（LLM 自然语言意图间接触发）时直接拒绝执行；
// 非管理员（读黑板 bot.is_super_user）、标签数不为 1、数据库或 RustFS 未配置时只回复提示。
func (pass *stickerDeletePass) Execute(ctx *conduit.MessageContext) error {
	// 删除属于破坏性管理员操作，不允许由 LLM 自然语言意图间接触发。
	if isCommandReentry(ctx) {
		pass.reply(ctx, "删除表情必须使用明确的管理员命令：/删除表情 <标签>")
		return nil
	}

	isAdmin, _ := ctx.Extra[blackboardIsSuperUser].(bool)
	if !isAdmin {
		pass.reply(ctx, "只有 Bot 管理员或超级管理员才能删除表情哦~")
		return nil
	}

	raw := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(ctx.RawMsg), "/删除表情"))
	tags := strings.Fields(raw)
	if len(tags) != 1 {
		pass.reply(ctx, "格式错误！用法：/删除表情 <标签>，例如：/删除表情 耍帅")
		return nil
	}
	if pass.db == nil {
		pass.reply(ctx, "数据库不可用，无法删除表情")
		return nil
	}
	if pass.store == nil {
		pass.reply(ctx, "表情存储未配置（RustFS 不可用），无法安全删除")
		return nil
	}

	tag := tags[0]
	deleted, err := pass.db.DeleteStickersByTag(ctx.Ctx, tag)
	if err != nil {
		pass.logger.Error("sticker: 按标签删除数据库记录失败", zap.String("tag", tag), zap.Error(err))
		pass.reply(ctx, "表情删除失败，请稍后重试")
		return nil
	}
	if len(deleted) == 0 {
		pass.reply(ctx, fmt.Sprintf("没有找到标签为「%s」的表情", tag))
		return nil
	}

	seenObjectKeys := make(map[string]struct{}, len(deleted))
	for _, sticker := range deleted {
		if _, seen := seenObjectKeys[sticker.ObjectKey]; seen {
			continue
		}
		seenObjectKeys[sticker.ObjectKey] = struct{}{}

		referenced, err := pass.db.IsMediaObjectReferenced(ctx.Ctx, sticker.ObjectKey)
		if err != nil {
			pass.logger.Warn("sticker: 检查共享对象引用失败，保留 RustFS 对象",
				zap.Uint("id", sticker.ID), zap.String("object_key", sticker.ObjectKey), zap.Error(err))
			continue
		}
		if referenced {
			continue
		}
		if err := pass.store.Delete(ctx.Ctx, sticker.ObjectKey); err != nil {
			pass.logger.Warn("sticker: RustFS 对象清理失败",
				zap.Uint("id", sticker.ID), zap.String("object_key", sticker.ObjectKey), zap.Error(err))
		}
	}

	pass.reply(ctx, fmt.Sprintf("已删除标签「%s」下的 %d 张表情", tag, len(deleted)))
	return nil
}

func (pass *stickerDeletePass) reply(ctx *conduit.MessageContext, content string) {
	conduit.AppendOutput(ctx, &conduit.Message{
		UserID:  ctx.UserID,
		GroupID: ctx.GroupID,
		IsGroup: ctx.IsGroup,
		Content: content,
	})
}

// stickerSendPass 发送表情：带标签时按标签检索发送一张，无参数时发送最新收藏的一张。
type stickerSendPass struct {
	db     *database.DB
	store  *media.ObjectStore
	logger *zap.Logger
}

// Execute 发送表情：带参数时按标签检索最新匹配的一张（SearchStickers 按时间倒序取第一条），
// 无参数时发送最新收藏的一张；命中记录经 RustFS 预签名（10 分钟）后以纯 URL 输出，
// 由 bot 层识别为图片段发送。
// 由 plugin.sticker.pipeline.send 在 isSendStickerCommand 命中 /发表情 后调用；
// 检索失败、无匹配、存储未配置或预签名失败时只回复对应提示。
func (pass *stickerSendPass) Execute(ctx *conduit.MessageContext) error {
	keyword := strings.TrimSpace(strings.TrimPrefix(ctx.RawMsg, "/发表情"))

	// 无参数（手动 /发表情 或意图路由未提取到标签）时发送最新一张；
	// 库清单改由仅管理员可用的 /表情列表 提供。
	if keyword == "" {
		pass.sendLatest(ctx)
		return nil
	}

	stickers, err := pass.db.SearchStickers(ctx.Ctx, keyword, 5)
	if err != nil {
		pass.logger.Error("sticker: 检索失败", zap.String("keyword", keyword), zap.Error(err))
		conduit.AppendOutput(ctx, &conduit.Message{
			UserID: ctx.UserID, GroupID: ctx.GroupID, IsGroup: ctx.IsGroup,
			Content: "表情检索失败，请稍后重试",
		})
		return nil
	}
	if len(stickers) == 0 {
		conduit.AppendOutput(ctx, &conduit.Message{
			UserID: ctx.UserID, GroupID: ctx.GroupID, IsGroup: ctx.IsGroup,
			Content: fmt.Sprintf("表情库里没有匹配「%s」的表情，用 /表情列表 看看有哪些吧", keyword),
		})
		return nil
	}

	hit := stickers[0]
	if pass.store == nil {
		conduit.AppendOutput(ctx, &conduit.Message{
			UserID: ctx.UserID, GroupID: ctx.GroupID, IsGroup: ctx.IsGroup,
			Content: "表情存储未配置（RustFS 不可用），无法发送",
		})
		return nil
	}
	presignedURL, err := pass.store.Presign(ctx.Ctx, hit.ObjectKey, 10*time.Minute)
	if err != nil {
		pass.logger.Error("sticker: 生成图片 URL 失败", zap.Uint("id", hit.ID), zap.Error(err))
		conduit.AppendOutput(ctx, &conduit.Message{
			UserID: ctx.UserID, GroupID: ctx.GroupID, IsGroup: ctx.IsGroup,
			Content: "表情发送失败，请稍后重试",
		})
		return nil
	}
	// 纯 URL 输出 → bot 层识别为图片段发送
	conduit.AppendOutput(ctx, &conduit.Message{
		UserID: ctx.UserID, GroupID: ctx.GroupID, IsGroup: ctx.IsGroup,
		Content: presignedURL,
	})
	return nil
}

// sendLatest 无参数时的默认行为：发送表情库最新收藏的一张（随机取图接口已移除，以取最新一张代替）。
// 库为空、存储未配置或取图失败时给出对应提示。
func (pass *stickerSendPass) sendLatest(ctx *conduit.MessageContext) {
	if pass.db == nil || pass.store == nil {
		conduit.AppendOutput(ctx, &conduit.Message{
			UserID: ctx.UserID, GroupID: ctx.GroupID, IsGroup: ctx.IsGroup,
			Content: "表情存储未配置（RustFS 不可用），无法发送",
		})
		return
	}
	stickers, err := pass.db.ListStickers(ctx.Ctx, 1)
	if err != nil {
		pass.logger.Error("sticker: 取表情失败", zap.Error(err))
		conduit.AppendOutput(ctx, &conduit.Message{
			UserID: ctx.UserID, GroupID: ctx.GroupID, IsGroup: ctx.IsGroup,
			Content: "表情获取失败，请稍后重试",
		})
		return
	}
	if len(stickers) == 0 {
		conduit.AppendOutput(ctx, &conduit.Message{
			UserID: ctx.UserID, GroupID: ctx.GroupID, IsGroup: ctx.IsGroup,
			Content: "表情库还是空的，发张图配上 /添加表情 标签 来收藏吧",
		})
		return
	}
	sticker := stickers[0]
	presignedURL, err := pass.store.Presign(ctx.Ctx, sticker.ObjectKey, 10*time.Minute)
	if err != nil {
		pass.logger.Error("sticker: 表情 URL 生成失败", zap.Uint("id", sticker.ID), zap.Error(err))
		conduit.AppendOutput(ctx, &conduit.Message{
			UserID: ctx.UserID, GroupID: ctx.GroupID, IsGroup: ctx.IsGroup,
			Content: "表情发送失败，请稍后重试",
		})
		return
	}
	// 纯 URL 输出 → bot 层识别为图片段发送
	conduit.AppendOutput(ctx, &conduit.Message{
		UserID: ctx.UserID, GroupID: ctx.GroupID, IsGroup: ctx.IsGroup,
		Content: presignedURL,
	})
}

// stickerListPass 查看表情库清单，仅管理员（普通管理员 + 超管）可用。
type stickerListPass struct {
	db     *database.DB
	logger *zap.Logger
}

// Execute 输出表情库清单（最多 20 条，含 ID 与标签）。
// 由 plugin.sticker.pipeline.list 在 isListStickerCommand 完整匹配 /表情列表 后调用；
// 仅管理员可用（读黑板 bot.is_super_user），非管理员、查询失败或库为空时只回复对应提示。
func (pass *stickerListPass) Execute(ctx *conduit.MessageContext) error {
	// 管理员校验：bot 层将普通管理员与超管统一注入 bot.is_super_user
	isAdmin, _ := ctx.Extra[blackboardIsSuperUser].(bool)
	if !isAdmin {
		conduit.AppendOutput(ctx, &conduit.Message{
			UserID: ctx.UserID, GroupID: ctx.GroupID, IsGroup: ctx.IsGroup,
			Content: "只有管理员能查看表情列表哦~",
		})
		return nil
	}

	stickers, err := pass.db.ListStickers(ctx.Ctx, 20)
	if err != nil {
		pass.logger.Error("sticker: 列表查询失败", zap.Error(err))
		conduit.AppendOutput(ctx, &conduit.Message{
			UserID: ctx.UserID, GroupID: ctx.GroupID, IsGroup: ctx.IsGroup,
			Content: "表情库查询失败，请稍后重试",
		})
		return nil
	}
	if len(stickers) == 0 {
		conduit.AppendOutput(ctx, &conduit.Message{
			UserID: ctx.UserID, GroupID: ctx.GroupID, IsGroup: ctx.IsGroup,
			Content: "表情库还是空的，发张图配上 /添加表情 标签 来收藏吧",
		})
		return nil
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("表情库共 %d 张：\n", len(stickers)))
	for i, s := range stickers {
		var tags []string
		if err := json.Unmarshal([]byte(s.Tags), &tags); err != nil {
			tags = nil
		}
		b.WriteString(fmt.Sprintf("%d. [%d] %s\n", i+1, s.ID, strings.Join(tags, "/")))
	}
	conduit.AppendOutput(ctx, &conduit.Message{
		UserID: ctx.UserID, GroupID: ctx.GroupID, IsGroup: ctx.IsGroup,
		Content: strings.TrimRight(b.String(), "\n"),
	})
	return nil
}

// toolPickSticker 是 AI 工具处理器：按情绪/语境检索表情库，
// 命中则返回可发送的图片 URL（预签名），未命中返回空结果提示。
func (p *StickerPlugin) toolPickSticker(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Emotion string `json:"emotion"`
		QingXu  string `json:"情绪"` // 兼容部分 LLM 直接传中文键
	}
	_ = json.Unmarshal([]byte(argsJSON), &args) // 解析失败走下方兜底，不中断
	emotion := strings.TrimSpace(args.Emotion)
	if emotion == "" {
		emotion = strings.TrimSpace(args.QingXu)
	}
	if emotion == "" {
		// 兜底：LLM 可能直接传词而非 JSON（如 "开心"）
		emotion = strings.Trim(strings.TrimSpace(argsJSON), "\"'` \n\t")
	}
	if emotion == "" {
		return "", fmt.Errorf("emotion 不能为空")
	}
	if p.db == nil {
		return "表情库不可用", nil
	}

	stickers, err := p.db.SearchStickers(ctx, emotion, 5)
	if err != nil {
		return "", fmt.Errorf("表情检索失败: %w", err)
	}
	if len(stickers) == 0 {
		return "表情库里没有匹配「" + emotion + "」的表情，直接回复纯文本即可", nil
	}

	// SearchStickers 按时间倒序返回，第一条即最新匹配。
	hit := stickers[0]
	if p.store == nil {
		return fmt.Sprintf("找到表情但对象存储不可用，无法生成图片URL"), nil
	}
	presignedURL, err := p.store.Presign(ctx, hit.ObjectKey, 10*time.Minute)
	if err != nil {
		return "", fmt.Errorf("表情URL生成失败: %w", err)
	}
	// 返回 URL 供 LLM 直接作为图片输出
	return presignedURL, nil
}

// stickerHTTPClient 图片下载客户端（10s 超时；大小上限见 downloadImage）。
var stickerHTTPClient = &http.Client{Timeout: 10 * time.Second}

// downloadImage 下载图片内容，限制大小（10MB）。
func downloadImage(ctx context.Context, url string) ([]byte, string, error) {
	const maxBytes = 10 << 20 // 10MB

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", fmt.Errorf("构造下载请求失败: %w", err)
	}
	req.Header.Set("User-Agent", "lanmei-dream/1.0")

	resp, err := stickerHTTPClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("下载失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("下载 HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("读取响应失败: %w", err)
	}
	if len(data) > maxBytes {
		return nil, "", fmt.Errorf("图片超过大小上限")
	}

	mime := resp.Header.Get("Content-Type")
	if mime == "" || mime == "application/octet-stream" {
		mime = media.SniffMime(data)
	}
	return data, mime, nil
}
