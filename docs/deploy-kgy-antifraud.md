# kgy 生产服务器部署记录（2026-10-14，LanmeiAntiFraud 反诈服务，**已回滚**）

> **⚠️ 2026-10-14 深夜回滚（主人拍板）**：服务上线后 swap 从 626M 涨到 2.5G、整机 available 仅剩 1.1G，内存代价不可接受，决定**放弃部署该 HTTP 服务**。服务器侧已完全还原：compose 恢复 `.bak-1014` 备份、容器与镜像已删、镜像包已清。本文保留作为**未来若升级服务器内存或改用外部推理 API 时的完整操作手册**——构建流程、坑与验收数值均验证有效，换模型权重/重部署时直接对照执行。

## 部署基线（回滚前快照）

| 项 | 值 |
|---|---|
| 服务 | LanmeiAntiFraud（zip 提供，源码非 git 仓库） |
| 镜像 | `lanmei-antifraud:1014`（本地 buildx linux/amd64 交叉构建，gzip 后 1.5G，load 后 2.58G） |
| 模型产物 | `deploy.zip`（model.pt2 + model.pt + meta + tokenizer）→ 镜像内 `/app/artifacts/export/deploy/` |
| 底座模型 | `macbert.zip`（chinese-macbert-base，仅取 8 个必需文件，剔 flax/tf 权重）→ 镜像内 `/app/models/macbert/` |
| 接入方式 | 追加进 `~/lanmei-dream/docker-compose.yml`（备份 `docker-compose.yml.bak-1014`），加入 `lanmei-dream_default` 网络 |
| 启用时间 | 2026-10-14 22:55 前后（health `model_loaded=true, forward_backend=pt2`） |

## 服务定义（compose 追加段）

```yaml
  antifraud:
    image: lanmei-antifraud:latest
    container_name: lanmei-antifraud
    init: true
    restart: unless-stopped
    mem_limit: 3g
    cpus: 2.0
    ports:
      - "127.0.0.1:8000:8000"   # 只绑回环，供宿主验证；未暴露公网
    environment:
      LMAF_WORKERS: 2           # 匹配 2C
      LMAF_TASK_TTL: 3600
      OMP_NUM_THREADS: 2
    networks:
      - default
```

`docker compose config --quiet` 通过；diff 仅 +16 行服务段，未触碰服务器定制文件。未来蓝妹 Go 侧接入走 `http://antifraud:8000`（同 compose 网络）。

## API 速查

- `POST /api/v1/infer` 提交任务（`{"texts":[...],"explain":bool}`，返回 ticket，202）
- `GET /api/v1/infer/{ticket}` 轮询结果；`GET /api/v1/health`、`GET /api/v1/meta`
- 结果字段：`label_name`（10 分类：正常/广告骚扰/虚假购物/刷单/冒充客服/冒充公检法/冒充领导熟人/投资理财/贷款征信/关系型）+ `confidence` + `probabilities`

## 构建要点（下次换模型/升级时对照）

- **构建上下文**：源码 zip（GBK 文件名，用 python zipfile `cp437→gbk` 修复）+ `deploy.zip → artifacts/export/deploy/` + macbert 仅 `.dockerignore` 点名的 8 个文件 → `models/macbert/`。
- **基础镜像拉取是本次最大的坑**：本机直连 docker.io 被污染（解析到无关 IP）；国内镜像源（daocloud/1ms/dockerpull 等）全不可用或返回 HTML 假镜像。**可用路线：宿主代理 + skopeo**：
  ```bash
  HTTPS_PROXY=http://127.0.0.1:7890 skopeo copy --override-os linux --override-arch amd64 \
    docker://docker.io/library/python:3.14.8-slim docker-daemon:python:3.14.8-slim
  ```
  基础镜像落到本地 daemon 后，`buildx build --platform linux/amd64` 不会再碰 docker.io。buildx 的 `--build-arg HTTP_PROXY` / builder `--driver-opt env.*` 都**不影响 buildkitd 拉基础镜像**，别在这两条路上浪费时间。pip 依赖走清华源（Dockerfile `PIP_INDEX`）。
- torch==2.14.0 与导出 model.pt2 的版本一致，跨版本导图不保证可加载——升级 torch 需要训练侧重新导出。
- 传输：rsync `--partial --inplace`，1.5G 约 5 分钟。**服务器 rsync 3.2.7 不认 `--info=progress2`**（会报 usage 退出 1）。

## 验收结果（2026-10-14）

- 本地（arm64 宿主跑 amd64 容器）：health `model_loaded=true, forward_backend=pt2`；「点击链接领取红包，逾期冻结账户」→ 正常 0.8061，与官方文档数值逐位一致。
- 生产：health 同上；批量 2 条 → 正常 0.8061 / 正常 0.9892；诈骗样本 3 条 → 广告骚扰 0.94、广告骚扰 0.75、正常 0.67（冒充客服样本置信度偏低，属模型本身水平，非部署问题）。
- 资源：常驻 **1.32G / 3G 限**，低于文档预估的 2.5–3G；整机 available 从 1.8G 降到 1.1G，swap 无显著承压。注意常驻比文档预估省的原因待观察（可能是预签名/加载策略差异），若后续内存爬升到 2.5G+ 属正常。

## 遗留事项

- ~~蓝妹 Go 侧尚无反诈调用代码~~ **2026-10-14 回滚后状态：服务已下线，未来重部署需从头走本文构建流程（三个 zip 在主人 Downloads：LanmeiAntiFraud.zip / deploy.zip / macbert.zip）。**
- ~~服务器上镜像包已删~~。
- 冒充客服/公检法类样本识别置信度一般（0.67 判正常），如未来重部署后误报率高需向训练侧反馈（换模型权重 = 重新走本文构建流程）。
- **内存教训**：该服务常驻 ~1.3G，在 3.6G 内存的服务器上会挤出 ~2G swap，重部署前先确认内存升级到位，或改走外部推理 API / 按需启停（`docker compose stop antifraud` + 消息触发时拉起，冷启动 ~10s）方案。
