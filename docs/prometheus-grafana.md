# Prometheus and Grafana

agg keeps the exact per-entity state (per product, per page) itself. Prometheus is an export target: it scrapes a
bounded selection of current values and keeps their history, so you can graph them in Grafana and alert on them with
Alertmanager.

## Export endpoints

Exports → New export:

- **Format**: Prometheus text format (0.0.4) or JSON.
- **Scope**: aggregates, formulas and windows to include.
- **Per-dimension series**: none (totals only), top K values per window, or an allowlist (e.g. ten key products).
  The editor shows the number of series per scrape before you save.
- **Cache**: seconds a rendered response is reused (default 15).

You get a URL `/export/<id>/metrics` and a token, shown once and stored hashed. Tokens can be rotated (the old one
stops working immediately) and revoked by deleting the export. Exports are read-only; the export token gives access
to nothing else.

```yaml
scrape_configs:
  - job_name: agg
    scrape_interval: 60s
    scheme: https
    metrics_path: /export/<id>/metrics
    authorization:
      type: Bearer
      credentials: <token>        # or credentials_file: /etc/prometheus/agg_token
    static_configs:
      - targets: ['agg.example.com']
```

## Metrics

| Metric | Type | Labels | |
|---|---|---|---|
| `agg_value` | gauge | `site, aggregate, window` | window value of count / sum / avg / min / max / percentile / count_distinct aggregates |
| `agg_dimension_value` | gauge | `site, aggregate, window, <dimension>` | the same per group value (top K / allowlist only) |
| `agg_events_total` | counter | `site, aggregate` | all-time count of `count` aggregates (since creation or reset) |
| `agg_dimension_events_total` | counter | `site, aggregate, <dimension>` | the same per group value |
| `agg_last_timestamp_seconds` | gauge | `site, aggregate[, <dimension>]` | unix time of the last matching event |
| `agg_last_value` | gauge | `site, aggregate[, <dimension>]` | last numeric value (non-numeric values are left out) |
| `agg_formula_value` | gauge | `site, formula` | formula value |

```
agg_value{site="shop",aggregate="purchases",window="24h"} 123
agg_dimension_value{site="shop",aggregate="product_purchases",window="24h",product="73"} 5
agg_events_total{site="shop",aggregate="purchases"} 5012
agg_formula_value{site="shop",formula="aov"} 182.4
```

Rules the export follows (and the tests check):

- Window values are **gauges**: they go down when old events leave the window. All-time counts are **counters**;
  a data reset looks like a counter reset, which `rate()` and `increase()` handle.
- Totals and per-dimension series are in **separate families**, so `sum(agg_value{…})` never double counts.
- No timestamps on samples: Prometheus records the scrape time.
- Missing values are left out, never `NaN` or `Inf`. A count with no events is a real `0`. Allowlisted values with no
  data are exported as `0`, so series do not disappear.
- The dimension label is the dimension name (`product`). Names that clash with `site`, `aggregate`, `window`,
  `formula`, `job`, `instance`, `le` or `quantile` get a `dim_` prefix.
- Output is sorted and stable, served with `Content-Type: text/plain; version=0.0.4; charset=utf-8`, and gzipped when
  the scraper accepts it. It passes `promtool check metrics`.

## PromQL examples

```promql
# purchases in the last 24 h (computed by agg, exact to the minute)
agg_value{aggregate="purchases", window="24h"}

# purchases per minute, computed by Prometheus from the counter
rate(agg_events_total{aggregate="purchases"}[5m]) * 60

# top 5 products in the last hour
topk(5, agg_dimension_value{aggregate="product_purchases", window="1h"})

# no purchase for 2 hours (alert)
time() - agg_last_timestamp_seconds{aggregate="last_purchase"} > 7200

# today vs the same time a week ago
agg_value{aggregate="revenue", window="24h"} / agg_value{aggregate="revenue", window="24h"} offset 7d
```

## Grafana

- `deploy/grafana/dashboards/agg.json`: a dashboard with site / aggregate / window variables: current values,
  values over time, events per minute, top dimension values, formulas and time since the last event.
- `deploy/grafana/provisioning/`: Prometheus datasource and dashboard provisioning.
- `docker-compose.yml`: agg + Prometheus + Grafana. Put the export id in `deploy/prometheus/prometheus.yml` and the
  token in `deploy/prometheus/agg_token`.

Without Prometheus, use the JSON export with the Grafana **Infinity** datasource: URL `/export/<id>/metrics`, header
`Authorization: Bearer <token>`, type JSON, root `metrics`. Each row has `metric`, `labels` and `value`.

## Internal metrics

`AGG_INTERNAL_METRICS_TOKEN=…` enables `/internal/metrics` (bearer token) with the instance's own metrics:
events received / accepted / dropped by reason, flush duration and errors, buffered events, database size.
