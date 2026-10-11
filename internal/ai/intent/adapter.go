package intent

import "context"

// JudgeAdapter 是意图+提及判断适配器：把"一条消息 → 意图与提及判断"抽象为可替换实现。
// 两种实现并存，部署时按配置二选一（[ai.intent_judge].api_key 非空即 Jev，否则 LLM）：
//   - LLMAdapter：现有生成式 LLM 路径（prompt + chat completions）
//   - JevAdapter：TypeSafe Jev 结构化决策模型（OpenRouter System One 端点）
//
// 实现必须并发安全；调用方保证传入的 commands/tools 为快照副本。
type JudgeAdapter interface {
	// Judge 判断用户消息的意图；judgeCtx 非 nil 时同时完成群聊提及判断。
	// 返回 error 仅表示调用失败（网络/协议错误），由上层按现有逻辑降级；
	// 判断结果本身的异常（非法意图值、未知命令名等）由实现就地降级，不返回 error。
	Judge(ctx context.Context, userMsg string, judgeCtx *JudgeContext, commands []CommandDef, tools []ToolDef) (*Result, error)
}
