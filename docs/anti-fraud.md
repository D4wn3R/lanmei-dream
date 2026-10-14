# 反诈插件（LanmeiAntiFraud 接入）

接入 [LanmeiAntiFraud](https://github.com/zrurf/LanmeiAntiFraud) 文本反诈推理服务：消息异步送模型判定，
高置信度违规自动撤回 + 禁言并上报管理员，中置信度入库并上报，由管理员在管理面板审批。

## 1. 工作原理

- 插件形态：内置插件 `internal/bizplugin/antifraud`（Go，编译进主程序），以「消息观察者」
  （`plugin.MessageObserver`）旁路观察消息——只看不抢，不参与行为树决策，不影响机器人回复。
- 异步：观察者只做过滤与非阻塞入队；模型调用、处置、上报都在插件自有协程中执行，
  不占用消息管线预算，队列满时丢弃并计数，绝不阻塞主流程。
- 判定：服务返回 10 类 softmax 概率，插件计算**违规分 = 1 − P(正常)**，并要求 top1 类别不是「正常」。
- 处置：
  - 违规分 ≥ `auto_threshold`（默认 0.90）：自动撤回 + 禁言 + 入库（`auto_handled`）+ 上报；
  - 违规分 ≥ `report_threshold`（默认 0.60）：入库（`pending`）+ 上报，等待管理员审批；
  - 低于上报阈值：不记录、不通知。
- 群内告知：撤回成功后，会在违规所在群 @ 该成员说明违规类别与处置（如「这条「刷单」消息蓝妹撤回啦，先去冷静 1 小时吧」），
  文案不含违规内容；同用户同文本的重复违规只提示一次（避免刷屏），撤回失败时不在群里公开指控。
- 限流与保护：用户级冷却、判决缓存（相同文本不重复调用模型）、批量聚合、提交速率上限、
  连续失败熔断；同用户同文本在合并窗口内只留一条记录（重复计数），但**每条新消息都会各自撤回**。
- 记录与审批：违规记录写入插件自管的表 `antifraud_records`（插件启动时自动建表，不进主库迁移列表），
  管理面板「反诈」页可查看、审批、驳回与删除。

## 2. 部署

### 2.1 compose 一键部署（推荐）

```bash
# 1) 构建反诈服务镜像（首次需联网拉取基础镜像与 torch 依赖；模型资产已在该仓库内）
export LANMEI_ANTIFRAUD_DIR=/path/to/LanmeiAntiFraud   # 默认 ../LanmeiAntiFraud/LanmeiAntiFraud
docker compose --profile antifraud build antifraud

# 2) 启动（antifraud 与 lanmei 同网络，lanmei 已注入 LANMEI_PLUGIN_ANTIFRAUD_ENDPOINT=http://antifraud:8000）
docker compose --profile antifraud up -d

# 3) 开启插件开关（config/config.toml）
#    [plugin.builtins]
#    antifraud = true
docker compose restart lanmei
```

`antifraud` 服务**不映射宿主端口**：模型服务自身无鉴权，只允许 compose 内网调用。
如需外部访问，请自行加反向代理与鉴权（服务端 `/docs` 也是开放的）。

### 2.2 离线分发（目标机无外网）

```bash
# 联网机器（已构建镜像）
make antifraud-image-save      # 生成 antifraud-image.tar.gz（约 1~1.5GB）
scp antifraud-image.tar.gz 目标机:

# 目标机
make antifraud-image-load
docker compose --profile antifraud up -d
```

镜像可用 `LANMEI_ANTIFRAUD_IMAGE` 指定（默认 `lanmei-antifraud:latest`），便于用私有仓库分发。

### 2.3 独立部署

反诈服务也可以完全独立部署（用它自带的 compose/Dockerfile），此时只需让主程序知道地址：

```bash
LANMEI_PLUGIN_ANTIFRAUD_ENDPOINT=http://<host>:<port>   # 覆盖 config.toml
```

插件对服务不可达是容错的：调用失败按熔断策略暂停，恢复后自动继续，期间不阻塞任何消息处理。

## 3. 配置

优先级：环境变量 > `config/config.toml`（见 `internal/config/init.go` 的 env 映射表）。

| 配置项（`[plugin.antifraud]`） | 环境变量 | 默认 | 说明 |
| --- | --- | --- | --- |
| `endpoint` | `LANMEI_PLUGIN_ANTIFRAUD_ENDPOINT` | 空 | 推理服务地址；为空时即使开关打开也不注册插件 |
| `auto_threshold` | `LANMEI_PLUGIN_ANTIFRAUD_AUTO_THRESHOLD` | 0.90 | 自动处置阈值（违规分） |
| `report_threshold` | `LANMEI_PLUGIN_ANTIFRAUD_REPORT_THRESHOLD` | 0.60 | 入库并上报阈值 |
| `mute_seconds` | `LANMEI_PLUGIN_ANTIFRAUD_MUTE_SECONDS` | 3600 | 自动处置禁言时长（默认 1 小时）；0 表示只撤回不禁言 |
| `min_chars` | — | 4 | 参与判定的最短文本长度（按 rune 计） |
| `monitor_private` | — | false | 是否判定私聊消息 |
| `group_whitelist` / `group_blacklist` | — | 空 | 白名单非空时只判定白名单内的群 |
| `skip_super_users` | — | true | 跳过超管与动态管理员的消息 |
| `max_content_chars` | — | 1000 | 送判与入库的文本截断长度 |
| `user_cooldown_seconds` | — | 10 | 同一用户两次判定的最小间隔 |
| `queue_size` | — | 256 | 待判定队列长度，满则丢弃并计数 |
| `batch_size` / `batch_window_millis` | — | 16 / 200 | 批量聚合上限与窗口 |
| `max_batches_per_second` | — | 2 | 提交速率上限（批/秒） |
| `request_timeout_seconds` | — | 8 | 单批判定总预算（含轮询） |
| `cache_ttl_seconds` | — | 600 | 相同文本的判决缓存时长 |
| `dedup_window_seconds` | — | 120 | 同用户同文本的记录合并窗口 |
| `circuit_failures` / `circuit_open_seconds` | — | 5 / 60 | 连续失败熔断阈值与熔断时长 |
| `notice_group_id` | `LANMEI_PLUGIN_ANTIFRAUD_NOTICE_GROUP_ID` | 空 | 反诈告警群 ID；为空时只私聊管理员 |
| `panel_base_url` | `LANMEI_PLUGIN_ANTIFRAUD_PANEL_BASE_URL` | 空 | 面板地址，用于上报中的记录链接（如 `https://panel.example.com`） |
| `retention_days` | — | 90 | 违规记录保留天数；0 表示永久（`pending` 永不自动清理） |

插件开关：`[plugin.builtins].antifraud = true`。

上报目标：静态超管（`LANMEI_BOT_SUPER_USERS`）与 `bot_admin` 表内的动态管理员（按违规所在平台过滤），
以及可选的告警群。告警群 ID 应与违规消息同平台。

阈值校准建议：面板「近 7 天违规分分布」给出各区间计数。
若 0.9 以上区间极少而 0.6~0.7 区间堆积，说明自动阈值偏高（大量违规走人工），可逐步下调；
反之若 0.9 以上出现误判，应上调阈值并复查记录内容。

## 4. 管理面板

菜单「反诈」（路由 `/#/antifraud`）：

- 概览：待审批数、今日新增、今日处置、动作失败数，以及近 7 天违规分分布与类别计数。
- 「待审批」标签：`status=pending` 的记录，支持按群 / 用户 / 关键字过滤。
- 「全部记录」：所有状态，含 `auto_handled`（已自动处置）、`approved`、`rejected`。
- 详情抽屉：完整内容、类别概率分布、专家门控权重、消息 ID、处置结果与审批记录。
- 操作：
  - **通过并处置**：执行撤回 + 禁言（可覆盖禁言时长），失败原因原样记录并回显；
    对已通过的记录再次操作会重新下发（用于重试此前失败的动作）；
  - **驳回**：标记为误报，不执行任何动作（若此前已自动处置，已执行的动作无法撤销）；
  - **删除**：仅超管 + step-up 可执行。

权限：查看与审批驳回只需登录面板；删除记录需超管 + 二次验证。审批与删除都会写入操作审计。

## 5. 机器人权限要求

撤回他人消息与禁言成员都要求机器人**是群管理员或群主**：

- 撤回：机器人为管理员/群主时不受消息发出时间限制；仅为普通成员时只能撤回自己 2 分钟内的消息；
- 禁言：需管理员权限，且不能禁言群主。

权限不足时平台返回错误码，插件会把它原样记录到 `recall_error` / `mute_error` 并在面板与上报文案中展示。
注意：面板审批时若机器人已失去管理员权限，动作同样会失败——这是平台权限问题，不是插件缺陷。

## 6. 排障

| 现象 | 排查方向 |
| --- | --- |
| 面板提示「反诈插件未启用」 | `[plugin.builtins].antifraud` 未开，或 `endpoint` 为空；查看启动日志的 `bizplugin: 反诈插件已启用但未配置推理服务地址` 告警 |
| 日志频繁 `判定请求失败` / 收到「判定服务连续失败」通知 | 反诈服务未启动或地址不通：`docker compose logs antifraud`、`curl http://antifraud:8000/api/v1/health` 应返回 `"model_loaded": true` |
| 消息被判违规但没有撤回/禁言 | 查看记录的 `recall_error`/`mute_error`（多为机器人无群管理权限），或 `mute_seconds=0` |
| 记录数远少于消息数 | 正常：未达上报阈值不记录；另有用户冷却、队列丢弃（日志 `判定队列已满`）与判决缓存 |
| 判定延迟高 | 单条 CPU 推理约 0.5s；提高 `LMAF_WORKERS`、适当放大 `batch_size`，或改用 GPU 镜像（`LANMEI_ANTIFRAUD_TORCH_INDEX=cu130`） |

## 7. 验证清单

不接真实模型也能端到端自测（用任意能返回固定响应的 HTTP 服务模拟推理服务）：

```bash
# 1) 假推理服务（示例：python -m http.server 不行，需要 ticket 协议，可用下面的极简替身）
python - <<'PY'
import json, threading
from http.server import BaseHTTPRequestHandler, HTTPServer
items = []
class H(BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers['Content-Length']); body = json.loads(self.rfile.read(n))
        items.clear()
        for t in body['texts']:
            items.append({"text": t, "label_id": 3, "label_key": "brushing", "label_name": "刷单",
                          "confidence": 0.97, "probabilities": [0.03,0,0,0.97,0,0,0,0,0,0],
                          "experts_opinion": []})
        self.send_response(202); self.end_headers(); self.wfile.write(json.dumps({"ticket":"t1","status":"queued"}).encode())
    def do_GET(self):
        self.send_response(200); self.end_headers()
        self.wfile.write(json.dumps({"ticket":"t1","status":"done","result":{"items":items}}).encode())
HTTPServer(('0.0.0.0', 8000), H).serve_forever()
PY

# 2) 让插件指向该替身（本地开发）
export LANMEI_PLUGIN_ANTIFRAUD_ENDPOINT=http://127.0.0.1:8000

# 3) 开启插件（config/config.toml）后启动：任意群消息都会因「违规分 0.97 ≥ 0.90」被自动处置，
#    记录出现在面板「反诈」页；手动验证撤回/禁言真实生效需要机器人具备群管理员权限。
```

单元测试（不需要服务与数据库，使用 httptest + SQLite）：

```bash
go test ./internal/bizplugin/antifraud/...
```

## 8. 未覆盖范围

- 图片、语音、视频内容不判定（模型是文本分类，只取文本段，`at` 段会被排除）；
- 没有申诉入口：误判由管理员在面板驳回并备注；
- 不做 IG 归因解释（服务 `explain=true` 约 3.3s/条，需要的话可在插件里按阈值开启）；
- 私聊默认不判定（可用 `monitor_private` 打开）。
