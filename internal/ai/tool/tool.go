// Package tool 定义 AI 可调用工具（Tool）与并发安全的工具注册表（Registry），
// 供对话（ChatService 工具循环）注册、检索与执行工具。
package tool

import (
	"context"

	"github.com/cloudwego/eino/schema"
)

// Tool 描述一个可供 AI 调用的工具。
//
// 契约：
//   - Info 与 Handler 均不能为空（Registry.Register 会校验）；
//   - Handler 由 Registry.Call 在锁外执行，必须自行保证并发安全
//     （同一工具可能被多条会话的 goroutine 同时调用）；
//   - argsJSON 为 LLM 生成的 JSON 参数字符串，Handler 需自行解析与校验；
//     解析/执行失败返回 error 即可——对话循环会把错误文本作为工具结果回传给 LLM
//     （不中断对话），因此错误信息宜写成模型可理解的说明。
type Tool struct {
	// Info Eino 标准工具元信息，可直接传给 ToolCallingChatModel.WithTools()
	Info *schema.ToolInfo
	// Handler 工具执行函数，argsJSON 为 LLM 传来的 JSON 编码参数
	Handler func(ctx context.Context, argsJSON string) (string, error)
}

// CallerIdentity 工具调用者身份：当前对话发送者的平台标识。
// 由工具调用循环在执行 handler 前注入 ctx，工具据此识别"现在是谁在对话"，
// 无需 LLM 传递 user_id（与 conduit MessageContext.UserID、插件 KV 键同源），
// 从根上避免模型抄写 ID 出错。
type CallerIdentity struct {
	// Platform 消息平台（qq/wechat/telegram/napcat…）
	Platform string
	// PlatformUserID 发送者平台用户 ID 字符串（如 QQ 号 "123456"）
	PlatformUserID string
	// GroupID 由宿主注入，模型不能指定限流/缓存作用域。
	GroupID string
}

// callerKey ctx 键类型（私有，避免与其他包的 WithValue 键冲突）
type callerKey struct{}

// WithCaller 将调用者身份注入 ctx，供工具 handler 读取。
func WithCaller(ctx context.Context, id CallerIdentity) context.Context {
	return context.WithValue(ctx, callerKey{}, id)
}

// CallerFrom 从 ctx 读取调用者身份；未注入时返回零值与 false。
func CallerFrom(ctx context.Context) (CallerIdentity, bool) {
	id, ok := ctx.Value(callerKey{}).(CallerIdentity)
	return id, ok
}
