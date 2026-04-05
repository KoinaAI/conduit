# Conduit

[中文说明](./README.md)

![Backend Tests](https://github.com/KoinaAI/conduit/actions/workflows/backend-tests.yml/badge.svg)
![Backend Image](https://github.com/KoinaAI/conduit/actions/workflows/backend-image.yml/badge.svg)

`Conduit`, Chinese brand name `汇流`, is a self-hosted AI gateway focused primarily on personal use. Its main purpose is not to act as a shared team platform, but to let you choose between multiple upstream sources, protocols, and accounts from a single stable entrypoint, typically deployed on your own server for use across your own devices.

The current public repository prioritizes the backend, but the product itself is not intended to remain backend-only. The frontend console is still part of the overall project; it is simply not the first public artifact in this repository.

## Intended usage

Conduit is mainly designed for personal deployment scenarios such as:

- consolidating multiple upstream AI sources behind one endpoint
- using the same gateway from laptops, phones, tablets, or remote shells
- avoiding repeated switching between upstream Base URLs, API keys, and model names
- exposing your own stable model aliases instead of upstream vendor identifiers
- collecting unified usage, token, cost, and request history data
- managing relay-style integrations that require sync or scheduled maintenance

## Feature overview

### Protocol compatibility

Conduit currently supports:

- OpenAI Compatible `chat/completions`
- OpenAI `responses`
- OpenAI realtime
- Anthropic `messages`
- Gemini `generateContent`
- Gemini `streamGenerateContent`
- models listing

### Routing and billing

- alias-based route mapping
- multiple upstream targets per alias
- ordered route transformer pipelines for upstream-request and downstream-response header/JSON rewrites
- scenario routing, Codex turn-state stickiness, and optional Redis-backed sticky sessions
- usage extraction from JSON, SSE, and WebSocket responses
- local cost calculation through pricing profiles
- public pricing-catalog sync in addition to relay-managed pricing sync
- request history and per-request attempt tracking

### Control plane

- provider management
- route management
- pricing profile management
- integration management
- gateway key management
- active provider probes
- OpenAPI document output

### Integration support

- NewAPI integrations
- OneHub integrations
- separate management credentials and request credentials
- sync workflows
- scheduled check-in maintenance
- scheduled public pricing catalog sync

### CLI tooling

- bundled `conduit-cli`
- health checks, stats queries, gateway-key creation, and shell environment export helpers

## Repository layout

- `backend/`
  Go backend source code and unit tests.
- `backend/cmd/conduit-cli/`
  Helper CLI for personal deployment workflows.
- `Dockerfile`
  Backend container image entrypoint.
- `.github/workflows/`
  GitHub Actions workflows. The repository runs backend unit tests and builds or publishes the backend GHCR image.

## What the public repository contains

The current public repository includes:

- core backend implementation
- unit tests
- backend container build assets
- GHCR publication workflow
- baseline project documentation

The current public repository does not include:

- private operational scripts
- environment-specific sample data
- real upstream credentials
- the full public release of the frontend codebase

That does not mean the frontend is abandoned. It only means the backend is being published first as the stable foundation.

## Architecture

Conduit is easiest to think of as three layers:

1. Request plane
   Exposes unified compatibility endpoints to clients.
2. Control plane
   Manages providers, routes, pricing profiles, integrations, and gateway keys.
3. Persistence and background jobs
   Stores runtime state in SQLite or PostgreSQL and runs scheduled sync, check-in, and probe tasks.

Key entry points:

- application assembly and route registration: `backend/internal/app/app.go`
- admin API handlers: `backend/internal/admin/handlers.go`
- gateway runtime: `backend/internal/gateway/`
- integration sync and maintenance: `backend/internal/integration/service.go`
- persistence layer: `backend/internal/store/store.go`
- environment configuration: `backend/internal/config/config.go`

## Prerequisites

### Runtime dependencies

- Go `1.24.x`
- Docker

### Deployment recommendations

- run Conduit on your own VPS or home server
- put it behind a reverse proxy with HTTPS
- store the SQLite database on a persistent volume
- restrict `/api/admin/*` to your own devices or trusted networks

## Quick deployment

### Deploy directly from GHCR

Default image:

- `ghcr.io/koinaai/conduit-backend:latest`

Typical deployment:

```bash
mkdir -p /srv/conduit

docker pull ghcr.io/koinaai/conduit-backend:latest

docker run -d \
  --name conduit-backend \
  --restart unless-stopped \
  -p 18092:8080 \
  -v /srv/conduit:/data \
  -e GATEWAY_ADMIN_TOKEN='replace-with-a-strong-admin-token' \
  -e GATEWAY_BOOTSTRAP_GATEWAY_KEY='optional-bootstrap-gateway-key' \
  -e GATEWAY_STATE_PATH='/data/gateway.db' \
  ghcr.io/koinaai/conduit-backend:latest
```

Health check:

```bash
curl http://127.0.0.1:18092/healthz
```

### Run locally from source

```bash
cd backend
GATEWAY_ADMIN_TOKEN='replace-with-a-strong-admin-token' \
go run ./cmd/gateway
```

Default behavior:

- bind address: `:8080`
- state file: `./data/gateway.db`

## Detailed deployment notes

### GHCR publication

The repository includes a dedicated backend image workflow that publishes to GHCR:

- workflow file: `.github/workflows/backend-image.yml`
- Dockerfile: `./Dockerfile`
- image name: `ghcr.io/koinaai/conduit-backend`
- default tag: `latest`
- extra tags: `sha-<commit>`, branch tags, and `vX.Y.Z` on release tags

Trigger behavior:

- pushes to `main` build and publish
- version tags `v*` build and publish
- `pull_request` builds for validation only
- `workflow_dispatch` allows manual publication

If the package is still private in GHCR, log in before pulling:

```bash
docker login ghcr.io
```

### Environment variables

Primary variables for direct container deployment:

- `GATEWAY_ADMIN_TOKEN`
  Authentication token for admin routes.
- `GATEWAY_BOOTSTRAP_GATEWAY_KEY`
  Creates an initial gateway key at startup.
- `GATEWAY_BIND`
  Backend bind address, default `:8080`.
- `GATEWAY_STATE_PATH`
  SQLite database path.
- `GATEWAY_DATABASE_URL`
  Optional PostgreSQL DSN. When set, it takes precedence over `GATEWAY_STATE_PATH`.
- `GATEWAY_ENABLE_REALTIME`
  Enables or disables realtime support.
- `GATEWAY_REQUEST_HISTORY`
  Maximum retained request-history entries.
- `GATEWAY_PROBE_INTERVAL_SECONDS`
  Provider probe interval in seconds.
- `GATEWAY_PRICING_SYNC_ENABLED`
  Enables scheduled public pricing-catalog sync.
- `GATEWAY_PRICING_CATALOG_URL`
  Public pricing catalog URL. Defaults to `https://models.dev/api.json`.
- `GATEWAY_PRICING_SYNC_INTERVAL_SECONDS`
  Pricing-catalog sync interval in seconds.
- `GATEWAY_REDIS_ADDR`
  Optional Redis address for cross-instance sticky-session sharing.
- `GATEWAY_REDIS_PASSWORD`
  Optional Redis password.
- `GATEWAY_REDIS_DB`
  Redis logical DB index.
- `GATEWAY_REDIS_KEY_PREFIX`
  Redis key prefix.

### Direct server runtime guidance

For personal deployment, the simplest supported shape is a single long-running container:

- persist `/data` on a host directory or volume
- explicitly set `GATEWAY_STATE_PATH=/data/gateway.db`
- put Nginx, Caddy, or Traefik in front if you need public HTTPS
- upgrade by pulling a newer image and recreating the container with the same bind mount and environment

### Reverse proxy guidance

If you expose Conduit publicly, place it behind Nginx, Caddy, or Traefik and follow these rules:

- proxy `/v1/*` and `/healthz` to the backend
- add extra access control for `/api/admin/*`
- preserve long-lived connection settings for SSE and websocket traffic
- always terminate TLS properly

### Storage guidance

Conduit supports both SQLite and PostgreSQL:

- for personal single-node deployments, SQLite remains the default and simplest option
- use `GATEWAY_DATABASE_URL` when you want to plug Conduit into an existing PostgreSQL environment
- if you stay on SQLite, keep the database on durable storage and snapshot it before upgrades
- do not rely on container layers for long-term state

## How to use it

A typical usage flow is:

1. start the backend
2. create providers or integrations through the admin surface
3. configure routes and pricing profiles
4. create gateway keys
5. use Conduit as the single endpoint from your own clients and devices

For personal multi-device usage, a practical setup is:

- run one Conduit instance on your server
- issue separate gateway keys for different clients if needed
- use your own model aliases everywhere
- inspect request history when comparing usage across devices or tools

## API documentation policy

This README intentionally does not duplicate the full API reference.

Detailed API information should come from:

- `/api/admin/openapi.json` on a running instance
- source-level Go comments
- route registration and handler implementations

If you want to generate your own API reference or SDK, build it from the OpenAPI output and source comments rather than treating the README as a static API manual.

## Frontend note

Conduit is not intended to remain a backend-focused release forever. The frontend console remains part of the overall product direction and is meant to provide a more convenient way to manage providers, routes, integrations, gateway keys, and history.

The backend is published first because:

- it is the system foundation
- protocol compatibility and persistence need to stabilize first
- frontend work is still ongoing, not cancelled

## Development and testing

### Run unit tests locally

```bash
cd backend
go test ./...
```

### GitHub Actions

The repository includes a backend unit test workflow:

- file: `.github/workflows/backend-tests.yml`
- triggers: `push`, `pull_request`
- behavior: runs the full backend unit-test suite inside `backend/`
- file: `.github/workflows/backend-image.yml`
- triggers: `push main`, `push tag v*`, `pull_request`, `workflow_dispatch`
- behavior: builds the backend image and publishes it to GHCR on non-PR events

## Security and operational notes

- use a strong random `GATEWAY_ADMIN_TOKEN`
- never commit real upstream credentials
- protect the database file because it may contain sensitive runtime state
- do not expose `/api/admin/*` directly to untrusted networks
- review request history, probe results, and scheduler behavior regularly

## Version

The current public milestone is `v0.1.0`, focused on:

- establishing the backend structure
- stabilizing protocol compatibility layers
- stabilizing the persistence model
- ensuring backend unit tests run continuously

## License

No license file is currently included. Add an explicit license before broader redistribution.
