package intent

// Analyzer 的分发与公共逻辑测试：命令/工具快照透传、超时包裹、降级语义。

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// recordingAdapter 记录 Judge 入参并回放预设结果，验证 Analyzer 的委托行为。
type recordingAdapter struct {
	calls     int
	userMsg   string
	judgeCtx  *JudgeContext
	commands  []CommandDef
	tools     []ToolDef
	ctxHasDDL bool
	result    *Result
	err       error
}

func (r *recordingAdapter) Judge(ctx context.Context, userMsg string, judgeCtx *JudgeContext, commands []CommandDef, tools []ToolDef) (*Result, error) {
	r.calls++
	r.userMsg = userMsg
	r.judgeCtx = judgeCtx
	r.commands = commands
	r.tools = tools
	_, r.ctxHasDDL = ctx.Deadline()
	if r.err != nil {
		return nil, r.err
	}
	return r.result, nil
}

func TestAnalyzerDelegatesAndSnapshots(t *testing.T) {
	rec := &recordingAdapter{result: &Result{Intent: IntentCommand, CommandName: "签到", Confidence: 0.9}}
	analyzer := NewAnalyzer(rec,
		[]CommandDef{{Name: "签到", Description: "每日签到"}},
		[]ToolDef{{Name: "weather", Description: "天气"}}, 0)

	res, err := analyzer.Analyze(context.Background(), "帮我签到", nil)
	if err != nil {
		t.Fatalf("Analyze 返回错误: %v", err)
	}
	if res != rec.result {
		t.Fatalf("结果未透传: %+v", res)
	}
	if rec.calls != 1 || rec.userMsg != "帮我签到" {
		t.Fatalf("入参未透传: calls=%d userMsg=%q", rec.calls, rec.userMsg)
	}
	if !reflect.DeepEqual(rec.commands, []CommandDef{{Name: "签到", Description: "每日签到"}}) {
		t.Fatalf("命令快照不符合: %+v", rec.commands)
	}
	if !reflect.DeepEqual(rec.tools, []ToolDef{{Name: "weather", Description: "天气"}}) {
		t.Fatalf("工具快照不符合: %+v", rec.tools)
	}

	// 刷新命令/工具后，下次调用应拿到新快照
	analyzer.UpdateCommands([]CommandDef{{Name: "帮助", Description: "help"}})
	analyzer.UpdateTools(nil)
	if _, err := analyzer.Analyze(context.Background(), "你好", nil); err != nil {
		t.Fatalf("Analyze 返回错误: %v", err)
	}
	if !reflect.DeepEqual(rec.commands, []CommandDef{{Name: "帮助", Description: "help"}}) {
		t.Fatalf("刷新后命令快照未更新: %+v", rec.commands)
	}
	if rec.tools != nil {
		t.Fatalf("刷新后工具快照未更新: %+v", rec.tools)
	}
}

func TestAnalyzerNilAdapterDegrades(t *testing.T) {
	analyzer := NewAnalyzer(nil, nil, nil, 0)
	res, err := analyzer.Analyze(context.Background(), "hi", nil)
	if err != nil {
		t.Fatalf("nil 适配器不应返回错误: %v", err)
	}
	if res.Intent != IntentChat || res.Confidence != 1.0 {
		t.Fatalf("nil 适配器应降级 chat/1.0: %+v", res)
	}
}

func TestAnalyzerErrorPassthrough(t *testing.T) {
	rec := &recordingAdapter{err: errors.New("boom")}
	analyzer := NewAnalyzer(rec, nil, nil, 0)
	if _, err := analyzer.Analyze(context.Background(), "hi", nil); err == nil {
		t.Fatal("适配器错误应向上透传（由 Pass 层降级）")
	}
}

func TestAnalyzerTimeoutWrapping(t *testing.T) {
	rec := &recordingAdapter{result: &Result{Intent: IntentChat}}
	NewAnalyzer(rec, nil, nil, time.Second).Analyze(context.Background(), "a", nil) //nolint:errcheck // 测试桩不返回错误
	if !rec.ctxHasDDL {
		t.Fatal("timeout>0 时应给 Judge 套上带 deadline 的 ctx")
	}
	NewAnalyzer(rec, nil, nil, 0).Analyze(context.Background(), "a", nil) //nolint:errcheck // 测试桩不返回错误
	if rec.ctxHasDDL {
		t.Fatal("timeout=0 时不应额外设置 deadline")
	}
}
