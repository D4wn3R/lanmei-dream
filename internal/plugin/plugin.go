package plugin

import (
	"context"

	"github.com/DaWesen/lanmei-dream/internal/ai/tool"
	"github.com/DaWesen/lanmei-dream/internal/command"
	"github.com/DaWesen/lanmei-dream/internal/database"
	"github.com/DaWesen/lanmei-dream/internal/gateway"
	"github.com/cloudwego/eino/schema"
	"github.com/zrurf/conduit"
	"go.uber.org/zap"
)

// Plugin 是插件的顶层接口，定义 Info 与 OnInit/OnStart/OnStop 生命周期钩子：
// OnInit 注册 Pass/Pipeline/Subtree 到 Conduit 引擎，OnStart 对外提供服务，
// OnStop 清理资源与后台任务。
type Plugin interface {
	// Info 返回插件元信息，在 OnInit 之前调用，用于注册和展示。
	Info() PluginInfo

	// OnInit 初始化插件：在此注册 Pass/Pipeline/Subtree、斜杠命令，
	// 并完成内部状态初始化（如建表检查、缓存预热）。
	OnInit(ctx *PluginContext) error

	// OnStart 启动插件，在 OnInit 之后调用，适合启动后台任务
	// （如定时检查、事件监听）；不需要后台任务时直接返回 nil。
	OnStart(ctx *PluginContext) error

	// OnStop 停止插件，在插件卸载或引擎关闭时调用，
	// 应清理后台 goroutine、网络连接与文件句柄。
	OnStop(ctx *PluginContext) error
}

// PluginInfo 描述插件的元数据，Registry 据此展示、注册命令并检查依赖。
type PluginInfo struct {
	// ID 插件唯一标识，全局不可重复；小写字母加下划线（如 "signin"），
	// 同时作为 Pass/Pipeline/Subtree 的 ID 前缀（如 "plugin.signin.pass.execute"）。
	ID string

	// Name 插件显示名称，如 "签到"
	Name string

	// Description 插件功能描述
	Description string

	// Version 插件版本号，如 "1.0.0"
	Version string

	// Commands 插件提供的斜杠命令列表，Registry 在 OnInit 阶段注册到 command.System。
	Commands []CommandDef

	// SubtreeID 插件注册的行为树子树 ID；纯命令插件可留空。
	// Registry 会以 SubtreeRef 形式将其挂载到主行为树。
	SubtreeID string

	// Tools 插件提供的 AI 工具列表，Registry 在 OnInit 阶段注册到 ToolReg。
	Tools []ToolDef
}

// CommandDef 描述一个斜杠命令。
type CommandDef struct {
	// Name 命令名（不含 / 前缀），如 "签到"
	Name string

	// Description 命令描述，用于帮助信息和意图分析
	Description string

	// Order 帮助列表排序权重，小的在前；0（未设置）排在末尾
	Order int
}

// ToolDef 描述插件提供的 AI 工具
type ToolDef struct {
	Name        string
	Description string
	Parameters  *schema.ParamsOneOf // 使用 Eino 标准参数定义
	Handler     func(ctx context.Context, argsJSON string) (string, error)
}

// MessageObserver 是插件的可选扩展接口：实现它的插件可旁路观察每条已完成处理的
// 消息上下文，但不参与行为树决策、不改变消息走向。
//
// 与 Pass/Subtree 的区别：行为树的子树一旦匹配成功便独占该消息（其余分支不再运行），
// 因此「只看不抢」的插件必须用观察者而不是子树。
//
// 实现约束：ObserveMessage 在消息处理回调中同步调用，必须立即返回，只允许入队、计数等
// 轻量操作；网络、数据库、等待类工作须转到插件自有协程。实现方自行保证并发安全，
// 并自行过滤不关心的消息（如私聊、事件、机器人自身消息）。
type MessageObserver interface {
	ObserveMessage(ctx *conduit.MessageContext)
}

// MessageActions 是插件可用的出站动作能力：撤回消息、禁言群成员、主动发送文本。
//
// 由网关实现并在注册表装配时注入 PluginContext.Actions；为 nil 时插件无法执行出站动作。
// 撤回 / 禁言返回平台动作响应，失败原因（如机器人无群管理权限）由响应如实表达。
type MessageActions interface {
	// RecallMessage 撤回指定消息；connID 失效时按 platform 回退到同平台连接。
	RecallMessage(ctx context.Context, platform, connID, messageID string) (*gateway.ActionResponse, error)

	// BanGroupMember 禁言群成员，seconds 为 0 表示解除禁言。
	BanGroupMember(ctx context.Context, platform, connID, groupID, userID string, seconds int) (*gateway.ActionResponse, error)

	// SendTextTo 主动发送纯文本到指定群（isGroup=true）或私聊用户。
	SendTextTo(ctx context.Context, platform, connID string, isGroup bool, targetID, text string) error

	// SendAtTextTo 在群里发送「@成员 + 文本」的公开消息，用于违规告知等群内提示。
	SendAtTextTo(ctx context.Context, platform, connID, groupID, atUserID, text string) error
}

// PluginContext 是插件生命周期方法的上下文参数，提供对引擎、存储、数据库、
// 命令系统和注册表的访问。所有插件的 OnInit/OnStart/OnStop 共享同一实例。
type PluginContext struct {
	// Engine Conduit 消息处理引擎，插件通过它注册 Pass、Pipeline、Subtree
	Engine *conduit.Engine

	// Store 全局状态存储，用于读写跨请求的持久化数据（Redis，易失）
	Store conduit.StateStore

	// KV 受限键值存储（PostgreSQL 后端，持久化不丢）。
	// 插件私有业务数据应优先使用本存储（语义类似前端 IndexedDB），
	// 而非直接操作 DB；按 pluginID 自动隔离命名空间。
	KV *database.PluginKVStore

	DB *database.DB

	// Actions 出站动作能力（撤回 / 禁言 / 主动发送），由网关实现；为 nil 时不可用。
	Actions MessageActions

	CmdSys *command.System

	// Registry 插件注册表，插件通过它跟踪注册的资源（TrackPass/TrackPipeline），
	// 以便卸载时自动清理。
	Registry *Registry

	ToolReg *tool.Registry

	Logger *zap.Logger

	Ctx context.Context
}

// PassID 生成插件 Pass 的注册 ID，格式 plugin.{pluginID}.pass.{passName}
// （如 plugin.signin.pass.execute）。
func PassID(pluginID, passName string) string {
	return "plugin." + pluginID + ".pass." + passName
}

// PipelineID 生成插件 Pipeline 的注册 ID，格式 plugin.{pluginID}.pipeline.{pipelineName}
// （如 plugin.signin.pipeline.main）。
func PipelineID(pluginID, pipelineName string) string {
	return "plugin." + pluginID + ".pipeline." + pipelineName
}

// SubtreeID 生成插件 Subtree 的注册 ID，格式 plugin.{pluginID}.subtree
// （如 plugin.signin.subtree）。
func SubtreeID(pluginID string) string {
	return "plugin." + pluginID + ".subtree"
}

// StoreKey 生成插件状态存储的键，格式 plugin:{pluginID}:{key}；
// 借助 conduit.MakeStoreKey 保证命名空间隔离。
func StoreKey(pluginID, key string) string {
	return conduit.MakeStoreKey("plugin", pluginID, key)
}
