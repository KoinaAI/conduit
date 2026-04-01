# Conduit

[English README](./README.en.md)

`Conduit`，中文名 `汇流`，是一个自用的多协议 AI 网关后端。它把 OpenAI Compatible、OpenAI Responses、Anthropic Messages、Gemini，以及 NewAPI / OneHub 这类中转账户统一到一个入口中，提供路由映射、请求计费、管理 API 与签到调度能力。

## 当前仓库包含的内容

这个仓库当前只包含后端部分：

- `backend/`
  Go 后端，负责协议兼容、路由选择、usage 采集、计费计算、管理 API、集成同步和签到调度。
- `deploy/`
  后端镜像与单服务 `compose` 部署文件。

前端控制台暂未纳入当前仓库。

## 已实现的协议与能力

- 支持 `POST /v1/chat/completions`
- 支持 `POST /v1/responses`
- 支持 `GET /v1/realtime`
- 支持 `POST /v1/messages`
- 支持 `POST /v1beta/models/{model}:generateContent`
- 支持 `POST /v1beta/models/{model}:streamGenerateContent`
- 支持 `GET /v1/models`
- 支持把多个上游模型映射到统一别名，并按目标列表自动调度
- 支持从普通 JSON、SSE、WebSocket 响应中提取 usage，并记录本地请求历史与成本
- 支持 NewAPI / OneHub 集成接入、余额同步、模型价格导入、每日签到调度
- 支持将管理 Access Key 与实际 Relay API Key 分离配置，适配双凭据场景
- 支持基于 `X-Admin-Token` 或 `Authorization: Bearer ...` 的管理认证
- 支持 OpenAPI 文档输出，便于自动生成后台 API 文档

## 快速启动

### Docker Compose

```bash
cd deploy
GATEWAY_ADMIN_TOKEN='change-this-admin-token' \
docker compose up --build -d
```

默认端口：

- 后端网关：`http://127.0.0.1:18092`

常用环境变量：

- `GATEWAY_ADMIN_TOKEN`
- `GATEWAY_BOOTSTRAP_GATEWAY_KEY`
- `GATEWAY_ENABLE_REALTIME`
- `GATEWAY_REQUEST_HISTORY`
- `GATEWAY_PROBE_INTERVAL_SECONDS`

### 本地直接运行

```bash
cd backend
go run ./cmd/gateway
```

默认健康检查：

```bash
curl http://127.0.0.1:8080/healthz
```

## 部署文件

- `deploy/docker/backend.Dockerfile`
- `deploy/compose.yaml`

## 安全说明

- 本地状态文件中可能包含上游站点返回的敏感凭据，生产环境请妥善保护挂载的数据目录。
- 管理接口默认由 `X-Admin-Token` 保护，公网部署时建议通过反向代理与 HTTPS 一并保护。
- 公开仓库仅保留通用后端实现，不包含任何私有运维资产或环境相关内容。
