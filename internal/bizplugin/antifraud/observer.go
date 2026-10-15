package antifraud

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zrurf/conduit"
	"go.uber.org/zap"

	"github.com/DaWesen/lanmei-dream/internal/gateway"
	pluginpkg "github.com/DaWesen/lanmei-dream/internal/plugin"
)

// 消息 Extra 的键，与 internal/bot 注入的键一致（插件侧按字面量读取，避免依赖 bot 包）。
const (
	extraPlatform           = "platform"
	extraMessageType        = "bot.message_type"
	extraSegments           = "bot.segments"
	extraSelfID             = "self_id"
	extraIsSuperUser        = "bot.is_super_user"
	extraMessageTypeMessage = "message"
)

// ObserveMessage 实现 plugin.MessageObserver：过滤后把消息投入判定队列并立即返回。
//
// 该方法在消息处理协程中同步调用，只做常量级判断与非阻塞入队，不做任何 IO；
// 队列满时丢弃并计数（判定是旁路能力，绝不能反过来阻塞消息处理）。
func (p *Plugin) ObserveMessage(ctx *conduit.MessageContext) {
	if p.stopped.Load() {
		return
	}
	t, ok := p.buildTask(ctx)
	if !ok {
		return
	}
	select {
	case p.queue <- t:
	default:
		if dropped := p.dropped.Add(1); dropped == 1 || dropped%100 == 0 {
			p.logger.Warn("antifraud: 判定队列已满，丢弃消息（判定速度跟不上消息量）",
				zap.Int64("dropped", dropped), zap.Int("queue_size", p.cfg.QueueSize))
		}
	}
}

// buildTask 做准入判断并组装判定任务；返回 false 表示该消息不参与判定。
func (p *Plugin) buildTask(ctx *conduit.MessageContext) (task, bool) {
	if ctx == nil {
		return task{}, false
	}
	// 只判定普通消息：notice / request 等事件无内容语义
	if mt, ok := ctx.Extra[extraMessageType].(string); ok && mt != extraMessageTypeMessage {
		return task{}, false
	}
	if ctx.IsGroup {
		if containsID(p.cfg.GroupBlacklist, ctx.GroupID) {
			return task{}, false
		}
		if len(p.cfg.GroupWhitelist) > 0 && !containsID(p.cfg.GroupWhitelist, ctx.GroupID) {
			return task{}, false
		}
	} else if !p.cfg.MonitorPrivate {
		return task{}, false
	}
	// 机器人自己的消息
	if selfID, _ := ctx.Extra[extraSelfID].(string); selfID != "" && selfID == ctx.UserID {
		return task{}, false
	}
	// 超管与动态管理员的消息（避免误伤管理操作）
	if p.cfg.SkipSuperUsers {
		if isSuper, _ := ctx.Extra[extraIsSuperUser].(bool); isSuper {
			return task{}, false
		}
	}

	text := judgedText(ctx)
	// 斜杠命令不判定（命令由命令系统处理，且多为管理操作）
	if text == "" || strings.HasPrefix(text, "/") {
		return task{}, false
	}
	if utf8.RuneCountInString(text) < p.cfg.MinChars {
		return task{}, false
	}
	text = truncateRunes(text, p.cfg.MaxContentChars)

	if !p.allowByCooldown(ctx) {
		return task{}, false
	}

	platform, _ := ctx.Extra[extraPlatform].(string)
	nickname, _ := ctx.Extra["nickname"].(string)
	messageID, _ := ctx.Extra["message_id"].(string)
	connID, _ := ctx.Extra["conn_id"].(string)
	return task{
		platform:  platform,
		connID:    connID,
		groupID:   ctx.GroupID,
		userID:    ctx.UserID,
		nickname:  nickname,
		messageID: messageID,
		text:      text,
		hash:      hashContent(text),
		isGroup:   ctx.IsGroup,
		seenAt:    time.Now(),
	}, true
}

// judgedText 取参与判定的文本：只取 text 段（排除 at 与图片段，避免 @昵称 干扰判定），
// 无段信息时回退原始文本。
func judgedText(ctx *conduit.MessageContext) string {
	if segs, ok := ctx.Extra[extraSegments].([]gateway.NormalizedSegment); ok {
		parts := make([]string, 0, len(segs))
		for _, seg := range segs {
			if seg.Type != "text" || strings.TrimSpace(seg.Text) == "" {
				continue
			}
			parts = append(parts, seg.Text)
		}
		if len(parts) > 0 {
			return strings.TrimSpace(strings.Join(parts, " "))
		}
		return ""
	}
	return strings.TrimSpace(ctx.RawMsg)
}

// allowByCooldown 按用户维度限流：冷却期内同一用户不再判定，保护推理服务不被刷。
//
// 返回：允许判定为 true；状态存储不可用或写入失败时放行（判定是旁路保护，不做 fail-closed）。
func (p *Plugin) allowByCooldown(ctx *conduit.MessageContext) bool {
	if p.stateStore == nil || p.cfg.UserCooldownSeconds <= 0 {
		return true
	}
	platform, _ := ctx.Extra[extraPlatform].(string)
	if platform == "" {
		platform = "unknown"
	}
	scope := "private"
	if ctx.IsGroup {
		scope = "group:" + ctx.GroupID
	}
	key := pluginpkg.StoreKey(pluginID, "cooldown:"+platform+":"+scope+":user:"+ctx.UserID)

	opCtx, cancel := context.WithTimeout(context.Background(), limiterTimeout)
	defer cancel()
	acquired, err := p.stateStore.SetIfNotExists(opCtx, key, "1", time.Duration(p.cfg.UserCooldownSeconds)*time.Second)
	if err != nil {
		p.logger.Warn("antifraud: 冷却状态写入失败，放行判定", zap.Error(err))
		return true
	}
	return acquired
}
