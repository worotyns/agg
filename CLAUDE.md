# agg — notes for Claude sessions

Start here: read `ARCHITECTURE.md` (components, ingestion, storage, security) and `README.md`.

- Scope of the core: custom-event collection only (simple JS SDK with retries + HTTP API; metadata helpers such as
  path, referrer, parsed User-Agent, optional IP; server-side privacy filter), aggregate engine, insights UI, alerts
  (Web Push, webhook), JSON values API (`/v1/values`), Prometheus/Grafana exports, MCP server.
  No third-party analytics or platform integrations: developers send the events they need.
  No on-site widgets / social proof rendering in the core — those are separate products built on the values API.
- Self-hosted first: single binary + SQLite (WAL); storage behind an interface for later adapters.
- Code, identifiers and repo docs in English.

## Implementation
- Go server (`cmd/agg`, `internal/*`), pure-Go SQLite, expr-lang expressions.
- UI: `web/ui` (Preact + htm, vendored, no build step); SDK: `web/sdk/agg.js` (plain ES5, no build); both embedded via `web/embed.go`.
- Licence: Elastic License 2.0. Author: Mateusz Worotynski <worotyns@icloud.com>. Public side project: no company names anywhere in the repo.
- Tests: `make test` (`go vet`, `go test ./...`, `node --test 'web/sdk/*.test.mjs'`). The Prometheus export tests in
  `internal/export` parse output with Prometheus' own parser and promlint — keep them passing when touching exports.
- Benchmarks: `go test ./internal/engine -run '^$' -bench .`; load test `scripts/loadtest` (see `BENCHMARKS.md`).
- Demo data: `node scripts/demo-traffic.mjs --url … --site pk_…`; landing page: `site/` (static).
