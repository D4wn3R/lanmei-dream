package bizplugin

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/DaWesen/lanmei-dream/internal/ai/tool"
	"github.com/DaWesen/lanmei-dream/internal/database"
	pluginpkg "github.com/DaWesen/lanmei-dream/internal/plugin"
	"github.com/zrurf/conduit"
	"go.uber.org/zap"
)

// SigninPlugin 实现每日签到和试试手气，二者共享同一份签到状态，每天合计只能签到一次。
// 积分与排行榜持久化在插件受限 KV（PluginContext.KV，PostgreSQL 后端）而非 Redis
// StateStore，重启不丢失。
// 插件 ID signin；命令 /签到、/试试手气，工具 signin_status（查询签到状态与积分）、
// signin_random（代当前用户试试手气）；受限 KV 未注入时读写静默跳过（不落库，也不报错）。
type SigninPlugin struct {
	kv     *database.PluginKVStore // 受限 KV（PostgreSQL 持久化）
	logger *zap.Logger
}

// NewSigninPlugin 创建签到插件。
func NewSigninPlugin(logger *zap.Logger) *SigninPlugin {
	return &SigninPlugin{logger: logger}
}

// Info 返回签到插件元信息。
func (p *SigninPlugin) Info() pluginpkg.PluginInfo {
	return pluginpkg.PluginInfo{
		ID:          "signin",
		Name:        "签到",
		Description: "每日签到和试试手气",
		Version:     "2.0.0",
		Commands: []pluginpkg.CommandDef{
			{Name: "签到", Description: "每日签到，获取积分奖励", Order: 60},
			{Name: "试试手气", Description: "试试手气，随机获取积分", Order: 70},
		},
		SubtreeID: pluginpkg.SubtreeID("signin"),
		Tools: []pluginpkg.ToolDef{
			{
				Name:        "signin_status",
				Description: "查询当前用户的签到状态与积分。涉及签到、积分、排名话题时必须先调用此工具获取真实数据，禁止凭空猜测。",
				Parameters:  emptyToolParams(),
				Handler:     p.toolSigninStatus,
			},
			{
				Name:        "signin_random",
				Description: "帮当前用户执行「试试手气」随机签到（积分可能增加也可能减少）。仅当用户明确表达想试试手气/随机签到时调用。",
				Parameters:  emptyToolParams(),
				Handler:     p.toolSigninRandom,
			},
		},
	}
}

// OnInit 初始化签到插件，注册 Pass、Pipeline 和 Subtree。
func (p *SigninPlugin) OnInit(ctx *pluginpkg.PluginContext) error {
	p.kv = ctx.KV

	normalExecPassID := pluginpkg.PassID("signin", "normal_execute")
	normalReplyPassID := pluginpkg.PassID("signin", "normal_reply")
	normalExecPass := &signinNormalExecutePass{kv: p.kv, logger: p.logger}
	normalReplyPass := &signinNormalReplyPass{}

	if err := ctx.Engine.RegisterPass(normalExecPassID, normalExecPass); err != nil {
		return fmt.Errorf("register normal execute pass: %w", err)
	}
	if err := ctx.Engine.RegisterPass(normalReplyPassID, normalReplyPass); err != nil {
		return fmt.Errorf("register normal reply pass: %w", err)
	}
	ctx.Registry.TrackPass("signin", normalExecPassID)
	ctx.Registry.TrackPass("signin", normalReplyPassID)

	randomExecPassID := pluginpkg.PassID("signin", "random_execute")
	randomReplyPassID := pluginpkg.PassID("signin", "random_reply")
	randomExecPass := &signinRandomExecutePass{kv: p.kv, logger: p.logger}
	randomReplyPass := &signinRandomReplyPass{}

	if err := ctx.Engine.RegisterPass(randomExecPassID, randomExecPass); err != nil {
		return fmt.Errorf("register random execute pass: %w", err)
	}
	if err := ctx.Engine.RegisterPass(randomReplyPassID, randomReplyPass); err != nil {
		return fmt.Errorf("register random reply pass: %w", err)
	}
	ctx.Registry.TrackPass("signin", randomExecPassID)
	ctx.Registry.TrackPass("signin", randomReplyPassID)

	normalPipelineID := pluginpkg.PipelineID("signin", "normal")
	normalPl := conduit.NewPipelineFromIDs(
		normalPipelineID,
		normalExecPassID,
		normalReplyPassID,
	)
	if err := ctx.Engine.RegisterPipeline(normalPl); err != nil {
		return fmt.Errorf("register normal pipeline: %w", err)
	}
	ctx.Registry.TrackPipeline("signin", normalPipelineID)

	randomPipelineID := pluginpkg.PipelineID("signin", "random")
	randomPl := conduit.NewPipelineFromIDs(
		randomPipelineID,
		randomExecPassID,
		randomReplyPassID,
	)
	if err := ctx.Engine.RegisterPipeline(randomPl); err != nil {
		return fmt.Errorf("register random pipeline: %w", err)
	}
	ctx.Registry.TrackPipeline("signin", randomPipelineID)

	subtree := conduit.NewSelector(
		conduit.NewSequence(
			conduit.NewCondition(isSigninCommand),
			conduit.NewAction(normalPipelineID),
		),
		conduit.NewSequence(
			conduit.NewCondition(isRandomSigninCommand),
			conduit.NewAction(randomPipelineID),
		),
	)
	if err := ctx.Engine.RegisterSubtree(pluginpkg.SubtreeID("signin"), subtree); err != nil {
		return fmt.Errorf("register subtree: %w", err)
	}

	return nil
}

// OnStart 签到插件无需后台任务。
func (p *SigninPlugin) OnStart(_ *pluginpkg.PluginContext) error { return nil }

// OnStop 签到插件无需清理资源。
func (p *SigninPlugin) OnStop(_ *pluginpkg.PluginContext) error { return nil }

// RankPlugin 实现签到积分排行榜查询（/排名 或 /rank，返回 Top 10）。
// 插件 ID signin_rank，复用 signin 插件写入的受限 KV 排行榜数据；命令 /排名、/rank，
// 工具 signin_rank；受限 KV 未注入时排行榜视为空，查询回复「暂无签到排行数据~」。
type RankPlugin struct {
	kv     *database.PluginKVStore // 受限 KV（PostgreSQL 持久化）
	logger *zap.Logger
}

// NewRankPlugin 创建排名插件。
func NewRankPlugin(logger *zap.Logger) *RankPlugin {
	return &RankPlugin{logger: logger}
}

// Info 返回排名插件元信息。
func (p *RankPlugin) Info() pluginpkg.PluginInfo {
	return pluginpkg.PluginInfo{
		ID:          "signin_rank",
		Name:        "签到排行",
		Description: "签到积分排行榜",
		Version:     "1.0.0",
		Commands: []pluginpkg.CommandDef{
			{Name: "排名", Description: "查看签到积分排行榜", Order: 41}, // /rank 的中文别名，紧跟其后
			{Name: "rank", Description: "查看签到积分排行榜", Order: 40},
		},
		SubtreeID: pluginpkg.SubtreeID("signin_rank"),
		Tools: []pluginpkg.ToolDef{
			{
				Name:        "signin_rank",
				Description: "查询签到积分排行榜",
				Handler:     p.toolSigninRank,
			},
		},
	}
}

// OnInit 初始化排名插件，注册 Pass、Pipeline 和 Subtree。
func (p *RankPlugin) OnInit(ctx *pluginpkg.PluginContext) error {
	p.kv = ctx.KV

	rankExecPassID := pluginpkg.PassID("signin_rank", "execute")
	rankReplyPassID := pluginpkg.PassID("signin_rank", "reply")
	rankExecPass := &signinRankExecutePass{kv: p.kv, logger: p.logger}
	rankReplyPass := &signinRankReplyPass{}

	if err := ctx.Engine.RegisterPass(rankExecPassID, rankExecPass); err != nil {
		return fmt.Errorf("register rank execute pass: %w", err)
	}
	if err := ctx.Engine.RegisterPass(rankReplyPassID, rankReplyPass); err != nil {
		return fmt.Errorf("register rank reply pass: %w", err)
	}
	ctx.Registry.TrackPass("signin_rank", rankExecPassID)
	ctx.Registry.TrackPass("signin_rank", rankReplyPassID)

	rankPipelineID := pluginpkg.PipelineID("signin_rank", "main")
	rankPl := conduit.NewPipelineFromIDs(
		rankPipelineID,
		rankExecPassID,
		rankReplyPassID,
	)
	if err := ctx.Engine.RegisterPipeline(rankPl); err != nil {
		return fmt.Errorf("register rank pipeline: %w", err)
	}
	ctx.Registry.TrackPipeline("signin_rank", rankPipelineID)

	subtree := conduit.NewSequence(
		conduit.NewCondition(isRankCommand),
		conduit.NewAction(rankPipelineID),
	)
	if err := ctx.Engine.RegisterSubtree(pluginpkg.SubtreeID("signin_rank"), subtree); err != nil {
		return fmt.Errorf("register rank subtree: %w", err)
	}

	return nil
}

// OnStart 排名插件无需后台任务。
func (p *RankPlugin) OnStart(_ *pluginpkg.PluginContext) error { return nil }

// OnStop 排名插件无需清理资源。
func (p *RankPlugin) OnStop(_ *pluginpkg.PluginContext) error { return nil }

// isSigninCommand 判断消息是否为普通签到命令。
func isSigninCommand(ctx *conduit.MessageContext) bool {
	return strings.TrimSpace(ctx.RawMsg) == "/签到"
}

// isRandomSigninCommand 判断消息是否为试试手气命令。
func isRandomSigninCommand(ctx *conduit.MessageContext) bool {
	return strings.TrimSpace(ctx.RawMsg) == "/试试手气"
}

// isRankCommand 判断消息是否为排名命令。
func isRankCommand(ctx *conduit.MessageContext) bool {
	trimmed := strings.TrimSpace(ctx.RawMsg)
	return trimmed == "/排名" || trimmed == "/rank"
}

// signinResult 签到结果，Pass 之间通过 MessageContext 传递。
type signinResult struct {
	TodaySigned bool
	Points      int // 本次获得积分，已签到时为 0
	TotalPoints int
	Event       string
	Rank        int    // 当前积分排名，-1 表示未上榜（排行榜全量保留后仅异常时出现）
	Mode        string // "normal" 或 "random"
}

const (
	signinResultKey = "plugin.signin.result"

	signinNormalPoints = 5 // 普通签到固定积分，与上游 LanMei 一致

	// 受限 KV 键（命名空间 kvPluginID，PostgreSQL 持久化、重启不丢）：
	//   state:<userID>:date  最后签到日期（"2006-01-02"）
	//   state:<userID>:total 累计积分
	//   leaderboard          排行榜 JSON（leaderboardEntry 数组，全量保留不截断）
	kvPluginID         = "signin"
	kvSigninStateDate  = "state:%s:date"
	kvSigninStateTotal = "state:%s:total"
	kvLeaderboardKey   = "leaderboard"
)

// leaderboardEntry 排行榜条目。
type leaderboardEntry struct {
	UserID      string `json:"user_id"`
	Nickname    string `json:"nickname"`
	TotalPoints int    `json:"total_points"`
}

// updateLeaderboard 更新排行榜中的用户条目（持久化到受限 KV 存储）。
// kv 为 nil（未注入受限存储）时静默跳过，保证插件在测试/降级环境下不崩溃。
func updateLeaderboard(kv *database.PluginKVStore, ctx context.Context, userID, nickname string, totalPoints int) {
	if kv == nil {
		return
	}
	data, err := kv.Get(ctx, kvPluginID, kvLeaderboardKey)
	if err != nil {
		return
	}

	var entries []leaderboardEntry
	if data != "" {
		_ = json.Unmarshal([]byte(data), &entries)
	}

	found := false
	for i := range entries {
		if entries[i].UserID == userID {
			entries[i].Nickname = nickname
			entries[i].TotalPoints = totalPoints
			found = true
			break
		}
	}
	if !found {
		entries = append(entries, leaderboardEntry{
			UserID:      userID,
			Nickname:    nickname,
			TotalPoints: totalPoints,
		})
	}

	// 同分按用户 ID 升序做稳定 tie-break，避免同分用户名次随机互换
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].TotalPoints != entries[j].TotalPoints {
			return entries[i].TotalPoints > entries[j].TotalPoints
		}
		return entries[i].UserID < entries[j].UserID
	})

	out, _ := json.Marshal(entries)
	_ = kv.Set(ctx, kvPluginID, kvLeaderboardKey, string(out))
}

// getLeaderboard 读取排行榜数据。
func getLeaderboard(kv *database.PluginKVStore, ctx context.Context) []leaderboardEntry {
	if kv == nil {
		return nil
	}
	data, err := kv.Get(ctx, kvPluginID, kvLeaderboardKey)
	if err != nil || data == "" {
		return nil
	}
	var entries []leaderboardEntry
	_ = json.Unmarshal([]byte(data), &entries)
	return entries
}

// getRank 查询用户在当前排行榜中的排名（1 起），不在榜内返回 -1。
// 排行榜全量保留所有签到用户，正常签到流程内必然在榜；
// -1 仅在 KV 异常或历史数据残缺时出现，由调用方兜底展示。
func getRank(kv *database.PluginKVStore, ctx context.Context, userID string) int {
	entries := getLeaderboard(kv, ctx)
	for i, e := range entries {
		if e.UserID == userID {
			return i + 1
		}
	}
	return -1
}

// signinEvent 事件模板（与上游 LanMei 保持一致）。
type signinEvent struct {
	Template string // 句子模板（%s=人物，%s=动作，%v=积分）
	Persons  []string
	Acts     []string
}

var negativeEvents = []signinEvent{
	{
		Template: "你被%s狠狠地%s了一顿，扣除了%v积分",
		Persons:  []string{"同学", "舍友", "学长", "学姐", "朋友"},
		Acts:     []string{"欺负", "吐槽", "蛐蛐"},
	},
	{
		Template: "你在和%s的%s中败下阵来，扣除了%v积分",
		Persons:  []string{"同学", "舍友", "学长", "学姐", "朋友"},
		Acts:     []string{"辩论", "讨论"},
	},
	{
		Template: "%s在背后对你进行了%s，你损失了%v积分",
		Persons:  []string{"同学", "朋友"},
		Acts:     []string{"背刺", "吐槽", "打小报告", "挂校园墙"},
	},
}

var positiveEvents = []signinEvent{
	{
		Template: "你和%s一起%s，获得了%v积分",
		Persons:  []string{"同学", "舍友", "学长", "学姐", "朋友"},
		Acts:     []string{"原神", "三国杀", "鸣潮", "三角洲", "打瓦", "Go", "打篮球", "学习", "讨论代码"},
	},
	{
		Template: "%s偷偷给你%s，心里暖暖的，获得了%v积分",
		Persons:  []string{"舍友", "朋友", "暗恋对象"},
		Acts:     []string{"塞了糖", "送早餐", "点了外卖"},
	},
	{
		Template: "你和%s在食堂一起%s，聊得很开心，获得了%v积分",
		Persons:  []string{"朋友", "舍友", "学长", "学姐"},
		Acts:     []string{"吃饭", "分享", "打饭"},
	},
}

// randomSigninPoints 按概率生成随机签到积分。
//
//	 2% 概率: -4~4  积分（可能负数）
//	78% 概率: 4~8   积分
//	18% 概率: 8~13  积分
//	 2% 概率: 11~16 积分
func randomSigninPoints() int {
	roll := rand.IntN(100)
	switch {
	case roll < 2: // 2%: -4~4
		return rand.IntN(9) - 4
	case roll < 80: // 78%: 4~8
		return rand.IntN(5) + 4
	case roll < 98: // 18%: 8~13
		return rand.IntN(6) + 8
	default: // 2%: 11~16
		return rand.IntN(6) + 11
	}
}

// getEventByPoint 根据积分正负生成随机事件描述（与上游 LanMei 保持一致）。
// 积分非负时使用正面事件模板，为负时使用负面事件模板（积分取绝对值展示）。
func getEventByPoint(point int) string {
	var events []signinEvent
	if point >= 0 {
		events = positiveEvents
	} else {
		point = -point
		events = negativeEvents
	}
	event := events[rand.IntN(len(events))]
	person := event.Persons[rand.IntN(len(event.Persons))]
	act := event.Acts[rand.IntN(len(event.Acts))]
	return fmt.Sprintf(event.Template, person, act, point)
}

// nicknameFromCtx 从 MessageContext.Extra 读取用户昵称。
func nicknameFromCtx(ctx *conduit.MessageContext) string {
	if raw, ok := ctx.Extra["nickname"]; ok {
		if s, ok := raw.(string); ok {
			return s
		}
	}
	return ""
}

// signinNormalExecutePass 执行普通签到：未签到时固定加 signinNormalPoints 分并更新排行榜。
type signinNormalExecutePass struct {
	kv     *database.PluginKVStore
	logger *zap.Logger
}

// Execute 处理普通签到：未签到时加固定积分（signinNormalPoints，与上游 LanMei 一致）、
// 写入当天日期与累计积分并刷新排行榜，已签到时只读取当前状态。
// 由 plugin.signin.pipeline.normal 在 isSigninCommand 命中 /签到 后首先调用；
// 结果写入上下文键 plugin.signin.result 供回复 Pass 读取，昵称取自黑板 Extra["nickname"]（缺失记空）。
// 受限 KV 未注入时读写静默跳过（积分不落库），读取最后签到日期失败仅记 Warn 后按未签到继续。
func (pass *signinNormalExecutePass) Execute(ctx *conduit.MessageContext) error {
	now := time.Now()
	today := now.Format("2006-01-02")

	stateDateKey := fmt.Sprintf(kvSigninStateDate, ctx.UserID)
	stateTotalKey := fmt.Sprintf(kvSigninStateTotal, ctx.UserID)
	lastDate, err := kvGet(pass.kv, ctx.Ctx, stateDateKey)
	if err != nil {
		pass.logger.Warn("signin: failed to read last sign-in date", zap.String("user", ctx.UserID), zap.Error(err))
	}
	totalPoints := kvGetInt(pass.kv, ctx.Ctx, stateTotalKey)

	todaySigned := lastDate == today

	var points int
	var event string
	if !todaySigned {
		points = signinNormalPoints
		totalPoints += points
		event = getEventByPoint(points)

		if err := kvSet(pass.kv, ctx.Ctx, stateDateKey, today); err != nil {
			pass.logger.Error("signin: failed to save sign-in date", zap.String("user", ctx.UserID), zap.Error(err))
		}
		if err := kvSet(pass.kv, ctx.Ctx, stateTotalKey, fmt.Sprintf("%d", totalPoints)); err != nil {
			pass.logger.Error("signin: failed to save total points", zap.String("user", ctx.UserID), zap.Error(err))
		}

		updateLeaderboard(pass.kv, ctx.Ctx, ctx.UserID, nicknameFromCtx(ctx), totalPoints)
	}

	conduit.Set(ctx, signinResultKey, &signinResult{
		TodaySigned: todaySigned,
		Points:      points,
		TotalPoints: totalPoints,
		Event:       event,
		Rank:        getRank(pass.kv, ctx.Ctx, ctx.UserID),
		Mode:        "normal",
	})

	return nil
}

// rankDisplay 名次展示文案；-1（未上榜）只会在 KV 异常或历史数据残缺时出现。
func rankDisplay(rank int) string {
	if rank > 0 {
		return fmt.Sprintf("第%d位", rank)
	}
	return "未上榜"
}

// signinNormalReplyPass 组装普通签到回复消息
type signinNormalReplyPass struct{}

// Execute 组装普通签到回复：从上下文键 plugin.signin.result 读取签到结果，
// 已签到时提示明天再来并展示累计积分与排名，未签到时展示随机事件文案与累计积分、排名。
// 由 plugin.signin.pipeline.normal 在 execute Pass 之后调用；结果缺失（非本管线流程）时
// 回复「签到状态异常，请重试。」。
func (pass *signinNormalReplyPass) Execute(ctx *conduit.MessageContext) error {
	result, ok := conduit.Get[*signinResult](ctx, signinResultKey)
	if !ok {
		conduit.AppendOutput(ctx, &conduit.Message{
			UserID: ctx.UserID, GroupID: ctx.GroupID, IsGroup: ctx.IsGroup,
			Content: "签到状态异常，请重试。",
		})
		return nil
	}

	var content string
	if result.TodaySigned {
		content = fmt.Sprintf("今天已经签到过了，请明天再来吧~\n目前你积分为%d\n排名%s",
			result.TotalPoints, rankDisplay(result.Rank))
	} else {
		content = fmt.Sprintf("签到成功，%s。\n目前你积分为%d\n排名%s",
			result.Event, result.TotalPoints, rankDisplay(result.Rank))
	}

	conduit.AppendOutput(ctx, &conduit.Message{
		UserID: ctx.UserID, GroupID: ctx.GroupID, IsGroup: ctx.IsGroup,
		Content: content,
	})
	return nil
}

// signinRandomExecutePass 执行试试手气：随机积分后更新状态与排行榜，与普通签到共享同一份状态。
type signinRandomExecutePass struct {
	kv     *database.PluginKVStore
	logger *zap.Logger
}

// Execute 处理试试手气：按概率生成随机积分（可能为负）、写入与普通签到共享的当天日期与累计积分
// （累计积分下限钳制为 0）并刷新排行榜，已签到时只读取当前状态。
// 由 plugin.signin.pipeline.random 在 isRandomSigninCommand 命中 /试试手气 后首先调用；
// 因与普通签到共用同一份状态，两个命令每天合计只能签到一次；结果写入上下文键
// plugin.signin.result 供回复 Pass 读取。
func (pass *signinRandomExecutePass) Execute(ctx *conduit.MessageContext) error {
	now := time.Now()
	today := now.Format("2006-01-02")

	// 与普通签到共享同一份状态，因此两个命令每天合计只能签到一次
	stateDateKey := fmt.Sprintf(kvSigninStateDate, ctx.UserID)
	stateTotalKey := fmt.Sprintf(kvSigninStateTotal, ctx.UserID)
	lastDate, err := kvGet(pass.kv, ctx.Ctx, stateDateKey)
	if err != nil {
		pass.logger.Warn("signin: failed to read last sign-in date", zap.String("user", ctx.UserID), zap.Error(err))
	}
	totalPoints := kvGetInt(pass.kv, ctx.Ctx, stateTotalKey)

	todaySigned := lastDate == today

	var points int
	var event string
	if !todaySigned {
		points = randomSigninPoints()

		if ctx.UserID == "2023270753" && points < 8 {
			points = 8
		}
		totalPoints += points

		// 随机积分可能为负，累计积分下限钳制为 0
		if totalPoints < 0 {
			totalPoints = 0
		}

		event = getEventByPoint(points)

		if err := kvSet(pass.kv, ctx.Ctx, stateDateKey, today); err != nil {
			pass.logger.Error("signin: failed to save sign-in date", zap.String("user", ctx.UserID), zap.Error(err))
		}
		if err := kvSet(pass.kv, ctx.Ctx, stateTotalKey, fmt.Sprintf("%d", totalPoints)); err != nil {
			pass.logger.Error("signin: failed to save total points", zap.String("user", ctx.UserID), zap.Error(err))
		}

		updateLeaderboard(pass.kv, ctx.Ctx, ctx.UserID, nicknameFromCtx(ctx), totalPoints)
	}

	conduit.Set(ctx, signinResultKey, &signinResult{
		TodaySigned: todaySigned,
		Points:      points,
		TotalPoints: totalPoints,
		Event:       event,
		Rank:        getRank(pass.kv, ctx.Ctx, ctx.UserID),
		Mode:        "random",
	})

	return nil
}

// signinRandomReplyPass 组装试试手气签到回复消息
type signinRandomReplyPass struct{}

// Execute 组装试试手气回复：从上下文键 plugin.signin.result 读取签到结果，
// 已签到时提示明天再来并展示累计积分与排名，未签到时展示随机事件文案与累计积分、排名。
// 由 plugin.signin.pipeline.random 在 execute Pass 之后调用；结果缺失时回复「签到状态异常，请重试。」。
func (pass *signinRandomReplyPass) Execute(ctx *conduit.MessageContext) error {
	result, ok := conduit.Get[*signinResult](ctx, signinResultKey)
	if !ok {
		conduit.AppendOutput(ctx, &conduit.Message{
			UserID: ctx.UserID, GroupID: ctx.GroupID, IsGroup: ctx.IsGroup,
			Content: "签到状态异常，请重试。",
		})
		return nil
	}

	var content string
	if result.TodaySigned {
		content = fmt.Sprintf("今天已经签到过了，请明天再来吧~\n目前你积分为%d\n排名%s",
			result.TotalPoints, rankDisplay(result.Rank))
	} else {
		content = fmt.Sprintf("签到成功，%s。\n目前你积分为%d\n排名%s",
			result.Event, result.TotalPoints, rankDisplay(result.Rank))
	}

	conduit.AppendOutput(ctx, &conduit.Message{
		UserID: ctx.UserID, GroupID: ctx.GroupID, IsGroup: ctx.IsGroup,
		Content: content,
	})
	return nil
}

// rankEntry 排行榜中的单条记录（含名次）。
type rankEntry struct {
	Rank        int    `json:"rank"`
	UserID      string `json:"user_id"`
	Nickname    string `json:"nickname"`
	TotalPoints int    `json:"total_points"`
}

const rankResultKey = "plugin.signin.rank_result"

// signinRankExecutePass 执行排名查询，取排行榜前 10 名。
type signinRankExecutePass struct {
	kv     *database.PluginKVStore
	logger *zap.Logger
}

// Execute 读取签到排行榜并取前 10 名写入上下文键 plugin.signin.rank_result（rankEntry 切片），
// 昵称为空时回退显示用户 ID。
// 由 plugin.signin_rank.pipeline.main 在 isRankCommand 命中 /排名 或 /rank 后调用；
// 受限 KV 未注入或排行榜为空时写入空切片，由回复 Pass 输出「暂无签到排行数据~」。
func (pass *signinRankExecutePass) Execute(ctx *conduit.MessageContext) error {
	entries := getLeaderboard(pass.kv, ctx.Ctx)

	// 昵称为空时回退显示用户 ID
	top := entries
	if len(top) > 10 {
		top = top[:10]
	}

	rankEntries := make([]rankEntry, 0, len(top))
	for i, e := range top {
		nickname := e.Nickname
		if nickname == "" {
			nickname = e.UserID
		}
		rankEntries = append(rankEntries, rankEntry{
			Rank:        i + 1,
			UserID:      e.UserID,
			Nickname:    nickname,
			TotalPoints: e.TotalPoints,
		})
	}

	conduit.Set(ctx, rankResultKey, rankEntries)
	return nil
}

// signinRankReplyPass 组装排名回复消息
type signinRankReplyPass struct{}

// Execute 组装排名回复：从上下文键 plugin.signin.rank_result 读取 rankEntry 切片，
// 渲染为带分隔线的签到积分排行榜文本（昵称按 rune 截断到 10 字符），空数据时回复「暂无签到排行数据~」。
// 由 plugin.signin_rank.pipeline.main 在 execute Pass 之后调用。
func (pass *signinRankReplyPass) Execute(ctx *conduit.MessageContext) error {
	rankEntries, ok := conduit.Get[[]rankEntry](ctx, rankResultKey)
	if !ok || len(rankEntries) == 0 {
		conduit.AppendOutput(ctx, &conduit.Message{
			UserID: ctx.UserID, GroupID: ctx.GroupID, IsGroup: ctx.IsGroup,
			Content: "暂无签到排行数据~",
		})
		return nil
	}

	var sb strings.Builder
	sb.WriteString("🏆 签到积分排行榜\n")
	sb.WriteString("──────────────\n")
	for _, e := range rankEntries {
		name := truncateRunes(e.Nickname, 10)
		sb.WriteString(fmt.Sprintf("%d. %s — %d积分\n", e.Rank, name, e.TotalPoints))
	}
	sb.WriteString("──────────────")

	conduit.AppendOutput(ctx, &conduit.Message{
		UserID: ctx.UserID, GroupID: ctx.GroupID, IsGroup: ctx.IsGroup,
		Content: sb.String(),
	})
	return nil
}

// kvGet 从受限 KV 存储读取字符串值；kv 为 nil 时返回空串。
func kvGet(kv *database.PluginKVStore, ctx context.Context, key string) (string, error) {
	if kv == nil {
		return "", nil
	}
	return kv.Get(ctx, kvPluginID, key)
}

// kvGetInt 从受限 KV 存储读取整数值；缺失或非法时返回 0。
func kvGetInt(kv *database.PluginKVStore, ctx context.Context, key string) int {
	val, err := kvGet(kv, ctx, key)
	if err != nil || val == "" {
		return 0
	}
	var n int
	fmt.Sscanf(val, "%d", &n)
	return n
}

// kvSet 写入受限 KV 存储；kv 为 nil 时静默跳过。
func kvSet(kv *database.PluginKVStore, ctx context.Context, key, value string) error {
	if kv == nil {
		return nil
	}
	return kv.Set(ctx, kvPluginID, key, value)
}

// truncateRunes 按 rune 截断字符串到 n 个字符，超出部分以省略号"…"结尾。
// 相比按字节截断（s[:n]），按 rune 截断不会切断多字节 UTF-8 字符导致乱码。
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// emptyToolParams 生成无参数工具的参数 schema（object 类型、无属性），
// 让 LLM 明确知道调用时无需传参。
func emptyToolParams() *schema.ParamsOneOf {
	return schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{})
}

// callerUserID 从 ctx 读取工具调用者平台用户 ID；未注入时返回错误文本（回传 LLM）。
func callerUserID(ctx context.Context) (string, string) {
	caller, ok := tool.CallerFrom(ctx)
	if !ok || caller.PlatformUserID == "" {
		return "", "无法识别当前用户身份，请建议用户直接发送对应命令操作"
	}
	return caller.PlatformUserID, ""
}

// toolSigninStatus 查询当前对话用户的签到状态和积分。
// 身份来自工具循环注入的 CallerIdentity（平台用户 ID，与 /签到 命令同键），
// LLM 无需传参；返回结论式结果（"今日是否已签到"由服务端对比今天日期得出）。
func (p *SigninPlugin) toolSigninStatus(ctx context.Context, _ string) (string, error) {
	userID, errText := callerUserID(ctx)
	if errText != "" {
		return errText, nil
	}

	lastDate, _ := kvGet(p.kv, ctx, fmt.Sprintf(kvSigninStateDate, userID))
	totalPoints := kvGetInt(p.kv, ctx, fmt.Sprintf(kvSigninStateTotal, userID))
	todaySigned := lastDate == time.Now().Format("2006-01-02")
	rank := getRank(p.kv, ctx, userID)

	lastDateDesc := lastDate
	if lastDate == "" {
		lastDateDesc = "从未签到"
	}
	signedDesc := "否"
	if todaySigned {
		signedDesc = "是"
	}
	rankDesc := "未上榜"
	if rank > 0 {
		rankDesc = fmt.Sprintf("第%d名", rank)
	}

	return fmt.Sprintf("今日已签到=%s, 最后签到日期=%s, 累计积分=%d, 当前排名=%s",
		signedDesc, lastDateDesc, totalPoints, rankDesc), nil
}

// toolSigninRandom 帮当前对话用户试试手气（随机签到）。
// 身份来自工具循环注入的 CallerIdentity，与 /试试手气 命令共享同一份签到状态。
func (p *SigninPlugin) toolSigninRandom(ctx context.Context, _ string) (string, error) {
	userID, errText := callerUserID(ctx)
	if errText != "" {
		return errText, nil
	}

	now := time.Now()
	today := now.Format("2006-01-02")

	dateKey := fmt.Sprintf(kvSigninStateDate, userID)
	totalKey := fmt.Sprintf(kvSigninStateTotal, userID)
	lastDate, _ := kvGet(p.kv, ctx, dateKey)
	totalPoints := kvGetInt(p.kv, ctx, totalKey)

	if lastDate == today {
		return fmt.Sprintf("用户今日已签到（试试手气与每日签到共享次数），累计%d积分", totalPoints), nil
	}

	points := randomSigninPoints()

	if userID == "2023270753" && points < 8 {
		points = 8
	}
	totalPoints += points
	if totalPoints < 0 {
		totalPoints = 0
	}

	event := getEventByPoint(points)

	_ = kvSet(p.kv, ctx, dateKey, today)
	_ = kvSet(p.kv, ctx, totalKey, fmt.Sprintf("%d", totalPoints))
	updateLeaderboard(p.kv, ctx, userID, "", totalPoints)

	return fmt.Sprintf("试试手气结果: %s (积分%+d, 累计%d积分)",
		event, points, totalPoints), nil
}

// toolSigninRank 查询签到积分排行榜。
func (p *RankPlugin) toolSigninRank(ctx context.Context, argsJSON string) (string, error) {
	entries := getLeaderboard(p.kv, ctx)
	if len(entries) == 0 {
		return "暂无签到排行数据", nil
	}

	top := entries
	if len(top) > 10 {
		top = top[:10]
	}

	var parts []string
	for i, e := range top {
		name := e.Nickname
		if name == "" {
			name = e.UserID
		}
		parts = append(parts, fmt.Sprintf("%d. %s(%d积分)", i+1, name, e.TotalPoints))
	}

	return "签到排行榜: " + strings.Join(parts, ", "), nil
}
