# Conduit

[English README](./README.en.md)

![Backend Tests](https://github.com/KoinaAI/conduit/actions/workflows/backend-tests.yml/badge.svg)

`Conduit`，中文名 `汇流`，是一个面向多协议场景的自托管 AI 网关后端。它将 OpenAI Compatible、OpenAI Responses、Anthropic Messages、Gemini，以及基于 NewAPI / OneHub 的账户型集成统一到一个控制面与请求面中，提供模型别名路由、usage 采集、计费计算、管理 API、请求历史与定时维护能力。

当前公开仓库只包含后端实现，不包含前端控制台、私有运维脚本或任何环境相关资产。

## 项目定位

Conduit 解决的是“多上游、多协议、多账号”的统一出口问题。它适合以下类型的部署：

- 个人或团队内部统一出口，将多个 AI 提供方收敛到同一套网关接口
- 需要使用模型别名，而不是在业务侧暴露真实上游模型名
- 需要对 usage、token、成本与请求历史进行统一观测
- 需要将管理 Access Key 与实际 Relay API Key 分离配置
- 需要通过管理 API 自动维护集成账户、模型价格与签到任务

## 核心能力

### 网关协议面

- `POST /v1/chat/completions`
- `POST /v1/responses`
- `GET /v1/realtime`
- `POST /v1/messages`
- `POST /v1beta/models/{model}:generateContent`
- `POST /v1beta/models/{model}:streamGenerateContent`
- `GET /v1/models`

### 控制面

- Provider 管理
- Route 管理
- Pricing Profile 管理
- Integration 管理
- Gateway Key 管理
- Request History 查询
- Provider Probe 主动探测
- OpenAPI 文档输出

### 运行时能力

- 模型别名映射到多个上游目标
- 按协议能力与目标列表进行路由选择
- 从 JSON、SSE、WebSocket 响应提取 usage
- 本地统一计算 billing
- 记录请求历史与单次请求的上游 attempt 明细
- 定时执行集成同步与签到维护逻辑
- 将旧 JSON 状态迁移到 SQLite 存储

## 仓库结构

- `backend/`
  Go 后端源码与单元测试。
- `deploy/`
  Docker 构建文件与单服务 `compose` 部署文件。
- `.github/workflows/`
  GitHub Actions 工作流。当前仓库会在每次 `push` 与 `pull_request` 时执行后端单元测试。

## 架构概览

Conduit 由三层组成：

1. 请求面
   负责对外暴露兼容接口，如 `chat/completions`、`responses`、`messages`、Gemini 生成接口与 realtime。
2. 控制面
   负责配置 Provider、Route、Pricing Profile、Integration、Gateway Key，并输出 OpenAPI 文档。
3. 持久化与后台任务
   使用 SQLite 保存配置、请求历史与 attempt 记录，并通过调度器执行签到与探测任务。

关键实现入口：

- 应用装配与路由入口：`backend/internal/app/app.go`
- 环境配置：`backend/internal/config/config.go`
- 管理 API：`backend/internal/admin/handlers.go`
- 网关逻辑：`backend/internal/gateway/`
- 集成同步与签到：`backend/internal/integration/service.go`
- 持久化层：`backend/internal/store/store.go`

## 部署前提

### 运行依赖

- Go `1.24.x`，用于本地开发或调试
- Docker 与 Docker Compose，推荐用于部署

### 网络与安全建议

- 生产环境应通过反向代理暴露服务，并启用 HTTPS
- 管理接口必须配置强随机的 `GATEWAY_ADMIN_TOKEN`
- 建议使用独立的数据卷保存状态数据库
- 如启用公网访问，建议通过额外的网络边界限制 `/api/admin/*`

## 快速开始

### 方式一：Docker Compose 部署

```bash
cd deploy
GATEWAY_ADMIN_TOKEN='replace-with-a-strong-admin-token' \
GATEWAY_BOOTSTRAP_GATEWAY_KEY='optional-bootstrap-gateway-key' \
docker compose up --build -d
```

默认端口：

- 网关监听：`http://127.0.0.1:18092`

健康检查：

```bash
curl http://127.0.0.1:18092/healthz
```

### 方式二：本地直接运行

```bash
cd backend
GATEWAY_ADMIN_TOKEN='replace-with-a-strong-admin-token' \
go run ./cmd/gateway
```

默认监听地址：

- `:8080`

默认状态文件路径：

- `./data/gateway.db`

## 详细部署说明

### Docker Compose 变量说明

`deploy/compose.yaml` 当前暴露的关键环境变量如下：

- `GATEWAY_ADMIN_TOKEN`
  管理面令牌。访问 `/api/admin/*` 时通过 `X-Admin-Token` 或 `Authorization: Bearer ...` 提供。
- `GATEWAY_BOOTSTRAP_GATEWAY_KEY`
  可选。服务启动时自动创建一把可用于请求面的初始网关密钥。
- `GATEWAY_BIND`
  服务监听地址，默认 `:8080`。
- `GATEWAY_STATE_PATH`
  SQLite 状态数据库路径，默认 `/data/gateway.db`。
- `GATEWAY_ENABLE_REALTIME`
  是否启用 `/v1/realtime`。
- `GATEWAY_REQUEST_HISTORY`
  请求历史保留条数。
- `GATEWAY_PROBE_INTERVAL_SECONDS`
  后台主动探测 Provider 的间隔秒数。

### 推荐的生产部署方式

1. 通过 Docker Compose 启动服务
2. 将数据卷挂载到稳定存储
3. 使用 Nginx、Caddy 或 Traefik 终止 TLS
4. 仅对业务客户端开放 `/v1/*`
5. 对 `/api/admin/*` 加额外访问控制
6. 定期备份 `gateway.db`

### 反向代理示例要点

- `/v1/*` 与 `/healthz` 可直接反代到后端
- `/api/admin/*` 仅建议在受控网络下暴露
- 若使用 SSE 或 websocket，需保留长连接和升级头

## 使用指南

### 1. 检查服务是否正常

```bash
curl http://127.0.0.1:18092/healthz
```

预期响应：

```json
{"status":"ok"}
```

### 2. 查看管理面元信息

```bash
curl \
  -H 'X-Admin-Token: replace-with-a-strong-admin-token' \
  http://127.0.0.1:18092/api/admin/meta
```

### 3. 获取 OpenAPI 文档

```bash
curl \
  -H 'X-Admin-Token: replace-with-a-strong-admin-token' \
  http://127.0.0.1:18092/api/admin/openapi.json
```

### 4. 创建 Provider

```bash
curl -X POST \
  -H 'Content-Type: application/json' \
  -H 'X-Admin-Token: replace-with-a-strong-admin-token' \
  http://127.0.0.1:18092/api/admin/providers \
  -d '{
    "id": "provider-primary",
    "name": "Primary OpenAI-Compatible Provider",
    "kind": "openai-compatible",
    "base_url": "https://provider.example.com",
    "api_key": "replace-with-provider-key",
    "enabled": true,
    "capabilities": ["openai.chat", "openai.responses"]
  }'
```

### 5. 创建 Pricing Profile

```bash
curl -X POST \
  -H 'Content-Type: application/json' \
  -H 'X-Admin-Token: replace-with-a-strong-admin-token' \
  http://127.0.0.1:18092/api/admin/pricing-profiles \
  -d '{
    "id": "pricing-default",
    "name": "Default Pricing",
    "currency": "USD",
    "input_per_million": 2.5,
    "output_per_million": 10.0,
    "cached_input_per_million": 0.5,
    "reasoning_per_million": 0.0,
    "request_flat": 0.0
  }'
```

### 6. 创建 Route

```bash
curl -X POST \
  -H 'Content-Type: application/json' \
  -H 'X-Admin-Token: replace-with-a-strong-admin-token' \
  http://127.0.0.1:18092/api/admin/routes \
  -d '{
    "alias": "gpt-main",
    "pricing_profile_id": "pricing-default",
    "targets": [
      {
        "id": "target-primary",
        "account_id": "provider-primary",
        "upstream_model": "gpt-4.1",
        "weight": 1,
        "enabled": true,
        "markup_multiplier": 1.2,
        "protocols": ["openai.chat", "openai.responses"]
      }
    ]
  }'
```

### 7. 创建 Gateway Key

```bash
curl -X POST \
  -H 'Content-Type: application/json' \
  -H 'X-Admin-Token: replace-with-a-strong-admin-token' \
  http://127.0.0.1:18092/api/admin/gateway-keys \
  -d '{
    "name": "client-key",
    "allowed_models": ["gpt-main"],
    "allowed_protocols": ["openai.chat", "openai.responses"],
    "max_concurrency": 8,
    "rate_limit_rpm": 300
  }'
```

返回结果里会包含一段一次性明文 `secret`，客户端调用请求面时使用这把 key。

### 8. 调用请求面

```bash
curl -X POST \
  -H 'Content-Type: application/json' \
  -H 'X-API-Key: replace-with-gateway-secret' \
  http://127.0.0.1:18092/v1/chat/completions \
  -d '{
    "model": "gpt-main",
    "messages": [
      {"role": "user", "content": "hello"}
    ]
  }'
```

### 9. 查看请求历史

```bash
curl \
  -H 'X-Admin-Token: replace-with-a-strong-admin-token' \
  http://127.0.0.1:18092/api/admin/request-history
```

### 10. 查看某次请求的 attempt 明细

```bash
curl \
  -H 'X-Admin-Token: replace-with-a-strong-admin-token' \
  http://127.0.0.1:18092/api/admin/request-history/{request_id}/attempts
```

## 集成使用说明

Conduit 支持将账户型中转站点纳入管理面，由后端完成同步与签到逻辑。当前支持的集成类型：

- `newapi`
- `onehub`

典型流程如下：

1. 创建 Integration
2. 手动触发同步或等待后台任务执行
3. 将集成账户转化为可用 Provider
4. 按需自动生成 Route
5. 使用 Pricing Profile 补足本地计费规则

对于双凭据场景，Conduit 支持：

- 管理面使用 `access_key`
- 请求面使用 `relay_api_key`

## 数据持久化说明

Conduit 当前使用 SQLite 作为持久化后端，默认文件为：

- 开发环境：`./data/gateway.db`
- Docker Compose：`/data/gateway.db`

持久化内容包括：

- Provider
- Route
- Pricing Profile
- Integration
- Gateway Key
- Request History
- Request Attempt 明细

如果检测到旧版 JSON 状态文件，系统会在启动时自动尝试迁移，并保留一个 `*.legacy.json` 备份副本。

## 开发与测试

### 本地运行单元测试

```bash
cd backend
go test ./...
```

### GitHub Actions

仓库内置工作流：

- 文件位置：`.github/workflows/backend-tests.yml`
- 触发条件：`push`、`pull_request`
- 执行内容：在 `backend/` 目录执行 `go test ./...`

## 运维建议

- 使用强随机的 `GATEWAY_ADMIN_TOKEN`
- 不要把生产环境 Provider 密钥写入镜像
- 为 `gateway.db` 配置定期备份
- 通过反向代理限制管理面访问来源
- 对请求历史保留条数做容量规划
- 将网关日志、请求历史和反向代理日志结合排查故障

## 安全说明

- 本地状态数据库可能包含上游返回的敏感凭据，必须按机密数据处理
- 创建 Gateway Key 时返回的明文 `secret` 只应在安全通道中分发
- 公开仓库仅保留通用后端实现，不包含任何私有运维资产或环境相关内容

## 许可证

当前仓库暂未附带许可证文件。如需对外分发，请先补充明确的 license 策略。
