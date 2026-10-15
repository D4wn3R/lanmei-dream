package antifraud

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DaWesen/lanmei-dream/internal/gateway"
)

// TestPipelineAutoDisposition 覆盖完整异步链路：观察者入队 → 批量判定 → 自动撤回+禁言 → 入库+上报。
func TestPipelineAutoDisposition(t *testing.T) {
	srv := fakeInferService(t, func(string) InferItem { return violationItem(nil) })
	actions := &fakeActions{}
	p, store := newTestPlugin(t, testConfig(srv.URL), actions)
	if err := p.OnStart(nil); err != nil {
		t.Fatalf("OnStart() error = %v", err)
	}
	defer func() { _ = p.OnStop(nil) }()

	p.ObserveMessage(testMessageContext("2002", "g-1", "点击链接领取红包，逾期冻结账户"))

	rec := waitForRecord(t, store, func(rec *Record) bool {
		return rec.Status == StatusAutoHandled && rec.RecallStatus == ActionOK
	})
	if rec.LabelKey != "brushing" || rec.ViolationScore < 0.9 {
		t.Fatalf("记录判定字段不正确：%+v", rec)
	}
	if rec.MuteStatus != ActionOK || rec.MuteSeconds != 3600 {
		t.Fatalf("自动禁言未生效：%+v", rec)
	}
	recalls, bans, sends := actions.snapshot()
	if recalls != 1 || bans != 1 {
		t.Fatalf("动作调用次数 recall=%d mute=%d, want 1/1", recalls, bans)
	}
	if sends == 0 {
		t.Fatal("自动处置应上报管理员")
	}
	announces := actions.announceSnapshot()
	if len(announces) != 1 {
		t.Fatalf("撤回成功应发一条群内告知，实际 %d 条", len(announces))
	}
	if announces[0][0] != "g-1" || announces[0][1] != "2002" {
		t.Fatalf("群内告知应 @ 违规成员本人：%v", announces[0])
	}
	mustContain(t, announces[0][2], "刷单")
	mustContain(t, announces[0][2], "1 小时")
	if strings.Contains(announces[0][2], "点击链接") {
		t.Fatalf("群内告知不得复述违规内容：%s", announces[0][2])
	}
}

// TestPipelineReportLevelWaitsForApproval 中置信度只入库并上报，不执行动作。
func TestPipelineReportLevelWaitsForApproval(t *testing.T) {
	srv := fakeInferService(t, func(string) InferItem {
		return violationItem([]float64{0.3, 0, 0, 0.7, 0, 0, 0, 0, 0, 0})
	})
	actions := &fakeActions{}
	p, store := newTestPlugin(t, testConfig(srv.URL), actions)
	if err := p.OnStart(nil); err != nil {
		t.Fatalf("OnStart() error = %v", err)
	}
	defer func() { _ = p.OnStop(nil) }()

	p.ObserveMessage(testMessageContext("2002", "g-1", "兼职刷单日结，私聊我"))

	rec := waitForRecord(t, store, func(rec *Record) bool { return rec.Status == StatusPending })
	recalls, bans, sends := actions.snapshot()
	if recalls != 0 || bans != 0 {
		t.Fatalf("未达自动阈值不应执行动作：recall=%d mute=%d", recalls, bans)
	}
	if sends == 0 {
		t.Fatal("待审批记录应上报管理员")
	}
	if rec.ReviewNote != "" || rec.ReviewedAt != nil {
		t.Fatal("待审批记录不应有审批信息")
	}
	if announces := actions.announceSnapshot(); len(announces) != 0 {
		t.Fatalf("未处置不应发群内告知：%v", announces)
	}
}

// TestPipelineSkipsNormalContent 正常内容不入库、不通知。
func TestPipelineSkipsNormalContent(t *testing.T) {
	srv := fakeInferService(t, func(string) InferItem { return normalItem() })
	actions := &fakeActions{}
	p, store := newTestPlugin(t, testConfig(srv.URL), actions)
	if err := p.OnStart(nil); err != nil {
		t.Fatalf("OnStart() error = %v", err)
	}
	defer func() { _ = p.OnStop(nil) }()

	p.ObserveMessage(testMessageContext("2002", "g-1", "大家晚上好啊"))
	time.Sleep(300 * time.Millisecond)

	list, total, err := store.List(context.Background(), Filter{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if total != 0 || len(list) != 0 {
		t.Fatalf("正常内容不应入库：total=%d", total)
	}
	if _, _, sends := actions.snapshot(); sends != 0 {
		t.Fatal("正常内容不应上报")
	}
}

// TestPipelineDeduplicatesRepeatedText 同用户同文本在合并窗口内只留一条记录并累加计数。
func TestPipelineDeduplicatesRepeatedText(t *testing.T) {
	srv := fakeInferService(t, func(string) InferItem { return violationItem(nil) })
	actions := &fakeActions{}
	p, store := newTestPlugin(t, testConfig(srv.URL), actions)
	if err := p.OnStart(nil); err != nil {
		t.Fatalf("OnStart() error = %v", err)
	}
	defer func() { _ = p.OnStop(nil) }()

	msgA := testMessageContext("2002", "g-1", "点击链接领取红包")
	msgB := testMessageContext("2002", "g-1", "点击链接领取红包")
	msgB.Extra["message_id"] = "m-2"
	p.ObserveMessage(msgA)
	p.ObserveMessage(msgB)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		list, _, err := store.List(context.Background(), Filter{})
		if err == nil && len(list) > 0 && list[0].RepeatCount >= 2 {
			if len(list) != 1 {
				t.Fatalf("重复文本应合并为一条记录，实际 %d 条", len(list))
			}
			if _, bans, _ := actions.snapshot(); bans != 2 {
				t.Fatalf("每条重复消息都应执行禁言，实际 %d 次", bans)
			}
			if announces := actions.announceSnapshot(); len(announces) != 1 {
				t.Fatalf("合并的重复违规只应提示一次，实际 %d 次", len(announces))
			}
			// 合并记录后仍要各自撤回：撤回目标必须是本条消息，而不是记录里的首条消息
			actions.mu.Lock()
			recalls := append([]string(nil), actions.recalls...)
			actions.mu.Unlock()
			if len(recalls) != 2 || recalls[0] != "m-1" || recalls[1] != "m-2" {
				t.Fatalf("撤回目标不正确：%v", recalls)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	list, _, _ := store.List(context.Background(), Filter{})
	t.Fatalf("等待重复记录合并超时：%+v", list)
}

// TestPipelineRecordsActionFailures 平台动作失败时原样记录原因，不影响入库与上报。
func TestPipelineRecordsActionFailures(t *testing.T) {
	srv := fakeInferService(t, func(string) InferItem { return violationItem(nil) })
	actions := &fakeActions{
		recallResp: &gateway.ActionResponse{Status: "failed", RetCode: 100, Message: "权限不足"},
	}
	p, store := newTestPlugin(t, testConfig(srv.URL), actions)
	if err := p.OnStart(nil); err != nil {
		t.Fatalf("OnStart() error = %v", err)
	}
	defer func() { _ = p.OnStop(nil) }()

	p.ObserveMessage(testMessageContext("2002", "g-1", "点击链接领取红包"))

	rec := waitForRecord(t, store, func(rec *Record) bool { return rec.RecallStatus == ActionFailed })
	if rec.RecallError != "retcode=100 msg=权限不足" {
		t.Fatalf("失败原因未原样记录：%q", rec.RecallError)
	}
	if rec.Status != StatusAutoHandled {
		t.Fatalf("动作失败不影响记录状态：%s", rec.Status)
	}
	if announces := actions.announceSnapshot(); len(announces) != 0 {
		t.Fatalf("撤回失败时不应在群里公开指控：%v", announces)
	}
}

// TestCircuitBreakerDropsBatches 连续失败达到阈值后熔断，期间不再调用推理服务。
func TestCircuitBreakerDropsBatches(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	p, _ := newTestPlugin(t, testConfig(srv.URL), &fakeActions{})
	for i := 0; i < p.cfg.CircuitFailures; i++ {
		p.judge(context.Background(), []task{testTask("2002", "文本内容")}, []string{"文本内容"}, map[string]int{"文本内容": 0})
	}
	if p.openUntil.Load() == 0 {
		t.Fatal("连续失败后应进入熔断")
	}

	before := hits
	p.flush(context.Background(), []task{testTask("2002", "另一段文本")})
	if hits != before {
		t.Fatal("熔断期间不应再调用推理服务")
	}
}

// TestApplyMergesDuplicateWithinWindow 覆盖合并分支的边界：窗口为 0 时不做合并。
func TestApplyWithoutDedupWindow(t *testing.T) {
	actions := &fakeActions{}
	cfg := testConfig("")
	cfg.DedupWindowSeconds = 0
	cfg.MuteSeconds = 0
	p, store := newTestPlugin(t, cfg, actions)

	v := toVerdict(violationItem(nil))
	p.apply(testTask("2002", "点击链接领取红包"), v)
	p.apply(testTask("2002", "点击链接领取红包"), v)

	_, total, err := store.List(context.Background(), Filter{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if total != 2 {
		t.Fatalf("关闭合并窗口后应逐条入库，total = %d", total)
	}
	if _, bans, _ := actions.snapshot(); bans != 0 {
		t.Fatal("禁言时长配置为 0 时不应禁言")
	}
}
