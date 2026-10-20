package bizplugin

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/DaWesen/lanmei-dream/internal/database"
	"github.com/DaWesen/lanmei-dream/internal/model"
)

// newSigninTestKV 创建独立的 SQLite 受限 KV 存储并建表。
func newSigninTestKV(t *testing.T) *database.PluginKVStore {
	t.Helper()
	orm, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "signin.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := orm.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	if err := orm.AutoMigrate(&model.PluginKV{}); err != nil {
		t.Fatalf("AutoMigrate(plugin_kv) error = %v", err)
	}
	return database.NewPluginKVStore(orm)
}

// TestUpdateLeaderboard_NoTruncation 排行榜全量保留：超过 100 名后所有用户仍在榜且排名正确。
func TestUpdateLeaderboard_NoTruncation(t *testing.T) {
	kv := newSigninTestKV(t)
	ctx := context.Background()

	// 150 个用户按积分递增签到，验证不截断在 100 名
	for i := 1; i <= 150; i++ {
		updateLeaderboard(kv, ctx, userIDA(i), "用户"+userIDA(i), i)
	}

	entries := getLeaderboard(kv, ctx)
	if len(entries) != 150 {
		t.Fatalf("排行榜条目数 = %d, want 150（不应截断）", len(entries))
	}

	// 每个用户都应查到真实排名：积分 i 的用户排名 = 151 - i
	for i := 1; i <= 150; i++ {
		if got := getRank(kv, ctx, userIDA(i)); got != 151-i {
			t.Fatalf("用户 %s 排名 = %d, want %d", userIDA(i), got, 151-i)
		}
	}
}

// TestUpdateLeaderboard_TieBreakStable 同分用户按 ID 稳定排序，多次更新结果一致。
func TestUpdateLeaderboard_TieBreakStable(t *testing.T) {
	kv := newSigninTestKV(t)
	ctx := context.Background()

	// 三个同分用户，更新顺序与 ID 顺序相反
	updateLeaderboard(kv, ctx, "300", "c", 8)
	updateLeaderboard(kv, ctx, "200", "b", 8)
	updateLeaderboard(kv, ctx, "100", "a", 8)

	entries := getLeaderboard(kv, ctx)
	wantIDs := []string{"100", "200", "300"}
	if len(entries) != len(wantIDs) {
		t.Fatalf("排行榜条目数 = %d, want %d", len(entries), len(wantIDs))
	}
	for i, want := range wantIDs {
		if entries[i].UserID != want {
			t.Fatalf("同分排序位置 %d = %s, want %s（应按用户 ID 稳定排序）", i+1, entries[i].UserID, want)
		}
		if got := getRank(kv, ctx, want); got != i+1 {
			t.Fatalf("用户 %s 排名 = %d, want %d", want, got, i+1)
		}
	}
}

// TestGetRank_NotOnBoard 未上榜返回 -1。
func TestGetRank_NotOnBoard(t *testing.T) {
	kv := newSigninTestKV(t)

	if got := getRank(kv, context.Background(), "404"); got != -1 {
		t.Fatalf("未上榜用户排名 = %d, want -1", got)
	}
}

// TestRankDisplay 回复文案：正排名展示名次，-1 兜底为未上榜。
func TestRankDisplay(t *testing.T) {
	if got := rankDisplay(1); got != "第1位" {
		t.Errorf("rankDisplay(1) = %q, want %q", got, "第1位")
	}
	if got := rankDisplay(120); got != "第120位" {
		t.Errorf("rankDisplay(120) = %q, want %q", got, "第120位")
	}
	if got := rankDisplay(-1); got != "未上榜" {
		t.Errorf("rankDisplay(-1) = %q, want %q", got, "未上榜")
	}
}

// userIDA 生成数字递增的用户 ID 字符串。
func userIDA(i int) string {
	return "u" + string(rune('0'+i/10)) + string(rune('0'+i%10))
}
