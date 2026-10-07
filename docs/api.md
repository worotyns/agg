# HTTP API

## Public (site key, CORS `*`)

### `POST /e`: ingest

Body (`text/plain` or `application/json`, ≤ 256 KB, ≤ 100 events):

```json
{ "site": "pk_…", "events": [
  { "name": "purchase", "ts": 1791380000000, "id": "order-1", "visitorId": "…", "props": { "value": 120, "items": [] },
    "meta": { "path": "/thank-you", "referrer": "news.example.com", "language": "en-US" } }
] }
```

Only `name` is required. `meta` accepts `path`, `referrer` and `language`; the server adds `browser`, `os`, `device`
(from the User-Agent header), `bot` and, if enabled for the site, `ip`.

Returns `202 {"accepted": 1, "dropped": 0}`; `404` unknown site, `403` origin not allowed, `429` rate limited.

### `GET /v1/values`: values

```
GET /v1/values?site=pk_…&v=purchases_24h,product_viewers_5m,aov&product=73
```

```json
{ "values": { "purchases_24h": 5, "product_viewers_5m": "10+", "aov": null }, "dimensions": { "product": "73" }, "unknown": ["aov"] }
```

- Only aggregates and formulas marked **public** are returned; anything else is `null` and listed in `unknown`.
- `public_bucketed` values are rounded: 0–9 exact, then `"10+"`, `"100+"`, `"1000+"`…
- Extra query parameters are dimension values (`product=73`) and select one group of grouped aggregates.
- `null` means "no value". Show nothing then, never a placeholder number.
- `Cache-Control: public, max-age=30`, plus a 10-second server cache.

### `GET /v1/top`: top lists

```
GET /v1/top?site=pk_…&aggregate=bestsellers&window=24h&limit=10&category=shoes
```

```json
{ "aggregate": "bestsellers", "window": "24h", "by": "product", "items": [ { "key": "73", "label": "Trail shoe", "value": 41 } ] }
```

`by` chooses the dimension (default: the rank dimension, else the group dimension). For `last_timestamp` aggregates
the items are the most recent groups and `value` is a unix time.

### `GET /v1/config?site=pk_…`

Tracking settings for the SDK. `GET /agg.js` serves the SDK. `GET /healthz` returns `ok`.

## Exports (export token)

`GET /export/<id>/metrics` with `Authorization: Bearer <token>` or `?token=`. See
[prometheus-grafana.md](prometheus-grafana.md).

## Admin API

Everything the UI does is available under `/api` with `Authorization: Bearer <token>`: the admin token or a named
API token (API & MCP in the UI; revocable). The UI itself uses a session cookie. JSON in, JSON out.

| | |
|---|---|
| `GET/POST /api/sites`, `GET/PUT/DELETE /api/sites/{site}` | sites and tracking settings |
| `GET/POST /api/sites/{site}/aggregates`, `GET/PUT/DELETE …/aggregates/{id}` | aggregates |
| `POST …/aggregates/{id}/reset`, `POST …/aggregates/{id}/rebuild` | reset / rebuild from raw events |
| `GET …/aggregates/{id}/values?product=73` | all windows with previous values and change |
| `GET …/aggregates/{id}/top?window=24h&by=product&limit=10` | top list |
| `GET …/aggregates/{id}/series?range=24h` | series (`1h` by minute, `24h`/`7d` by hour, `30d`/`90d` by day) |
| `POST /api/sites/{site}/dry-run` | `{ def, events? }`: run a definition on events (or recent stored ones) without saving |
| `GET …/events?limit=50&name=purchase&visitor=…&before=<rawId>`, `GET …/event-names` | recent raw events, newest first (limit ≤ 1000; `before` pages to older events), counts per name (24 h) |
| `GET/POST …/formulas`, `PUT/DELETE …/formulas/{id}`, `POST …/formulas/preview` | formulas |
| `GET/POST …/exports`, `PUT/DELETE …/exports/{id}`, `POST …/exports/{id}/rotate` | export endpoints |
| `GET …/insights?window=24h` | pinned tiles, top lists and formulas |
| `GET …/variables`, `GET …/values?v=…&public=1` | variable list; values as admin (or as the public endpoint) |
| `GET /api/presets`, `POST …/presets/{id}` | presets; apply one to a site (`{ "timezone": "Europe/Warsaw" }`) |
| `GET/POST …/alerts`, `PUT/DELETE …/alerts/{id}`, `GET …/alerts/{id}/events`, `POST …/alerts/{id}/test`, `POST …/alerts/check` | alerts, history, test notification, evaluate a condition now |
| `GET /api/notifications`, `POST /api/notifications/read` | in-app notifications |
| `GET /api/push`, `POST/DELETE /api/push/subscriptions`, `POST /api/push/test` | Web Push key and subscribed browsers |
| `GET/POST /api/tokens`, `DELETE /api/tokens/{id}` | API tokens |

The MCP server is on `POST /mcp` (see [mcp.md](mcp.md)); `/llms.txt` describes agg for language models.

Example:

```sh
curl -H "Authorization: Bearer $AGG_ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"purchases","title":"Purchases","events":["purchase"],"op":"count","visibility":"public"}' \
  http://localhost:8080/api/sites/1/aggregates
```
