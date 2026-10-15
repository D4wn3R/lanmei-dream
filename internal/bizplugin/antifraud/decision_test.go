package antifraud

import (
	"math"
	"testing"

	"github.com/DaWesen/lanmei-dream/internal/config"
)

func TestViolationScore(t *testing.T) {
	tests := []struct {
		name          string
		probabilities []float64
		wantViolation float64
	}{
		{"正常占主导", []float64{0.95, 0.01, 0.01, 0.01, 0.01, 0.01, 0, 0, 0, 0}, 0.05},
		{"违规占主导", []float64{0.05, 0, 0, 0.95, 0, 0, 0, 0, 0, 0}, 0.95},
		{"概率缺失", nil, 0},
		{"概率为负溢出保护", []float64{1.5}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := violationScore(tt.probabilities); math.Abs(got-tt.wantViolation) > 1e-9 {
				t.Fatalf("violationScore() = %v, want %v", got, tt.wantViolation)
			}
		})
	}
}

func TestClassify(t *testing.T) {
	thresholds := Thresholds{Auto: 0.9, Report: 0.6}

	tests := []struct {
		name string
		item InferItem
		want Level
	}{
		{
			name: "违规分超过自动阈值",
			item: violationItem([]float64{0.04, 0, 0, 0.96, 0, 0, 0, 0, 0, 0}),
			want: LevelAuto,
		},
		{
			name: "违规分落在上报区间",
			item: violationItem([]float64{0.3, 0, 0, 0.7, 0, 0, 0, 0, 0, 0}),
			want: LevelReport,
		},
		{
			name: "违规分低于上报阈值",
			item: violationItem([]float64{0.5, 0, 0, 0.5, 0, 0, 0, 0, 0, 0}),
			want: LevelNone,
		},
		{
			name: "top1 为正常类则不处置",
			item: InferItem{LabelID: normalLabelID, LabelKey: "normal", LabelName: "正常",
				Probabilities: []float64{0.4, 0.35, 0.25, 0, 0, 0, 0, 0, 0, 0}},
			want: LevelNone,
		},
		{
			name: "概率缺失",
			item: InferItem{LabelID: 3, LabelKey: "brushing"},
			want: LevelNone,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classify(toVerdict(tt.item), thresholds); got != tt.want {
				t.Fatalf("classify() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNormalizeConfigKeepsAutoThresholdNotBelowReport(t *testing.T) {
	cfg := normalizeConfig(config.AntifraudConfig{Endpoint: "http://x", AutoThreshold: 0.5, ReportThreshold: 0.8})
	if cfg.AutoThreshold != cfg.ReportThreshold || cfg.AutoThreshold != 0.8 {
		t.Fatalf("自动阈值应被抬到上报阈值：auto=%v report=%v", cfg.AutoThreshold, cfg.ReportThreshold)
	}
}

func TestNormalizeConfigFillsDefaults(t *testing.T) {
	cfg := normalizeConfig(config.AntifraudConfig{Endpoint: "http://x"})
	if cfg.BatchSize != 16 || cfg.QueueSize != 256 || cfg.MinChars != 4 {
		t.Fatalf("零值未按默认补齐：%+v", cfg)
	}
	if cfg.RequestTimeoutSeconds != 8 || cfg.CircuitFailures != 5 {
		t.Fatalf("限流默认值不正确：%+v", cfg)
	}
	if cfg.AutoThreshold != 0.9 || cfg.ReportThreshold != 0.6 {
		t.Fatalf("阈值默认值应为 0.90/0.60：auto=%v report=%v", cfg.AutoThreshold, cfg.ReportThreshold)
	}
}

func TestParseStaticSuperUsers(t *testing.T) {
	got := parseStaticSuperUsers("qq:100, qq:200,napcat:300,非法项,wechat:")
	if len(got) != 2 {
		t.Fatalf("平台分组数量 = %d, want 2", len(got))
	}
	if len(got["qq"]) != 2 || got["napcat"][0] != "300" {
		t.Fatalf("解析结果不符合预期：%+v", got)
	}
}

func TestFormatDuration(t *testing.T) {
	cases := map[int]string{0: "0 秒", 30: "30 秒", 300: "5 分钟", 7200: "2 小时"}
	for seconds, want := range cases {
		if got := formatDuration(seconds); got != want {
			t.Fatalf("formatDuration(%d) = %q, want %q", seconds, got, want)
		}
	}
}
