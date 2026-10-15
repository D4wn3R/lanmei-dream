package antifraud

import (
	"strings"
	"testing"
)

// TestRecallNoticeText 覆盖群内告知文案：说清类别与处置、不复述违规内容、语气贴合人设。
func TestRecallNoticeText(t *testing.T) {
	t.Run("撤回并禁言", func(t *testing.T) {
		rec := &Record{LabelName: "刷单", Content: "兼职刷单日结", MuteStatus: ActionOK, MuteSeconds: 3600}
		got := recallNoticeText(rec)
		mustContain(t, got, "刷单")
		mustContain(t, got, "1 小时")
		if strings.Contains(got, rec.Content) {
			t.Fatalf("文案不得包含违规内容：%s", got)
		}
		if strings.ContainsAny(got, "#*`") || strings.Contains(got, "😀") {
			t.Fatalf("文案不应使用 Markdown 或 emoji：%s", got)
		}
	})

	t.Run("仅撤回未禁言", func(t *testing.T) {
		rec := &Record{LabelKey: "advertising", MuteStatus: ActionSkipped}
		got := recallNoticeText(rec)
		mustContain(t, got, "advertising")
		if strings.Contains(got, "冷静") {
			t.Fatalf("未禁言时不应提禁言：%s", got)
		}
	})

	t.Run("禁言失败不改口", func(t *testing.T) {
		rec := &Record{LabelName: "贷款征信", MuteStatus: ActionFailed, MuteError: "retcode=100"}
		got := recallNoticeText(rec)
		if strings.Contains(got, "冷静") || strings.Contains(got, "retcode") {
			t.Fatalf("禁言失败时既不宣称禁言也不暴露错误码：%s", got)
		}
	})
}
