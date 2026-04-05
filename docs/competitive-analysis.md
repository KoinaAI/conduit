# Conduit Competitive Analysis & Optimization Roadmap

> Comparison of Conduit against five open-source AI gateway / proxy projects.
> Date: 2026-04-03

> Update: 2026-04-05
> This document started as a gap analysis snapshot. Since then, Conduit has already landed structured logging, routing decision tracing, stats APIs, request-history retention increase, scenario routing, public pricing sync, Codex turn-state compatibility, provider-level proxy support, log rotation, OTEL tracing, automatic backups, Redis-backed sticky session sharing, a helper CLI, PostgreSQL persistence support, and route transformer pipelines. Treat the "Current State" sections below as historical analysis, not as the current code truth.

## Compared Projects

| Project | Stack | Stars | Positioning |
|---------|-------|-------|-------------|
| **Conduit** | Go + SQLite | — | Self-hosted multi-protocol AI gateway |
| [Octopus](https://github.com/bestruirui/octopus) | Go (Gin) + SQLite/MySQL/PG | ~3k | LLM aggregation & load balancing for individuals |
| [Claude Code Hub](https://github.com/ding113/claude-code-hub) | Next.js + Hono + PG + Redis | ~2k | Session-aware AI proxy with team cost management |
| [CC Switch](https://github.com/farion1231/cc-switch) | Tauri + Rust + SQLite | ~1k | Desktop manager for 5 AI CLI tools |
| [AIO Coding Hub](https://github.com/dyndynjyxa/aio-coding-hub) | Tauri + Rust + SQLite | ~1k | Desktop gateway for Claude Code / Codex / Gemini CLI |
| [Claude Code Router](https://github.com/musistudio/claude-code-router) | Node.js (Fastify) + TS | ~1k | Scenario-based routing proxy with pluggable transformers |

---

## 1. Management UI

### Current State

Conduit has **no frontend UI**. All configuration is done via the REST admin API. The OpenAPI spec covers only 8 of 25 endpoints.

### Competitor Capabilities

| Capability | Octopus | Claude Code Hub | CC Switch | AIO Coding Hub | Claude Code Router |
|------------|---------|-----------------|-----------|----------------|-------------------|
| Web dashboard | Embedded in binary | Next.js full-stack | Tauri desktop | Tauri desktop | React + Vite |
| Provider CRUD | Yes | Yes | Yes | Yes | Yes |
| Route/model management | Yes (groups) | Yes | Yes (presets) | Yes (workspaces) | Yes (scenarios) |
| API key management | Yes | Yes | Local only | Local only | Env-based |
| Request log viewer | Yes (filterable) | Yes (filterable) | Yes (paginated) | Yes | Yes (debug page) |
| Real-time stats | Partial | Yes (live sessions) | Yes (charts) | Yes (heatmaps) | No |
| Dark mode / theming | Yes | Yes | Yes (system) | Yes | Yes |
| Multi-language (i18n) | No | 5 languages | 3 languages | 2 languages | No |

### Recommendations

1. **P0 — Embed a lightweight web UI into the Go binary.** Use Go's `embed` package to serve a React or Vue SPA at `/`. Octopus proves this works well in a single-binary Go project. Start with:
   - Provider & route configuration
   - Gateway key management
   - Request history viewer with filters
   - Basic stats overview (requests/day, tokens, cost)

2. **P1 — Complete the OpenAPI spec.** 17 of 25 endpoints are missing. This blocks third-party UI integration and code generation.

3. **P2 — Add i18n support.** At minimum Chinese and English, since the project README is bilingual.

---

## 2. Observability & Logging

### Current State

Conduit uses `log.Printf` for all logging. No structured log format. No external observability integration. No decision audit trail.

### Competitor Capabilities

| Capability | Octopus | Claude Code Hub | CC Switch | AIO Coding Hub | Claude Code Router |
|------------|---------|-----------------|-----------|----------------|-------------------|
| Structured logging | No | Pino (JSON) | Rust tracing | Rust tracing | pino + rotating-file |
| OTEL / tracing integration | No | OTEL + Langfuse | No | No | No |
| Decision chain audit | No | Yes (per-session) | No | No | No |
| Provider health dashboard | No | Yes | Yes (circuit status) | Yes | No |
| Log rotation / retention | No | No | No | No | Yes |

### Recommendations

1. **P0 — Switch to `log/slog` (Go 1.21+ structured logger).** Zero external dependencies. Output JSON in production, text in development. Add request ID, provider ID, route alias, and latency to every log line.

2. **P1 — Add a routing decision audit trail.** For each proxied request, record:
   - Candidates considered and their scores
   - Which candidate was selected and why
   - Circuit breaker state at decision time
   - Retry decisions and backoff durations
   Store as a JSON field on `RequestRecord`. Claude Code Hub's "Decision Chain" feature is a strong reference.

3. **P2 — Add OTEL trace exporter.** Expose spans for: auth → route resolution → upstream request → response translation → billing. Allow operators to ship traces to Jaeger/Grafana Tempo.

4. **P2 — Log rotation.** Support `GATEWAY_LOG_FILE` with size-based rotation, or defer to external log collection (stdout + Docker log driver).

---

## 3. Billing & Cost Management

### Current State

Conduit tracks per-key daily USD budget and per-minute RPM. Pricing profiles are manually configured. Statistics are limited to per-request records (max 200 retained).

### Competitor Capabilities

| Capability | Octopus | Claude Code Hub | CC Switch | AIO Coding Hub | Claude Code Router |
|------------|---------|-----------------|-----------|----------------|-------------------|
| Multi-window quotas (hourly/daily/weekly/monthly) | No | 5 windows (Redis Lua) | No | 4 windows | No |
| Auto pricing sync | models.dev | No | No | Built-in JSON | No |
| Cost analytics / leaderboard | Per key/channel/model | Per user + leaderboard | Per provider/model | Per CLI/vendor/model | No |
| Atomic cost tracking | DB batch | Redis Lua scripts | SQLite | SQLite | None |
| Per-key cost cap | No | Yes | No | Yes (rolling) | No |
| Token-level granularity | Yes | Yes | Yes | Yes | tiktoken local |

### Recommendations

1. **P1 — Add multi-window quota enforcement.** Support `hourly_budget_usd`, `weekly_budget_usd`, `monthly_budget_usd` on `GatewayKey` in addition to the existing `daily_budget_usd`. Use rolling windows (current timestamp - duration) rather than fixed calendar boundaries.

2. **P1 — Auto-sync model pricing.** Fetch pricing from a public source (e.g., `models.dev` API as Octopus does, or a bundled JSON file). Allow manual overrides per pricing profile. Run on a configurable schedule (e.g., daily).

3. **P1 — Aggregated statistics API.** Add endpoints:
   - `GET /api/admin/stats/summary` — total requests, tokens, cost, error rate (today / 7d / 30d)
   - `GET /api/admin/stats/by-key` — breakdown per gateway key
   - `GET /api/admin/stats/by-provider` — breakdown per provider
   - `GET /api/admin/stats/by-model` — breakdown per model alias
   Precompute daily rollups in a `stats_daily` SQLite table.

4. **P2 — Increase default request history retention.** 200 records is very low for production. Make it configurable with a higher default (e.g., 10,000) or add time-based retention (e.g., 30 days).

---

## 4. Routing Strategy

### Current State

Conduit uses a single fixed strategy: sort by priority → latency → weight, then try candidates in order with retry. Circuit breaker per provider+endpoint. Sticky sessions with TTL.

### Competitor Capabilities

| Capability | Octopus | Claude Code Hub | CC Switch | AIO Coding Hub | Claude Code Router |
|------------|---------|-----------------|-----------|----------------|-------------------|
| Round-robin | Yes | No | No | No | No |
| Random | Yes | No | No | No | No |
| Failover (ordered) | Yes | Yes | Yes | Yes | Yes |
| Weighted | Yes | Yes | No | No | No |
| Priority + weight + latency | No | Yes | No | No | No |
| Scenario-based routing | No | No | No | No | Yes (background/thinking/long-context/web-search) |
| Custom JS/expression router | No | No | No | No | Yes |
| Group affinity | No | Yes (user→group) | No | No | No |
| Latency-based scoring | Yes (delay scoring) | No | No | Yes (ping cache) | No |
| Session affinity | Yes (sticky iterator) | Yes (5min Redis) | No | Yes | No |

### Recommendations

1. **P1 — Make load-balancing mode configurable per route.** Add a `strategy` field to `ModelRoute`:
   - `priority-weight` (current behavior, default)
   - `round-robin`
   - `random`
   - `failover` (strict priority order, no weight randomization)
   Octopus's 4-mode approach is a good reference.

2. **P2 — Add scenario/tag-based routing.** Allow gateway keys or request headers to specify a "scenario" tag (e.g., `x-routing-scenario: background`). Define per-scenario route overrides:
   ```json
   {
     "alias": "claude-sonnet",
     "scenarios": {
       "background": { "targets": [...cheaper providers...] },
       "thinking": { "targets": [...reasoning-optimized providers...] }
     }
   }
   ```
   Claude Code Router's scenario dispatch is the reference here.

3. **P3 — Expose routing decisions via response headers.** Add headers like `X-Conduit-Provider`, `X-Conduit-Endpoint`, `X-Conduit-Latency-Ms` for client-side debugging. Several competitors expose similar metadata.

---

## 5. Security

### Current State

Conduit has gateway key auth (bcrypt + HMAC lookup), admin token auth, CORS control, SSRF protection (DNS pinning), credential permanent disable on 401/403. No brute-force protection.

### Competitor Capabilities

| Capability | Octopus | Claude Code Hub | CC Switch | AIO Coding Hub | Claude Code Router |
|------------|---------|-----------------|-----------|----------------|-------------------|
| Brute-force lockout | No | 20 fails / 300s → 600s lock | No | No | No |
| API key negative cache | No | Yes (Vacuum Filter) | No | No | No |
| Config export sanitization | No | No | No | No | Yes (auto-redact secrets) |
| SOCKS proxy per provider | No | Yes | No | No | No |
| Extended Thinking signature verification | No | No | No | Yes | No |
| Rate limit by IP | No | No | No | No | No |

### Recommendations

1. **P1 — Add brute-force protection.** Track failed authentication attempts per source IP (or per key prefix). After N failures in T seconds, reject all attempts from that source for a cooldown period. Claude Code Hub's 20/300s→600s policy is a reasonable starting point. Store counters in memory (no persistence needed).

2. **P1 — Add config export sanitization.** The `GET /api/admin/state` endpoint returns provider API keys in plain text. Add a `?redact=true` query parameter (default true) that replaces sensitive fields with masked previews. Claude Code Router's preset sharing with automatic sensitive-data redaction is the reference.

3. **P2 — Add API key negative cache.** After a gateway key fails authentication, cache the lookup hash → "invalid" mapping in memory for a short TTL (e.g., 60s). This avoids repeated bcrypt comparisons for the same invalid key, reducing CPU cost under attack. Claude Code Hub calls this the "Vacuum Filter."

4. **P3 — Support per-provider outbound proxy.** Allow configuring an HTTP/SOCKS proxy per provider for environments where upstream access requires a proxy. Add `ProxyURL` field to `Provider`.

---

## 6. Protocol Translation

### Current State

After PR #6, Conduit supports bidirectional translation between OpenAI Chat, Anthropic Messages, and Gemini protocols — including tool calls, images, and multimodal content. This is a strength.

### Competitor Capabilities

| Capability | Octopus | Claude Code Hub | CC Switch | AIO Coding Hub | Claude Code Router |
|------------|---------|-----------------|-----------|----------------|-------------------|
| OpenAI Chat | Yes | Yes | Yes | Yes | Yes |
| OpenAI Responses | Yes | No | No | No | No |
| Anthropic Messages | Yes | Yes | No | No | No |
| Gemini | Yes | Yes | Via rectifier | Yes | No |
| Codex CLI | No | Yes | Yes | Yes | Yes |
| Tool call translation | Partial | No (passthrough) | Via rectifier | No | Via transformers |
| Image/multimodal translation | No | No | No | No | No |
| Pluggable transformers | No | No | No | No | Yes |
| Local token counting | No | No | No | Yes | Yes (tiktoken) |

### Recommendations

1. **P1 — Add Codex CLI protocol support.** Three of five competitors support it. Codex uses a slightly different OpenAI-compatible format. Add a `/v1/responses`-compatible path that handles Codex-specific headers and response format.

2. **P2 — Add local token counting as a fallback.** When upstream doesn't return `usage` fields (some providers omit them for streaming), estimate token counts locally. Go libraries like `tiktoken-go` can provide this. This prevents zero-cost billing for requests where usage data is lost (current Bug #12).

3. **P2 — Consider a pluggable transformer pipeline.** Claude Code Router's approach of user-defined request/response transformers is powerful for edge cases (e.g., injecting system prompts, rewriting model names, stripping fields). This could be implemented as a chain of Go plugins or embedded Lua/expr scripts on each route.

---

## 7. Session & Context Management

### Current State

Conduit has sticky sessions (binding a session ID to a provider+endpoint+credential for a configurable TTL). No session context caching or session history.

### Competitor Capabilities

| Capability | Octopus | Claude Code Hub | CC Switch | AIO Coding Hub | Claude Code Router |
|------------|---------|-----------------|-----------|----------------|-------------------|
| Sticky sessions | Yes | Yes | No | Yes | No |
| Context cache (reuse provider within time window) | No | 5-min Redis cache | No | No | No |
| Session history browser | No | No | Yes | Yes | No |
| Conversation tracking | No | Yes (per-session) | Yes | Yes | No |

### Recommendations

1. **P2 — Enrich sticky session metadata.** Expose session binding info in response headers (`X-Conduit-Session-Id`, `X-Conduit-Session-Provider`). Allow clients to explicitly start/end sessions via headers.

2. **P3 — Optional Redis session cache.** For high-concurrency deployments, allow offloading sticky session state to Redis. This enables horizontal scaling (multiple gateway instances sharing session state).

---

## 8. Deployment & Operations

### Current State

Conduit ships as a single Go binary with Docker support (multi-stage build, non-root user). SQLite only. No auto-backup. No embedded frontend.

### Competitor Capabilities

| Capability | Octopus | Claude Code Hub | CC Switch | AIO Coding Hub | Claude Code Router |
|------------|---------|-----------------|-----------|----------------|-------------------|
| Single binary with embedded UI | Yes | No (needs Node) | Yes (Tauri) | Yes (Tauri) | No (needs Node) |
| Multiple DB backends | SQLite/MySQL/PG | PostgreSQL | SQLite | SQLite | None |
| Redis support | No | Yes | No | No | No |
| Cloud sync | No | No | Dropbox/OneDrive/iCloud/WebDAV | No | No |
| Auto-backup rotation | No | No | Yes (10 copies) | No | No |
| Fail-open degradation | No | Yes (Redis down → continue) | No | No | No |
| Health check endpoint | No | Yes | No | No | Yes |
| Helm / K8s manifests | No | No | N/A | N/A | No |

### Recommendations

1. **P1 — Embed the frontend in the Go binary.** Use `go:embed` to include the built SPA assets. Serve at `/` alongside the existing API. Octopus is a good reference for this pattern.

2. **P1 — Add a proper health check.** The existing `GET /healthz` should return structured JSON: `{"status": "ok", "uptime_seconds": ..., "db_status": "ok", "active_keys": N}`. Include DB connectivity check.

3. **P2 — Support PostgreSQL as an alternative backend.** For teams running Conduit at scale, SQLite's single-writer limitation becomes a bottleneck. Abstract the store interface and add a PG implementation. Octopus's multi-DB approach (GORM) is a reference.

4. **P2 — Add automatic state backup.** Periodically snapshot the SQLite database (or JSON state) to a configurable path with rotation (keep last N backups). CC Switch keeps 10 rotating backups.

5. **P3 — Add Docker Compose example.** The old `compose.yaml` was deleted. Provide an example with Conduit + optional PG + optional Redis for production deployments.

---

## 9. CLI Tool Ecosystem Integration

### Current State

Conduit is a pure network gateway. It does not manage CLI tool configurations, MCP servers, prompts, skills, or workspaces.

### Competitor Capabilities

| Capability | CC Switch | AIO Coding Hub | Claude Code Router |
|------------|-----------|----------------|-------------------|
| Multi-CLI tool management | 5 tools | 3 tools | 1 tool |
| MCP server management | Unified across tools | Per-workspace | No |
| Prompt/skill management | Bidirectional sync + templates | Per-workspace + marketplace | Presets |
| Workspace isolation | No | Per-project | No |
| Skill marketplace | GitHub discovery | Git discovery | No |
| Config deep link import | `ccswitch://` URLs | No | No |

### Recommendations

This is a **positioning difference**, not necessarily a gap. Conduit is a server-side gateway; CC Switch and AIO Coding Hub are desktop tools. However:

1. **P3 — Consider an optional companion CLI tool** (`conduit-cli`) that can:
   - Configure Claude Code / Codex to point at the Conduit gateway
   - Generate and install gateway keys
   - Show real-time request stats from the terminal
   This would lower the barrier to entry for individual users.

---

## 10. Remaining Known Bugs

Two low-severity bugs from the security audit remain unresolved:

| # | Severity | Description | File |
|---|----------|-------------|------|
| 1 | Low | `credentialRuntimeKey` computes SHA-256 on every request; old entries never evicted from `r.credentials` map | `gateway/runtime.go:267-269` |
| 2 | Low | SSE passthrough path silently discards malformed JSON lines in `ObserveLine`, losing usage/billing data | `gateway/usage.go:54` |

### Recommendations

1. **Bug #1 fix:** Remove the API key hash from the runtime key. `providerID + credentialID` is already unique. Add periodic eviction of stale credential state entries (similar to the sticky session sweep).

2. **Bug #2 fix:** In `ObserveLine`, when `json.Unmarshal` fails, log a warning with the raw line content. Consider accumulating partial JSON across consecutive lines for providers that split JSON across SSE boundaries.

---

## Priority Summary

| Priority | Items | Theme |
|----------|-------|-------|
| **P0** | Embedded web UI, structured logging (`slog`) | Usability & operability baseline |
| **P1** | OpenAPI spec completion, multi-window quotas, auto pricing sync, stats API, configurable LB modes, brute-force protection, config export sanitization, Codex protocol, health check, embed frontend | Feature parity with leading competitors |
| **P2** | OTEL integration, decision audit trail, local token counting, pluggable transformers, PG backend, Redis session option, auto-backup, request history retention, scenario routing, negative key cache, per-provider proxy, log rotation | Differentiation & scale |
| **P3** | Companion CLI tool, expose routing headers, K8s manifests, Docker Compose example, i18n | Nice-to-have polish |
