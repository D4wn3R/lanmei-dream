# OpenSERP 联网搜索操作指南

蓝妹通过 Agent 原生 `web_search` 工具查询公开互联网。模型判断是否需要搜索，读取标题、摘要与来源后组织答案；用户无需执行 `/搜索`，也无需安装业务插件。功能默认关闭，修改配置后须重启蓝妹。

## 1. 能力与限制

- 使用现有对话模型和工具循环，不增加搜索判断模型或摘要模型，不增加数据库表、管理面板页面、付费搜索后端。
- 每条用户消息默认最多两次搜索：首次检索加一次补查；失败、参数错误、缓存命中和本轮重复查询都消耗名额。
- 每次最多五条摘要，不读取网页正文。获取时间 `fetched_at` 不是文章发布时间；缓存命中保留原获取时间。
- 启用联网后，流式对话的每轮正文先缓冲，确认没有工具调用才按原分段规则投递。即使最终没有搜索，也会增加首段等待时间。关闭联网时仍走原有实时流式路径。
- 搜索失败由模型说明；模型空回答、生成失败或工具循环耗尽时返回空内容，由宿主层的空响应重试与既有降级话术兜底，不把搜索 JSON 或内部固定文案当作用户回复。五轮工具交互后最多一次空工具收尾。
- 来源 ID 在单条消息内连续分配；回答中的 HTTP(S) 链接与本轮来源表核对，未知链接会被省略，其他工具实际返回的链接保留。这只是链接出处检查，不证明摘要支持每个结论，也不覆盖所有特殊格式/编码的链接。
- 模型自主调用不是强制联网保证。群聊仍须通过原有提及/话题准入；测试时请明确 @蓝妹，并要求“联网查证并附来源”。

## 2. 版本与部署前提

适配器标识：`openserp-2.1`。固定源码基线：

```text
112593b8e17c084f3e9943b8fd9861c6570feca0
```

协议根据上游固定版本的 [响应类型](https://github.com/karust/openserp/blob/112593b8e17c084f3e9943b8fd9861c6570feca0/core/response.go)、[配置](https://github.com/karust/openserp/blob/112593b8e17c084f3e9943b8fd9861c6570feca0/config.yaml) 和 [Dockerfile](https://github.com/karust/openserp/blob/112593b8e17c084f3e9943b8fd9861c6570feca0/Dockerfile) 核对。只接受 `meta.version="2.1"`、明确的 `results` 数组与引擎状态，不兼容旧数组响应，不猜测其他结构。

需要：

- 已能正常运行的蓝妹与支持 tool calling 的对话模型；仅配置 OpenSERP 而没有 LLM 不会注册搜索。
- Docker Engine / Docker Compose v2，Linux 容器模式；构建时能够访问 GitHub、基础镜像仓库与 Go 模块源。
- 目标服务器能访问所选搜索引擎。中文可先测 `bing,baidu`，实际可用性取决于网络、验证码和引擎策略。
- 本示例为 OpenSERP 限制 2 CPU、2 GiB 内存、256 MiB shared memory；这是起始资源限制，不是性能承诺，须观察 OOM 和延迟后调整。

仓库提供固定提交的源码构建，不虚构 Docker Hub 上存在对应提交标签。默认本地镜像名为 `lanmei-openserp:112593b8e17c084f3e9943b8fd9861c6570feca0`。如果改用预构建镜像，请将 `OPENSERP_IMAGE` 设置为自己验证过的固定版本或 `仓库@sha256:实际摘要`，并确认内容对应上述契约；不要使用 `latest`。

## 3. 与蓝妹一起运行在 Docker Compose

以下命令均在项目根目录执行。保留现有 `.env` 和业务配置，不要用示例覆盖已有凭据。

### 3.1 检查配置并构建

```sh
docker compose config --quiet
docker compose --profile websearch config --quiet
docker compose --profile websearch build openserp
docker compose --profile websearch up -d --no-build openserp
docker compose --profile websearch ps openserp
```

`openserp` 没有宿主机 `ports`，与蓝妹共用现有 Compose 默认网络。蓝妹没有对它的强制 `depends_on`，搜索服务宕机不阻止普通聊天启动。默认 `docker compose up -d` 不会启用该 profile。

挂载的上游配置是 `config/openserp.yaml`，容器内位置 `/usr/src/app/config.yaml`。已设置 TLS 校验开启、正文提取关闭、请求级代理 URL 覆盖关闭、验证码付费解算关闭、浏览器并发 2、单引擎尝试 4 秒、聚合等待 8 秒、自动重试 0。禁用上游结果缓存，由蓝妹按会话隔离缓存。

镜像内置健康检查在此示例中关闭；不要用高频真实搜索作为容器探活。容器为 `running` 只代表进程启动，不代表引擎搜索成功。

### 3.2 用公开关键词做一次低频探测

镜像包含 `wget`，可以在服务容器内部探测，不必开放端口：

```sh
docker compose --profile websearch exec -T openserp wget -qO- 'http://127.0.0.1:7000/mega/search?text=Go%20programming%20language&engines=bing,baidu&limit=5&mode=any&extract=0&format=json'
```

检查返回的 `meta.version`、`meta.engines_responded`、`meta.engines_failed`、`meta.engine_errors` 和 `results`。至少应有一个已响应引擎，结果应包含可访问的公开来源。`any` 是按顺序尝试直到引擎响应，并不保证“遇到空结果会自动换引擎”。若第一页只有验证码、协议版本不符或所有引擎失败，不要直接开启生产能力。

### 3.3 启用蓝妹的 Agent 工具

编辑现有 `.env`：

```dotenv
LANMEI_AI_WEB_SEARCH_ENABLED=true
LANMEI_AI_WEB_SEARCH_BASE_URL=http://openserp:7000
LANMEI_AI_WEB_SEARCH_ENGINES=bing,baidu
```

也可以在 `config/config.toml` 的 `[ai.web_search]` 设置；环境变量优先。注意 `.env.example` 里的 `false` 若已复制到 `.env`，会覆盖 TOML 中的 `true`。

重新构建并重建蓝妹容器，使代码、配置和环境变量生效：

```sh
docker compose --profile websearch up -d --build lanmei
docker compose logs --tail=100 lanmei
```

启动日志应出现 `Agent 联网搜索工具已注册` 和 `adapter=openserp-2.1`。仅注册不发起联网请求，因此上游暂时不可达时仍能启动；真正调用时返回 `unavailable` 或 `timeout`。

通过 IM 私聊或 @蓝妹询问：“请联网查证 Go 官方目前的发布信息，并附实际来源链接。”确认先出现 `websearch: search` 日志，再收到整理后的答案。不要只用普通闲聊判断联网是否可用。

## 4. 蓝妹在宿主机运行

容器网络名 `openserp` 不能直接供宿主机程序使用。可使用固定镜像启动一个只监听 loopback 的独立容器。

先完成上面的镜像构建，然后在项目根目录运行（POSIX shell）：

```sh
docker run -d --name lanmei-openserp-local --init --restart unless-stopped \
  --cpus=2 --memory=2g --shm-size=256m --no-healthcheck \
  -p 127.0.0.1:7000:7000 \
  --mount "type=bind,source=$(pwd)/config/openserp.yaml,target=/usr/src/app/config.yaml,readonly" \
  lanmei-openserp:112593b8e17c084f3e9943b8fd9861c6570feca0 serve -a 0.0.0.0 -p 7000

export LANMEI_AI_WEB_SEARCH_ENABLED=true
export LANMEI_AI_WEB_SEARCH_BASE_URL=http://127.0.0.1:7000
go run ./cmd/lanmei
```

PowerShell 对应命令（先保证已有其他基础设施/模型环境变量）：

```powershell
$openserpConfig = (Resolve-Path .\config\openserp.yaml).Path
docker run -d --name lanmei-openserp-local --init --restart unless-stopped --cpus=2 --memory=2g --shm-size=256m --no-healthcheck -p 127.0.0.1:7000:7000 --mount "type=bind,source=$openserpConfig,target=/usr/src/app/config.yaml,readonly" lanmei-openserp:112593b8e17c084f3e9943b8fd9861c6570feca0 serve -a 0.0.0.0 -p 7000
$env:LANMEI_AI_WEB_SEARCH_ENABLED = 'true'
$env:LANMEI_AI_WEB_SEARCH_BASE_URL = 'http://127.0.0.1:7000'
go run ./cmd/lanmei
```

不要同时启动同名独立容器；已存在时先检查并使用 `docker start lanmei-openserp-local`。宿主机 `go run` 不自动加载 `.env`，需要显式导出环境变量或修改 TOML。绝不要把绑定改为 `0.0.0.0:7000:7000` 对公网开放未认证 API。

## 5. 配置参考

全部配置属于 `[ai.web_search]`，对应环境变量为 `LANMEI_AI_WEB_SEARCH_` 加下表键的大写形式。无需在 TOML 写过该键，环境变量也会生效。`engines` 的环境变量使用逗号分隔、不带空格；TOML 使用字符串数组。

| 键 | 默认值 | 说明 / 校验范围 |
|---|---|---|
| `enabled` | `false` | 是否构造服务并注册工具；重启生效 |
| `base_url` | `http://openserp:7000` | HTTP(S) 服务根地址；不得含凭据、路径、查询、fragment |
| `engines` | `["bing","baidu"]` | 1–6 个不重复引擎：bing/baidu/google/yandex/duckduckgo/ecosia，顺序有意义 |
| `mode` | `any` | any/fast/balanced；切换后须重新验收延迟与结果质量 |
| `max_results` | `5` | 1–5；模型省略 limit 按 5 解析，再受管理员上限约束 |
| `timeout_seconds` | `10` | 单次请求预算，1–60 秒，包括连接/读取；客户端不自动重试 |
| `max_calls_per_turn` | `2` | 1–2，按 ToolCall 计，不按模型轮数计 |
| `total_search_seconds` | `15` | 累计实际搜索等待，1–60 秒；模型生成时间不计入 |
| `answer_reserve_seconds` | `10` | 为回答保留的原任务时间，1–60 秒 |
| `max_concurrency` | `2` | 进程内同时请求数，1–64；满时立即拒绝，无等待队列 |
| `requests_per_minute_per_scope` | `6` | 每会话令牌桶补充速率，1–600 |
| `burst_per_scope` | `2` | 会话突发容量，1–100 |
| `cache_ttl_seconds` | `120` | 成功缓存寿命，1–3600 秒，不提供 0=无限/禁用语义 |
| `cache_max_entries` | `256` | 1–4096；过期清除、容量满时淘汰最早插入项 |
| `max_response_bytes` | `1048576` | 上游响应读取上限，1–1048576 字节 |
| `max_output_bytes` | `16384` | 工具 JSON 上限，1024–16384 字节，删条目后重编码 |

非法配置在启用时使初始化失败，不会静默变成无限制。调整蓝妹预算时也应核对 `config/openserp.yaml` 的单引擎耗时、重试与聚合等待；增加浏览器并发还需相应调整 CPU/内存限制。

群聊 scope 为 `platform:group:groupID`，私聊为 `platform:dm:platformUserID`，由宿主生成，不能通过模型参数指定。缺少可信身份时不跨请求缓存，仍有进程并发限制和单轮预算。

缓存键包含 scope、去首尾空白后的原查询、引擎顺序、模式、有效 limit 和适配版本；不会跨群共享。只有 `ok` 缓存，partial/空结果/失败不跨请求缓存。跨请求缓存命中仍计会话限流；同一消息的重复查询复用本轮结果、不再请求上游，但消耗工具名额。本轮失败结果也会记住，避免自动重复轰炸同一查询。

## 6. 验收与回归

### 本地自动化检查（不访问真实搜索、不消耗模型额度）

```sh
go test ./internal/websearch ./internal/ai/... ./internal/config ./internal/bot ./internal/plugin
go test -race ./internal/websearch ./internal/ai/tool ./internal/ai ./internal/plugin
go vet ./...
go build ./...
go test ./...
```

覆盖参数、敏感关键词、HTTP 协议/重定向/响应大小、取消、并发、缓存作用域与容量、单轮预算、多个 ToolCall 的 ID 对应、延迟工具分片、原始结果不外发、空工具收尾、用量累计、降级提示、重试证据和插件冲突回滚。race 需要受支持的 64 位架构与 C 编译器。

`internal/websearch/testdata` 是从固定版本响应类型构造的契约样本，**不是真实网络抓包**。本次开发环境没有运行中的 Docker daemon，未完成镜像构建/启动、真实引擎成功率和取消后浏览器释放的验收。不能把本地 mock 测试通过视为已达到生产可用率。

本次 Windows 开发验证中，新增与相关包测试、64 位 race、全项目 vet/build 和两种 Compose 配置检查通过。`go test ./...` 未全绿：原有 `internal/bizplugin/random_beauty` 的五个测试在清理临时 SQLite 文件时因连接仍占用而失败；该模块未作修改，仍需在目标环境/CI 复核。

### 目标服务器验收

1. 保存实际构建提交、镜像 ID/仓库 digest、两个配置文件及启用时间；查询使用公开词，不上传私聊内容。
2. 分散执行至少 30 个中文问题，涵盖时效信息、技术查证、常识、无关或确实无结果问题。避免短时间刷满令牌桶后误判可用率。
3. 分别采集成功、空结果、部分失败和错误响应，与固定协议样本核对。只在“明确成功响应且 results 为空”时认定无结果。
4. 检查答案引用是否确实来自工具、是否支持结论、是否把摘要当全文；记录明确要求搜索却没调用的漏触发情况。
5. 停止 OpenSERP 后继续普通聊天；需要联网的问题应说明不可用，不能输出 JSON 或捏造已验证结论。
6. 在测试环境取消搜索或停止蓝妹，检查在途请求及时退出；观察 OpenSERP 浏览器进程/内存是否及时回落。客户端取消不等于后端一定释放，需实测。
7. 用 `docker stats --no-stream` 记录峰值资源，按 `websearch: search` 日志计算有效响应率与 P50/P95。可先以有效响应率 ≥90%、P95 不超过配置预算为上线门槛，但这是验收目标而非产品承诺。

可记录：问题类型、是否触发、status、results 数、cached、耗时、来源人工评价；无需保存原始群消息或密钥。原始响应若需保留作 fixture，应脱敏后再纳入版本控制。

## 7. 故障排查

| 现象/状态 | 含义与处理 |
|---|---|
| 没有工具注册日志 | 检查 enabled 的环境覆盖、LLM 是否配置；容器改 `.env` 后须重建而非仅 restart |
| `invalid_arguments` | 参数未知、query 空/超长、limit 非整数/越界或命中敏感格式；只允许 query、limit |
| `unavailable` | 非 200、网络故障、全部引擎失败、服务关闭等；检查容器与内网地址，不会把内部地址交给模型 |
| `timeout` | 请求或剩余搜索预算到期；先检查网络/验证码和服务器工作耗时，不要直接无限增大超时 |
| `rate_limited` | 上游 429、进程并发满或会话限流；等待令牌补充，检查重复调用与并发负载 |
| `budget_exceeded` | 每消息名额/耗时/回答预留不足，或自动重试禁止新搜索；正常防护，不应让模型循环绕过 |
| `invalid_response` | HTML 验证码、版本/结构变化、超大响应、全部链接被过滤等；不是“没有相关资料” |
| `partial` | 有可用结果但部分引擎失败，或输出裁剪；答案须说明局限，结果不进入跨请求缓存 |
| `no_results` | 合法成功响应明确给出空结果；可在剩余名额内改关键词，不应断言事实不存在 |
| 停留很久才出第一段 | 联网路径的决策缓冲行为；检查实际 LLM 与搜索耗时，未必是连接故障 |
| 搜到了却只收到降级提示 | 模型未生成最终答案/协议异常/循环耗尽，宿主空响应重试仍无正文后发送既定话术；查看模型健康和用量，不要改成输出最后一条 tool JSON |
| 容器退出或 OOM | 查看容器状态与资源限制；浏览器启动和搜索峰值需目标机器实测 |

日志：蓝妹搜索服务仅记录进程内请求序号、scope 哈希、引擎、耗时、状态、结果数、缓存命中及预算拒绝事件，不记录搜索原词、摘要和结果正文；请求序号重启后重新开始。现有聊天/网关日志和模型服务可能仍保存用户消息，上游错误日志也应检查与限制留存；不能据此宣称整套系统不留私人数据。

## 8. 隐私、安全与回滚

自建 OpenSERP 仍会把搜索词发送到外部搜索引擎。工具会拦截常见密钥、Bearer/JWT、账号密码字段、邮箱和手机号等模式，但无法识别所有私人信息，不能替代运营侧隐私规则。

结果移除 HTML 标签/控制字符、按 Unicode 裁剪、过滤凭据 URL 与明显本地/内网目标，去掉 fragment 去重，不跟随链接、不抓正文。不能将文本清洗视为完整的提示注入防护；模型仍须把资料当数据而非指令。没有对结果域名做 DNS/重定向探测，因此不应将这个过滤器复用为任意网页抓取的 SSRF 防线。

原始资料只保存在单轮上下文及内存短期缓存，不写入知识库/用户画像。最终对话仍按项目原有记录规则存储，带工具的回答保留工具来源标签；源资料不会被自动写成永久事实。

关闭能力：将实际生效的 `LANMEI_AI_WEB_SEARCH_ENABLED` 或 TOML `enabled` 改为 `false`，重建/重启蓝妹，再停可选服务：

```sh
docker compose up -d --no-deps --force-recreate lanmei
docker compose --profile websearch stop openserp
```

宿主机运行则修改环境变量/配置后重启进程，并执行 `docker stop lanmei-openserp-local`（若使用独立容器）。不需要数据库回滚，无需卸载插件，也不需要删除任何数据卷。重启会清空进程内搜索缓存和会话限流状态。

升级上游时不要只换镜像：先更新固定提交/摘要，核对协议、错误字段和配置，再执行 mock 回归、低频真实验收及资源/取消测试。若出现不兼容，关闭功能或恢复已验证镜像与配置。
