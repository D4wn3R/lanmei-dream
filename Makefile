# 蓝妹（lanmei-dream）常用命令入口。
# 前置依赖：docker compose（全栈部署）；Go 与 Node.js（本地开发）。

.PHONY: help up down restart logs ps build dev-server dev-web dev \n	antifraud-image-save antifraud-image-load

# 未指定目标时默认显示帮助
.DEFAULT_GOAL := help

help: ## 显示所有命令
	@echo "蓝妹启动命令："
	@echo "  make up          构建并启动全部服务（含管理面板，统一入口 http://<host>:80）"
	@echo "  make down        停止全部服务"
	@echo "  make restart     重启 lanmei 服务"
	@echo "  make logs        跟随 lanmei 日志"
	@echo "  make ps          查看服务状态"
	@echo "  make build       编译 Go 后端"
	@echo "  make antifraud-image-save  导出反诈推理服务镜像为离线包（antifraud-image.tar.gz）"
	@echo "  make antifraud-image-load  导入反诈推理服务离线镜像包"
	@echo "  make dev-server  本地运行后端（管理面板 :8090）"
	@echo "  make dev-web     本地运行管理面板前端（vite :5173，/api 代理到 :8090）"
	@echo ""
	@echo "本地开发请开两个终端：make dev-server + make dev-web"

# Docker 全栈：构建镜像并启动所有服务（生产/一键部署，含管理面板）
up: ## 构建并启动全部服务
	docker compose up -d --build

down: ## 停止全部服务
	docker compose down

restart: ## 重启 lanmei
	docker compose restart lanmei

logs: ## 跟随 lanmei 日志
	docker compose logs -f lanmei

ps: ## 服务状态
	docker compose ps

# 本地开发：前后端分开运行（程序不会自动读取 .env，敏感配置需自行导出为环境变量）
build: ## 编译 Go 后端
	go build ./cmd/lanmei

dev-server: ## 本地运行后端（管理面板需 config.toml 开启 [manager] enabled=true 并提供 MANAGER 凭据）
	go run ./cmd/lanmei

dev-web: ## 本地运行管理面板前端（vite 默认 :5173，/api 代理到 127.0.0.1:8090）
	cd manager && npm install && npm run dev

# 反诈推理服务（LanmeiAntiFraud）离线分发：镜像约 2~3GB（含模型资产），在联网机器导出后拷到目标机导入。
# 目标机执行：make antifraud-image-load 后，用 `docker compose --profile antifraud up -d` 启动。
antifraud-image-save: ## 导出反诈服务镜像为离线包
	docker save ${LANMEI_ANTIFRAUD_IMAGE:-lanmei-antifraud:latest} | gzip > antifraud-image.tar.gz
	@echo "已导出 antifraud-image.tar.gz，拷贝到目标机后执行 make antifraud-image-load"

antifraud-image-load: ## 导入反诈服务离线镜像包
	@test -f antifraud-image.tar.gz || (echo "缺少 antifraud-image.tar.gz" >&2; exit 1)
	gzip -dc antifraud-image.tar.gz | docker load
	@echo "镜像已导入：docker compose --profile antifraud up -d"
