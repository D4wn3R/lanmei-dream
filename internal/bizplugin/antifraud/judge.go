package antifraud

import (
	"context"
	"encoding/json"
	"time"

	"go.uber.org/zap"

	pluginpkg "github.com/DaWesen/lanmei-dream/internal/plugin"
)

// verdictHit 是一次缓存命中（任务与其已判定结果）。
type verdictHit struct {
	task    task
	verdict Verdict
}

// flush 处理一批任务：缓存命中直接复用判决，未命中的合并去重后提交模型。
//
// 重复文本（刷屏）在批内只提交一次，结果按哈希回填给所有同文本任务。
func (p *Plugin) flush(ctx context.Context, batch []task) {
	if until := p.openUntil.Load(); until > time.Now().UnixNano() {
		p.dropBatch(batch, "熔断中")
		return
	}

	hits := make([]verdictHit, 0, len(batch))
	misses := make([]task, 0, len(batch))
	uniqueTexts := make([]string, 0, len(batch))
	textIndex := make(map[string]int, len(batch))
	for _, t := range batch {
		if v, ok := p.loadVerdict(t.hash); ok {
			hits = append(hits, verdictHit{task: t, verdict: v})
			continue
		}
		if _, ok := textIndex[t.hash]; !ok {
			textIndex[t.hash] = len(uniqueTexts)
			uniqueTexts = append(uniqueTexts, t.text)
		}
		misses = append(misses, t)
	}

	for _, h := range hits {
		p.apply(h.task, h.verdict)
	}
	if len(misses) > 0 {
		p.judge(ctx, misses, uniqueTexts, textIndex)
	}
}

// judge 把未命中的文本提交推理服务，回填判决后逐条处置。
//
// 提交失败时整批丢弃（不重试）：判定是旁路能力，宁可漏判也不允许积压与放大。
func (p *Plugin) judge(ctx context.Context, misses []task, texts []string, textIndex map[string]int) {
	// 提交速率限制：两次提交之间保持最小间隔，保护推理服务
	if p.batchInterval > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(p.batchInterval):
		}
	}

	callCtx, cancel := context.WithTimeout(ctx, time.Duration(p.cfg.RequestTimeoutSeconds)*time.Second)
	defer cancel()
	items, err := p.client.Infer(callCtx, texts)
	if err != nil {
		p.noteFailure(err)
		return
	}
	p.noteSuccess()

	for _, t := range misses {
		idx, ok := textIndex[t.hash]
		if !ok || idx >= len(items) {
			continue
		}
		v := toVerdict(items[idx])
		p.saveVerdict(t.hash, v)
		p.apply(t, v)
	}
}

// dropBatch 丢弃一批任务并计数（熔断期间不调用模型）。
func (p *Plugin) dropBatch(batch []task, reason string) {
	dropped := p.dropped.Add(int64(len(batch)))
	if dropped == int64(len(batch)) || dropped%100 < int64(len(batch)) {
		p.logger.Warn("antifraud: 批量丢弃待判定消息",
			zap.String("reason", reason), zap.Int("batch", len(batch)), zap.Int64("dropped", dropped))
	}
}

// verdictKey 生成判决缓存键（按文本哈希）。
func (p *Plugin) verdictKey(hash string) string {
	return pluginpkg.StoreKey(pluginID, "verdict:"+hash)
}

// loadVerdict 读取判决缓存；未配置缓存或读取失败时按未命中处理。
func (p *Plugin) loadVerdict(hash string) (Verdict, bool) {
	if p.stateStore == nil || p.cfg.CacheTTLSeconds <= 0 || hash == "" {
		return Verdict{}, false
	}
	opCtx, cancel := context.WithTimeout(context.Background(), limiterTimeout)
	defer cancel()
	raw, err := p.stateStore.Get(opCtx, p.verdictKey(hash))
	if err != nil {
		p.logger.Warn("antifraud: 判决缓存读取失败", zap.Error(err))
		return Verdict{}, false
	}
	if raw == "" {
		return Verdict{}, false
	}
	var v Verdict
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return Verdict{}, false
	}
	return v, true
}

// saveVerdict 写入判决缓存；写失败只记日志（缓存缺失只影响调用量，不影响正确性）。
func (p *Plugin) saveVerdict(hash string, v Verdict) {
	if p.stateStore == nil || p.cfg.CacheTTLSeconds <= 0 || hash == "" {
		return
	}
	payload, err := json.Marshal(v)
	if err != nil {
		return
	}
	opCtx, cancel := context.WithTimeout(context.Background(), limiterTimeout)
	defer cancel()
	if err := p.stateStore.Set(opCtx, p.verdictKey(hash), string(payload), time.Duration(p.cfg.CacheTTLSeconds)*time.Second); err != nil {
		p.logger.Warn("antifraud: 判决缓存写入失败", zap.Error(err))
	}
}

// noteFailure 累计连续失败；达到阈值后熔断一段时间，并只上报一次服务不可用。
func (p *Plugin) noteFailure(err error) {
	consecutive := p.failures.Add(1)
	p.logger.Warn("antifraud: 判定请求失败", zap.Int64("consecutive", consecutive), zap.Error(err))
	if consecutive < int64(p.cfg.CircuitFailures) {
		return
	}

	open := time.Duration(p.cfg.CircuitOpenSeconds) * time.Second
	p.openUntil.Store(time.Now().Add(open).UnixNano())
	p.failures.Store(0)
	p.logger.Error("antifraud: 判定连续失败，熔断暂停调用",
		zap.Int("failures", p.cfg.CircuitFailures), zap.Duration("open", open))
	if p.outageNotified.CompareAndSwap(false, true) {
		p.notifyOutage(err, open)
	}
}

// noteSuccess 结束连续失败计数；恢复计数后允许下一次失联再上报一次。
func (p *Plugin) noteSuccess() {
	if p.failures.Swap(0) > 0 {
		p.logger.Info("antifraud: 判定请求已恢复")
	}
	p.outageNotified.Store(false)
}
