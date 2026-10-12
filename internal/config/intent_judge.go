package config

import "github.com/spf13/viper"

// IntentJudgeConfig 意图判断适配器配置（[ai.intent_judge]）。
//
// 无显式 adapter 开关，沿用"key 非空即启用"惯例（同 ai.llm_api_key）：
// api_key 配置 OpenRouter API Key 即启用 Jev 适配器（TypeSafe System-1 决策模型），
// 留空走 LLM 意图判断（原路径）。
type IntentJudgeConfig struct {
	// APIKey OpenRouter API Key（Jev 的鉴权与计费均走 OpenRouter）
	APIKey string `mapstructure:"api_key"`
	// BaseURL System One 决策端点（Jev 非聊天模型，不走 OpenAI chat 格式）
	BaseURL string `mapstructure:"base_url"`
	// Model Jev 模型 slug（建议固定版本号以获得稳定阈值）
	Model string `mapstructure:"model"`
	// TimeoutSeconds 单次决策调用超时（秒）；命令参数补抽复用同值
	TimeoutSeconds int `mapstructure:"timeout_seconds"`
}

func setIntentJudgeDefaults(v *viper.Viper) {
	v.SetDefault("ai.intent_judge.api_key", "")
	v.SetDefault("ai.intent_judge.base_url", "https://openrouter.ai/api/alpha/decisions")
	v.SetDefault("ai.intent_judge.model", "typesafe/jev-1.13")
	v.SetDefault("ai.intent_judge.timeout_seconds", 3)
}
