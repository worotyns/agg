# Architecture

agg is one Go binary with an embedded SQLite database. Websites send custom events; agg keeps running aggregates
over sliding windows (counts, sums, distinct counts, last values, top-N per dimension) and serves them to a web UI,
a JSON values API, Prometheus, alerts and an MCP server for AI assistants.

```
 browser / backend                         agg (single binary)
┌──────────────┐  POST /e (batched)   ┌──────────────────────────────────────────────┐
│  agg.js SDK │ ───────────────────▶ │ server ─▶ engine ─▶ in-memory batch           │
│  track()     │                      │              │           │ flush every 1 s     │
│  page views  │                      │              ▼           ▼                     │
└──────────────┘                      │        rules (expr)   store (SQLite, WAL)      │
                                      │                          ▲                     │
 UI, values API, Prometheus, MCP ◀─── │ server ─▶ query ─────────┘                     │
 Web Push, webhooks             ◀─── │ alert (every minute) ─▶ query                   │
                                      └──────────────────────────────────────────────┘
```

## Repository layout

| Path | What it is |
|---|---|
| `cmd/agg` | entry point: `serve`, `reset-admin-token`, `version`; flags and environment variables |
| `internal/server` | HTTP layer: ingest, public values API, admin API, auth, exports, MCP, the embedded UI |
| `internal/engine` | ingestion: validation, enrichment, rule evaluation, guards, buffering and flushing |
| `internal/query` | reads: window values, previous windows, variables, formulas, top-N, series |
| `internal/store` | the `Storage` interface and its SQLite implementation (schema, migrations, retention) |
| `internal/model` | domain types shared by all packages: sites, aggregates, formulas, windows, granularities |
| `internal/alert` | alert evaluation loop, state machine (ok → pending → firing), notifications |
| `internal/webpush` | Web Push without dependencies: VAPID (RFC 8292) and payload encryption (RFC 8291) |
| `internal/export` | Prometheus text format and JSON exports |
| `internal/preset` | ready-made setups (website, shop, saas, publisher) |
| `internal/geo` | optional country/city lookup of the client IP through an external GeoIP service (cache, circuit breaker) |
| `internal/privacy` | server-side removal of personal-data property names |
| `internal/testutil` | real store + engine + querier with a controllable clock, for tests |
| `web/sdk` | `agg.js`, the browser SDK (plain ES5, no build step) and its tests |
| `web/ui` | the admin UI: Preact + htm, vendored, no build step |
| `web/embed.go` | embeds the SDK and the UI into the binary |
| `docs/` | user documentation: SDK, aggregates, alerts, API, MCP, Prometheus/Grafana |
| `deploy/` | example Prometheus and Grafana configuration |
| `site/` | static landing page |
| `scripts/` | demo traffic generator and the load test (see [BENCHMARKS.md](BENCHMARKS.md)) |

## Domain model

- **Site**: one website or app. Has a public key (`pk_…`) used by the SDK, allowed origins and tracking settings
  (automatic page views, visitor id, IP collection, consent mode, extra blocked fields).
- **Aggregate**: a named definition evaluated on every matching event:
  - `events`: event names it listens to; `where`: an optional filter expression;
  - `explode`: an optional array path; each element becomes `item` (e.g. order lines);
  - `groupBy` / `rankBy`: optional dimensions (`{dimension, expr, label}`), for top-N lists and per-value numbers;
  - `op`: `count`, `sum` (of `value`), `avg`, `min`, `max`, `p50`, `p95`, `p99` (of `value`), `count_distinct` (of `value`, default the visitor id), `last_value`,
    `last_timestamp`;
  - `visibility`: `private`, `public` or `public_bucketed` (rounded, e.g. "100+") for the public values API.
- **Window**: `5m`, `1h`, `6h`, `24h` (minute buckets), `7d` (hour buckets), `30d` (day buckets). Every window also has
  a *previous* window of the same length, for trends.
- **Variable**: `<aggregate>_<window>`, `<aggregate>_prev_<window>`, `<aggregate>_total`, or `<aggregate>` for last
  values. Formulas, alerts, exports and the values API all read variables.
- **Formula**: an expression over variables, evaluated at read time (`revenue_24h / purchases_24h`). Stores nothing.
- **Alert**: a condition over variables, checked every minute, with an optional "for N minutes", schedule and
  channels (in-app, Web Push, webhook).
- **Export**: a token-protected Prometheus or JSON endpoint with a chosen set of variables.

Expressions (filters, values, dimensions, formulas, alert conditions) use
[expr-lang/expr](https://github.com/expr-lang/expr): sandboxed, compiled once, and unknown names fail at compile time.

## Ingestion

```
POST /e
  → per-IP rate limit (50 req/s, burst 200), size limits, Origin check against the site's allowed origins
  → engine.Ingest, per event:
      normalize the name, drop oversized props, remove blocked property names (privacy filter)
      add meta: path, referrer, language from the client; browser, os, device, bot from the User-Agent; country (and city) from the IP if `AGG_GEOIP_URL` is set and the site allows it; ip if enabled
      drop automatic page views from bots
      per-visitor cap (300 events/min), dedupe by event id (48 h)
      for each aggregate listening to the event name: where → explode → groupBy/rankBy → value → delta
      cap distinct group values per aggregate (default 50,000)
  → deltas and the raw event go into one in-memory Batch (under a mutex)
  → HTTP 200 {accepted, dropped}
```

The SDK (`agg.js`) keeps a queue in `localStorage`, sends batches of up to 50 events / 60 KB, retries network errors,
429 and 5xx with exponential backoff, and hands the queue to `sendBeacon` when the page is hidden. Events are sent as
`text/plain` so no CORS preflight is needed.

### Flush

A loop swaps the in-memory `Batch` for an empty one every second and writes it in **one SQLite transaction** through the
single writer connection: bucket counters (upserts), distinct members, last values, totals, labels, dedupe keys and raw
events. If the write fails, the batch is merged back and retried on the next tick. `Close` flushes what is left, so a
normal shutdown loses nothing.

Requests never wait for SQLite; they only take the engine mutex. The consequence is that accepted events become
queryable up to one flush later (≈1 s at normal load; see [BENCHMARKS.md](BENCHMARKS.md) for overload behaviour).

## Storage

SQLite in WAL mode via the pure-Go driver `modernc.org/sqlite` (no cgo): one writer connection, a pool of eight
read-only connections. Everything sits behind the `store.Storage` interface so another backend can be added later.

| Table | Holds |
|---|---|
| `buckets` | count, sum, min and max per aggregate × granularity × group × member × time bucket |
| `hist` | log-scaled histogram bins (≈2.5% error) per bucket, for `p50`/`p95`/`p99` |
| `distinct_members` | member hashes per aggregate × granularity × group × bucket (exact distinct counts) |
| `totals` | all-time count and sum per aggregate and group |
| `last_values` | latest value and time per aggregate and group |
| `labels` | display labels for group keys (e.g. product id → name) |
| `matched` | events matched per aggregate per hour, for diagnostics |
| `dedupe` | event ids seen, with expiry |
| `events_raw` | raw events, kept 7 days by default: live event view, tester, rebuilds |
| `sites`, `aggregates`, `formulas`, `exports`, `alerts`, `alert_events`, `notifications`, `push_subscriptions`, `api_tokens`, `settings` | configuration and state |

Retention: minute buckets 49 h (exact 24 h windows and their previous windows), hour buckets 15 days, day buckets
400 days, raw events 7 days. A cleanup job runs every 5 minutes.

A window value is the sum (or distinct count) of the buckets in its range, so values are exact and windows slide
smoothly. **Rebuild** replays raw events through a changed definition; **reset** clears an aggregate's data.

## Reads

`internal/query` turns a request into bucket ranges, reads them and applies formulas. It serves:

- the admin API (`/api/...`) used by the UI;
- the public values API (`GET /v1/values`, `GET /v1/top`): only public aggregates, cached (`Cache-Control: max-age=30`);
- exports (`/export/{id}/metrics`): Prometheus text format 0.0.4 or JSON, token-protected;
- the MCP server (`POST /mcp`): JSON-RPC tools to inspect events and manage aggregates, formulas and alerts.

## Alerts

`internal/alert` evaluates each enabled alert every minute: it builds the variables the condition needs, runs the
expression, and moves the alert through `ok → pending → firing → ok` (honouring "for N minutes" and schedules).
Transitions create in-app notifications and are sent to subscribed browsers (Web Push, VAPID keys generated on first
start) and to an optional webhook (Slack/Mattermost/Discord compatible).

## Authentication and security

- **Ingest** uses the public site key; restrict a site to its origins in production.
- **Admin**: one admin token (hashed in the database; printed once on first start or set with `AGG_ADMIN_TOKEN`).
  The UI exchanges it for an HMAC-signed, `HttpOnly`, `SameSite=Strict` session cookie bound to the token hash, so
  changing the token logs everyone out. Cookie-authenticated writes must come from the same origin.
- **API tokens**: named, revocable bearer tokens for programs and the MCP server.
- **Brute-force protection**: wrong admin/API tokens are limited to 5 attempts per minute per client IP, shared across
  UI login, Bearer requests and MCP. A blocked client gets HTTP 429 with `Retry-After`, even with a correct token.
- **Privacy**: property names such as `email`, `phone`, `password`, `card_number` are removed on the server at any depth;
  IP addresses are stored only when a site enables it; with GeoIP on, the IP is sent to the configured lookup service and only the country code (and city, if the site chose it) is kept; the visitor id is random and kept in `localStorage`.
- Behind a reverse proxy, run with `-trust-proxy` so the client IP comes from `X-Forwarded-For`, or better with
  `-client-ip-header` (e.g. `Fly-Client-IP`, `CF-Connecting-IP`): a header the proxy always overwrites, unlike the
  first `X-Forwarded-For` entry, which a client can forge.

## UI

A single-page app in `web/ui` (Preact + htm, no build step), served at `/` and embedded in the binary. Pages: Insights
(pinned tiles and top lists), Events (live stream, and an audit log of stored raw events with filters by name and
visitor, paging and JSON download), Aggregates (editor with a tester against recent events), Formulas, Alerts, Exports,
Values API, Tracking guide (the JavaScript API and event design, with the site's key filled in), Settings, and API & MCP
tokens.

## Testing

`make test` runs `go vet`, the Go tests and the SDK tests (`node --test`). Tests use a real SQLite database and a
controllable clock (`internal/testutil`). The Prometheus export tests parse the output with Prometheus' own parser and
`promlint`. Benchmarks and the load test are described in [BENCHMARKS.md](BENCHMARKS.md).
