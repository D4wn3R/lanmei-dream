package antifraud

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestStoreListFiltersAndStats(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	records := []*Record{
		{Platform: "qq", GroupID: "g-1", UserID: "u-1", Content: "刷单广告", ContentHash: "h1",
			LabelKey: "brushing", LabelName: "刷单", ViolationScore: 0.95, Status: StatusAutoHandled,
			RecallStatus: ActionOK, MuteStatus: ActionOK, LastSeenAt: now},
		{Platform: "qq", GroupID: "g-1", UserID: "u-2", Content: "冒充客服退款", ContentHash: "h2",
			LabelKey: "impersonation_customer_service", LabelName: "冒充客服", ViolationScore: 0.72,
			Status: StatusPending, RecallStatus: ActionSkipped, MuteStatus: ActionSkipped, LastSeenAt: now},
		{Platform: "napcat", GroupID: "g-2", UserID: "u-3", Content: "贷款刷流水", ContentHash: "h3",
			LabelKey: "loan_credit", LabelName: "贷款征信", ViolationScore: 0.85, Status: StatusPending,
			RecallStatus: ActionFailed, RecallError: "retcode=100 msg=权限不足", MuteStatus: ActionSkipped, LastSeenAt: now},
	}
	for _, rec := range records {
		if err := store.Create(ctx, rec); err != nil {
			t.Fatalf("Create() error = %v", err)
		}
	}

	t.Run("按状态过滤", func(t *testing.T) {
		list, total, err := store.List(ctx, Filter{Status: StatusPending, Limit: 10})
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if total != 2 || len(list) != 2 {
			t.Fatalf("待审批记录数 = %d, want 2", total)
		}
	})

	t.Run("按平台与群过滤", func(t *testing.T) {
		_, total, err := store.List(ctx, Filter{Platform: "qq", GroupID: "g-1", Limit: 10})
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if total != 2 {
			t.Fatalf("qq/g-1 记录数 = %d, want 2", total)
		}
	})

	t.Run("关键字匹配内容", func(t *testing.T) {
		_, total, err := store.List(ctx, Filter{Keyword: "退款", Limit: 10})
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if total != 1 {
			t.Fatalf("关键字匹配数 = %d, want 1", total)
		}
	})

	t.Run("统计", func(t *testing.T) {
		stats, err := store.Stats(ctx)
		if err != nil {
			t.Fatalf("Stats() error = %v", err)
		}
		if stats.Pending != 2 || stats.TodayTotal != 3 {
			t.Fatalf("统计不正确：%+v", stats)
		}
		if stats.TodayAuto != 1 {
			t.Fatalf("今日处置数 = %d, want 1", stats.TodayAuto)
		}
		if stats.ActionFailed != 1 {
			t.Fatalf("动作失败数 = %d, want 1", stats.ActionFailed)
		}
		var buckets int64
		for _, b := range stats.ScoreBuckets {
			buckets += b.Count
		}
		if buckets != 3 {
			t.Fatalf("违规分分布合计 = %d, want 3", buckets)
		}
		if len(stats.LabelCounts) != 3 {
			t.Fatalf("类别计数条数 = %d, want 3", len(stats.LabelCounts))
		}
	})
}

func TestStoreFindRecentDuplicateAndTouchRepeat(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	rec := &Record{Platform: "qq", GroupID: "g-1", UserID: "u-1", Content: "刷单广告",
		ContentHash: "h1", Status: StatusPending, RepeatCount: 1, LastSeenAt: now}
	if err := store.Create(ctx, rec); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	dup, err := store.FindRecentDuplicate(ctx, "g-1", "u-1", "h1", now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("FindRecentDuplicate() error = %v", err)
	}
	if dup == nil || dup.ID != rec.ID {
		t.Fatalf("未命中重复记录：%+v", dup)
	}

	if dup, err := store.FindRecentDuplicate(ctx, "g-1", "u-1", "h1", now.Add(time.Minute)); err != nil || dup != nil {
		t.Fatalf("窗口外不应命中：dup=%+v err=%v", dup, err)
	}

	if err := store.TouchRepeat(ctx, rec.ID, now.Add(time.Second)); err != nil {
		t.Fatalf("TouchRepeat() error = %v", err)
	}
	updated, err := store.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if updated.RepeatCount != 2 {
		t.Fatalf("重复计数 = %d, want 2", updated.RepeatCount)
	}
}

func TestStoreDeleteBeforeKeepsPending(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	old := &Record{Platform: "qq", GroupID: "g-1", UserID: "u-1", Status: StatusApproved, ContentHash: "h1"}
	oldPending := &Record{Platform: "qq", GroupID: "g-1", UserID: "u-2", Status: StatusPending, ContentHash: "h2"}
	for _, rec := range []*Record{old, oldPending} {
		if err := store.Create(ctx, rec); err != nil {
			t.Fatalf("Create() error = %v", err)
		}
	}

	removed, err := store.DeleteBefore(ctx, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("DeleteBefore() error = %v", err)
	}
	if removed != 1 {
		t.Fatalf("删除行数 = %d, want 1（pending 不清理）", removed)
	}
	if _, err := store.Get(ctx, oldPending.ID); err != nil {
		t.Fatalf("待审批记录不应被删除：%v", err)
	}
	if _, err := store.Get(ctx, old.ID); !errors.Is(err, context.Canceled) && err == nil {
		t.Fatal("已终结记录应被删除")
	}
}

func TestStoreListAdminTargets(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	targets, err := store.ListAdminTargets(ctx)
	if err != nil {
		t.Fatalf("ListAdminTargets() error = %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("空表应返回空列表：%+v", targets)
	}
}
