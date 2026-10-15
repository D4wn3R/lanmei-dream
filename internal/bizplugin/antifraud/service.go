package antifraud

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"
)

// ErrRecordNotFound 表示违规记录不存在（管理面板据此返回 404）。
var ErrRecordNotFound = errors.New("antifraud: 违规记录不存在")

// errActionsUnavailable 表示出站动作能力缺失（如网关未注入），无法执行处置。
var errActionsUnavailable = errors.New("antifraud: 出站动作能力不可用，无法执行撤回/禁言")

// ListRecords 分页查询违规记录。
func (p *Plugin) ListRecords(ctx context.Context, f Filter) ([]Record, int64, error) {
	if p.store == nil {
		return nil, 0, errors.New("antifraud: 插件尚未初始化")
	}
	return p.store.List(ctx, f)
}

// GetRecord 读取单条违规记录。
//
// 返回：记录不存在时返回 ErrRecordNotFound。
func (p *Plugin) GetRecord(ctx context.Context, id uint) (*Record, error) {
	if p.store == nil {
		return nil, errors.New("antifraud: 插件尚未初始化")
	}
	rec, err := p.store.Get(ctx, id)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrRecordNotFound
	}
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// Stats 返回面板概览统计。
func (p *Plugin) Stats(ctx context.Context) (Stats, error) {
	if p.store == nil {
		return Stats{}, errors.New("antifraud: 插件尚未初始化")
	}
	return p.store.Stats(ctx)
}

// ApprovalOptions 是审批通过时的处置选项。
type ApprovalOptions struct {
	// MuteSeconds 禁言时长（秒）；<=0 时使用插件配置的默认时长
	MuteSeconds int
	// Note 审批备注
	Note string
}

// Approve 审批通过：执行撤回与禁言并记录结果，返回更新后的记录。
//
// 参数：
//   - id：违规记录主键
//   - reviewer：面板管理员用户名
//
// 返回：记录不存在返回 ErrRecordNotFound；出站动作能力缺失返回错误（此时不改变记录状态）。
//
// 注意：对已通过的记录再次调用会重新下发动作，用于重试此前失败的撤回/禁言。
func (p *Plugin) Approve(ctx context.Context, id uint, reviewer string, opts ApprovalOptions) (*Record, error) {
	rec, err := p.GetRecord(ctx, id)
	if err != nil {
		return nil, err
	}
	if p.actions == nil {
		return nil, errActionsUnavailable
	}

	muteSeconds := opts.MuteSeconds
	if muteSeconds <= 0 {
		muteSeconds = p.cfg.MuteSeconds
	}
	p.executeActions(rec, taskFromRecord(rec), actionPlan{Recall: true, MuteSeconds: muteSeconds, Announce: true})

	rec.Status = StatusApproved
	rec.ReviewedBy = reviewer
	now := time.Now()
	rec.ReviewedAt = &now
	rec.ReviewNote = opts.Note
	if err := p.store.Save(ctx, rec); err != nil {
		return nil, err
	}
	p.logger.Info("antifraud: 违规记录审批通过",
		zap.Uint("record", rec.ID),
		zap.String("reviewer", reviewer),
		zap.String("recall", rec.RecallStatus),
		zap.String("mute", rec.MuteStatus),
	)
	return rec, nil
}

// Reject 审批驳回：标记为误报，不执行任何处置。
//
// 注意：若记录此前已被自动处置或审批通过，已执行的动作无法撤销，驳回只改变记录状态与备注。
func (p *Plugin) Reject(ctx context.Context, id uint, reviewer, note string) (*Record, error) {
	rec, err := p.GetRecord(ctx, id)
	if err != nil {
		return nil, err
	}
	rec.Status = StatusRejected
	rec.ReviewedBy = reviewer
	now := time.Now()
	rec.ReviewedAt = &now
	rec.ReviewNote = note
	if err := p.store.Save(ctx, rec); err != nil {
		return nil, err
	}
	p.logger.Info("antifraud: 违规记录已被驳回", zap.Uint("record", rec.ID), zap.String("reviewer", reviewer))
	return rec, nil
}

// DeleteRecord 删除一条违规记录（面板超管操作）。
func (p *Plugin) DeleteRecord(ctx context.Context, id uint) error {
	if p.store == nil {
		return errors.New("antifraud: 插件尚未初始化")
	}
	return p.store.Delete(ctx, id)
}

// taskFromRecord 由记录还原处置所需的消息上下文（审批与重试场景没有原始 task）。
func taskFromRecord(rec *Record) task {
	return task{
		platform:  rec.Platform,
		connID:    rec.ConnID,
		groupID:   rec.GroupID,
		userID:    rec.UserID,
		nickname:  rec.Nickname,
		messageID: rec.MessageID,
		text:      rec.Content,
		hash:      rec.ContentHash,
		isGroup:   rec.GroupID != "",
	}
}
