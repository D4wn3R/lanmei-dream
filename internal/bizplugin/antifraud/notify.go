package antifraud

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"
)

// announceRecall 在违规所在群 @ 该成员，说明违规类型与处置结果。
//
// 只在撤回成功后调用（见 actionPlan.Announce）且只对群聊生效：私聊没有可 @ 的目标，
// 撤回失败时也不公开指控。文案不含违规内容，避免二次传播；发送失败只记日志，不影响处置。
func (p *Plugin) announceRecall(rec *Record, t task) {
	if p.actions == nil || !t.isGroup || rec.GroupID == "" || rec.UserID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), actionTimeout)
	defer cancel()
	if err := p.actions.SendAtTextTo(ctx, t.platform, t.connID, rec.GroupID, rec.UserID, recallNoticeText(rec)); err != nil {
		p.logger.Warn("antifraud: 群内违规告知发送失败",
			zap.Uint("record", rec.ID), zap.String("group", rec.GroupID), zap.Error(err))
	}
}

// recallNoticeText 生成群内违规告知文案：只说类别与处置，不复述违规内容。
//
// 语气贴合机器人人设（简短口语 + 最多一个颜文字，不用 emoji 与 Markdown）。
func recallNoticeText(rec *Record) string {
	label := labelText(rec)
	if rec.MuteStatus == ActionOK {
		return fmt.Sprintf("这条「%s」消息蓝妹撤回啦，先去冷静 %s 吧(￣ω￣;)", label, formatDuration(rec.MuteSeconds))
	}
	return fmt.Sprintf("这条「%s」消息蓝妹撤回啦，注意一下哦", label)
}

// notify 上报违规记录：私聊超管（静态配置 + bot_admin 动态管理员），并按配置发到告警群。
//
// 单个目标发送失败只记日志：上报是提醒手段，不能因某个管理员不可达而中断处置流程。
func (p *Plugin) notify(ctx context.Context, rec *Record) {
	if p.actions == nil {
		return
	}
	text := p.formatNotice(rec)
	delivered := false

	for _, target := range p.adminTargets(ctx) {
		connID := rec.ConnID
		if target.Platform != rec.Platform {
			connID = "" // 跨平台目标不带来源连接，由网关按平台选择连接
		}
		sendCtx, cancel := context.WithTimeout(ctx, actionTimeout)
		err := p.actions.SendTextTo(sendCtx, target.Platform, connID, false, target.UserID, text)
		cancel()
		if err != nil {
			p.logger.Warn("antifraud: 违规记录私聊上报失败",
				zap.String("platform", target.Platform), zap.String("user", target.UserID), zap.Error(err))
			continue
		}
		delivered = true
	}

	if p.cfg.NoticeGroupID != "" {
		sendCtx, cancel := context.WithTimeout(ctx, actionTimeout)
		err := p.actions.SendTextTo(sendCtx, rec.Platform, rec.ConnID, true, p.cfg.NoticeGroupID, text)
		cancel()
		if err != nil {
			p.logger.Warn("antifraud: 违规记录告警群上报失败",
				zap.String("group", p.cfg.NoticeGroupID), zap.Error(err))
		} else {
			delivered = true
		}
	}

	if delivered && rec.ID > 0 {
		if err := p.store.MarkNotified(ctx, rec.ID, time.Now()); err != nil {
			p.logger.Warn("antifraud: 上报时间写入失败", zap.Error(err))
		}
	}
}

// notifyOutage 上报推理服务失联（每个失联周期只发一次，见 noteFailure）。
func (p *Plugin) notifyOutage(cause error, open time.Duration) {
	if p.actions == nil {
		return
	}
	text := fmt.Sprintf("【反诈告警】判定服务连续失败，已暂停调用 %s，期间消息不再判定。\n原因：%s",
		open, truncateText(cause.Error(), 200))

	ctx, cancel := context.WithTimeout(context.Background(), 3*actionTimeout)
	defer cancel()

	for _, target := range p.adminTargets(ctx) {
		sendCtx, cancelSend := context.WithTimeout(ctx, actionTimeout)
		err := p.actions.SendTextTo(sendCtx, target.Platform, "", false, target.UserID, text)
		cancelSend()
		if err != nil {
			p.logger.Warn("antifraud: 服务失联上报失败",
				zap.String("platform", target.Platform), zap.String("user", target.UserID), zap.Error(err))
		}
	}
	if p.cfg.NoticeGroupID == "" {
		return
	}
	sendCtx, cancelSend := context.WithTimeout(ctx, actionTimeout)
	defer cancelSend()
	if err := p.actions.SendTextTo(sendCtx, "", "", true, p.cfg.NoticeGroupID, text); err != nil {
		p.logger.Warn("antifraud: 服务失联告警群上报失败", zap.String("group", p.cfg.NoticeGroupID), zap.Error(err))
	}
}

// AdminTarget 是一个上报目标（平台 + 用户ID）。
type AdminTarget struct {
	Platform string
	UserID   string
}

// adminTargets 汇总上报目标：静态超管配置与 bot_admin 动态管理员，按平台去重后排序。
func (p *Plugin) adminTargets(ctx context.Context) []AdminTarget {
	var targets []AdminTarget
	seen := make(map[string]struct{})

	add := func(platform, userID string) {
		if platform == "" || userID == "" {
			return
		}
		key := platform + ":" + userID
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		targets = append(targets, AdminTarget{Platform: platform, UserID: userID})
	}

	for platform, ids := range p.superUsers {
		for _, id := range ids {
			add(platform, id)
		}
	}
	if p.store != nil {
		admins, err := p.store.ListAdminTargets(ctx)
		if err != nil {
			p.logger.Warn("antifraud: 动态管理员查询失败", zap.Error(err))
		}
		for _, admin := range admins {
			add(admin.Platform, admin.UserID)
		}
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].Platform != targets[j].Platform {
			return targets[i].Platform < targets[j].Platform
		}
		return targets[i].UserID < targets[j].UserID
	})
	return targets
}

// formatNotice 生成违规上报文案。
func (p *Plugin) formatNotice(rec *Record) string {
	var b strings.Builder
	b.WriteString("【反诈告警】")
	if rec.Status == StatusAutoHandled {
		b.WriteString("已自动处置")
	} else {
		b.WriteString("待审批")
	}
	fmt.Fprintf(&b, "\n类别：%s（置信度 %.2f，违规分 %.2f）", labelText(rec), rec.Confidence, rec.ViolationScore)
	if rec.GroupID != "" {
		fmt.Fprintf(&b, "\n群：%s", rec.GroupID)
	}
	fmt.Fprintf(&b, "\n用户：%s（%s）", nicknameText(rec), rec.UserID)
	fmt.Fprintf(&b, "\n内容：%s", truncateRunes(singleLine(rec.Content), 200))
	fmt.Fprintf(&b, "\n处置：%s；%s", recallText(rec), muteText(rec))
	if rec.RepeatCount > 1 {
		fmt.Fprintf(&b, "\n重复：同文本违规 %d 次", rec.RepeatCount)
	}
	if link := p.recordURL(rec); link != "" {
		fmt.Fprintf(&b, "\n详情：%s", link)
	}
	return b.String()
}

// labelText 返回违规类别名（缺失时回退类别标识）。
func labelText(rec *Record) string {
	if rec.LabelName != "" {
		return rec.LabelName
	}
	if rec.LabelKey != "" {
		return rec.LabelKey
	}
	return "未知类别"
}

// nicknameText 返回发送者展示名（缺失时回退用户 ID）。
func nicknameText(rec *Record) string {
	if strings.TrimSpace(rec.Nickname) != "" {
		return rec.Nickname
	}
	return rec.UserID
}

// recallText 描述撤回动作结果。
func recallText(rec *Record) string {
	switch rec.RecallStatus {
	case ActionOK:
		return "已撤回"
	case ActionFailed:
		return "撤回失败（" + rec.RecallError + "）"
	default:
		return "未撤回"
	}
}

// muteText 描述禁言动作结果。
func muteText(rec *Record) string {
	switch rec.MuteStatus {
	case ActionOK:
		return fmt.Sprintf("已禁言 %s", formatDuration(rec.MuteSeconds))
	case ActionFailed:
		return "禁言失败（" + rec.MuteError + "）"
	default:
		return "未禁言"
	}
}

// formatDuration 把秒数格式化为「X 秒 / X 分钟 / X 小时」。
func formatDuration(seconds int) string {
	switch {
	case seconds <= 0:
		return "0 秒"
	case seconds%3600 == 0:
		return fmt.Sprintf("%d 小时", seconds/3600)
	case seconds%60 == 0:
		return fmt.Sprintf("%d 分钟", seconds/60)
	default:
		return fmt.Sprintf("%d 秒", seconds)
	}
}

// recordURL 生成面板中的记录详情链接（未配置 panel_base_url 时返回空串）。
func (p *Plugin) recordURL(rec *Record) string {
	if p.cfg.PanelBaseURL == "" || rec.ID == 0 {
		return ""
	}
	base := strings.TrimRight(p.cfg.PanelBaseURL, "/")
	return fmt.Sprintf("%s/#/antifraud?id=%d", base, rec.ID)
}
