package bizplugin

import (
	"fmt"
	"time"

	"github.com/DaWesen/lanmei-dream/internal/ai"
	"github.com/DaWesen/lanmei-dream/internal/ai/llm"
	randombeauty "github.com/DaWesen/lanmei-dream/internal/bizplugin/random_beauty"
	"github.com/DaWesen/lanmei-dream/internal/config"
	"github.com/DaWesen/lanmei-dream/internal/media"
	pluginpkg "github.com/DaWesen/lanmei-dream/internal/plugin"
	"go.uber.org/zap"
)

// BusinessRegistry 按配置（[plugin.builtins]）动态注册内置业务插件，替代 main.go 中的硬编码注册。
//
// 注册前先查注册表：同名插件若已由 Wasm 动态加载（如 wasm 版签到/欢迎）则跳过内置实现，
// 保证"注册表优先、Wasm 优先"；插件私有数据统一走受限 KV（PluginContext.KV，PostgreSQL）。
type BusinessRegistry struct {
	cfg                *config.PluginBuiltinsConfig // 内置插件开关（[plugin.builtins]）
	registry           *pluginpkg.Registry          // 插件注册表（内置与 Wasm 插件共用）
	ncmURL             string                       // 网易云音乐 API 地址（点歌插件使用）
	musicSendMode      string                       // 点歌发送方式：auto/card/link
	store              *media.ObjectStore           // RustFS 对象存储（表情库/入群欢迎插件使用，未配置时为 nil）
	vision             *ai.VisionService            // 视觉理解服务（随机美图插件使用，未配置时为 nil）
	llmClient          llm.LLMClient                // LLM 客户端（海龟汤插件出题/判定使用，未配置时为 nil）
	turtleSoupTimeout  time.Duration                // 海龟汤出题/判定 LLM 调用独立超时（<=0 不限制）
	quizDir            string                       // 编程答题题库根目录
	randomBeautyConfig config.RandomBeautyConfig    // 随机美图插件配置
	logger             *zap.Logger
}

// NewBusinessRegistry 创建内置业务插件注册表。
func NewBusinessRegistry(cfg *config.PluginBuiltinsConfig, registry *pluginpkg.Registry, logger *zap.Logger) *BusinessRegistry {
	return &BusinessRegistry{
		cfg:      cfg,
		registry: registry,
		logger:   logger,
	}
}

// SetNCMURL 设置网易云音乐 API 地址（点歌插件依赖）。
func (r *BusinessRegistry) SetNCMURL(url string) {
	r.ncmURL = url
}

// SetMusicSendMode 设置点歌发送方式（auto/card/link，适配不同反向代理工具）。
func (r *BusinessRegistry) SetMusicSendMode(mode string) {
	r.musicSendMode = mode
}

// SetObjectStore 设置 RustFS 对象存储（表情库/入群欢迎插件依赖；未配置时收藏与欢迎图不可用）。
func (r *BusinessRegistry) SetObjectStore(store *media.ObjectStore) {
	r.store = store
}

// SetVisionService 设置视觉理解服务（随机美图插件依赖；未配置时为 nil）。
func (r *BusinessRegistry) SetVisionService(v *ai.VisionService) {
	r.vision = v
}

// SetLLMClient 设置 LLM 客户端（海龟汤插件依赖；未配置时该插件命令提示不可用）。
func (r *BusinessRegistry) SetLLMClient(client llm.LLMClient) {
	r.llmClient = client
}

// SetTurtleSoupTimeout 设置海龟汤出题/判定 LLM 调用的独立超时（<=0 不限制）。
func (r *BusinessRegistry) SetTurtleSoupTimeout(timeout time.Duration) {
	r.turtleSoupTimeout = timeout
}

// SetQuizDir 设置编程答题题库根目录。
func (r *BusinessRegistry) SetQuizDir(dir string) {
	r.quizDir = dir
}

// SetRandomBeautyConfig 设置随机美图插件的上游与安全阈值配置。
func (r *BusinessRegistry) SetRandomBeautyConfig(cfg config.RandomBeautyConfig) {
	r.randomBeautyConfig = cfg
}

// RegisterBuiltins 按配置注册所有内置业务插件。
//
// 每个插件：配置未启用或同名插件已由 Wasm 加载时跳过并记日志，否则注册内置实现；
// 任一注册失败立即返回错误，由调用方决定是否终止启动。
func (r *BusinessRegistry) RegisterBuiltins() error {
	if r.registry == nil {
		return fmt.Errorf("bizplugin: 插件注册表为空，无法注册内置插件")
	}
	logger := r.logger
	if logger == nil {
		logger = zap.NewNop()
	}

	// builtins 为 nil 时退化为零值配置（所有开关关闭）
	builtins := r.cfg
	if builtins == nil {
		builtins = &config.PluginBuiltinsConfig{}
	}

	register := func(sw bool, pluginID string, plugin pluginpkg.Plugin) error {
		if !sw {
			logger.Info("bizplugin: 插件已由配置关闭，跳过注册", zap.String("plugin", pluginID))
			return nil
		}
		if _, loaded := r.registry.Get(pluginID); loaded {
			logger.Info("bizplugin: 插件已由 Wasm 动态加载，跳过内置注册", zap.String("plugin", pluginID))
			return nil
		}
		if err := r.registry.Register(plugin); err != nil {
			return fmt.Errorf("bizplugin: 注册 %s 插件失败: %w", pluginID, err)
		}
		return nil
	}

	if err := register(builtins.Signin, "signin", NewSigninPlugin(logger)); err != nil {
		return err
	}

	if err := register(builtins.Welcome, "welcome", NewWelcomePlugin(r.store, logger)); err != nil {
		return err
	}

	if err := register(builtins.Poke, "poke", NewPokePlugin(logger)); err != nil {
		return err
	}

	if err := register(builtins.ThreeG, "three_g", NewThreeGPlugin(logger)); err != nil {
		return err
	}

	if err := register(builtins.ZhaoxinGroup, "zhaoxin_group", NewZhaoxinGroupPlugin(logger)); err != nil {
		return err
	}

	if err := register(builtins.Rank, "signin_rank", NewRankPlugin(logger)); err != nil {
		return err
	}

	if err := register(builtins.Cat, "cat", NewCatPlugin()); err != nil {
		return err
	}

	if err := register(builtins.BaLogo, "balogo", NewBaLogoPlugin()); err != nil {
		return err
	}

	if err := register(builtins.Ping, "ping", NewPingPlugin()); err != nil {
		return err
	}

	if err := register(builtins.GitHubCard, "github_card", NewGitHubCardPlugin()); err != nil {
		return err
	}

	if err := register(builtins.Music, "music", NewMusicPlugin(r.ncmURL, r.musicSendMode, logger)); err != nil {
		return err
	}

	if err := register(builtins.Sticker, "sticker", NewStickerPlugin(r.store, logger)); err != nil {
		return err
	}

	if err := register(builtins.TurtleSoup, "turtle_soup", NewTurtleSoupPlugin(r.llmClient, logger, r.turtleSoupTimeout)); err != nil {
		return err
	}

	if err := register(builtins.AnswerQuestion, "answer_question", NewAnswerQuestionPlugin(r.quizDir)); err != nil {
		return err
	}

	if err := register(builtins.DailyQuote, "daily_quote", NewDailyQuotePlugin(logger)); err != nil {
		return err
	}

	if !builtins.RandomBeauty {
		if err := register(false, "random_beauty", nil); err != nil {
			return err
		}
	} else if _, loaded := r.registry.Get("random_beauty"); loaded {
		logger.Info("bizplugin: 插件已由 Wasm 动态加载，跳过内置注册", zap.String("plugin", "random_beauty"))
	} else {
		p, err := randombeauty.New(r.randomBeautyConfig, r.vision, r.store, logger)
		if err != nil {
			return fmt.Errorf("bizplugin: 创建 random_beauty 插件失败: %w", err)
		}
		if err := register(true, "random_beauty", p); err != nil {
			return err
		}
	}

	return nil
}
