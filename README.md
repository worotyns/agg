# agg

Simple analytics and product insights for your project. Self-hosted, one binary, SQLite.

Website: [agg.worotyns.ovh](https://agg.worotyns.ovh) · Source: [github.com/worotyns/agg](https://github.com/worotyns/agg)

- **Website analytics out of the box:** page views, visitors, top pages and referrers, over the last hour, day,
  week or month, compared with the period before.
- **Product insights you define:** feature usage, sign-ups per plan, active users, purchases per product,
  bestseller per category: count, sum, distinct count, last value or last time, grouped by any field, over sliding
  windows from 5 minutes to 30 days.
- **Collect** with a 3 KB script: automatic page views and your own events, `agg.track('sign_up', { plan: 'pro' })`.
  Batched, retried when offline, enriched with page, browser, OS and device. No cookies; obvious personal data
  (emails, phone numbers…) is stripped on the server.
- **Look** at it in a simple built-in UI, or in Grafana through a Prometheus export. **Read** values as JSON
  (`/v1/values`, `/v1/top`) from your site or another app.
- **Get alerted** when something happens or stops happening: browser push notifications (no external service),
  in-app notifications and webhooks (Slack, Discord, Mattermost).
- **Ask your AI assistant**: the built-in MCP server lets Claude and other assistants plan and apply the setup and read
  the numbers. Presets for websites, shops, SaaS and blogs give you a working setup in one click.

It is not a full analytics suite (no sessions, funnels or user profiles). It computes the numbers you define, exactly,
as events arrive.

## Setup: simple website analytics in 5 minutes

**1. Run the server** (Go 1.27+):

```sh
git clone https://github.com/worotyns/agg && cd agg
go build -o agg ./cmd/agg
./agg serve
```

or with the published image (linux/amd64 and arm64):

```sh
docker run -d --name agg -p 8080:8080 -v agg-data:/data ghcr.io/worotyns/agg:latest
docker logs agg                 # shows the admin token
```

or from source with Docker / Podman Compose:

```sh
docker compose up -d agg        # or: podman compose up -d agg
docker compose logs agg         # shows the admin token
```

The first start prints an **admin token**:

```
  Admin token (shown once, keep it safe): agg_admin_…
```

**2. Create a site.** Open <http://localhost:8080>, log in with the token and add a site (e.g. "My blog"). Pick a
preset; **Website analytics** gives you:

| | |
|---|---|
| `page_views` | every `page_view` event |
| `visitors` | distinct visitors (random id in `localStorage`, no cookies) |
| `pages` | page views per path: your top pages |
| `referrers` | page views per external referrer host |
| `views_per_visitor` | formula |
| `no_traffic` | alert: no page views for 6 hours (after the site had traffic) |

It also counts visitors per browser and device. Other presets add to it, each with the `agg.track()` calls to put
in your code: **Online shop** (purchases, revenue, products, bestsellers per category, conversion, no-orders and
revenue-drop alerts), **SaaS / product** (sign-ups per plan, active users, feature usage) and **Blog / publisher**
(most read articles, readers per section). Settings → Presets adds one to an existing site.

**3. Add the snippet** from **Settings → Install** to every page, before `</head>`:

```html
<script async src="https://agg.example.com/agg.js" data-site="pk_…"></script>
```

**4. Look.** Within seconds the first `page_view` shows up under **Events**. **Insights** shows page views and
visitors with a trend against the previous period, plus the **top 10 pages** and referrers; switch between 1h, 24h,
7d and 30d. Click a tile for the history chart and all windows.

**5. (Optional) Use the numbers elsewhere.** The admin API gives you the top 10 pages as JSON (`3` is the id of the
`pages` aggregate, shown in its URL in the UI):

```sh
curl -H "Authorization: Bearer $AGG_ADMIN_TOKEN" \
  "http://localhost:8080/api/sites/1/aggregates/3/top?window=24h&limit=10"
```

or mark the `pages` aggregate **public** (Aggregates → Pages → Edit → Values API visibility) and read it without a
token, e.g. to show "most read" on the site itself:

```
GET /v1/top?site=pk_…&aggregate=pages&window=7d&limit=10
GET /v1/values?site=pk_…&v=page_views_24h,visitors_24h
```

### Try it without a website

```sh
# 200 visits of a fake shop: page views, product views, add to cart, purchases
node scripts/demo-traffic.mjs --url http://localhost:8080 --site pk_… --visitors 200
# a few visits per second, until Ctrl+C
node scripts/demo-traffic.mjs --url http://localhost:8080 --site pk_… --live
```

or open `examples/shop.html?server=http://localhost:8080&site=pk_…` in a browser and click the buttons: it sends
`product_view`, `add_to_cart`, `purchase` and `feature_used` with the real SDK.

## Next: product insights

Send events from your app with `agg.track()`:

```js
agg.track('feature_used', { feature: 'export_pdf' });
agg.track('sign_up', { plan: 'pro' });
```

Then **Aggregates → New aggregate** and pick a template: **Feature usage** (count per feature), **Users per feature**
(distinct users per feature), **Active users**, **Sign-ups per plan**, **Visitors per browser**, or for a shop
**Purchases**, **Revenue**, **Purchases per product**, **Bestseller per category**, **Last purchase time**, **Visitors
viewing a product**. Expressions read what you sent (`props.plan`) and what agg added (`meta.path`,
`meta.device`, `meta.browser`…). The **Tester** next to the form runs the definition on your real recent events before
you save, so you see what will be counted.

Combine values with **Formulas**, e.g. `revenue_24h / purchases_24h` (average order value) or
`signups_24h / visitors_24h * 100` (sign-up rate).

## Alerts

**Alerts → Enable on this device** turns on browser notifications (repeat on each device, phone included), then
**New alert**, e.g. `now - last_purchase > 3 * 3600` (no order for 3 hours) or
`revenue_prev_24h > 0 && revenue_24h < 0.5 * revenue_prev_24h`. Alerts are checked every minute, support
"for", cooldown and quiet hours, and can also post to a Slack / Discord / Mattermost webhook. Browser notifications
need HTTPS (or localhost). See [docs/alerts.md](docs/alerts.md).

## AI assistants (MCP)

**API & MCP → Create token**, then:

```sh
claude mcp add --transport http agg http://localhost:8080/mcp --header "Authorization: Bearer agg_api_…"
```

Ask "plan analytics for my SaaS" or "set up my shop with bestsellers per category and an alert when there are no
orders". The assistant uses presets, checks what your site really sends, dry-runs aggregates on real events and asks
before deleting. Tokens are named and revocable and cannot log in to the UI. See [docs/mcp.md](docs/mcp.md);
[`/llms.txt`](site/llms.txt) describes agg for language models.

## Prometheus and Grafana

Create an export under **Exports**. You get a URL, a token and a ready `prometheus.yml` snippet. `docker-compose.yml`
runs agg, Prometheus and Grafana with a provisioned dashboard. See [docs/prometheus-grafana.md](docs/prometheus-grafana.md).

## Configuration

Flags or environment variables:

| Flag | Env | Default | |
|---|---|---|---|
| `-addr` | `AGG_ADDR` | `:8080` | listen address |
| `-db` | `AGG_DB` | `agg.db` | SQLite file (WAL mode; also `-wal`, `-shm` files) |
| `-admin-token` | `AGG_ADMIN_TOKEN` | generated | admin token; if empty, one is generated on first start and printed once |
| `-public-url` | `AGG_PUBLIC_URL` | from request | public URL, used in snippets and scrape configs |
| `-raw-retention-days` | `AGG_RAW_RETENTION_DAYS` | `7` | days of raw events kept for rebuilds and the tester |
| `-max-keys-per-aggregate` | `AGG_MAX_KEYS_PER_AGGREGATE` | `50000` | cap on distinct group values per aggregate |
| `-trust-proxy` | `AGG_TRUST_PROXY` | off | use `X-Forwarded-For` / `X-Forwarded-Proto` from your reverse proxy |
| `-internal-metrics-token` | `AGG_INTERNAL_METRICS_TOKEN` | off | enables `/internal/metrics` (ingest rate, flush time, DB size) with this bearer token |
| `-vapid-subject` | `AGG_VAPID_SUBJECT` | public URL | contact sent to Web Push services (`mailto:` or `https:`) |

Access: the admin token (random, printed once on first start) logs in to the UI. Programs and AI assistants use
named API tokens (API & MCP), which can be revoked one by one. Lost the admin token?
`agg reset-admin-token -db agg.db` prints a new one and logs out all sessions.

### Running in production

- Put it behind a reverse proxy with TLS (Caddy, nginx) and set `AGG_PUBLIC_URL` and `AGG_TRUST_PROXY=1`.
- Set **Allowed origins** for each site: the site key is public by design.
- Back up the SQLite file with [Litestream](https://litestream.io) (continuous, to S3-compatible storage) or
  `sqlite3 agg.db ".backup backup.db"`. Do not copy the file while the server is writing.
- Retention: minute buckets 49 h, hour buckets 15 days, day buckets 400 days, raw events 7 days (configurable).

## Documentation

- [Aggregates](docs/aggregates.md): events, where, explode, operations, group by, windows, formulas, rebuilds.
- [Browser SDK](docs/sdk.md): snippet, `agg.track()`, page views, privacy filter, consent.
- [HTTP API](docs/api.md): ingest, values API, top lists, admin API.
- [Alerts](docs/alerts.md): conditions, states, browser push, webhooks.
- [MCP](docs/mcp.md): connecting AI assistants, tools.
- [Prometheus and Grafana](docs/prometheus-grafana.md): export endpoints, metrics, PromQL examples.
- [Architecture](ARCHITECTURE.md): components, ingestion and flush, storage, security.
- [Benchmarks](BENCHMARKS.md): 1M-event load test, throughput, consistency, read latency.

## Development

```sh
make test                # go vet, Go tests, browser SDK tests
go run ./cmd/agg serve  # http://localhost:8080
go test ./internal/engine -run '^$' -bench .   # engine benchmarks
```

The code layout and how the pieces fit together are described in [ARCHITECTURE.md](ARCHITECTURE.md); the load test
in [BENCHMARKS.md](BENCHMARKS.md).

## License

[Elastic License 2.0](LICENSE): free to use, modify and self-host, including commercially; you may not offer it to
others as a hosted or managed service.
