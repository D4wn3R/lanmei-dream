package intent

// JevAdapter 测试：请求 schema（state/questions 组装）、answers → Result 映射、
// 降级路径（HTTP 错误/空 answers/未知选项值）与命令参数补抽。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/DaWesen/lanmei-dream/internal/ai/llm"
	"go.uber.org/zap"
)

// fakeLLM 可编程的 llm.LLMClient 桩，用于命令参数补抽测试。
type fakeLLM struct {
	resp  *llm.ChatResponse
	err   error
	calls int
}

func (f *fakeLLM) Chat(ctx context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

// newTestJev 构造指向 httptest 服务器的 JevAdapter。
func newTestJev(t *testing.T, handler http.HandlerFunc, llmClient llm.LLMClient) *JevAdapter {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewJevAdapter(JevOptions{BaseURL: srv.URL, APIKey: "test-key", Timeout: time.Second}, llmClient, zap.NewNop())
}

// jevBody 序列化决策响应。
func jevBody(t *testing.T, answers map[string]jevAnswer) []byte {
	t.Helper()
	b, err := json.Marshal(jevResponse{Model: "typesafe/jev-1.13-test", Answers: answers})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func choiceAns(v string, conf float64) jevAnswer {
	return jevAnswer{Type: "choice", Choice: v, Confidence: conf, Probabilities: map[string]float64{v: conf}}
}

// captureJevRequest 返回一个把请求体解码到 *req 的 handler 包装。
func captureJevRequest(t *testing.T, req *jevRequest, reply []byte) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(req); err != nil {
			t.Errorf("请求体解码失败: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(reply)
	}
}

var testCommands = []CommandDef{{Name: "签到", Description: "每日签到"}, {Name: "发表情", Description: "发表情包"}}
var testTools = []ToolDef{{Name: "weather", Description: "天气"}}

func TestJevRequestSchemaGroup(t *testing.T) {
	var got jevRequest
	answers := map[string]jevAnswer{
		"intent":            choiceAns("command", 0.95),
		"command":           choiceAns("签到", 0.9),
		"is_talking_to_bot": {Type: "noul", Noul: floatPtr(0.96)},
		"mention_role":      choiceAns("imperative_object", 0.9),
	}
	adapter := newTestJev(t, captureJevRequest(t, &got, jevBody(t, answers)), nil)

	res, err := adapter.Judge(context.Background(), "帮我签到",
		&JudgeContext{BotNames: []string{"蓝妹"}, Recent: []JudgeMessage{{Speaker: "user", Content: "在吗"}}},
		testCommands, testTools)
	if err != nil {
		t.Fatalf("Judge 返回错误: %v", err)
	}

	if got.Model != DefaultJevModel {
		t.Fatalf("model slug 不符: %q", got.Model)
	}
	if got.State["current_message"] != "帮我签到" {
		t.Fatalf("state 缺 current_message: %+v", got.State)
	}
	if !reflect.DeepEqual(got.State["bot_names"], []any{"蓝妹"}) {
		t.Fatalf("state.bot_names 不符: %+v", got.State["bot_names"])
	}
	if got.State["recent"] == nil {
		t.Fatal("群聊 state 应包含 recent")
	}
	for _, key := range []string{"intent", "command", "tool", "is_talking_to_bot", "mention_role"} {
		if _, ok := got.Questions[key]; !ok {
			t.Fatalf("群聊问题集缺 %s", key)
		}
	}
	if got.Questions["intent"].Type != "choice" || len(got.Questions["intent"].Criteria) != 4 {
		t.Fatalf("intent 问题 schema 不符: %+v", got.Questions["intent"])
	}
	if got.Questions["is_talking_to_bot"].Type != "noul" {
		t.Fatalf("is_talking_to_bot 应为 noul: %+v", got.Questions["is_talking_to_bot"])
	}
	if _, ok := got.Questions["command"].Criteria["签到"]; !ok {
		t.Fatalf("command 问题应含动态命令选项: %+v", got.Questions["command"])
	}
	if _, ok := got.Questions["command"].Criteria["none"]; !ok {
		t.Fatalf("command 问题应含 none 兜底选项: %+v", got.Questions["command"])
	}
	if _, ok := got.Questions["tool"].Criteria["weather"]; !ok {
		t.Fatalf("tool 问题应含动态工具选项: %+v", got.Questions["tool"])
	}
	if _, ok := got.Questions["tool"].Criteria["none"]; !ok {
		t.Fatalf("tool 问题应含 none 兜底选项: %+v", got.Questions["tool"])
	}

	if res.Intent != IntentCommand || res.CommandName != "签到" || res.Confidence != 0.95 {
		t.Fatalf("意图映射不符: %+v", res)
	}
	if !res.IsTalkingToBot || res.MentionConfidence != 0.96 || res.MentionRole != "imperative_object" {
		t.Fatalf("提及映射不符: %+v", res)
	}
	if res.MentionEvidence != "" {
		t.Fatalf("Jev 非生成式，MentionEvidence 应为空: %q", res.MentionEvidence)
	}
}

func TestJevRequestSchemaPrivate(t *testing.T) {
	var got jevRequest
	answers := map[string]jevAnswer{"intent": choiceAns("chat", 0.98)}
	adapter := newTestJev(t, captureJevRequest(t, &got, jevBody(t, answers)), nil)

	if _, err := adapter.Judge(context.Background(), "你好", nil, testCommands, testTools); err != nil {
		t.Fatalf("Judge 返回错误: %v", err)
	}
	if _, ok := got.State["bot_names"]; ok {
		t.Fatal("私聊 state 不应含 bot_names")
	}
	if _, ok := got.State["recent"]; ok {
		t.Fatal("私聊 state 不应含 recent")
	}
	for _, key := range []string{"is_talking_to_bot", "mention_role"} {
		if _, ok := got.Questions[key]; ok {
			t.Fatalf("私聊不应携带提及问题 %s", key)
		}
	}
}

func TestJevUnknownIntentDegrades(t *testing.T) {
	answers := map[string]jevAnswer{"intent": choiceAns("bogus", 0.7)}
	adapter := newTestJev(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(jevBody(t, answers))
	}, nil)
	res, err := adapter.Judge(context.Background(), "hi", nil, testCommands, testTools)
	if err != nil {
		t.Fatalf("未知意图值不应返回错误: %v", err)
	}
	if res.Intent != IntentChat || res.Confidence != 0.7 {
		t.Fatalf("未知意图值应降级 chat 且保留置信度: %+v", res)
	}
}

func TestJevUnknownCommandCleared(t *testing.T) {
	answers := map[string]jevAnswer{
		"intent":  choiceAns("command", 0.9),
		"command": choiceAns("不存在的命令", 0.9),
	}
	adapter := newTestJev(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(jevBody(t, answers))
	}, nil)
	res, err := adapter.Judge(context.Background(), "hi", nil, testCommands, testTools)
	if err != nil {
		t.Fatalf("Judge 返回错误: %v", err)
	}
	if res.CommandName != "" {
		t.Fatalf("越界命令名应清空: %q", res.CommandName)
	}
}

// TestJevCommandNoneSilent none 兜底：命令意图命中但无匹配命令时 CommandName 留空，
// 上层 IntentCommandExecPass 对空命令名静默跳过（不执行、不回复）。
func TestJevCommandNoneSilent(t *testing.T) {
	answers := map[string]jevAnswer{
		"intent":  choiceAns("command", 0.85),
		"command": choiceAns("none", 0.9),
	}
	adapter := newTestJev(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(jevBody(t, answers))
	}, nil)
	res, err := adapter.Judge(context.Background(), "蓝妹闭嘴", nil, testCommands, testTools)
	if err != nil {
		t.Fatalf("Judge 返回错误: %v", err)
	}
	if res.CommandName != "" {
		t.Fatalf("none 响应应清空命令名（静默路径）: %q", res.CommandName)
	}
	if res.Intent != IntentCommand {
		t.Fatalf("intent 应保持 command（由上层空名保护静默）: %+v", res)
	}
}

func TestJevHTTPError(t *testing.T) {
	adapter := newTestJev(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}, nil)
	if _, err := adapter.Judge(context.Background(), "hi", nil, testCommands, testTools); err == nil {
		t.Fatal("HTTP 5xx 应返回 error（上层降级 chat/0.5）")
	}
}

func TestJevEmptyAnswersError(t *testing.T) {
	adapter := newTestJev(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"model":"jev","answers":{},"usage":{}}`))
	}, nil)
	if _, err := adapter.Judge(context.Background(), "hi", nil, testCommands, testTools); err == nil {
		t.Fatal("空 answers 应返回 error")
	}
}

func TestJevRetryOn429(t *testing.T) {
	calls := 0
	adapter := newTestJev(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jevBody(t, map[string]jevAnswer{"intent": choiceAns("chat", 0.9)}))
	}, nil)
	res, err := adapter.Judge(context.Background(), "hi", nil, testCommands, testTools)
	if err != nil {
		t.Fatalf("429 后单次重试应成功: %v", err)
	}
	if calls != 2 {
		t.Fatalf("应恰好重试一次: calls=%d", calls)
	}
	if res.Intent != IntentChat {
		t.Fatalf("重试结果映射不符: %+v", res)
	}
}

func TestJevExtractCommandArgs(t *testing.T) {
	answers := map[string]jevAnswer{
		"intent":  choiceAns("command", 0.95),
		"command": choiceAns("发表情", 0.9),
	}
	fake := &fakeLLM{resp: &llm.ChatResponse{Content: "```json\n[\"Go\"]\n```"}}
	adapter := newTestJev(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(jevBody(t, answers))
	}, fake)

	res, err := adapter.Judge(context.Background(), "发个Go的表情", nil, testCommands, testTools)
	if err != nil {
		t.Fatalf("Judge 返回错误: %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("命令意图命中应补抽一次参数: calls=%d", fake.calls)
	}
	if !reflect.DeepEqual(res.CommandArgs, []string{"Go"}) {
		t.Fatalf("参数补抽不符: %+v", res.CommandArgs)
	}

	// 补抽失败静默返回空参，不影响 Judge 结果
	failing := &fakeLLM{err: errors.New("llm down")}
	adapter2 := newTestJev(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(jevBody(t, answers))
	}, failing)
	res2, err := adapter2.Judge(context.Background(), "发个Go的表情", nil, testCommands, testTools)
	if err != nil {
		t.Fatalf("补抽失败不应返回错误: %v", err)
	}
	if res2.CommandArgs != nil {
		t.Fatalf("补抽失败应返回空参: %+v", res2.CommandArgs)
	}
}

func TestJevNonCommandSkipsArgExtraction(t *testing.T) {
	fake := &fakeLLM{resp: &llm.ChatResponse{Content: "[]"}}
	adapter := newTestJev(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(jevBody(t, map[string]jevAnswer{"intent": choiceAns("chat", 0.9)}))
	}, fake)
	if _, err := adapter.Judge(context.Background(), "你好", nil, testCommands, testTools); err != nil {
		t.Fatalf("Judge 返回错误: %v", err)
	}
	if fake.calls != 0 {
		t.Fatalf("非命令意图不应触发补抽: calls=%d", fake.calls)
	}
}

func floatPtr(f float64) *float64 { return &f }
