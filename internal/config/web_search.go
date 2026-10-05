package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/viper"
)

// WebSearchConfig 是 Agent 的联网能力配置，不参与业务插件生命周期。
type WebSearchConfig struct {
	Enabled                   bool     `mapstructure:"enabled"`
	BaseURL                   string   `mapstructure:"base_url"`
	Engines                   []string `mapstructure:"engines"`
	Mode                      string   `mapstructure:"mode"`
	MaxResults                int      `mapstructure:"max_results"`
	TimeoutSeconds            int      `mapstructure:"timeout_seconds"`
	MaxCallsPerTurn           int      `mapstructure:"max_calls_per_turn"`
	TotalSearchSeconds        int      `mapstructure:"total_search_seconds"`
	AnswerReserveSeconds      int      `mapstructure:"answer_reserve_seconds"`
	MaxConcurrency            int      `mapstructure:"max_concurrency"`
	RequestsPerMinutePerScope int      `mapstructure:"requests_per_minute_per_scope"`
	BurstPerScope             int      `mapstructure:"burst_per_scope"`
	CacheTTLSeconds           int      `mapstructure:"cache_ttl_seconds"`
	CacheMaxEntries           int      `mapstructure:"cache_max_entries"`
	MaxResponseBytes          int      `mapstructure:"max_response_bytes"`
	MaxOutputBytes            int      `mapstructure:"max_output_bytes"`
}

func webSearchDefaults() map[string]any {
	return map[string]any{
		"enabled": false, "base_url": "http://openserp:7000",
		"engines": []string{"bing", "baidu"}, "mode": "any", "max_results": 5,
		"timeout_seconds": 10, "max_calls_per_turn": 2, "total_search_seconds": 15,
		"answer_reserve_seconds": 10, "max_concurrency": 2,
		"requests_per_minute_per_scope": 6, "burst_per_scope": 2,
		"cache_ttl_seconds": 120, "cache_max_entries": 256,
		"max_response_bytes": 1048576, "max_output_bytes": 16384,
	}
}

func setWebSearchDefaults(v *viper.Viper) {
	for key, value := range webSearchDefaults() {
		v.SetDefault("ai.web_search."+key, value)
	}
}

func applyWebSearchEnv(v *viper.Viper) {
	for key := range webSearchDefaults() {
		if value, ok := os.LookupEnv("LANMEI_AI_WEB_SEARCH_" + strings.ToUpper(key)); ok {
			if key == "engines" {
				v.Set("ai.web_search."+key, strings.Split(value, ","))
			} else {
				v.Set("ai.web_search."+key, value)
			}
		}
	}
}

// Validate 拒绝无界额度和模型可控的目标；内网服务地址是合法的管理员配置。
func (c WebSearchConfig) Validate() error {
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return fmt.Errorf("ai.web_search.base_url 必须是无凭据、路径、查询或片段的 HTTP(S) 服务地址")
	}
	if c.Mode != "any" && c.Mode != "fast" && c.Mode != "balanced" {
		return fmt.Errorf("ai.web_search.mode 必须是 any、fast 或 balanced")
	}
	if len(c.Engines) == 0 || len(c.Engines) > 6 {
		return fmt.Errorf("ai.web_search.engines 必须包含 1 至 6 个引擎")
	}
	seen := make(map[string]bool)
	for _, engine := range c.Engines {
		switch engine {
		case "bing", "baidu", "google", "yandex", "duckduckgo", "ecosia":
		default:
			return fmt.Errorf("ai.web_search.engines 包含不支持的引擎")
		}
		if seen[engine] {
			return fmt.Errorf("ai.web_search.engines 不可重复")
		}
		seen[engine] = true
	}
	values := map[string]struct{ value, max int }{
		"max_results": {c.MaxResults, 5}, "max_calls_per_turn": {c.MaxCallsPerTurn, 2},
		"timeout_seconds": {c.TimeoutSeconds, 60}, "total_search_seconds": {c.TotalSearchSeconds, 60},
		"answer_reserve_seconds": {c.AnswerReserveSeconds, 60}, "max_concurrency": {c.MaxConcurrency, 64},
		"requests_per_minute_per_scope": {c.RequestsPerMinutePerScope, 600}, "burst_per_scope": {c.BurstPerScope, 100},
		"cache_ttl_seconds": {c.CacheTTLSeconds, 3600}, "cache_max_entries": {c.CacheMaxEntries, 4096},
		"max_response_bytes": {c.MaxResponseBytes, 1048576}, "max_output_bytes": {c.MaxOutputBytes, 16384},
	}
	for name, bound := range values {
		if bound.value <= 0 || bound.value > bound.max {
			return fmt.Errorf("ai.web_search.%s 必须在 1..%d 内", name, bound.max)
		}
	}
	if c.MaxOutputBytes < 1024 {
		return fmt.Errorf("ai.web_search.max_output_bytes 不可小于 1024")
	}
	return nil
}
