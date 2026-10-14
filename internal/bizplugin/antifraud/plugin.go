// Package antifraud 是接入 LanmeiAntiFraud 推理服务的反诈插件。
//
// 插件以「消息观察者」（plugin.MessageObserver）形式旁路观察群消息：只把待判定的文本
// 投入自有队列后立即返回，模型调用、处置（撤回 / 禁言）、上报都在插件协程中异步执行，
// 不占用消息管线预算、不影响机器人回复。
//
// 处置策略（阈值全部可配，见 config.AntifraudConfig）：
//   - 违规分（1 - P(正常)）达到 AutoThreshold：自动撤回 + 禁言并上报管理员；
//   - 达到 ReportThreshold：入库并上报，由管理员在管理面板审批；
//   - 低于 ReportThreshold：不记录、不通知。
//
// 违规记录写入本插件自管的表 antifraud_records（插件自行迁移），管理面板经
// Plugin 上的查询与审批方法读取与处置。
package antifraud

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/zrurf/conduit"
	"go.uber.org/zap"

	"github.com/DaWesen/lanmei-dream/internal/config"
	pluginpkg "github.com/DaWesen/lanmei-dream/internal/plugin"
)

const (
	// pluginID 插件标识，同时作为状态存储与资源 ID 的命名空间。
	pluginID = "antifraud"

	// migrateTimeout 建表超时。
	migrateTimeout = 15 * time.Second
	// actionTimeout 单次出站动作（撤回 / 禁言 / 发送上报）的等待上限。
	actionTimeout = 6 * time.Second
	// stopTimeout OnStop 等待后台协程退出的上限（让在途批次跑完）。
	stopTimeout = 12 * time.Second
	// retentionInterval 过期记录清理的检查间隔。
	retentionInterval = 12 * time.Hour
	// limiterTimeout 冷却/缓存等 Redis 操作的独立超时（不依赖消息 ctx）。
	limiterTimeout = time.Second
)

// Plugin 是反诈插件实例。
type Plugin struct {
	cfg    config.AntifraudConfig
	th     Thresholds
	client *Client
	logger *zap.Logger

	// superUsers 静态超管（LANMEI_BOT_SUPER_USERS）按平台分组，用于违规上报
	superUsers map[string][]string

	// 由 PluginContext 在 OnInit 注入
	actions    pluginpkg.MessageActions
	store      *Store
	stateStore conduit.StateStore

	// 判定流水线
	queue         chan task
	batchWindow   time.Duration
	batchInterval time.Duration
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	stopped       atomic.Bool

	// 运行观测：丢弃计数、连续失败计数、熔断截止时间（UnixNano）、失联上报去重
	dropped        atomic.Int64
	failures       atomic.Int64
	openUntil      atomic.Int64
	outageNotified atomic.Bool
}

// task 是一条待判定的消息（观察者入队、判定协程消费）。
type task struct {
	platform  string
	connID    string
	groupID   string
	userID    string
	nickname  string
	messageID string
	text      string
	hash      string
	isGroup   bool
	seenAt    time.Time
}

// New 创建反诈插件。
//
// 参数：
//   - cfg：插件配置；零值按默认补齐，endpoint 缺失或非法时返回错误
//   - superUsers：静态超管原文（platform:userID,...），用于违规上报私聊
//   - logger：日志器，nil 时使用 Nop
//
// 返回：配置非法时返回错误；OnInit 前不会产生任何副作用。
func New(cfg config.AntifraudConfig, superUsers string, logger *zap.Logger) (*Plugin, error) {
	cfg = normalizeConfig(cfg)
	if logger == nil {
		logger = zap.NewNop()
	}
	client, err := NewClient(cfg.Endpoint, time.Duration(cfg.RequestTimeoutSeconds)*time.Second)
	if err != nil {
		return nil, err
	}
	return &Plugin{
		cfg:           cfg,
		th:            Thresholds{Auto: cfg.AutoThreshold, Report: cfg.ReportThreshold},
		client:        client,
		logger:        logger,
		superUsers:    parseStaticSuperUsers(superUsers),
		queue:         make(chan task, cfg.QueueSize),
		batchWindow:   time.Duration(cfg.BatchWindowMillis) * time.Millisecond,
		batchInterval: batchInterval(cfg.MaxBatchesPerSecond),
	}, nil
}

// normalizeConfig 补齐零值并夹取范围：非法取值一律回退默认值，避免运行期反复出错。
func normalizeConfig(cfg config.AntifraudConfig) config.AntifraudConfig {
	cfg.Endpoint = strings.TrimSpace(cfg.Endpoint)
	if cfg.ReportThreshold <= 0 || cfg.ReportThreshold > 1 {
		cfg.ReportThreshold = 0.6
	}
	if cfg.AutoThreshold <= 0 || cfg.AutoThreshold > 1 {
		cfg.AutoThreshold = 0.9
	}
	// 自动阈值不得低于上报阈值，否则「达上报未达自动」的区间为空，语义自相矛盾
	if cfg.AutoThreshold < cfg.ReportThreshold {
		cfg.AutoThreshold = cfg.ReportThreshold
	}
	if cfg.MuteSeconds < 0 {
		cfg.MuteSeconds = 0
	}
	if cfg.MinChars <= 0 {
		cfg.MinChars = 4
	}
	if cfg.MaxContentChars <= 0 {
		cfg.MaxContentChars = 1000
	}
	if cfg.UserCooldownSeconds < 0 {
		cfg.UserCooldownSeconds = 0
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 256
	}
	if cfg.BatchSize <= 0 || cfg.BatchSize > maxBatch {
		cfg.BatchSize = 16
	}
	if cfg.BatchWindowMillis <= 0 {
		cfg.BatchWindowMillis = 200
	}
	if cfg.MaxBatchesPerSecond <= 0 {
		cfg.MaxBatchesPerSecond = 2
	}
	if cfg.RequestTimeoutSeconds <= 0 {
		cfg.RequestTimeoutSeconds = 8
	}
	if cfg.CacheTTLSeconds < 0 {
		cfg.CacheTTLSeconds = 0
	}
	if cfg.DedupWindowSeconds < 0 {
		cfg.DedupWindowSeconds = 0
	}
	if cfg.CircuitFailures <= 0 {
		cfg.CircuitFailures = 5
	}
	if cfg.CircuitOpenSeconds <= 0 {
		cfg.CircuitOpenSeconds = 60
	}
	if cfg.RetentionDays < 0 {
		cfg.RetentionDays = 0
	}
	cfg.NoticeGroupID = strings.TrimSpace(cfg.NoticeGroupID)
	cfg.PanelBaseURL = strings.TrimSpace(cfg.PanelBaseURL)
	return cfg
}

// batchInterval 把「每秒批次数」换算成两次提交的最小间隔；<=0 表示不限速。
func batchInterval(perSecond float64) time.Duration {
	if perSecond <= 0 {
		return 0
	}
	return time.Duration(float64(time.Second) / perSecond)
}

// Info 返回插件元信息；插件不注册命令、工具与子树，只做旁路观察。
func (p *Plugin) Info() pluginpkg.PluginInfo {
	return pluginpkg.PluginInfo{
		ID:      pluginID,
		Name:    "反诈",
		Version: "1.0.0",
		Description: fmt.Sprintf("调用反诈推理服务判定消息（自动阈值 %.2f / 上报阈值 %.2f），"+
			"高置信度违规自动撤回并禁言，中置信度入库待管理员审批", p.th.Auto, p.th.Report),
	}
}

// OnInit 注入依赖并建表；此时尚未启动判定协程。
func (p *Plugin) OnInit(ctx *pluginpkg.PluginContext) error {
	p.actions = ctx.Actions
	if p.actions == nil {
		p.logger.Warn("antifraud: 未注入出站动作能力，判定结果无法自动处置与上报（仍会入库）")
	}
	if ctx.DB == nil {
		return fmt.Errorf("antifraud: 数据库不可用，无法保存违规记录")
	}
	p.store = NewStore(ctx.DB.Orm)
	migrateCtx, cancel := context.WithTimeout(context.Background(), migrateTimeout)
	defer cancel()
	if err := p.store.Migrate(migrateCtx); err != nil {
		return err
	}
	p.stateStore = ctx.Store
	if p.stateStore == nil {
		p.logger.Warn("antifraud: 状态存储未配置，用户冷却与判决缓存不可用")
	}
	return nil
}

// OnStart 启动判定协程与过期记录清理。
func (p *Plugin) OnStart(_ *pluginpkg.PluginContext) error {
	runCtx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.wg.Go(func() { p.run(runCtx) })
	if p.cfg.RetentionDays > 0 {
		p.wg.Go(func() { p.retentionLoop(runCtx) })
	}
	p.logger.Info("antifraud: 插件已启动",
		zap.String("endpoint", p.cfg.Endpoint),
		zap.Float64("auto_threshold", p.th.Auto),
		zap.Float64("report_threshold", p.th.Report),
		zap.Int("mute_seconds", p.cfg.MuteSeconds),
	)
	return nil
}

// OnStop 停止后台协程，并给在途批次留出完成时间。
func (p *Plugin) OnStop(_ *pluginpkg.PluginContext) error {
	p.stopped.Store(true)
	if p.cancel == nil {
		return nil
	}
	p.cancel()
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(stopTimeout):
		p.logger.Warn("antifraud: 后台协程退出超时，强制结束", zap.Duration("timeout", stopTimeout))
	}
	return nil
}

// run 是判定主循环：按批次聚合窗口攒批，攒满或窗口到期即提交一批。
func (p *Plugin) run(ctx context.Context) {
	batch := make([]task, 0, p.cfg.BatchSize)
	timer := time.NewTimer(p.batchWindow)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case t := <-p.queue:
			batch = append(batch, t)
			if len(batch) >= p.cfg.BatchSize {
				p.flush(ctx, batch)
				batch = batch[:0]
				resetTimer(timer, p.batchWindow)
			}
		case <-timer.C:
			if len(batch) > 0 {
				p.flush(ctx, batch)
				batch = batch[:0]
			}
			timer.Reset(p.batchWindow)
		}
	}
}

// resetTimer 重置聚合窗口计时器，先排空已触发的信号避免立即再次触发。
func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

// retentionLoop 周期清理过期的已终结记录（pending 保留，避免丢失待审批项）。
func (p *Plugin) retentionLoop(ctx context.Context) {
	ticker := time.NewTicker(retentionInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cutoff := time.Now().AddDate(0, 0, -p.cfg.RetentionDays)
			removed, err := p.store.DeleteBefore(ctx, cutoff)
			if err != nil {
				p.logger.Warn("antifraud: 过期违规记录清理失败", zap.Error(err))
				continue
			}
			if removed > 0 {
				p.logger.Info("antifraud: 已清理过期违规记录", zap.Int64("removed", removed))
			}
		}
	}
}

// hashContent 计算送判文本的 SHA-256，用作判决缓存键与重复记录合并依据。
func hashContent(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// truncateRunes 按 rune 截断文本，避免把多字节字符切坏。
func truncateRunes(text string, max int) string {
	if max <= 0 || utf8.RuneCountInString(text) <= max {
		return text
	}
	runes := []rune(text)
	return string(runes[:max])
}

// truncateText 截断供日志/错误展示的文本。
func truncateText(text string, max int) string {
	text = singleLine(text)
	if max <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	return string(runes[:max]) + "…"
}

// singleLine 把多行文本压成单行，避免上报文案被消息里的换行撑散。
func singleLine(text string) string {
	collapsed := strings.Join(strings.Fields(strings.ReplaceAll(text, "\n", " ")), " ")
	return collapsed
}

// containsID 判断 ID 是否在集合中（空集合视为不匹配）。
func containsID(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

// parseStaticSuperUsers 解析 LANMEI_BOT_SUPER_USERS 原文（platform:userID,...）为平台分组。
func parseStaticSuperUsers(raw string) map[string][]string {
	result := map[string][]string{}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, ":", 2)
		if len(parts) != 2 {
			continue
		}
		platform, userID := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if platform == "" || userID == "" {
			continue
		}
		result[platform] = append(result[platform], userID)
	}
	for _, ids := range result {
		sort.Strings(ids)
	}
	return result
}
