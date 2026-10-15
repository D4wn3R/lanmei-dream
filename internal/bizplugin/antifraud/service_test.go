package antifraud

import (
	"context"
	"errors"
	"testing"
	"time"
)

// pendingRecord 写入一条待审批记录（审批流程测试的起点）。
func pendingRecord(t *testing.T, store *Store) *Record {
	t.Helper()
	rec := &Record{
		Platform:       "qq",
		ConnID:         "conn-1",
		GroupID:        "g-1",
		UserID:         "u-1",
		Nickname:       "测试用户",
		MessageID:      "m-9",
		Content:        "兼职刷单日结，私聊我",
		ContentHash:    "h-9",
		LabelKey:       "brushing",
		LabelName:      "刷单",
		Confidence:     0.8,
		ViolationScore: 0.7,
		Status:         StatusPending,
		RepeatCount:    1,
		LastSeenAt:     time.Now(),
		RecallStatus:   ActionSkipped,
		MuteStatus:     ActionSkipped,
	}
	if err := store.Create(context.Background(), rec); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	return rec
}

func TestApproveExecutesActionsAndRecordsReview(t *testing.T) {
	actions := &fakeActions{}
	p, store := newTestPlugin(t, testConfig(""), actions)
	rec := pendingRecord(t, store)

	updated, err := p.Approve(context.Background(), rec.ID, "admin", ApprovalOptions{Note: "确认违规"})
	if err != nil {
		t.Fatalf("Approve() error = %v", err)
	}
	if updated.Status != StatusApproved || updated.ReviewedBy != "admin" || updated.ReviewNote != "确认违规" {
		t.Fatalf("审批信息未写入：%+v", updated)
	}
	if updated.RecallStatus != ActionOK || updated.MuteStatus != ActionOK || updated.MuteSeconds != 3600 {
		t.Fatalf("审批通过应执行撤回与禁言：%+v", updated)
	}
	if recalls, bans, _ := actions.snapshot(); recalls != 1 || bans != 1 {
		t.Fatalf("动作调用次数 recall=%d mute=%d, want 1/1", recalls, bans)
	}
	announces := actions.announceSnapshot()
	if len(announces) != 1 || announces[0][1] != "u-1" {
		t.Fatalf("审批通过应 @ 违规成员发一条群内告知：%v", announces)
	}
	mustContain(t, announces[0][2], "刷单")

	// 再次通过用于重试失败动作：允许重新下发
	if _, err := p.Approve(context.Background(), rec.ID, "admin", ApprovalOptions{}); err != nil {
		t.Fatalf("重复 Approve() error = %v", err)
	}
	if recalls, _, _ := actions.snapshot(); recalls != 2 {
		t.Fatalf("重复审批应重新下发动作，recall=%d", recalls)
	}
}

func TestApproveHonoursMuteSecondsOverride(t *testing.T) {
	actions := &fakeActions{}
	p, store := newTestPlugin(t, testConfig(""), actions)
	rec := pendingRecord(t, store)

	updated, err := p.Approve(context.Background(), rec.ID, "admin", ApprovalOptions{MuteSeconds: 60})
	if err != nil {
		t.Fatalf("Approve() error = %v", err)
	}
	if updated.MuteSeconds != 60 {
		t.Fatalf("禁言时长应使用覆盖值，实际 %d", updated.MuteSeconds)
	}
}

func TestRejectMarksRecordWithoutActions(t *testing.T) {
	actions := &fakeActions{}
	p, store := newTestPlugin(t, testConfig(""), actions)
	rec := pendingRecord(t, store)

	updated, err := p.Reject(context.Background(), rec.ID, "admin", "误报")
	if err != nil {
		t.Fatalf("Reject() error = %v", err)
	}
	if updated.Status != StatusRejected || updated.ReviewNote != "误报" || updated.ReviewedAt == nil {
		t.Fatalf("驳回信息未写入：%+v", updated)
	}
	if recalls, bans, sends := actions.snapshot(); recalls != 0 || bans != 0 || sends != 0 {
		t.Fatalf("驳回不应执行动作或通知：recall=%d mute=%d send=%d", recalls, bans, sends)
	}
	if announces := actions.announceSnapshot(); len(announces) != 0 {
		t.Fatalf("驳回不应发群内告知：%v", announces)
	}
}

func TestApproveUnknownRecordReturnsNotFound(t *testing.T) {
	p, _ := newTestPlugin(t, testConfig(""), &fakeActions{})
	if _, err := p.Approve(context.Background(), 9999, "admin", ApprovalOptions{}); !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("Approve() error = %v, want ErrRecordNotFound", err)
	}
	if _, err := p.Reject(context.Background(), 9999, "admin", ""); !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("Reject() error = %v, want ErrRecordNotFound", err)
	}
}
