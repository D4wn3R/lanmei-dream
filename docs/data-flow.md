# 数据流说明

本文描述消息、对话与后台任务在各模块间的实际流转路径。架构与模块职责见 [architecture.md](architecture.md)。

## 可选联网搜索

正常群聊/私聊准入 → 原有 roleplay → 回答模型选择 `web_search` → 单消息预算与参数校验 → 会话限流/短期缓存 → OpenSERP → 清洗后的摘要以 `tool` 消息回到同一模型 → 最终答案与来源。没有新命令或独立搜索行为树。

联网路径完整读取每轮模型输出后合并 ToolCalls；有调用时不投递该轮正文，无调用才分段发送。每消息最多两次搜索，调用失败和缓存命中也计数；累计搜索默认 15 秒、单次 10 秒，并为最终生成保留原任务 10 秒。达到五轮工具交互后绑定空工具列表收尾一次，模型失败或空回复使用固定提示。自动空回答重试继承原截止时间与 TurnState，已有证据仍用 tool role 回传，不重新搜索。详细配置及真实部署验收见 [web-search.md](web-search.md)。

## 消息处理流

```
IM 平台消息
  │
  └─ NapCat(OneBot 11) / Onebots(OneBot 12) → 反向 WebSocket → gateway.Server
       │
       ├─ 鉴权（AccessToken）+ 协议/平台识别 + NormalizeV11/NormalizeV12
       └─ NormalizedMessage → bot.Bot.OnMessage
            │
            ├─ 入口过滤：空消息丢弃 → message_id 去重（Redis SETNX，5 分钟）
            │              → 封禁用户静默丢弃 → 记录会话最近消息
            ├─ 构造 InputMessage.Extra（平台/用户/昵称/消息 ID/连接 ID/SelfID/
            │   超管标记/消息段/MIME/at 目标/事件字段）+ ResponseCallback
            └─ engine.Submit(input)   ← 异步，不阻塞网关读循环
                 │
                 └─ 行为树 Tick（Selector，优先级从上到下）
                      ├─ 插件子树：plugin.*.subtree（动态注册顺序）
                      ├─ IsSegment      → pipeline.roleplay_segment（流式段落交付）
                      ├─ IsNotice       → pipeline.notice（事件仅记录，插件可先行消费）
                      ├─ IsAdminCommand → pipeline.admin（超管校验 → CommandPass）
                      ├─ IsCommand      → pipeline.command（路由 → 执行）
                      ├─ IsMedia        → pipeline.media（媒体处理 → 路由）
                      └─ 兜底           → pipeline.topic_gate
                           ├─ 群聊：topic.Manager 决策 → 回复 / 静默（topic_ignore）
                           └─ 私聊：放行 → pipeline.intent_analysis
                                ├─ intent=command → pipeline.intent_command_exec
                                ├─ intent=chat/tool → pipeline.roleplay（流式）
                                ├─ intent=ignore → pipeline.intent_ignore
                                └─ 其他/缺失 → pipeline.fallback
```

- `Submit` 的处理结果由 `ResponseCallback` 异步接收：成功 → 发送输出；`IsYielded` → 启动 `streamSegments` 消费段落；错误 → 回复兜底话术；管线超时（20s）→ 引擎执行 `pipeline.fallback`。
- 出站发送统一经 `gateway.Hub.SendSegments`：插件写入的原生段优先，否则遍历 `ctx.Output` 纯文本；纯图片 URL 回复转为 image 段（内网 RustFS 预签名 URL 先转 base64）。

## 网关协议流

```
IM 平台消息
  │
  ├─ NapCat (NTQQ) ── 反向 WebSocket ── OneBot 11 ──┐
  │                                                ├─→ gateway.Server
  └─ Onebots ──────── 反向 WebSocket ── OneBot 12 ─┘
                                                     │
   端点：/onebot（自动识别）/onebot/v12 /onebot/v11
   鉴权：Authorization: Bearer <token> 或 ?access_token=；未配置 AccessToken 则跳过
   识别：Sec-WebSocket-Protocol（"11.x"/"12.x"、wechat/telegram/napcat 关键字）
         与 ?protocol= / ?platform= 查询参数（查询参数优先；默认 OneBot 12 + QQ）
                                                     │
   入站标准化：NormalizeV12 / NormalizeV11
     ├─ message 事件：段解析（V11 支持 CQ 码与 raw_message 回退）→ 纯文本 + 段/meta
     ├─ notice 事件：白名单映射（进群/退群/好友/戳一戳/撤回/禁言/解禁）→ EventType/Data
     ├─ request 事件：好友/加群请求透传
     └─ meta 事件：更新 SelfID/实现名，不投递
                                                     │
                                              NormalizedMessage
                                                     │
                                               bot.handleMessage
```

出站（`Hub.SendSegments`）按连接协议自动选择动作与段格式：

| 差异点 | OneBot 12 | OneBot 11（NapCat 方言） |
|---|---|---|
| 发送动作 | `send_message`（detail_type=group/private） | `send_group_msg` / `send_private_msg` |
| at 段目标字段 | `user_id` | `qq` |
| 引用段字段 | `message_id` | `id` |
| 数字 ID | 字符串 | 顶层为数字，段内数值统一转字符串 |
| 连接存活 | 任意帧刷新 90s 读超时 | 同左；心跳帧仅刷新超时，不作业务处理 |

API 响应帧（含 `echo`/`status`）只做失败告警，不进入消息管线。

## 话题决策流（群聊）

```
群消息（非命令、含纯媒体消息）
  │
  └─ pipeline.topic_gate → TopicGatePass.Execute
       ├─ 私聊 / topic 未启用 → Route 到 pipeline.intent_analysis
       └─ 群聊：
            ├─ 构造 IncomingMsg（平台/群/用户/昵称/at 目标/时间）
            ├─ 提示词注入检测（命中记 Warn 日志）
            ├─ 一次 LLM 调用：意图分类 + 提及判断（注入机器人名称与最近 6 条对话）
            │    └─ 失败降级：IntentChat/0.5 且按未提及处理
            ├─ topic.Manager.HandleGroupMessage(ctx, msg, judge)
            │    ├─ at 命中 SelfID → 强提及 → 创建/重入话题 → 回复
            │    ├─ 强提及（强证据角色 + 证据达弱阈值，或置信度达强阈值）→ 回复
            │    ├─ 弱提及非成员 → 静默拉入话题 + 授回复配额（本次不回复）
            │    ├─ 弱提及成员 / 成员续聊 → 按回复配额决定回复或静默
            │    ├─ 成员但语义不相关 → 脱离话题（成员清空则转冷却）
            │    └─ 窗口内无触碰的话题 → 转冷却（后台扫描超时后归档）
            └─ 命中话题时写黑板：TopicID/Label/TopicContext/MentionMode
                 └─ Route：回复 → roleplay / intent_command_exec
                            不回复 → pipeline.topic_ignore（保存消息，不回复）
```

`RoleplayStreamPass` 在流式回复完成后调用 `TopicManager.RecordBotReply`：追加 Bot 发言、刷新活跃时间、消耗并重授回复配额；冷却话题由 `topic.Archiver` 归档为群级记忆与群画像事实。

## 意图分析流

```
自然语言消息 → pipeline.intent_analysis（私聊）或 TopicGatePass（群聊）
  │
  ├─ intent.Analyzer.Analyze(ctx, msg, judgeCtx)
  │    ├─ LLM 未配置 → {intent:"chat", confidence:1.0}
  │    ├─ 构造 system prompt：可用命令列表 + 可用工具列表 +（群聊）提及判断规则
  │    ├─ 调用 LLM（独立超时 intent_timeout_seconds，默认 8s；禁用思考）
  │    │    └─ 调用失败 → {intent:"chat", confidence:0.5}
  │    └─ parseResult：容错提取 JSON；解析失败/非法意图 → chat；置信度 clamp [0,1]
  │
  ├─ 群聊附带提及判断字段：is_talking_to_bot / mention_role /
  │   mention_confidence / mention_evidence（证据必填，供强证据角色分档）
  │
  └─ 路由（4 种意图）
       ├─ intent="command" → pipeline.intent_command_exec
       │     └─ 按命令名查找并复用 ExecuteCommandPass 执行（参数透传 LLM 提取结果；
       │        命令不存在时回复「不认识命令」）
       ├─ intent="tool"    → pipeline.roleplay（与 chat 同路，由工具循环执行工具）
       ├─ intent="chat"    → pipeline.roleplay
       └─ intent="ignore"  → pipeline.intent_ignore / topic_ignore（保存消息，不回复）
```

## AI 对话与工具调用流

```
RoleplayStreamPass → ChatService.ChatStream（独立 60s 上下文）
  │
  ├─ 1. assembleContext：System Prompt + 表情规则 + 防注入规则
  │      + LOD（L2→L1→L0，3000 token 预算）+ RAG 多路召回 top5
  │      + 长期事实画像（≤10 条）+ 知识库隐式召回（默认 3 条）+ 当前消息
  │
  ├─ 2. 绑定工具（Eino ChatWithTools）→ 失败则退回基础模型（本轮不注入工具）
  │
  ├─ 3. 流式工具循环（最多 5 轮）
  │    ├─ 文本轮：chunk 经 StreamSegmenter 按空行/代码块边界分段 → 写入段落通道
  │    ├─ 工具轮：首个 chunk 带 ToolCalls → 执行工具 → 追加 assistant/tool 消息 → 下一轮
  │    │    ├─ 内置插件工具 → tool.Registry.Call → Handler（注入 CallerIdentity）
  │    │    └─ WASM 插件工具 → WasmPlugin.handleToolCall
  │    │         → HandleRequest{event_type:"tool_call"} → lanmei_handle → 输出文本
  │    └─ 工具失败：错误文本作为工具结果回传，由 LLM 决策
  │
  ├─ 4. 循环结束取最后一条 assistant 文本（全空时取最后一条消息兜底）
  │
  ├─ 5. 异步触发（ChatStream 返回前）：写向量记忆（memory_vectors）
  │      + Compressor.MaybeCompress（不阻塞生成，与下一步并行）
  │
  └─ 6. RoleplayStreamPass 保存 L0：user 原文 + assistant 回复
        （调用过工具时 assistant 标记 plugin 来源并记录首个工具名）
```

非流式路径 `ChatService.Chat` 共用同一 `assembleContext` 与工具循环（上限 5 轮），供内部调用。

## 流式段落投递流

```
RoleplayStreamPass.Execute
  │
  ├─ 创建段落通道 chan string（缓冲 32）并写入 ctx.data
  ├─ 独立 goroutine 运行 ChatStream，流式增量写入通道
  └─ 返回 ErrPassYielded → 引擎调用 ResponseCallback
       │
       └─ Bot.streamSegments(ctx, msg)
            ├─ 读取段落通道（缺失则回复兜底话术）
            └─ for 每个段落：
                 ├─ 非首段：按「距上次实际发送 + 字数×打字速度 ±抖动」等待
                 │    （typing_speed_ms=0 时禁用；间隔 clamp 到 min/max）
                 ├─ child = ctx.NewChildInput(segment) + Extra[IsSegment]=true
                 ├─ child.ResponseCallback：发送该段（仅首段参与引用/at 判定）
                 ├─ engine.Submit(child) → 行为树 IsSegment → pipeline.roleplay_segment
                 └─ <-done：上一段发送完成后才提交下一段（保证顺序）
```

流结束（`close(segCh)`）后由 `runStream` 保存对话、记录话题 Bot 回复与表情情绪窗口；空响应重试一次（关闭思考），仍为空时发送提示段。

## 命令管线流

```
消息（/命令 开头）→ pipeline.command
  │
  ├─ CommandRouterPass.Execute
  │    ├─ 解析 /命令名 参数（strings.Fields）
  │    ├─ command.System.Lookup(命令名)
  │    │    └─ 未注册：写入「未知命令」提示（不中断管线，后续 Pass 静默跳过）
  │    ├─ 将命令名/参数/handler 写入 ctx.data（bot.command.name/args/handler）
  │    └─ 空命令：返回错误中断管线
  │
  └─ ExecuteCommandPass.Execute
       ├─ 从 ctx.data 读取命令信息（handler 为空则静默返回）
       ├─ 构造 command.Context（平台/用户/群/昵称/消息 ID/连接 ID/SelfID/
       │   at 目标/超管标记/重入标记）
       ├─ 执行 Handler：Reply 文本 → ctx.Output；
       │   ReplySegments → 出站段键（段优先于文本）；
       │   SuppressRequesterAt → 抑制自动 at
       └─ handler 错误原样返回，由引擎中断并走错误回调（非 fallback 管线）
```

管理管线 `pipeline.admin` 不同：`/admin` 前缀由行为树直接进入，先经 `AdminGuardPass` 校验超管（非超管写拒绝回复并中断），再经 `CommandPass` 调用 `command.System.Process` 执行；`/admin` 与 `/添加管理员` 命令在 `bot.New` 中注册。

插件命令（含斜杠命令与自然语言意图命中）由 `plugin.Registry.makeCommandHandler` 重入引擎：构造带 `bot.command.reentry` 标记的子消息提交 `engine.Process`，由插件子树消费；子上下文写出的原生段与抑制 at 标记回传给原命令上下文。

## 插件系统流

### 注册与生命周期

```
plugin.Registry
  │
  ├─ 内置插件（bizplugin.BusinessRegistry.RegisterBuiltins）
  │    ├─ 按 [plugin.builtins] 开关逐项注册；同名已由 WASM 加载则跳过
  │    └─ Registry.Register → InitPlugins（OnInit：注册 Pass/Pipeline/子树上命令/工具）
  │                         → StartPlugins（OnStart：启动后台任务）
  │                         → StopPlugins（逆序 OnStop）
  │
  ├─ WASM 插件（WasmManager）
  │    ├─ Install(url)        下载校验 + 全拒绝 Host Function 检查元数据 → Enabled=false
  │    ├─ Load(id)            校验托管文件 → 生产实例（state_* Host Function + 角色校验）
  │    │                      → lanmei_init → Registry.Register
  │    ├─ Start(id)           Registry.InitPlugin/StartPlugin → lanmei_start → Enabled=true
  │    ├─ Unload(id)          Enabled=false → Registry.Unregister → 关闭 Extism 实例
  │    ├─ Delete(id)          删除安装记录与托管文件（需先卸载）
  │    └─ LoadEnabled()       启动时恢复 Enabled 安装，失败回滚并记录 LoadError
  │
  └─ 资源清理：注册的 Subtree/Pipeline/Pass/命令/工具在卸载时由 Registry 自动注销，
     并触发 Bot 重建行为树
```

### 插件消息路径

- 插件子树以 `SubtreeRef` 挂到主行为树最前，按注册顺序匹配（插件自己的 Condition 判定消息是否属于本插件）。
- WASM 插件子树只匹配 `/<命令名>` 前缀，命中后由共享 `WasmCommandPass` 调用 Guest；调用前检查 `command.handle`，消费非空输出前检查 `message.reply`。
- 插件工具经 `tool.Registry` 暴露给 LLM；WASM 工具由 `WasmPlugin.handleToolCall` 构造 tool_call 事件调用 Guest。

### 插件设施流

```
插件调用设施
  │
  ├─ StateStore（conduit.StateStore，Redis；内置插件经 PluginContext.Store，WASM 经 Host Function）
  │    ├─ state_get / state_set / state_delete（基础操作，TTL 由调用方指定）
  │    ├─ compare_and_swap（Lua 脚本原子）
  │    ├─ incr_by（INCRBY）/ set_if_not_exists（SETNX）
  │    └─ WASM 场景：Guest key 为逻辑 key，宿主用 installationID 前缀隔离物理 key
  │
  ├─ 受限 KV（PluginContext.KV → PostgreSQL plugin_kv，按插件 ID 隔离命名空间）
  │    └─ 内置插件私有业务数据（如签到积分/排行榜）持久化，重启不丢
  │
  └─ 数据库/HTTP 访问设施（DBAccess / HTTPAccess，IndexedDB 隔离模型 + allow_hosts 白名单）
       └─ 代码与 WIT 已定义，尚未接入 WASM 生产实例（规划中）
```

## 安全检查流

```
WASM 命令处理 / 工具调用 / state_* Host Function
  │
  ├─ Layer 1: 主体与动作授权（Casbin，plugin_casbin_rule）
  │    ├─ 主体由宿主构造：plugin::<pluginID>::<installationID>（不信任 Guest 输入）
  │    ├─ Require(principal, action)：精确匹配角色-动作策略，无策略默认拒绝
  │    └─ 动作：command.handle / message.reply / state.read/write/delete 及管理动作
  │
  ├─ Layer 2: 加载期角色校验
  │    └─ lanmei_plugin_info.requested_roles 中 required=true 的角色未授予 → 拒绝加载
  │
  ├─ Layer 3: 运行时限额（RuntimeLimits，Extism manifest 与输入输出校验）
  │    ├─ 单次调用 3s / 内存 256 页 / Guest 输入 256 KiB / 输出 64 KiB
  │    └─ 输出 ≤8 条、单条 ≤4096 字节；state key ≤256 字节、value ≤64 KiB、TTL ≤30 天
  │
  ├─ Layer 4: 隔离
  │    ├─ state key 按 installationID 前缀隔离（跨安装实例互不可见）
  │    └─ 输出目标不可指定：宿主始终回复原事件目标
  │
  └─ Layer 5: 审计
       └─ zap audit 命名空间记录 principal/permission/decision/reason；拒绝另记 Warn
```

内置插件的管理命令（如 `/admin`、表情库删除命令）在命令层用 `IsSuperUser` 标记校验；`AdminGuardPass` 在管线层拦截非超管访问管理员命令。

## 基础设施流

```
main.go
  │
  ├─ config.Init（toml + 环境变量）
  ├─ infra.InitLogger（zap + lumberjack 轮转）
  └─ infra.Setup(ctx, cfg)
       ├─ PostgreSQL 连接 → Migrate：扩展（vector/pg_trgm）→ AutoMigrate 全部表
       │    → HNSW / GIN / tsvector 触发器索引 → 向量维度自适应
       ├─ PGVectorStore → inf.MemStore（memory.MemoryStore：向量/关键词/时间检索）
       ├─ Redis 连接 → RedisStore（conduit.StateStore：TTL/CAS/IncrBy/SetNX）
       │    └─ 用户缓存注入 database（GetOrCreateUser 免查库）
       └─ RustFS 对象存储（未配置时媒体缓存降级为仅描述/跳过）
```

进程退出时逆序关闭：插件停止 → 知识库 Close → 网关 Shutdown → 管理面板 Stop → 基础设施 Close。

## 记忆与压缩流

```
用户发消息 → 话题门控/意图分析 → RoleplayStreamPass → ChatService
                                                    │
                                                    ├─ 1. LOD 组装（3000 token 预算）
                                                    │     L2 主题 brief → L1 摘要 brief/detailed
                                                    │     → L0 原文（剩余预算，2~40 条）
                                                    │
                                                    ├─ 2. RAG 多路召回（向量 + 关键词 + 时间，top5）
                                                    │     + 事实画像（≤10 条）+ 知识库隐式召回（默认 3 条）
                                                    │
                                                    ├─ 3. 流式生成 + 工具调用循环（max 5 轮）
                                                    │
                                                    ├─ 4. 异步：写向量记忆（memory_vectors）
                                                    │      └─ Compressor.MaybeCompress(userID)
                                                    │
                                                    └─ 5. 保存 L0（conversations：user + assistant）
                                                                ├─ L0 ≥ 40 → 取最老 20 条 → LLM 压缩
                                                                │    → episode_summaries → 删原文
                                                                └─ L1 ≥ 10 → 取最老 5 条 → LLM 聚合
                                                                     → topic_clusters + 向量记忆 → 删旧摘要
```

- 压缩仅针对**私聊维度**（`group_id=''`）；群聊 L0 由话题归档沉淀为群级记忆（`memories` + `memory_vectors`，`user_id=0`）与 `group_facts` 群画像。
- 每次压缩按用户加锁串行；LLM 单次 60s 超时；先写摘要再删原文。
- 事实以带置信度的结构化条目保存，跨条目三态合并（重复确认提升置信度）；注入时低于门槛过滤并标注低置信/较早/矛盾。
- 后台 `MemoryMaintainer` 每 6 小时清理：每个 `(user, group)` 保留最近 200 条原文、每用户保留最近 50 个 L2 主题、删除 180 天未更新的向量记忆。

## 管线一览

| 管线 ID | Pass 链 | 触发条件 |
|---|---|---|
| `pipeline.topic_gate` | TopicGatePass（RouterPass） | 非命令、非媒体消息兜底（群聊决策/私聊放行） |
| `pipeline.intent_analysis` | IntentAnalysisPass（RouterPass） | 私聊自然语言（含私聊媒体放行） |
| `pipeline.roleplay` | RoleplayStreamPass | intent=chat/tool 或话题命中回复；流式并挂起 |
| `pipeline.roleplay_segment` | RoleplaySegmentPass | 流式段落子消息（IsSegment） |
| `pipeline.intent_command_exec` | IntentCommandExecPass（复用 ExecuteCommandPass） | intent=command |
| `pipeline.intent_ignore` | IntentIgnorePass | 私聊 intent=ignore（保存不回复） |
| `pipeline.topic_ignore` | TopicIgnorePass | 群聊决策不回复（保存不回复） |
| `pipeline.command` | CommandRouterPass → ExecuteCommandPass | `/` 开头 |
| `pipeline.admin` | AdminGuardPass → CommandPass | `/admin` 开头（超管校验） |
| `pipeline.media` | MediaPass → MediaRouterPass | 含 image/audio/video/file/record 段 |
| `pipeline.notice` | NoticeGatePass | notice/request 事件 |
| `pipeline.fallback` | FallbackPass | 仅管线超时（引擎降级管线） |
| `plugin.<id>.pipeline.<name>` | 插件注册的 Pass 链 | 由插件子树或插件命令重入触发 |

## 意图路由一览

| 意图类型 | 路由目标 | 说明 |
|---|---|---|
| intent=command | `pipeline.intent_command_exec` | 自然语言触发已注册斜杠命令，参数由 LLM 提取 |
| intent=tool | `pipeline.roleplay` | 与 chat 同路，由流式工具循环执行工具 |
| intent=chat | `pipeline.roleplay` | 普通角色扮演对话（LOD + RAG + 知识库） |
| intent=ignore | `pipeline.intent_ignore` / `pipeline.topic_ignore` | 保存消息、不生成回复 |

群聊场景由 `TopicGatePass` 先做“是否回复”决策：决策回复时按上表路由；决策不回复时进入 `pipeline.topic_ignore`，不调用意图路由。意图分析失败/缺失时，私聊走 `pipeline.fallback`，群聊按不回复处理。
