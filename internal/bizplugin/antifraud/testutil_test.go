package antifraud

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/zrurf/conduit"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/DaWesen/lanmei-dream/internal/config"
	"github.com/DaWesen/lanmei-dream/internal/gateway"
	"github.com/DaWesen/lanmei-dream/internal/model"
	pluginpkg "github.com/DaWesen/lanmei-dream/internal/plugin"
)

// newTestStore 创建独立的 SQLite 测试库并建表；测试结束关闭连接后再清理临时目录。
func newTestStore(t *testing.T) *Store {
	t.Helper()
	orm, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "antifraud.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := orm.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	store := NewStore(orm)
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	// 上报目标从 bot_admin 表读取，测试库需一并建表
	if err := orm.AutoMigrate(&model.BotAdmin{}); err != nil {
		t.Fatalf("AutoMigrate(bot_admin) error = %v", err)
	}
	return store
}

// newTestPlugin 组装插件（共用测试配置）并注入 SQLite 库、假状态存储与假动作能力。
func newTestPlugin(t *testing.T, cfg config.AntifraudConfig, actions pluginpkg.MessageActions) (*Plugin, *Store) {
	t.Helper()
	if cfg.Endpoint == "" {
		cfg.Endpoint = "http://127.0.0.1:1"
	}
	p, err := New(cfg, "qq:1001,qq:1002", zap.NewNop())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	store := newTestStore(t)
	p.store = store
	p.stateStore = newFakeStateStore()
	p.actions = actions
	return p, store
}

// testTask 构造判定任务（等价于观察者入队的内容）。
func testTask(userID, text string) task {
	return task{
		platform:  "qq",
		connID:    "conn-1",
		groupID:   "g-1",
		userID:    userID,
		nickname:  "测试用户",
		messageID: "m-" + userID,
		text:      text,
		hash:      hashContent(text),
		isGroup:   true,
		seenAt:    time.Now(),
	}
}

// testMessageContext 构造消息上下文，Extra 键与 internal/bot 注入的一致。
func testMessageContext(userID, groupID, text string) *conduit.MessageContext {
	extra := map[string]any{
		extraMessageType: extraMessageTypeMessage,
		extraPlatform:    "qq",
		"message_id":     "m-1",
		"conn_id":        "conn-1",
		"nickname":       "测试用户",
		"self_id":        "bot-1",
		extraSegments:    []gateway.NormalizedSegment{{Type: "text", Text: text}},
	}
	return conduit.NewMessageContext(&conduit.InputMessage{
		UserID:  userID,
		GroupID: groupID,
		Content: text,
		IsGroup: groupID != "",
		Extra:   extra,
	})
}

// fakeStateStore 是 conduit.StateStore 的内存实现（冷却与判决缓存测试使用）。
type fakeStateStore struct {
	mu   sync.Mutex
	data map[string]string
}

func newFakeStateStore() *fakeStateStore {
	return &fakeStateStore{data: map[string]string{}}
}

func (s *fakeStateStore) Get(_ context.Context, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data[key], nil
}

func (s *fakeStateStore) Set(_ context.Context, key, value string, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = value
	return nil
}

func (s *fakeStateStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key)
	return nil
}

func (s *fakeStateStore) Exists(_ context.Context, key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.data[key]
	return ok, nil
}

func (s *fakeStateStore) CompareAndSwap(_ context.Context, key, oldValue, newValue string, _ time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data[key] != oldValue {
		return false, nil
	}
	s.data[key] = newValue
	return true, nil
}

func (s *fakeStateStore) IncrBy(_ context.Context, key string, delta int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var value int64
	_, _ = fmt.Sscan(s.data[key], &value)
	value += delta
	s.data[key] = fmt.Sprint(value)
	return value, nil
}

func (s *fakeStateStore) SetIfNotExists(_ context.Context, key, value string, _ time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[key]; ok {
		return false, nil
	}
	s.data[key] = value
	return true, nil
}

func (s *fakeStateStore) Close() error { return nil }

// fakeActions 记录出站动作调用，可按需返回失败响应。
type fakeActions struct {
	mu         sync.Mutex
	recalls    []string    // 撤回的消息 ID
	bans       []string    // 禁言目标（groupID:userID:seconds）
	sends      [][2]string // 通知目标（平台:目标ID, 文本）
	announces  [][3]string // 群内告知（groupID, atUserID, 文本）
	recallResp *gateway.ActionResponse
	banResp    *gateway.ActionResponse
	sendErr    error
}

func (f *fakeActions) RecallMessage(_ context.Context, _, _, messageID string) (*gateway.ActionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recalls = append(f.recalls, messageID)
	if f.recallResp != nil {
		return f.recallResp, nil
	}
	return &gateway.ActionResponse{Status: "ok"}, nil
}

func (f *fakeActions) BanGroupMember(_ context.Context, _, _, groupID, userID string, seconds int) (*gateway.ActionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bans = append(f.bans, fmt.Sprintf("%s:%s:%d", groupID, userID, seconds))
	if f.banResp != nil {
		return f.banResp, nil
	}
	return &gateway.ActionResponse{Status: "ok"}, nil
}

func (f *fakeActions) SendTextTo(_ context.Context, _, _ string, isGroup bool, targetID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sends = append(f.sends, [2]string{fmt.Sprintf("%t:%s", isGroup, targetID), text})
	return f.sendErr
}

func (f *fakeActions) SendAtTextTo(_ context.Context, _, _, groupID, atUserID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.announces = append(f.announces, [3]string{groupID, atUserID, text})
	return f.sendErr
}

func (f *fakeActions) snapshot() (recalls, bans int, sends int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.recalls), len(f.bans), len(f.sends)
}

// announceSnapshot 返回群内告知的深拷贝（避免测试与并发写入竞争）。
func (f *fakeActions) announceSnapshot() [][3]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][3]string(nil), f.announces...)
}

// fakeInferService 返回 ticket 式假推理服务：提交即入队，首次查询 running，之后按 verdictFor 返回结果。
func fakeInferService(t *testing.T, verdictFor func(text string) InferItem) *httptest.Server {
	t.Helper()
	var (
		mu      sync.Mutex
		pending = map[string][]InferItem{}
		seen    = map[string]int{}
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/infer", func(w http.ResponseWriter, r *http.Request) {
		var req inferRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		items := make([]InferItem, 0, len(req.Texts))
		for _, text := range req.Texts {
			item := verdictFor(text)
			item.Text = text
			items = append(items, item)
		}
		mu.Lock()
		pending["t-1"] = items
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"ticket": "t-1", "status": "queued"})
	})
	mux.HandleFunc("/api/v1/infer/t-1", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		seen["t-1"]++
		if seen["t-1"] < 2 { // 首次查询模拟 queued，验证轮询路径
			_ = json.NewEncoder(w).Encode(map[string]any{"ticket": "t-1", "status": "queued"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ticket": "t-1", "status": "done",
			"result": map[string]any{"items": pending["t-1"]},
		})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// violationItem 构造一条高分违规判定（默认「刷单」，违规分 0.95）。
func violationItem(probabilities []float64) InferItem {
	if len(probabilities) == 0 {
		probabilities = []float64{0.05, 0.0, 0.0, 0.95, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0}
	}
	return InferItem{
		LabelID:       3,
		LabelKey:      "brushing",
		LabelName:     "刷单",
		Confidence:    probabilities[3],
		Probabilities: probabilities,
		ExpertsOpinion: []ExpertOpinion{
			{ID: 3, Key: "brushing", Weight: 0.7, Probabilities: probabilities},
		},
	}
}

// normalItem 构造一条正常判定（P(正常)=0.95）。
func normalItem() InferItem {
	probabilities := make([]float64, 10)
	probabilities[normalLabelID] = 0.95
	for i := 1; i < len(probabilities); i++ {
		probabilities[i] = 0.05 / 9
	}
	return InferItem{LabelID: normalLabelID, LabelKey: "normal", LabelName: "正常", Confidence: 0.95, Probabilities: probabilities}
}

// waitForRecord 轮询等待满足条件的记录出现（异步判定完成后断言）。
func waitForRecord(t *testing.T, store *Store, predicate func(*Record) bool) *Record {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		list, _, err := store.List(context.Background(), Filter{Limit: 20})
		if err == nil {
			for i := range list {
				if predicate(&list[i]) {
					return &list[i]
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("等待违规记录超时")
	return nil
}

// testConfig 返回一份启用全流程的测试配置（阈值与计划默认值一致）。
func testConfig(endpoint string) config.AntifraudConfig {
	return config.AntifraudConfig{
		Endpoint:              endpoint,
		AutoThreshold:         0.9,
		ReportThreshold:       0.6,
		MuteSeconds:           3600,
		MinChars:              2,
		SkipSuperUsers:        true,
		MaxContentChars:       500,
		UserCooldownSeconds:   0, // 测试默认不开冷却，需要时单独打开
		QueueSize:             8,
		BatchSize:             4,
		BatchWindowMillis:     10,
		MaxBatchesPerSecond:   100,
		RequestTimeoutSeconds: 3,
		CacheTTLSeconds:       0, // 默认不缓存，避免测试间互相影响
		DedupWindowSeconds:    120,
		CircuitFailures:       3,
		CircuitOpenSeconds:    60,
	}
}

// mustContain 断言文本包含子串。
func mustContain(t *testing.T, text, want string) {
	t.Helper()
	if !strings.Contains(text, want) {
		t.Fatalf("文本未包含 %q：%s", want, text)
	}
}
