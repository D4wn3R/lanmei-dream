# 意图判断 Jev 适配器（feat/intent-jev-adapter）

日期：2026-10-11 ｜ 状态：implemented ｜ 关联代码：internal/ai/intent、internal/config/intent_judge.go

## 决策

群聊「意图+提及判断」原本每次都调生成式 LLM（慢、识别不稳）。新增 **Jev（TypeSafe System-1 结构化决策模型）适配器**与 LLM 路径并存，经 `JudgeAdapter` 单方法接口二选一：

```go
Judge(ctx, userMsg, judgeCtx, commands, tools) (*Result, error)
```

- **选型规则**：`[ai.intent_judge].api_key` 非空即 Jev，留空走 LLM——沿用项目「key 非空即启用」惯例，无显式 adapter 键。
- **协议事实（重要，防再踩）**：OpenRouter 上的 Jev **不走 OpenAI chat 格式**，走专用决策端点 `POST https://openrouter.ai/api/alpha/decisions`（System One JSON：`{model, state, questions}` → `{answers, usage}`），鉴权用普通 OpenRouter API Key，端到端 70~500ms。
- **映射**：一次请求并行问 intent（choice）/command（choice，动态命令名）/tool（choice）/is_talking_to_bot（noul）/mention_role（choice，仅群聊）。noul 直接透传 `MentionConfidence`，话题系统 strong/weak 阈值消费不变。
- **非生成式的固有限制**：`MentionEvidence` 恒为空（仅日志用）；命令自由参数由 LLM 补抽（`Intent==command` 时一次小请求，失败静默空参）。
- **降级**：HTTP/协议错误返回 error，由 TopicGatePass/IntentAnalysisPass 现有逻辑降级 `chat/0.5`；未知意图值就地降 `IntentChat`（同 parseResult）。429/529 单次退避重试。

## 被弃方案（先说最强理由）

1. **纯 LLM prompt 调优**——零新依赖、零新代码路径，风险最低。不用：延迟下限由自回归生成决定（秒级），且结构化 JSON 输出的幻觉/坏值问题已被线上验证（parseResultLoose 就是为此存在的），调 prompt 无法根治。
2. **显式 `adapter = "llm"|"jev"` 配置键**——意图明确、未来可扩展第三实现。不用：与项目「key 非空即启用」惯例（llm/embedding）不一致，多一键多一份心智负担；项目已有两个实现先例均无开关键。
3. **Jev 失败/低置信度回退 LLM 级联**——可用性最高。不用：失败场景延迟翻倍，恰与本次提速目标相悖；现有降级已保证不静默（群聊仅 @ 可回复的兜底仍在）。置信度门控（官方 confidence-routing 模式）留作后续观察 Jev 实际表现后的可选演进。

## 影响

- `intent.NewAnalyzer` 签名变更：首参 `llmClient` → `JudgeAdapter`（唯一调用点 bot.go）；`bot.New` 末尾新增 `judge intent.JudgeAdapter` 参数（nil 回退 `NewLLMAdapter(llmClient)`）。
- Analyzer 不再持有 llmClient，只做公共逻辑（快照/超时/委托）；LLM 原有逻辑原样迁入 `LLMAdapter`。
- 部署启用方式：`.env` 配 `LANMEI_AI_INTENT_JUDGE_API_KEY`（env 优先于 TOML），或 config.toml 直接填；启动日志 `意图判断适配器 adapter=jev|llm`。
