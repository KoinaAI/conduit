# Conduit

[中文说明](./README.md)

`Conduit`, Chinese brand name `汇流`, is a self-hosted multi-protocol AI gateway backend. It unifies OpenAI Compatible, OpenAI Responses, Anthropic Messages, Gemini, and relay-style integrations such as NewAPI and OneHub behind a single control surface with routing, usage accounting, admin APIs, and scheduled maintenance.

## What is included in this repository

This repository currently contains the backend only:

- `backend/`
  Go backend for protocol compatibility, route selection, usage extraction, billing, admin APIs, integration sync, and scheduled maintenance.
- `deploy/`
  Backend image and single-service compose deployment files.

The frontend console is intentionally excluded for now.

## Implemented protocols and capabilities

- `POST /v1/chat/completions`
- `POST /v1/responses`
- `GET /v1/realtime`
- `POST /v1/messages`
- `POST /v1beta/models/{model}:generateContent`
- `POST /v1beta/models/{model}:streamGenerateContent`
- `GET /v1/models`
- Alias-based route mapping over multiple upstream model names
- Usage extraction from JSON, SSE, and WebSocket responses with local request history and cost calculation
- NewAPI / OneHub integration sync, balance refresh, model pricing import, and daily check-in scheduling
- Separate management access key and relay API key support for dual-credential providers
- Admin authentication via `X-Admin-Token` or `Authorization: Bearer ...`
- OpenAPI output for documentation generation

## Quick start

### Docker Compose

```bash
cd deploy
GATEWAY_ADMIN_TOKEN='change-this-admin-token' \
docker compose up --build -d
```

Default port:

- Gateway backend: `http://127.0.0.1:18092`

Common environment variables:

- `GATEWAY_ADMIN_TOKEN`
- `GATEWAY_BOOTSTRAP_GATEWAY_KEY`
- `GATEWAY_ENABLE_REALTIME`
- `GATEWAY_REQUEST_HISTORY`
- `GATEWAY_PROBE_INTERVAL_SECONDS`

### Run locally

```bash
cd backend
go run ./cmd/gateway
```

Health check:

```bash
curl http://127.0.0.1:8080/healthz
```

## Deployment files

- `deploy/docker/backend.Dockerfile`
- `deploy/compose.yaml`

## Security notes

- Local state may contain sensitive upstream credentials. Protect the mounted data directory in production.
- Admin APIs are protected by `X-Admin-Token`; public deployments should also enforce HTTPS and a proper reverse proxy boundary.
- This public repository contains only the generic backend implementation and excludes private operational assets or environment-specific material.
