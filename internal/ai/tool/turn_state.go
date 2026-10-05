package tool

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/cloudwego/eino/schema"
)

const WebSearchName = "web_search"

// SearchPolicy 按工具调用数和实际等待耗时计费，不把模型生成时间计入搜索耗时。
type SearchPolicy struct {
	MaxCalls                      int
	Timeout, Total, AnswerReserve time.Duration
}

// TurnState 只属于一条用户消息；宿主重试必须复用它及原始截止时间。
type TurnState struct {
	mu                       sync.Mutex
	original                 context.Context
	scope                    string
	calls                    int
	spent                    time.Duration
	active, exhausted, retry bool
	memo                     map[string]string
	sources                  map[string]string
	evidence                 []*schema.Message
}

type turnKey struct{}

func NewTurnState(ctx context.Context, id CallerIdentity) *TurnState {
	scope := ""
	if id.Platform != "" && id.Platform != "unknown" {
		if id.GroupID != "" {
			scope = id.Platform + ":group:" + id.GroupID
		} else if id.PlatformUserID != "" {
			scope = id.Platform + ":dm:" + id.PlatformUserID
		}
	}
	return &TurnState{original: ctx, scope: scope, memo: make(map[string]string), sources: make(map[string]string)}
}

func WithTurnState(ctx context.Context, state *TurnState) context.Context {
	return context.WithValue(ctx, turnKey{}, state)
}

func TurnStateFrom(ctx context.Context) *TurnState {
	state, _ := ctx.Value(turnKey{}).(*TurnState)
	return state
}

func (s *TurnState) Scope() string { return s.scope }

// Context 继承原任务取消与绝对截止时间，禁止通过重试延长时限。
func (s *TurnState) Context(ctx context.Context) (context.Context, context.CancelFunc) {
	var cancel context.CancelFunc
	if deadline, ok := s.original.Deadline(); ok {
		ctx, cancel = context.WithDeadline(ctx, deadline)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	stop := context.AfterFunc(s.original, cancel)
	if s.original.Err() != nil {
		cancel()
	}
	return WithTurnState(ctx, s), func() { stop(); cancel() }
}

// BeginSearch 原子占用名额；同轮并发调用拒绝排队，保证累计时间不被并发绕过。
func (s *TurnState) BeginSearch(ctx context.Context, p SearchPolicy) (context.Context, func(), bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exhausted || s.retry || s.active || s.original.Err() != nil || ctx.Err() != nil || s.calls >= p.MaxCalls {
		return ctx, func() {}, false
	}
	s.calls++
	s.exhausted = s.calls >= p.MaxCalls
	budget := min(p.Timeout, p.Total-s.spent)
	if deadline, ok := s.original.Deadline(); ok {
		budget = min(budget, time.Until(deadline)-p.AnswerReserve)
	}
	if deadline, ok := ctx.Deadline(); ok {
		budget = min(budget, time.Until(deadline)-p.AnswerReserve)
	}
	if budget <= 0 {
		s.exhausted = true
		return ctx, func() {}, false
	}
	s.active = true
	started := time.Now()
	searchCtx, cancel := context.WithTimeout(ctx, budget)
	var once sync.Once
	return searchCtx, func() {
		once.Do(func() {
			cancel()
			s.mu.Lock()
			defer s.mu.Unlock()
			s.spent += time.Since(started)
			s.active = false
			s.exhausted = s.exhausted || s.spent >= p.Total
		})
	}, true
}

func (s *TurnState) SearchAllowed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.exhausted && !s.retry && s.original.Err() == nil
}

func (s *TurnState) PrepareRetry() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retry = true
}

func (s *TurnState) Memo(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.memo[key]
	return value, ok
}

func (s *TurnState) SaveMemo(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.memo[key] = value
}

func (s *TurnState) SourceID(url string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.sources[url]; ok {
		return id
	}
	id := fmt.Sprintf("s%d", len(s.sources)+1)
	s.sources[url] = id
	return id
}

func (s *TurnState) HasSource(url string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.sources[url]
	return ok
}

// RecordEvidence 保存完整调用/结果对，重试仍以 tool role 提供资料，不提升为 system 指令。
func (s *TurnState) RecordEvidence(call schema.ToolCall, result string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evidence = append(s.evidence,
		&schema.Message{Role: schema.Assistant, Content: " ", ToolCalls: []schema.ToolCall{call}},
		&schema.Message{Role: schema.Tool, ToolCallID: call.ID, Content: result})
}

func (s *TurnState) RetryEvidence() []*schema.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.retry {
		return nil
	}
	return append([]*schema.Message(nil), s.evidence...)
}
