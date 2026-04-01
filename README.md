# Conduit

[English README](./README.en.md)

![Backend Tests](https://github.com/KoinaAI/conduit/actions/workflows/backend-tests.yml/badge.svg)

`Conduit`，中文名 `汇流`，是一个偏个人使用场景的自托管 AI 网关。它的主要目标不是给团队搭建统一平台，而是让你在多个来源、多个协议、多个账号之间做统一接入与切换，并把这套能力稳定地部署在自己的服务器上，供多设备共享使用。

当前公开仓库以后端为主，但项目本身并不只打算停留在后端。前端控制台仍然是整体产品的一部分，只是当前公开仓库优先发布后端基础能力、部署资产和单元测试。

## 适用场景

Conduit 主要面向以下个人使用场景：

- 你有多个上游来源，希望统一到一个固定入口
- 你会在电脑、手机、平板或远程终端上共用同一套网关配置
- 你不想在不同客户端里反复切换不同的 Base URL、API Key 和模型名
- 你希望把真实上游模型名隐藏在路由层，用自己定义的别名来访问
- 你希望统一记录 token、usage、成本和请求历史
- 你需要接入带账户管理能力的中转站点，并定期同步模型、价格或签到状态

## 功能概览

### 协议兼容

Conduit 当前支持以下常见请求面：

- OpenAI Compatible `chat/completions`
- OpenAI `responses`
- OpenAI realtime
- Anthropic `messages`
- Gemini `generateContent`
- Gemini `streamGenerateContent`
- Models 查询接口

### 路由与计费

- 将多个上游模型映射到统一别名
- 为同一别名配置多个目标并进行路由选择
- 从 JSON、SSE、WebSocket 响应中提取 usage
- 根据本地 Pricing Profile 计算成本
- 保存请求历史和单次请求的 attempt 明细

### 管理面

- Provider 管理
- Route 管理
- Pricing Profile 管理
- Integration 管理
- Gateway Key 管理
- Provider Probe 主动探测
- OpenAPI 文档输出

### 集成能力

- NewAPI 集成
- OneHub 集成
- 管理凭据与请求凭据分离
- 集成同步
- 签到调度

## 仓库结构

- `backend/`
  Go 后端源码与单元测试。
- `deploy/`
  Docker 构建文件与单服务 `compose` 部署文件。
- `.github/workflows/`
  GitHub Actions 工作流。当前仓库会在每次 `push` 与 `pull_request` 时执行后端单元测试。

## 当前公开仓库包含什么

当前公开仓库主要发布：

- 核心后端实现
- 单元测试
- Docker 部署文件
- 基础项目文档

当前公开仓库不包含：

- 私有运维脚本
- 环境相关样例数据
- 真实上游凭据
- 前端完整代码发布版本

这不表示前端不再继续开发，只表示当前公开版本以可部署、可验证的后端为先。

## 架构说明

Conduit 可以理解成三层：

1. 请求面
   对外提供统一兼容接口，客户端只需要面对 Conduit，不直接面对不同上游。
2. 控制面
   负责管理 Provider、Route、Pricing Profile、Integration 与 Gateway Key。
3. 持久化与后台任务
   使用 SQLite 保存状态，并通过后台调度器执行签到、同步和探测任务。

关键代码入口：

- 应用装配与路由注册：`backend/internal/app/app.go`
- 管理 API：`backend/internal/admin/handlers.go`
- 网关逻辑：`backend/internal/gateway/`
- 集成同步与签到：`backend/internal/integration/service.go`
- 持久化层：`backend/internal/store/store.go`
- 环境配置：`backend/internal/config/config.go`

## 部署前提

### 运行依赖

- Go `1.24.x`
- Docker
- Docker Compose

### 部署建议

- 推荐部署在你自己的 VPS 或家用服务器上
- 推荐通过反向代理暴露服务并启用 HTTPS
- 推荐把数据库文件挂载到独立持久化目录
- 推荐将管理面访问限制在你自己的设备或可信网络中

## 快速部署

### 使用 Docker Compose

```bash
cd deploy
GATEWAY_ADMIN_TOKEN='replace-with-a-strong-admin-token' \
GATEWAY_BOOTSTRAP_GATEWAY_KEY='optional-bootstrap-gateway-key' \
docker compose up --build -d
```

默认监听端口：

- `http://127.0.0.1:18092`

健康检查：

```bash
curl http://127.0.0.1:18092/healthz
```

### 本地直接运行

```bash
cd backend
GATEWAY_ADMIN_TOKEN='replace-with-a-strong-admin-token' \
go run ./cmd/gateway
```

默认行为：

- 监听地址：`:8080`
- 状态文件：`./data/gateway.db`

## 详细部署说明

### 环境变量

`deploy/compose.yaml` 当前暴露的关键变量如下：

- `GATEWAY_ADMIN_TOKEN`
  管理接口认证令牌。
- `GATEWAY_BOOTSTRAP_GATEWAY_KEY`
  启动时自动创建一把初始 Gateway Key。
- `GATEWAY_BIND`
  后端监听地址，默认 `:8080`。
- `GATEWAY_STATE_PATH`
  SQLite 数据库路径。
- `GATEWAY_ENABLE_REALTIME`
  是否启用 realtime 接口。
- `GATEWAY_REQUEST_HISTORY`
  请求历史保留条数。
- `GATEWAY_PROBE_INTERVAL_SECONDS`
  Provider 主动探测间隔秒数。

### 反向代理建议

如果你将服务放到公网，建议在 Conduit 前面放置 Nginx、Caddy 或 Traefik，并遵循以下原则：

- 将 `/v1/*` 与 `/healthz` 正常反代到后端
- 对 `/api/admin/*` 增加额外访问控制
- 为 SSE 和 websocket 保留长连接设置
- 始终启用 HTTPS

### 数据存储建议

Conduit 当前使用 SQLite 持久化状态，建议：

- 将数据库文件放在稳定的持久化卷中
- 定期做冷备份
- 升级前保留数据库快照
- 不要把数据库文件和镜像层混在一起

## 使用方式

典型使用流程如下：

1. 启动后端
2. 通过管理面创建 Provider 或 Integration
3. 配置 Route 和 Pricing Profile
4. 创建 Gateway Key
5. 在自己的客户端或脚本中把 Conduit 当成统一入口使用

对于个人多设备使用，推荐做法是：

- 在服务器上部署一套 Conduit
- 为常用客户端分配独立 Gateway Key
- 统一使用你自己的模型别名
- 通过请求历史查看不同设备或不同客户端的调用情况

## API 文档说明

本 README 不再展开详细 API 报告，也不把每个管理接口逐一写成手册。

详细 API 信息请通过以下方式获取：

- 运行中的 `/api/admin/openapi.json`
- 源码中的 Go 注释
- 路由注册与 handler 实现

如果你需要生成自己的 API 文档或客户端 SDK，建议直接基于 OpenAPI 输出和源代码注释进行生成，而不是依赖 README 的静态描述。

## 前端说明

Conduit 的目标并不只是一个“纯后端项目”。前端控制台仍然是整体产品的一部分，用于更直观地管理 Provider、Route、Integration、Gateway Key 和请求历史。

当前公开仓库先发布后端，是因为：

- 后端是整个系统的基础
- 后端更适合先稳定协议与持久化模型
- 前端仍会继续迭代，不等于取消

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
- 执行内容：在 `backend/` 目录运行全部后端单元测试

## 安全与运维建议

- 使用强随机的 `GATEWAY_ADMIN_TOKEN`
- 不要把真实上游凭据写入仓库
- 保护好数据库文件，因为其中可能包含敏感状态
- 不要把 `/api/admin/*` 直接暴露给不可信网络
- 定期检查请求历史、探测结果与调度状态

## 版本

当前公开版本目标为 `v0.1.0`，重点是：

- 确立后端结构
- 固化核心协议兼容层
- 固化基本持久化模型
- 保证后端单元测试可持续运行

## 许可证

当前仓库暂未附带许可证文件。如需对外分发，请先补充明确的 license 策略。
