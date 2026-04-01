# Conduit

[中文说明](./README.md)

![Backend Tests](https://github.com/KoinaAI/conduit/actions/workflows/backend-tests.yml/badge.svg)

`Conduit`, Chinese brand name `汇流`, is a self-hosted multi-protocol AI gateway backend. It consolidates OpenAI Compatible, OpenAI Responses, Anthropic Messages, Gemini, and account-based relay integrations such as NewAPI and OneHub into a single control plane and request plane with route mapping, usage extraction, billing, admin APIs, request history, and scheduled maintenance.

This public repository contains the backend only. It does not include the frontend console, private operational scripts, or environment-specific assets.

## Project scope

Conduit is designed for teams or individuals who need a unified AI gateway in front of multiple upstream providers and multiple wire protocols.

Typical use cases:

- exposing a single internal gateway instead of leaking upstream vendor details into application code
- mapping multiple upstream models to stable public aliases
- collecting unified token usage, request history, and cost accounting
- separating management access keys from relay API keys
- automating sync and maintenance flows for relay-style integrations

## Core capabilities

### Gateway surfaces

- `POST /v1/chat/completions`
- `POST /v1/responses`
- `GET /v1/realtime`
- `POST /v1/messages`
- `POST /v1beta/models/{model}:generateContent`
- `POST /v1beta/models/{model}:streamGenerateContent`
- `GET /v1/models`

### Control plane

- provider management
- route management
- pricing profile management
- integration management
- gateway key management
- request history inspection
- active provider probes
- OpenAPI document export

### Runtime behavior

- alias-to-target route mapping
- protocol-aware route selection
- usage extraction from JSON, SSE, and WebSocket responses
- local billing calculation
- request history and per-request upstream attempt recording
- scheduled sync and check-in maintenance jobs
- legacy JSON state import into SQLite

## Repository layout

- `backend/`
  Go backend source code and unit tests.
- `deploy/`
  Docker build assets and single-service compose deployment.
- `.github/workflows/`
  GitHub Actions workflows. The repository runs backend unit tests on every `push` and `pull_request`.

## Architecture overview

Conduit is organized into three major layers:

1. Request plane
   Exposes public compatibility endpoints such as `chat/completions`, `responses`, `messages`, Gemini generation endpoints, and realtime.
2. Control plane
   Manages providers, routes, pricing profiles, integrations, gateway keys, and OpenAPI output.
3. Persistence and background tasks
   Stores configuration and request records in SQLite and executes scheduled maintenance tasks such as sync, check-in, and active probes.

Key implementation entry points:

- application assembly and route wiring: `backend/internal/app/app.go`
- environment configuration: `backend/internal/config/config.go`
- admin API: `backend/internal/admin/handlers.go`
- gateway runtime: `backend/internal/gateway/`
- integration sync and check-in logic: `backend/internal/integration/service.go`
- persistence layer: `backend/internal/store/store.go`

## Prerequisites

### Runtime dependencies

- Go `1.24.x` for local development
- Docker and Docker Compose for recommended deployment

### Network and security recommendations

- terminate TLS at a reverse proxy in production
- always configure a strong `GATEWAY_ADMIN_TOKEN`
- store the database on a dedicated persistent volume
- place `/api/admin/*` behind an additional network boundary when exposed outside localhost

## Quick start

### Option 1: Docker Compose

```bash
cd deploy
GATEWAY_ADMIN_TOKEN='replace-with-a-strong-admin-token' \
GATEWAY_BOOTSTRAP_GATEWAY_KEY='optional-bootstrap-gateway-key' \
docker compose up --build -d
```

Default port:

- gateway endpoint: `http://127.0.0.1:18092`

Health check:

```bash
curl http://127.0.0.1:18092/healthz
```

### Option 2: Run locally

```bash
cd backend
GATEWAY_ADMIN_TOKEN='replace-with-a-strong-admin-token' \
go run ./cmd/gateway
```

Default bind address:

- `:8080`

Default state path:

- `./data/gateway.db`

## Detailed deployment guide

### Environment variables

The primary deployment variables exposed by `deploy/compose.yaml` are:

- `GATEWAY_ADMIN_TOKEN`
  Admin token for `/api/admin/*`. Accepted via `X-Admin-Token` or `Authorization: Bearer ...`.
- `GATEWAY_BOOTSTRAP_GATEWAY_KEY`
  Optional. Creates a bootstrap gateway key during startup.
- `GATEWAY_BIND`
  Backend bind address. Default: `:8080`.
- `GATEWAY_STATE_PATH`
  SQLite database location. Default: `/data/gateway.db`.
- `GATEWAY_ENABLE_REALTIME`
  Enables or disables `/v1/realtime`.
- `GATEWAY_REQUEST_HISTORY`
  Maximum retained request history entries.
- `GATEWAY_PROBE_INTERVAL_SECONDS`
  Probe interval for scheduled provider health checks.

### Recommended production layout

1. run the backend with Docker Compose
2. mount `/data` to persistent storage
3. terminate TLS with Nginx, Caddy, or Traefik
4. expose `/v1/*` to application clients
5. restrict `/api/admin/*` to trusted operators
6. back up `gateway.db` regularly

### Reverse proxy notes

- proxy `/v1/*` and `/healthz` directly to the backend
- avoid exposing `/api/admin/*` publicly without additional controls
- preserve long-lived connections and upgrade headers for SSE and websocket traffic

## Usage guide

### 1. Verify service health

```bash
curl http://127.0.0.1:18092/healthz
```

Expected response:

```json
{"status":"ok"}
```

### 2. Inspect admin metadata

```bash
curl \
  -H 'X-Admin-Token: replace-with-a-strong-admin-token' \
  http://127.0.0.1:18092/api/admin/meta
```

### 3. Fetch the OpenAPI document

```bash
curl \
  -H 'X-Admin-Token: replace-with-a-strong-admin-token' \
  http://127.0.0.1:18092/api/admin/openapi.json
```

### 4. Create a provider

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

### 5. Create a pricing profile

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

### 6. Create a route

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

### 7. Create a gateway key

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

The response includes a plaintext `secret`. Use that value for client-side access to the request plane.

### 8. Call the gateway

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

### 9. Inspect request history

```bash
curl \
  -H 'X-Admin-Token: replace-with-a-strong-admin-token' \
  http://127.0.0.1:18092/api/admin/request-history
```

### 10. Inspect upstream attempts for one request

```bash
curl \
  -H 'X-Admin-Token: replace-with-a-strong-admin-token' \
  http://127.0.0.1:18092/api/admin/request-history/{request_id}/attempts
```

## Integration guide

Conduit can manage account-style relay platforms through the admin API. Supported integration kinds:

- `newapi`
- `onehub`

Typical flow:

1. create an integration
2. trigger a sync or let the scheduler maintain it
3. materialize the integration as a provider
4. optionally auto-create routes
5. attach pricing profiles for local billing

For dual-credential integrations, Conduit supports:

- `access_key` for management-side synchronization
- `relay_api_key` for request-side provider traffic

## Persistence model

Conduit uses SQLite as its current persistence backend. Default locations:

- local development: `./data/gateway.db`
- Docker Compose: `/data/gateway.db`

Persisted entities include:

- providers
- routes
- pricing profiles
- integrations
- gateway keys
- request history
- request attempt records

If a legacy JSON state file is detected, the service attempts to import it automatically and leaves a `*.legacy.json` backup.

## Development and testing

### Run backend unit tests locally

```bash
cd backend
go test ./...
```

### GitHub Actions

The repository includes a backend unit test workflow:

- file: `.github/workflows/backend-tests.yml`
- triggers: `push`, `pull_request`
- job: runs `go test ./...` inside `backend/`

## Operational recommendations

- use a strong random `GATEWAY_ADMIN_TOKEN`
- do not bake provider keys into images
- back up `gateway.db` regularly
- protect `/api/admin/*` behind a reverse proxy or network boundary
- size `GATEWAY_REQUEST_HISTORY` according to your retention needs
- correlate gateway logs, request history, and reverse-proxy logs during incident analysis

## Security notes

- the local state database may contain sensitive upstream credentials and must be handled as secret material
- the plaintext gateway `secret` returned at creation time should only be distributed via a secure channel
- this public repository contains only the generic backend implementation and excludes private operational assets or environment-specific material

## License

No license file is currently included. Add an explicit license before broader redistribution.
