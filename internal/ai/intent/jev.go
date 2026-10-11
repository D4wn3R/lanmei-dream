package intent

// JevAdapter：基于 TypeSafe Jev（System-1 结构化决策模型）的意图判断适配器。
//
// Jev 不生成文本，对 state 并行回答一组带类型的问题（choice/noul），
// 经 OpenRouter 专用决策端点调用（非 OpenAI chat 格式），鉴权与计费走普通
// OpenRouter API Key。一次请求并行问齐意图/命令/工具/提及，端到端 70~500ms，
// 显著快于生成式 LLM 的意图分析；问题措辞与 buildIntentPrompt 的判断标准保持一致。
//
// 非生成式的固有限制（已在结果中如实反映）：
//   - 抽不出自由格式的命令参数 → 命令意图命中后用注入的 LLM 补抽（extractCommandArgs）
//   - 产不出提及证据文本 → MentionEvidence 恒为空（该字段现仅用于日志）

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/DaWesen/lanmei-dream/internal/ai/llm"
	"go.uber.org/zap"
)

// Jev 默认参数（可被 [ai.intent_judge] 配置覆盖）。
const (
	// DefaultJevBaseURL OpenRouter 的 System One 决策端点。
	DefaultJevBaseURL = "https://openrouter.ai/api/alpha/decisions"
	// DefaultJevModel Jev 模型 slug；固定版本号以获得稳定阈值，响应 model 字段会带日期快照。
	DefaultJevModel = "typesafe/jev-1.13"
	// DefaultJevTimeoutSeconds 单次决策调用默认超时（秒）。
	DefaultJevTimeoutSeconds = 3
	// maxJevChoiceOptions choice 问题选项数上限（API 硬上限 255，留余量）。
	maxJevChoiceOptions = 250
	// maxJevResponseBytes 响应体读取上限（answers 为小体积结构化数据）。
	maxJevResponseBytes = 1 << 18
	// jevRetryBackoff 429/529 时的单次重试退避（官方建议退避重试而非立即重试）。
	jevRetryBackoff = 300 * time.Millisecond
	// jevMentionThreshold noul → IsTalkingToBot 的布尔化阈值；
	// noul 同时透传给 MentionConfidence，话题系统仍按自己的 strong/weak 阈值消费。
	jevMentionThreshold = 0.5
)

// JevOptions JevAdapter 的构造参数（由 [ai.intent_judge] 配置映射而来）。
type JevOptions struct {
	// BaseURL System One 决策端点；空取 DefaultJevBaseURL。
	BaseURL string
	// APIKey OpenRouter API Key（鉴权与计费均走 OpenRouter，必填）。
	APIKey string
	// Model Jev 模型 slug；空取 DefaultJevModel。
	Model string
	// Timeout 单次决策调用超时；<=0 取 DefaultJevTimeoutSeconds。
	Timeout time.Duration
}

// JevAdapter 基于 Jev 的意图判断适配器。
// llmClient 用于命令意图命中后补抽自由格式参数（Jev 非生成式抽不出参数），
// 可为 nil（LLM 未配置时跳过补抽，命令空参执行）。
type JevAdapter struct {
	opts   JevOptions
	client *http.Client
	llm    llm.LLMClient
	logger *zap.Logger
}

// NewJevAdapter 创建 Jev 适配器。
func NewJevAdapter(opts JevOptions, llmClient llm.LLMClient, logger *zap.Logger) *JevAdapter {
	if opts.BaseURL == "" {
		opts.BaseURL = DefaultJevBaseURL
	}
	if opts.Model == "" {
		opts.Model = DefaultJevModel
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultJevTimeoutSeconds * time.Second
	}
	return &JevAdapter{
		opts:   opts,
		client: &http.Client{},
		llm:    llmClient,
		logger: logger,
	}
}

// Judge 发起一次 Jev 决策调用（意图/命令/工具/提及并行问答），映射为 *Result；
// 命令意图命中时追加一次 LLM 调用补抽自由格式参数。
func (a *JevAdapter) Judge(ctx context.Context, userMsg string, judgeCtx *JudgeContext, commands []CommandDef, tools []ToolDef) (*Result, error) {
	jctx, cancel := context.WithTimeout(ctx, a.opts.Timeout)
	defer cancel()

	payload, err := json.Marshal(a.buildRequest(userMsg, judgeCtx, commands, tools))
	if err != nil {
		return nil, fmt.Errorf("intent: jev marshal request: %w", err)
	}
	resp, err := a.callJev(jctx, payload)
	if err != nil {
		return nil, err
	}
	a.logger.Debug("intent: jev usage",
		zap.String("model", resp.Model),
		zap.Int("input_tokens", resp.Usage.InputTokens),
		zap.Float64("cost", resp.Usage.Cost))

	result := mapJevResponse(resp, commands, tools)
	if result.Intent == IntentCommand && result.CommandName != "" {
		result.CommandArgs = a.extractCommandArgs(ctx, userMsg, result.CommandName, commands)
	}
	return result, nil
}

// ---- 请求构建 ----

// jevQuestion 一条带类型的问题（本适配器只用 choice/noul 两种原语）。
type jevQuestion struct {
	Type         string            `json:"type"` // "choice" | "noul"
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria,omitempty"` // choice: 选项→描述；noul: {"true":..,"false":..}
}

// jevRequest System One 请求体：state + questions（多问题并行、相互独立地评估）。
type jevRequest struct {
	Model     string                 `json:"model"`
	State     map[string]any         `json:"state"`
	Questions map[string]jevQuestion `json:"questions"`
}

// buildRequest 组装决策请求：state 携带当前消息与群聊上下文（来自 JudgeContext），
// 问题集按可用命令/工具与群/私聊动态裁剪（choice 选项非空才携带该问题）。
func (a *JevAdapter) buildRequest(userMsg string, judgeCtx *JudgeContext, commands []CommandDef, tools []ToolDef) *jevRequest {
	state := map[string]any{"current_message": userMsg}
	if judgeCtx != nil {
		if len(judgeCtx.BotNames) > 0 {
			state["bot_names"] = judgeCtx.BotNames
		}
		if len(judgeCtx.Recent) > 0 {
			recent := make([]map[string]string, 0, len(judgeCtx.Recent))
			for _, m := range judgeCtx.Recent {
				recent = append(recent, map[string]string{"speaker": m.Speaker, "content": m.Content})
			}
			state["recent"] = recent
		}
	}

	questions := map[string]jevQuestion{
		"intent": {
			Type:         "choice",
			Instructions: "判断这条消息的主要意图",
			Criteria: map[string]string{
				"chat":    "闲聊、提问、角色扮演对话",
				"command": "用户想执行某个操作/命令",
				"tool":    "用户想使用某个工具/功能",
				"ignore":  "无需回复的内容：纯表情、系统通知、无意义重复，以及用户让机器人闭嘴/别说话/停止发言（机器人应静默不回应；机器人没有\"闭嘴\"命令，这类话不是要执行命令）",
			},
		},
	}
	if criteria := commandCriteria(commands); len(criteria) > 0 {
		criteria["none"] = "以上命令都不匹配：用户并非想执行列表中的命令"
		questions["command"] = jevQuestion{
			Type:         "choice",
			Instructions: "若用户想执行命令，是哪个命令；不匹配任何命令时回答 none",
			Criteria:     criteria,
		}
	}
	if criteria := toolCriteria(tools); len(criteria) > 0 {
		criteria["none"] = "以上工具都不匹配：用户并非想使用列表中的工具"
		questions["tool"] = jevQuestion{
			Type:         "choice",
			Instructions: "若用户想使用工具，是哪个工具；不匹配任何工具时回答 none",
			Criteria:     criteria,
		}
	}
	if judgeCtx != nil {
		questions["is_talking_to_bot"] = jevQuestion{
			Type:         "noul",
			Instructions: "判断用户是否在跟机器人说话（期望机器人回应），结合 recent 上下文做指代消解",
			Criteria: map[string]string{
				"true":  "直接称呼、让机器人做事、向机器人提问、表达对机器人的情感，或经指代消解确认在跟机器人说话",
				"false": "仅向他人提及/转述机器人，或完全不涉及机器人",
			},
		}
		questions["mention_role"] = jevQuestion{
			Type:         "choice",
			Instructions: "用户提及机器人的方式",
			Criteria: map[string]string{
				"at":                "直接 @ 机器人",
				"vocative":          "呼格，直接叫名字（如\"蓝妹，在吗\"）",
				"subject":           "机器人是句子主语（如\"蓝妹去不去爬山\"）",
				"imperative_object": "让机器人做事的祈使/请求宾语（如\"帮我签到\"）",
				"relative_clause":   "关系从句提及（如\"说蓝妹好的那个人是谁\"）",
				"conditional":       "条件句/假设句提及（如\"如果蓝妹在就好了\"）",
				"topic_marker":      "话题标记式提及（如\"说到蓝妹…\"）",
				"affection":         "情感/评价对象（如\"蓝妹真可爱\"）",
				"relay":             "传话/第三人称提及，不期望机器人回应（如\"你们知道蓝妹吗\"）",
				"none":              "未提及机器人",
			},
		}
	}
	return &jevRequest{Model: a.opts.Model, State: state, Questions: questions}
}

// commandCriteria / toolCriteria 把定义列表转为 choice criteria（选项名→语义描述）；
// 选项数超限截断（API 硬上限 255，留余量）。
func commandCriteria(commands []CommandDef) map[string]string {
	criteria := make(map[string]string, min(len(commands), maxJevChoiceOptions))
	for _, c := range commands[:min(len(commands), maxJevChoiceOptions)] {
		criteria[c.Name] = c.Description
	}
	return criteria
}

func toolCriteria(tools []ToolDef) map[string]string {
	criteria := make(map[string]string, min(len(tools), maxJevChoiceOptions))
	for _, t := range tools[:min(len(tools), maxJevChoiceOptions)] {
		criteria[t.Name] = t.Description
	}
	return criteria
}

// ---- 调用与响应解析 ----

type jevAnswer struct {
	Type          string             `json:"type"`
	Noul          *float64           `json:"noul"`
	Choice        string             `json:"choice"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

type jevUsage struct {
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	Cost         float64 `json:"cost"`
}

type jevResponse struct {
	Model   string               `json:"model"`
	Answers map[string]jevAnswer `json:"answers"`
	Usage   jevUsage             `json:"usage"`
}

// callJev 发起决策调用；429/529 退避后单次重试，其余非 2xx 与协议错误一律返回 error
//（由上层按现有逻辑降级，不在本适配器内回退）。
func (a *JevAdapter) callJev(ctx context.Context, payload []byte) (*jevResponse, error) {
	resp, err := a.post(ctx, payload)
	if err == nil && (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == 529) {
		_ = resp.Body.Close()
		select {
		case <-time.After(jevRetryBackoff):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		resp, err = a.post(ctx, payload)
	}
	if err != nil {
		return nil, fmt.Errorf("intent: jev call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJevResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("intent: jev read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("intent: jev http %d: %s", resp.StatusCode, bodySnippet(body))
	}
	var out jevResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("intent: jev decode: %w", err)
	}
	if len(out.Answers) == 0 {
		return nil, fmt.Errorf("intent: jev empty answers")
	}
	return &out, nil
}

// post 发送决策请求（Bearer 鉴权，标准 System One JSON 协议）。
func (a *JevAdapter) post(ctx context.Context, payload []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.opts.BaseURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.opts.APIKey)
	req.Header.Set("Content-Type", "application/json")
	return a.client.Do(req)
}

// mapJevResponse 把 answers 映射为 *Result；未知意图值就地降级 IntentChat（与 parseResult 语义一致，
// 保留返回的置信度），命令/工具名对照快照校验，防止越界选项名流入执行层。
func mapJevResponse(resp *jevResponse, commands []CommandDef, tools []ToolDef) *Result {
	result := &Result{Intent: IntentChat, Confidence: 0.5}
	if ans, ok := resp.Answers["intent"]; ok && ans.Type == "choice" {
		result.Confidence = clamp01(ans.Confidence)
		switch Intent(ans.Choice) {
		case IntentChat, IntentCommand, IntentTool, IntentIgnore:
			result.Intent = Intent(ans.Choice)
		}
	}

	if result.Intent == IntentCommand {
		if ans, ok := resp.Answers["command"]; ok && ans.Type == "choice" &&
			slices.ContainsFunc(commands, func(c CommandDef) bool { return c.Name == ans.Choice }) {
			result.CommandName = ans.Choice
		}
	}
	if result.Intent == IntentTool {
		if ans, ok := resp.Answers["tool"]; ok && ans.Type == "choice" &&
			slices.ContainsFunc(tools, func(t ToolDef) bool { return t.Name == ans.Choice }) {
			result.ToolName = ans.Choice
		}
	}
	if ans, ok := resp.Answers["is_talking_to_bot"]; ok && ans.Type == "noul" && ans.Noul != nil {
		result.IsTalkingToBot = *ans.Noul >= jevMentionThreshold
		result.MentionConfidence = clamp01(*ans.Noul)
	}
	if ans, ok := resp.Answers["mention_role"]; ok && ans.Type == "choice" {
		result.MentionRole = ans.Choice
	}
	return result
}

// ---- 命令参数补抽 ----

// extractCommandArgs 命令意图命中后用 LLM 补抽自由格式参数（如"发个Go的表情"→["Go"]；
// Jev 非生成式抽不出参数）。任何失败静默返回 nil：参数缺失时命令 handler 按现有逻辑回 usage 提示。
// 使用父级 ctx（沿用 Analyzer 的意图总闸超时），内部再套独立短超时保证快速失败。
func (a *JevAdapter) extractCommandArgs(ctx context.Context, userMsg, cmdName string, commands []CommandDef) []string {
	if a.llm == nil {
		return nil
	}
	var desc string
	for _, c := range commands {
		if c.Name == cmdName {
			desc = c.Description
			break
		}
	}
	prompt := fmt.Sprintf(`你是命令参数抽取器。从用户消息中提取命令「%s」（%s）的参数。
仅输出 JSON 字符串数组（如 ["Go"]），无参数输出 []，不要输出任何其他内容。`, cmdName, desc)

	actx, cancel := context.WithTimeout(ctx, a.opts.Timeout)
	defer cancel()
	resp, err := a.llm.Chat(actx, &llm.ChatRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: prompt},
			{Role: llm.RoleUser, Content: userMsg},
		},
		MaxTokens:       intPtr(128),
		DisableThinking: boolPtr(true),
	})
	if err != nil {
		a.logger.Warn("intent: jev 命令参数补抽失败", zap.String("command", cmdName), zap.Error(err))
		return nil
	}
	var args []string
	if err := json.Unmarshal([]byte(extractJSON(resp.Content)), &args); err != nil {
		a.logger.Warn("intent: jev 命令参数补抽解析失败", zap.String("command", cmdName))
		return nil
	}
	return args
}

// extractJSON 定位文本中的 JSON 数组（LLM 可能包在 markdown 代码块里）。
func extractJSON(raw string) string {
	start := strings.IndexByte(raw, '[')
	if start == -1 {
		return raw
	}
	end := strings.LastIndexByte(raw, ']')
	if end < start {
		return raw
	}
	return raw[start : end+1]
}

// bodySnippet 截断响应体用于错误日志。
func bodySnippet(body []byte) string {
	s := string(body)
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

// intPtr 返回 int 指针（ChatRequest 的可选字段用）。
func intPtr(n int) *int { return &n }
