package websearch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/DaWesen/lanmei-dream/internal/ai/tool"
	"github.com/DaWesen/lanmei-dream/internal/config"
	"go.uber.org/zap"
)

type cacheEntry struct {
	response          SearchResponse
	expires, inserted time.Time
}
type rateBucket struct {
	tokens float64
	at     time.Time
}

// Service 持有 HTTP 连接池、有限缓存和限流器；单轮预算保存在 context 的 TurnState。
type Service struct {
	cfg       config.WebSearchConfig
	backend   Searcher
	ctx       context.Context
	cancel    context.CancelFunc
	slots     chan struct{}
	mu        sync.Mutex
	closed    bool
	cache     map[string]cacheEntry
	rates     map[string]rateBucket
	wg        sync.WaitGroup
	once      sync.Once
	requestID atomic.Uint64
	logger    *zap.Logger
}

func NewService(ctx context.Context, cfg config.WebSearchConfig, logger *zap.Logger) (*Service, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	lifecycle, cancel := context.WithCancel(ctx)
	return &Service{cfg: cfg, backend: newOpenSERP(cfg), ctx: lifecycle, cancel: cancel,
		slots: make(chan struct{}, cfg.MaxConcurrency), cache: make(map[string]cacheEntry),
		rates: make(map[string]rateBucket), logger: logger}, nil
}

// Close 先拒绝新请求，再取消在途请求；最多等待两秒清理，允许重复调用。
func (s *Service) Close() {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.cancel()
		clear(s.cache)
		clear(s.rates)
		s.mu.Unlock()
		done := make(chan struct{})
		go func() { s.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		if backend, ok := s.backend.(*openSERP); ok {
			backend.client.CloseIdleConnections()
		}
	})
}

func (s *Service) policy() tool.SearchPolicy {
	return tool.SearchPolicy{MaxCalls: s.cfg.MaxCallsPerTurn,
		Timeout:       time.Duration(s.cfg.TimeoutSeconds) * time.Second,
		Total:         time.Duration(s.cfg.TotalSearchSeconds) * time.Second,
		AnswerReserve: time.Duration(s.cfg.AnswerReserveSeconds) * time.Second}
}

func (s *Service) cacheKey(scope string, request SearchRequest) string {
	// JSON 编码避免 scope/query 内的分隔符造成键碰撞；版本和引擎顺序属于契约。
	b, _ := json.Marshal([]any{scope, request.Query, s.cfg.Engines, s.cfg.Mode, request.Limit, AdapterVersion})
	hash := sha256.Sum256(b)
	return hex.EncodeToString(hash[:])
}

func (s *Service) allowScope(scope string, now time.Time) bool {
	if scope == "" {
		return true
	}
	// 有界身份表：只清除足以恢复全部令牌的闲置项，不通过淘汰活跃 scope 绕过限流。
	refill := float64(s.cfg.RequestsPerMinutePerScope) / 60
	idle := max(time.Minute, time.Duration(float64(s.cfg.BurstPerScope)/refill*float64(time.Second)))
	for key, bucket := range s.rates {
		if now.Sub(bucket.at) >= idle {
			delete(s.rates, key)
		}
	}
	bucket, ok := s.rates[scope]
	if !ok {
		if len(s.rates) >= 4096 {
			return false
		}
		bucket = rateBucket{tokens: float64(s.cfg.BurstPerScope), at: now}
	}
	bucket.tokens = min(float64(s.cfg.BurstPerScope), bucket.tokens+now.Sub(bucket.at).Seconds()*refill)
	bucket.at = now
	allowed := bucket.tokens >= 1
	if allowed {
		bucket.tokens--
	}
	s.rates[scope] = bucket
	return allowed
}

func cloneResponse(response SearchResponse) SearchResponse {
	response.Results = append([]SearchResult{}, response.Results...)
	return response
}

// Search 执行一次检索；失败转为稳定状态，不向模型泄露内部 URL 或底层错误。
func (s *Service) Search(ctx context.Context, request SearchRequest) (response SearchResponse, err error) {
	request.Query = normalizeQuery(request.Query)
	if !validQuery(request.Query) || request.Limit < 1 || request.Limit > 5 {
		return failure("invalid_arguments"), nil
	}
	request.Limit = min(request.Limit, s.cfg.MaxResults)
	scope := ""
	if state := tool.TurnStateFrom(ctx); state != nil {
		scope = state.Scope()
	}
	started := time.Now()
	id := s.requestID.Add(1)
	defer func() {
		hash := sha256.Sum256([]byte(scope))
		s.logger.Info("websearch: search", zap.Uint64("request_id", id), zap.String("scope_hash", hex.EncodeToString(hash[:8])),
			zap.Strings("engines", s.cfg.Engines), zap.Duration("elapsed", time.Since(started)),
			zap.String("status", response.Status), zap.Int("results", len(response.Results)), zap.Bool("cached", response.Cached))
	}()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(s.cfg.TimeoutSeconds)*time.Second)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	s.mu.Lock()
	if s.closed || s.ctx.Err() != nil || ctx.Err() != nil {
		s.mu.Unlock()
		return failure("unavailable"), nil
	}
	s.wg.Add(1)
	defer s.wg.Done()
	if !s.allowScope(scope, started) {
		s.mu.Unlock()
		return failure("rate_limited"), nil
	}
	key := s.cacheKey(scope, request)
	for k, entry := range s.cache {
		if !started.Before(entry.expires) {
			delete(s.cache, k)
		}
	}
	if entry, ok := s.cache[key]; ok && scope != "" {
		response = cloneResponse(entry.response)
		response.Cached = true
		s.mu.Unlock()
		return response, nil
	}
	s.mu.Unlock()
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		return failure("rate_limited"), nil
	}
	if ctx.Err() != nil {
		return failure("timeout"), nil
	}
	response, err = s.backend.Search(ctx, request)
	if err != nil {
		var netErr net.Error
		switch {
		case errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout():
			return failure("timeout"), nil
		case errors.Is(err, errInvalidResponse):
			return failure("invalid_response"), nil
		case errors.Is(err, errRateLimited):
			return failure("rate_limited"), nil
		default:
			return failure("unavailable"), nil
		}
	}
	response = cleanResults(response, request.Limit)
	// 只有完整成功缓存；partial/无结果/失败都不缓存，避免延续故障或抑制补查。
	if response.Status == "ok" && scope != "" {
		s.mu.Lock()
		if !s.closed && s.ctx.Err() == nil && ctx.Err() == nil {
			if len(s.cache) >= s.cfg.CacheMaxEntries {
				oldestKey := ""
				var oldest time.Time
				for k, entry := range s.cache {
					if oldestKey == "" || entry.inserted.Before(oldest) {
						oldestKey, oldest = k, entry.inserted
					}
				}
				delete(s.cache, oldestKey)
			}
			now := time.Now()
			s.cache[key] = cacheEntry{response: cloneResponse(response), inserted: now, expires: now.Add(time.Duration(s.cfg.CacheTTLSeconds) * time.Second)}
		}
		s.mu.Unlock()
	}
	return response, nil
}
