package antifraud

import (
	"context"
	"encoding/json"
	"time"

	"go.uber.org/zap"
	"gorm.io/datatypes"

	"github.com/DaWesen/lanmei-dream/internal/gateway"
)

// applyTimeout 是单条记录「入库 + 处置 + 上报」的总预算。
const applyTimeout = 15 * time.Second

// apply 按判定结果处置一条消息：达阈值则入库（必要时合并重复）、执行自动处置并上报。
//
// 判定之后的全部工作（数据库、出站动作、通知）使用独立于消息生命周期的 context，
// 保证插件停止或消息超时也不会留下「已处置但无记录」的中间状态。
func (p *Plugin) apply(t task, v Verdict) {
	level := classify(v, p.th)
	if level == LevelNone {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), applyTimeout)
	defer cancel()

	// 合并窗口内同用户同文本的重复违规：只累加计数，不重复入库与打扰管理员；
	// 自动档仍会对新消息执行撤回，因此不会漏处置。
	if p.cfg.DedupWindowSeconds > 0 {
		since := time.Now().Add(-time.Duration(p.cfg.DedupWindowSeconds) * time.Second)
		dup, err := p.store.FindRecentDuplicate(ctx, t.groupID, t.userID, t.hash, since)
		if err != nil {
			p.logger.Warn("antifraud: 查询重复违规记录失败", zap.Error(err))
		} else if dup != nil {
			if err := p.store.TouchRepeat(ctx, dup.ID, t.seenAt); err != nil {
				p.logger.Warn("antifraud: 重复违规计数更新失败", zap.Error(err))
			}
			// 同步内存副本：后续 Save 是整行写入，若不同步会把刚自增的计数覆盖回旧值
			dup.RepeatCount++
			dup.LastSeenAt = t.seenAt
			if level == LevelAuto {
				// 合并的重复违规：撤回新消息但不再重复 @ 提示（同文本刚提示过，且用户通常已被禁言）
				p.executeActions(dup, t, actionPlan{Recall: true, MuteSeconds: p.cfg.MuteSeconds})
				if err := p.store.Save(ctx, dup); err != nil {
					p.logger.Warn("antifraud: 处置结果更新失败", zap.Error(err))
				}
			}
			return
		}
	}

	rec := &Record{
		Platform:       t.platform,
		ConnID:         t.connID,
		GroupID:        t.groupID,
		UserID:         t.userID,
		Nickname:       t.nickname,
		MessageID:      t.messageID,
		Content:        t.text,
		ContentHash:    t.hash,
		LabelID:        v.LabelID,
		LabelKey:       v.LabelKey,
		LabelName:      v.LabelName,
		Confidence:     v.Confidence,
		ViolationScore: v.ViolationScore,
		Probabilities:  marshalJSON(v.Probabilities),
		ExpertWeights:  marshalJSON(v.ExpertWeights),
		Status:         StatusPending,
		RepeatCount:    1,
		LastSeenAt:     t.seenAt,
		RecallStatus:   ActionSkipped,
		MuteStatus:     ActionSkipped,
	}
	if level == LevelAuto {
		rec.Status = StatusAutoHandled
	}
	if err := p.store.Create(ctx, rec); err != nil {
		// 记录写入失败仍继续处置：拦截当前消息优先于留痕，但必须显式告警
		p.logger.Error("antifraud: 违规记录写入失败，继续执行处置", zap.Error(err))
	}

	if level == LevelAuto {
		p.executeActions(rec, t, actionPlan{Recall: true, MuteSeconds: p.cfg.MuteSeconds, Announce: true})
		if rec.ID > 0 {
			if err := p.store.Save(ctx, rec); err != nil {
				p.logger.Warn("antifraud: 处置结果更新失败", zap.Error(err))
			}
		}
	}

	p.notify(ctx, rec)
}

// actionPlan 描述一次处置要执行的动作。
type actionPlan struct {
	// Recall 是否撤回原消息
	Recall bool
	// MuteSeconds 禁言时长（秒）；<=0 表示禁言
	MuteSeconds int
	// Announce 撤回成功后在群内 @ 该成员说明违规类型（合并的重复违规不再提示，避免刷屏）
	Announce bool
}

// executeActions 按计划执行撤回与禁言，并把平台返回的真实结果写入记录（失败原因原样保存）。
//
// 撤回目标是本次违规消息（t.messageID）而非记录里的首条消息：重复违规会合并记录，
// 但每条新消息都要各自撤回。禁言目标取记录的群与用户（同一记录内三者必然一致）。
// 动作使用独立 context 与超时：即使插件正在停止，也要把已判定的违规处置完整落库。
func (p *Plugin) executeActions(rec *Record, t task, plan actionPlan) {
	rec.RecallStatus, rec.RecallError = ActionSkipped, ""
	rec.MuteStatus, rec.MuteError, rec.MuteSeconds = ActionSkipped, "", 0
	if p.actions == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*actionTimeout)
	defer cancel()

	switch {
	case !plan.Recall:
		// 未要求撤回，保持 skipped
	case t.messageID == "":
		rec.RecallStatus, rec.RecallError = ActionFailed, "缺少消息ID"
	default:
		resp, err := p.actions.RecallMessage(ctx, t.platform, t.connID, t.messageID)
		rec.RecallStatus, rec.RecallError = actionOutcome(resp, err)
	}

	switch {
	case plan.MuteSeconds <= 0:
		// 未要求禁言，保持 skipped
	case !t.isGroup || rec.GroupID == "":
		rec.MuteStatus, rec.MuteError = ActionFailed, "非群消息，无法禁言"
	default:
		resp, err := p.actions.BanGroupMember(ctx, t.platform, t.connID, rec.GroupID, rec.UserID, plan.MuteSeconds)
		rec.MuteStatus, rec.MuteError = actionOutcome(resp, err)
		if rec.MuteStatus == ActionOK {
			rec.MuteSeconds = plan.MuteSeconds
		}
	}

	p.logger.Info("antifraud: 已执行处置",
		zap.Uint("record", rec.ID),
		zap.String("group", rec.GroupID),
		zap.String("user", rec.UserID),
		zap.String("recall", rec.RecallStatus),
		zap.String("mute", rec.MuteStatus),
	)

	// 群内告知放在动作之后：文案要如实反映是否禁言成功，且撤回已完成不会自相矛盾
	if plan.Announce && rec.RecallStatus == ActionOK {
		p.announceRecall(rec, t)
	}
}

// actionOutcome 把出站动作的响应与错误归一为 (状态, 失败原因)。
func actionOutcome(resp *gateway.ActionResponse, err error) (status, reason string) {
	if err != nil {
		return ActionFailed, truncateText(err.Error(), 255)
	}
	if resp.Failed() {
		return ActionFailed, truncateText(resp.FailureText(), 255)
	}
	return ActionOK, ""
}

// marshalJSON 把判定明细编码为 jsonb 列值；编码失败返回 nil（列可空，不阻断处置）。
func marshalJSON(v any) datatypes.JSON {
	payload, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return datatypes.JSON(payload)
}
