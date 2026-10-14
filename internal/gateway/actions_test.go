package gateway

import (
	"context"
	"testing"
	"time"
)

// TestBuildModerationActions 覆盖撤回与禁言的协议差异：OneBot 11 用数字 ID，OneBot 12 用字符串 ID。
func TestBuildModerationActions(t *testing.T) {
	t.Run("OneBot 12", func(t *testing.T) {
		del := BuildDeleteMsgV12("123456")
		if del.Action != "delete_message" || del.Params["message_id"] != "123456" {
			t.Fatalf("delete_message 参数不正确：%+v", del)
		}
		ban := BuildSetGroupBanV12("g1", "u1", 300)
		if ban.Action != "set_group_ban" || ban.Params["group_id"] != "g1" ||
			ban.Params["user_id"] != "u1" || ban.Params["duration"] != 300 {
			t.Fatalf("set_group_ban 参数不正确：%+v", ban)
		}
	})

	t.Run("OneBot 11", func(t *testing.T) {
		del := BuildDeleteMsgV11("123456")
		if del.Action != "delete_msg" || del.Params["message_id"] != int64(123456) {
			t.Fatalf("delete_msg 参数不正确：%+v", del)
		}
		ban := BuildSetGroupBanV11("10001", "20002", 60)
		if ban.Action != "set_group_ban" || ban.Params["group_id"] != int64(10001) ||
			ban.Params["user_id"] != int64(20002) || ban.Params["duration"] != 60 {
			t.Fatalf("set_group_ban 参数不正确：%+v", ban)
		}
	})

	t.Run("非法 ID 按 0 下发", func(t *testing.T) {
		ban := BuildSetGroupBanV11("", "abc", 60)
		if ban.Params["group_id"] != int64(0) || ban.Params["user_id"] != int64(0) {
			t.Fatalf("非法 ID 应回退为 0：%+v", ban)
		}
	})
}

// TestHubSendToWaitCleansUpWaiter 连接不存在时立即返回错误，且不残留等待者。
func TestHubSendToWaitCleansUpWaiter(t *testing.T) {
	hub := NewHub()
	if _, err := hub.SendToWait("missing", BuildDeleteMsgV12("1"), 20*time.Millisecond); err == nil {
		t.Fatal("连接不存在时应返回错误")
	}
	hub.pendingMu.Lock()
	pending := len(hub.pending)
	hub.pendingMu.Unlock()
	if pending != 0 {
		t.Fatalf("等待者未清理，pending = %d", pending)
	}
}

// TestHubResolveActionResponse 覆盖动作响应配对：投递给等待者、重复响应丢弃、无等待者返回 false。
func TestHubResolveActionResponse(t *testing.T) {
	hub := NewHub()
	ch := make(chan ActionResponse, 1)
	hub.pendingMu.Lock()
	hub.pending = map[string]chan ActionResponse{"echo-1": ch} // 等价于 SendToWait 的登记
	hub.pendingMu.Unlock()

	if !hub.ResolveActionResponse("echo-1", &ActionResponse{Status: "ok"}) {
		t.Fatal("存在等待者时应返回 true")
	}
	select {
	case resp := <-ch:
		if resp.Status != "ok" {
			t.Fatalf("响应内容不正确：%+v", resp)
		}
	default:
		t.Fatal("响应未投递给等待者")
	}

	// 等待者已超时/已注销后到达的重复回包：返回 false 且不阻塞
	hub.pendingMu.Lock()
	delete(hub.pending, "echo-1")
	hub.pendingMu.Unlock()
	if hub.ResolveActionResponse("echo-1", &ActionResponse{Status: "failed"}) {
		t.Fatal("等待者已注销时重复响应应返回 false")
	}

	if hub.ResolveActionResponse("", &ActionResponse{Status: "ok"}) {
		t.Fatal("空 echo 应返回 false")
	}
	if hub.ResolveActionResponse("unknown", &ActionResponse{Status: "ok"}) {
		t.Fatal("无等待者应返回 false")
	}
	if hub.ResolveActionResponse("echo-1", nil) {
		t.Fatal("空响应应返回 false")
	}
}

// TestActionsResolve 覆盖连接定位：优先 connID，失效时按平台回退，无可用连接时报错。
func TestActionsResolve(t *testing.T) {
	hub := NewHub()
	hub.Register(&Connection{ID: "conn-qq", Platform: PlatformQQ})
	hub.Register(&Connection{ID: "conn-napcat", Platform: PlatformNapCat})
	actions := NewActions(hub)

	t.Run("优先使用 connID", func(t *testing.T) {
		conn, err := actions.resolve(string(PlatformQQ), "conn-napcat")
		if err != nil || conn.ID != "conn-napcat" {
			t.Fatalf("resolve() = %v, %v", conn, err)
		}
	})

	t.Run("connID 失效时按平台回退", func(t *testing.T) {
		conn, err := actions.resolve(string(PlatformNapCat), "gone")
		if err != nil || conn.ID != "conn-napcat" {
			t.Fatalf("resolve() = %v, %v", conn, err)
		}
	})

	t.Run("无同平台连接时报错", func(t *testing.T) {
		if _, err := actions.resolve(string(PlatformTelegram), "gone"); err == nil {
			t.Fatal("无可用连接时应返回错误")
		}
	})

	t.Run("ctx 已取消时不发起动作", func(t *testing.T) {
		if _, err := actions.waitTimeout(canceledContext()); err == nil {
			t.Fatal("ctx 已取消时应返回错误")
		}
	})
}

// canceledContext 返回一个已取消的 context，用于验证动作前置检查。
func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
