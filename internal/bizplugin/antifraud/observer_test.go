package antifraud

import (
	"testing"

	"github.com/zrurf/conduit"

	"github.com/DaWesen/lanmei-dream/internal/config"
	"github.com/DaWesen/lanmei-dream/internal/gateway"
)

func TestObserveBuildsTaskForGroupMessage(t *testing.T) {
	p, _ := newTestPlugin(t, testConfig(""), &fakeActions{})
	ctx := testMessageContext("2002", "g-1", "点击链接领取红包")

	got, ok := p.buildTask(ctx)
	if !ok {
		t.Fatal("群消息应通过准入")
	}
	if got.text != "点击链接领取红包" || got.groupID != "g-1" || got.userID != "2002" {
		t.Fatalf("任务内容不正确：%+v", got)
	}
	if got.hash == "" || got.hash != hashContent(got.text) {
		t.Fatalf("任务缺少内容哈希：%+v", got)
	}
}

func TestObserveFilters(t *testing.T) {
	tests := []struct {
		name   string
		cfg    func(*config.AntifraudConfig)
		mutate func(*conduit.MessageContext)
		want   bool
	}{
		{
			name:   "私聊默认不判定",
			mutate: func(ctx *conduit.MessageContext) { ctx.IsGroup = false; ctx.GroupID = "" },
			want:   false,
		},
		{
			name:   "私聊开启后判定",
			cfg:    func(c *config.AntifraudConfig) { c.MonitorPrivate = true },
			mutate: func(ctx *conduit.MessageContext) { ctx.IsGroup = false; ctx.GroupID = "" },
			want:   true,
		},
		{
			name:   "事件消息不判定",
			mutate: func(ctx *conduit.MessageContext) { ctx.Extra[extraMessageType] = "notice" },
			want:   false,
		},
		{
			name:   "机器人自身消息不判定",
			mutate: func(ctx *conduit.MessageContext) { ctx.UserID = "bot-1" },
			want:   false,
		},
		{
			name:   "超管消息不判定",
			mutate: func(ctx *conduit.MessageContext) { ctx.Extra[extraIsSuperUser] = true },
			want:   false,
		},
		{
			name:   "斜杠命令不判定",
			mutate: func(ctx *conduit.MessageContext) { setText(ctx, "/签到") },
			want:   false,
		},
		{
			name:   "过短文本不判定",
			mutate: func(ctx *conduit.MessageContext) { setText(ctx, "在") },
			want:   false,
		},
		{
			name: "黑名单群不判定",
			cfg:  func(c *config.AntifraudConfig) { c.GroupBlacklist = []string{"g-1"} },
			want: false,
		},
		{
			name: "白名单外群不判定",
			cfg:  func(c *config.AntifraudConfig) { c.GroupWhitelist = []string{"g-2"} },
			want: false,
		},
		{
			name: "白名单内群判定",
			cfg:  func(c *config.AntifraudConfig) { c.GroupWhitelist = []string{"g-1"} },
			want: true,
		},
		{
			name:   "纯图片消息不判定",
			mutate: func(ctx *conduit.MessageContext) { setSegments(ctx, []gateway.NormalizedSegment{{Type: "image"}}) },
			want:   false,
		},
		{
			name: "at 段被排除但正文参与判定",
			mutate: func(ctx *conduit.MessageContext) {
				setSegments(ctx, []gateway.NormalizedSegment{
					{Type: "at", Data: map[string]string{"user_id": "9999"}},
					{Type: "text", Text: "点击链接领取红包"},
				})
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig("")
			if tt.cfg != nil {
				tt.cfg(&cfg)
			}
			p, _ := newTestPlugin(t, cfg, &fakeActions{})
			ctx := testMessageContext("2002", "g-1", "点击链接领取红包")
			if tt.mutate != nil {
				tt.mutate(ctx)
			}
			if _, ok := p.buildTask(ctx); ok != tt.want {
				t.Fatalf("buildTask() ok = %v, want %v", ok, tt.want)
			}
		})
	}
}

// setText 替换消息的文本与文本段。
func setText(ctx *conduit.MessageContext, text string) {
	ctx.RawMsg = text
	setSegments(ctx, []gateway.NormalizedSegment{{Type: "text", Text: text}})
}

// setSegments 替换消息段（判定文本只取段内容）。
func setSegments(ctx *conduit.MessageContext, segs []gateway.NormalizedSegment) {
	ctx.Extra[extraSegments] = segs
}

func TestObserveDropsWhenQueueFull(t *testing.T) {
	cfg := testConfig("")
	cfg.QueueSize = 1
	p, _ := newTestPlugin(t, cfg, &fakeActions{})

	p.ObserveMessage(testMessageContext("2002", "g-1", "点击链接领取红包"))
	p.ObserveMessage(testMessageContext("2003", "g-1", "点击链接领取红包"))

	if got := p.dropped.Load(); got != 1 {
		t.Fatalf("队列满时丢弃计数 = %d, want 1", got)
	}
}

func TestObserveCooldownBlocksRepeatedJudgement(t *testing.T) {
	cfg := testConfig("")
	cfg.UserCooldownSeconds = 60
	p, _ := newTestPlugin(t, cfg, &fakeActions{})

	if _, ok := p.buildTask(testMessageContext("2002", "g-1", "点击链接领取红包")); !ok {
		t.Fatal("冷却期外应放行")
	}
	if _, ok := p.buildTask(testMessageContext("2002", "g-1", "第二条消息内容")); ok {
		t.Fatal("冷却期内同一用户不应重复判定")
	}
	if _, ok := p.buildTask(testMessageContext("2003", "g-1", "第二条消息内容")); !ok {
		t.Fatal("冷却按用户维度生效，其他用户应放行")
	}
}

func TestObserveAfterStopIsIgnored(t *testing.T) {
	p, _ := newTestPlugin(t, testConfig(""), &fakeActions{})
	p.stopped.Store(true)
	p.ObserveMessage(testMessageContext("2002", "g-1", "点击链接领取红包"))
	if len(p.queue) != 0 {
		t.Fatalf("停止后不应继续入队，队列入队数 = %d", len(p.queue))
	}
}

func TestJudgedTextFallsBackToRawMessage(t *testing.T) {
	ctx := testMessageContext("2002", "g-1", "回退文本内容")
	delete(ctx.Extra, extraSegments)
	if got := judgedText(ctx); got != "回退文本内容" {
		t.Fatalf("judgedText() = %q, want 回退文本内容", got)
	}
}
