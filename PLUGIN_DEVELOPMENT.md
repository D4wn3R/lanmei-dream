# 插件拓展规范

本规范定义蓝妹（lanmei-dream）插件系统的开发约定，面向在仓库内新增插件的开发者。WASM ABI 的字段级说明见 [internal/plugin/wasm_abi.md](internal/plugin/wasm_abi.md)，WIT 接口与 JSON ABI 的对应关系见 [schema/plugin/lanmei-plugin.wit](schema/plugin/lanmei-plugin.wit)，权限模型见 [internal/plugin/access_control.md](internal/plugin/access_control.md)。

## 1. 插件类型选择

| 类型 | 位置 | 适用场景 | 特性 |
|---|---|---|---|
| **Go 内置插件** | `internal/bizplugin/`（一般单文件一插件） | 核心业务、需直接使用宿主基础设施（DB、受限 KV、对象存储、视觉/LLM 服务、题库目录），或需参与行为树 | 编译期注册、随主进程发布、无沙箱 |
| **WASM 插件** | 独立 Go module（参考 `examples/plugin/signin`） | 第三方交付、动态安装/升级/卸载、同一插件多安装实例 | Extism 沙箱、UTF-8 JSON ABI、Casbin 角色/动作鉴权、运行时限额 |

选择原则：

- 需要宿主专用依赖（如媒体对象存储、视觉服务、LLM 客户端、题库目录）→ Go 内置插件
- 需要动态安装/卸载/升级，或同一插件多实例隔离 → WASM 插件
- 不确定时优先 Go 内置插件（开发成本低），后续需要再迁移到 WASM

## 2. 目录约定

```
internal/bizplugin/             # Go 内置插件（package bizplugin）
  signin.go                     # 主实现；一个文件可含多个插件（signin.go 同时实现 signin 与 signin_rank）
  random_beauty/                # 实现较复杂时可拆子包
  registry.go                   # RegisterBuiltins：按 [plugin.builtins] 开关统一注册
  README.md                     # 内置业务插件开发规范（事件类插件模范示例）
examples/plugin/<id>/           # WASM 插件参考实现（独立 Go module，tinygo 构建）
  go.mod
  main.go                       # 实现 lanmei.plugin/v1 ABI
data/plugins/                   # 运行期 Wasm 仓库（[plugin].root_dir，config.toml 中为 ./data/plugins，不入库）
  installed/<installationID>/   # Manager 托管目录（plugin.wasm），禁止手动修改
schema/plugin/lanmei-plugin.wit # WASM ABI 的 WIT 概念定义（改 ABI 时同步更新）
internal/plugin/wasm_abi.md     # JSON ABI 的字段、错误码、限额说明
```

当前没有本地投放目录（inbox）安装通道：WASM 插件只能通过 `/插件 安装 <HTTPS 直链>` 安装（见 `internal/plugin/wasm_command.go` 与 `WasmManager.Install`）。

## 3. Go 内置插件规范

### 3.1 骨架（参考实现 `internal/bizplugin/signin.go`）

```go
type XxxPlugin struct { /* 依赖在 OnInit 中从 PluginContext 获取 */ }

// Info 返回元信息（ID 唯一、命令/工具/子树声明）
func (p *XxxPlugin) Info() pluginpkg.PluginInfo {
    return pluginpkg.PluginInfo{
        ID:          "xxx",
        Name:        "名称",
        Description: "描述",
        Version:     "1.0.0",
        Commands:    []pluginpkg.CommandDef{{Name: "命令", Description: "描述", Order: 100}},
        SubtreeID:   pluginpkg.SubtreeID("xxx"),
        // 可选：Tools: []pluginpkg.ToolDef{{Name, Description, Parameters, Handler}}
    }
}

// OnInit 注册 Pass → Pipeline → Subtree，注册的资源全部 Track（卸载自动清理）
func (p *XxxPlugin) OnInit(ctx *pluginpkg.PluginContext) error { ... }

func (p *XxxPlugin) OnStart(*pluginpkg.PluginContext) error { return nil }
func (p *XxxPlugin) OnStop(*pluginpkg.PluginContext) error  { return nil }
```

`Info` 在注册、帮助列表、意图分析中可能被多次调用。`Registry.InitPlugin`（`internal/plugin/registry.go`）会自动把 `Info().Commands` 注册到命令系统、把 `Info().Tools` 注册到工具表，并按 `Info().SubtreeID` 生成子树引用；插件自己只需在 `OnInit` 中注册 Pass/Pipeline/Subtree。

`PluginContext` 可访问：`Engine`（Conduit 引擎）、`Store`（Redis 状态存储）、`KV`（PostgreSQL 受限 KV，按 pluginID 隔离）、`DB`、`CmdSys`、`Registry`、`ToolReg`、`Logger`、`Ctx`（见 `internal/plugin/plugin.go`）。

### 3.2 注册资源三步（顺序固定）

```go
// 1. 注册 Pass（业务逻辑），依赖通过结构体注入
passID := pluginpkg.PassID("xxx", "execute")
if err := ctx.Engine.RegisterPass(passID, &xxxExecutePass{...}); err != nil { return err }
ctx.Registry.TrackPass("xxx", passID)

// 2. 注册 Pipeline（通过 Pass ID 引用，支持热替换）
pipelineID := pluginpkg.PipelineID("xxx", "main")
if err := ctx.Engine.RegisterPipeline(conduit.NewPipelineFromIDs(pipelineID, passID, replyPassID)); err != nil { return err }
ctx.Registry.TrackPipeline("xxx", pipelineID)

// 3. 注册行为树 Subtree（路由条件 + 管线）
subtree := conduit.NewSequence(
    conduit.NewCondition(isXxxCommand),
    conduit.NewAction(pipelineID),
)
if err := ctx.Engine.RegisterSubtree(pluginpkg.SubtreeID("xxx"), subtree); err != nil { return err }
```

### 3.3 硬性约定

- **Pass 内不发送消息**：回复统一写入 `conduit.AppendOutput(ctx, &conduit.Message{...})`（纯文本）或 `conduit.Set(ctx, "bot.send.segments", []map[string]any{...})`（OneBot 原生段，富文本），由引擎与 bot 回调统一发送
- **跨 Pass 传数据**用 `conduit.Set(ctx, key, value)` / `conduit.Get[T](ctx, key)`，键统一带 `plugin.<插件ID>.` 前缀（如 `plugin.signin.result`），避免与其他插件冲突
- **状态持久化**分两种：
  - Redis `Store` 用 `StoreKey(pluginID, key)` 生成隔离 key（结果为 `plugin:<pluginID>:<key>`，见 `internal/plugin/plugin.go`）
  - 业务数据优先用 `ctx.KV`（`database.PluginKVStore`），宿主按 pluginID 隔离，逻辑 key 无需再带插件前缀
- 注册的 Pass/Pipeline 必须调用 `TrackPass`/`TrackPipeline`；Subtree 由 `Info().SubtreeID` 统一清理；自行注册到 `ToolReg` 的工具用 `TrackTool`（`Info().Tools` 中声明的工具由 Registry 自动跟踪）
- 在 `internal/bizplugin/registry.go` 的 `RegisterBuiltins` 中追加注册分支，并在 `internal/config` 的 `PluginBuiltinsConfig` 增加对应开关（见 `internal/bizplugin/README.md` 第 4 节）；开关由 `[plugin.builtins]` 控制，配置为 false 或同名 Wasm 插件已加载时跳过注册
- **撤回/禁言等动作**经 `PluginContext.Actions` 执行，不要自行构造 OneBot 动作；动作是否成功以平台响应的 `Status`/`RetCode` 为准，失败原因（如机器人无群管理权限）应原样记录而非自行推断
- 插件 ID 规则：`^[a-z][a-z0-9_]{0,63}$`（小写字母开头，后接小写字母/数字/下划线，长度 ≤64，见 `internal/plugin/wasm_abi.go`）

### 3.4 可提供的资源

| 资源 | 说明 |
|---|---|
| 斜杠命令 | `Info().Commands` 声明（`CommandDef{Name, Description, Order}`），命令名不含 `/`；Registry 在 OnInit 注册、卸载时自动注销 |
| AI 工具 | `Info().Tools` 声明 `ToolDef{Name, Description, Parameters, Handler}`（Parameters 为 Eino `*schema.ParamsOneOf`），注册后 LLM 可调用 |
| 行为树 Subtree | `Info().SubtreeID` + `OnInit` 中 `RegisterSubtree`，由 Registry 挂在主行为树插件分支（优先级高于核心分支） |
| 消息观察者 | 实现 `plugin.MessageObserver`（`ObserveMessage(ctx *conduit.MessageContext)`）即可旁路观察每条消息；不参与行为树决策、不消费消息（子树一旦匹配成功会独占消息，只想旁路观察时必须用它）。回调内必须立即返回，只允许入队/计数等轻量操作，网络与数据库操作须转到插件自有协程 |
| 出站动作 | `PluginContext.Actions`（`plugin.MessageActions`）：撤回消息、禁言群成员、主动发送文本（可指定群或私聊目标）、群内「@成员 + 文本」，返回平台动作响应以判断真实成败。为 nil 表示宿主未注入（如单测），插件应降级而不 panic |

## 4. WASM 插件规范

完整 JSON ABI 见 [internal/plugin/wasm_abi.md](internal/plugin/wasm_abi.md)，WIT 概念定义见 [schema/plugin/lanmei-plugin.wit](schema/plugin/lanmei-plugin.wit)。

### 4.1 开发与构建

参考实现 `examples/plugin/signin`（独立 module，依赖 `github.com/extism/go-pdk`）：

```bash
tinygo build -o signin.wasm -target wasi main.go
```

（仓库 E2E 测试用 `tinygo build -o <路径> -target=wasi .` 构建同一示例，两种写法等价。）

构建产物需自行托管到公网 HTTPS 直链（GitHub Raw、GitHub Release 等），再由 bot_owner 在会话中执行：

```text
/插件 安装 https://.../signin.wasm
```

安装只做下载、`plugin_info` 元数据校验（在默认拒绝 Host Function 的检查实例中）与落库，安装记录 `Enabled=false` 不加载；加载/启用由管理流程执行（`WasmManager.Install` / `Load` / `Start`）。托管文件写入 `installed/<installationID>/plugin.wasm`。

### 4.2 必需 Guest Export

| 导出 | 作用 |
|---|---|
| `lanmei_plugin_info` | 声明 id/name/version/commands/tools/申请的角色，无副作用，可能被多次调用 |
| `lanmei_init` | 校验配置、初始化自身状态，返回 ok=false 则宿主放弃加载 |
| `lanmei_handle` | 处理 `command` 或 `tool_call` 事件 |

可选：`lanmei_start` / `lanmei_stop`（启动通知与资源清理）。导出名是固定的字符串常量（`internal/plugin/wasm_abi.go`），`lanmei_handle` 对同一实例串行调用、单次超时 3 秒。

### 4.3 宿主能力（Host Function）

当前宿主只提供 6 个状态存储函数（`internal/plugin/wasm_host.go`），命名空间固定为 `lanmei:host/v1`：

| Host Function | 运行时校验的 Action | 约束方式 |
|---|---|---|
| `state_get` | `state.read` | key 按安装实例隔离 |
| `state_set` | `state.write` | 同上；`ttl_ms=0` 永不过期，正数最大 30 天 |
| `state_delete` | `state.delete` | 同上 |
| `compare_and_swap` / `incr_by` / `set_if_not_exists` | `state.write` | 原子操作，归入写动作 |

注意：

- 上表 Action 名（`state.read` 等）来自 `internal/plugin/access_control.go`，是 Casbin 策略实际校验的名字；`internal/plugin/capability.go` 中的 `Permission`（`state:read`、`http:get`、`db:read` 等）是能力声明侧的另一套标识，两者不同层，不要混用（详见 `internal/plugin/access_control.md`）
- 目前没有 HTTP/DB 的 Host Function：`http_access.go`、`db_access.go` 只是宿主内部设施；WIT 中声明的 `http-get`/`http-post`/`db-query`/`db-exec` 当前不可用，Guest 导入会导致实例化失败
- 插件需要先声明并获授对应角色（`role::plugin_command_basic` 覆盖 `command.handle`、`message.reply` 与 `state.read/write/delete`），Host Function 每次调用都会在宿主侧重新鉴权

**新增宿主能力的步骤**：

1. `internal/plugin/wasm_abi.go`：加请求/响应 DTO，并在 `HostFunctionActions` 登记函数名到动作的映射
2. `internal/plugin/wasm_host.go`：用 `newLimitedHostFunction` 实现正式版，并在 `NewDenyAllHostFunctions` 加拒绝版（导入签名必须一致）
3. `internal/plugin/access_control.go`：加 `ActionXxx` 常量并同步 `allActions()`；需要内置角色默认持有时同步 `builtinRoleActions()`
4. `internal/plugin/wasm_manager.go`：在创建正式实例处注入新 Host Function（`Load` 中的 `NewStateHostFunctions` 调用点）
5. 如同时新增能力声明标识，在 `internal/plugin/capability.go` 加 `Permission` 常量与权限集

## 5. 权限与安全

- **运行时鉴权**：Casbin RBAC，主体（`user::` / `plugin::` / `system::`）绑定角色，角色持有 `Action`（`<resource>.<verb>`，如 `state.read`）；无匹配策略即拒绝（deny-by-default），见 `internal/plugin/access_control.md`
- **声明不是授权**：WASM 插件在 `lanmei_plugin_info.requested_roles` 中申请内置角色（如 `role::plugin_command_basic`），管理主体必须为具体安装实例（`plugin::<plugin_id>::<installation_id>`）显式 `BindRole`；`required: true` 的角色未授予时加载失败
- **沙箱边界**：Extism manifest 清空 allowed hosts/paths，插件无网络/文件系统直连；宿主 Host Function 是唯一外部通道；文件大小、内存、输入输出 JSON、调用超时、state key/value 等限额见 wasm_abi.md 与 `RuntimeLimits`
- **fail-closed 原则**：权限检查在宿主侧强制执行，不信任 Guest 自报的 `granted_roles` / `effective_actions`
- 输出不能指定目标：宿主始终从触发事件回填回复目标，插件不得获取宿主未授予的 API Key、数据库凭据、系统 Prompt 等敏感信息

## 6. 开发检查清单

- [ ] 插件 ID 合法且未与现有插件冲突（`pluginReg.Get/List` 可查；同名时 Wasm 插件优先，内置注册自动跳过）
- [ ] Go 插件：Pass/Pipeline 全部 Track；Subtree 通过 `Info().SubtreeID` 声明；回复用 `AppendOutput` 或出站段
- [ ] WASM 插件：声明了全部必需角色且会被显式授予；不依赖 `effective_actions` 鉴权；只导入已实现的 Host Function
- [ ] 命令名不包含 `/`、空白、控制字符，且不与已注册命令冲突
- [ ] 状态 key：Go 插件用 `StoreKey(pluginID, key)` 或 `ctx.KV`；WASM 插件只用逻辑 key，不带 `plugin_id`、安装 ID、`/`、`..` 或控制字符（宿主按 `plugin:<installation_id>:<key>` 隔离）
- [ ] 生命周期方法可重复调用安全（幂等），OnStop 能清理所有资源
- [ ] `go build ./...` 通过；WASM 插件按 4.1 构建并跑通一次真实命令路径

## 7. 现有插件

内置插件由 `internal/bizplugin/registry.go` 的 `RegisterBuiltins` 按 `[plugin.builtins]` 开关统一注册；“配置开关”列为 `PluginBuiltinsConfig` 的配置键（与插件 ID 不完全同名，如 `signin_rank` 的开关是 `rank`）。

| ID | 配置开关 | 触发方式 | 命令 / 工具 | 关键依赖 |
|---|---|---|---|---|
| `signin` | `signin` | 命令 | `/签到`、`/试试手气`；工具 `signin_status`、`signin_random` | `PluginContext.KV` |
| `signin_rank` | `rank` | 命令 | `/rank`、`/排名`；工具 `signin_rank` | 读取 signin 写入的 KV |
| `welcome` | `welcome` | 事件（`group_increase` 入群） | 无 | 对象存储（未配置时欢迎图不可用） |
| `poke` | `poke` | 事件（戳一戳） | 无 | 无 |
| `three_g` | `three_g` | 关键词（消息含 `3G`/`3g`） | 无 | 无 |
| `zhaoxin_group` | `zhaoxin_group` | 命令 | `/招新群` | 无 |
| `cat` | `cat` | 命令 | `/哈基米`、`/猫猫`；工具 `cat_image` | 无 |
| `balogo` | `balogo` | 命令 | `/balogo`；工具 `balogo_generate` | 无 |
| `ping` | `ping` | 命令 | `/ping`；工具 `ping` | 无 |
| `github_card` | `github_card` | 消息关键词（GitHub 链接） | 无 | 无 |
| `music` | `music` | 命令 | `/music`；工具 `music_search` | NCM API 地址、发送模式 |
| `sticker` | `sticker` | 命令 | `/添加表情`、`/删除表情`、`/发表情`、`/表情列表`；工具 `pick_sticker` | 对象存储、视觉服务（未配置时收藏不可用） |
| `turtle_soup` | `turtle_soup` | 命令（LLM 文字游戏） | `/开汤`、`/问`、`/猜`、`/认输` | LLM 客户端、调用超时（未配置时命令提示不可用） |
| `answer_question` | `answer_question` | 命令（群抢答） | `/答题` | 题库目录 `[quiz].dir` |
| `daily_quote` | `daily_quote` | 命令（外部一言 API） | `/每日一句` | 无 |
| `random_beauty` | `random_beauty` | 命令（外部图片 API） | `/随机美图` | `RandomBeautyConfig`、视觉服务、对象存储 |
| `antifraud` | `antifraud` | 消息观察者（每条群消息异步判定） | 无 | 反诈推理服务地址与阈值（`AntifraudConfig`）、出站动作能力、PostgreSQL（自管表 `antifraud_records`）；详见 [docs/anti-fraud.md](docs/anti-fraud.md) |

另有 WASM 参考实现 `examples/plugin/signin`（与内置 `signin` 同名，动态加载时 Wasm 优先，内置实现自动跳过）。

> 本文档随插件系统演进同步更新；ABI 变更须同时更新 `schema/plugin/lanmei-plugin.wit` 与 `internal/plugin/wasm_abi.md`。
