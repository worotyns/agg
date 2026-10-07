# Aggregates

An aggregate turns a stream of events into a number that updates as events arrive. Each one is defined by:

| Part | Example | |
|---|---|---|
| **Events** | `purchase` | event names it listens to (normalized: `Add To Cart` → `add_to_cart`) |
| **Where** | `props.value > 100` | optional filter expression |
| **Explode** | `props.items` | optional: treat each array element as its own event, available as `item` |
| **Operation** | `sum` of `item.price * item.quantity` | `count`, `sum`, `count_distinct`, `last_value`, `last_timestamp` |
| **Group by** | dimension `product` = `item.id`, label `item.name` | optional: one value per product |
| **Rank within group** | dimension `product` inside group `category` | optional (count/sum): top-N inside each group |
| **Visibility** | `private`, `public`, `public_bucketed` | whether `/v1/values` serves it without a token |

## Expressions

Expressions use [expr](https://expr-lang.org) syntax and see:

- `event`: the event name;
- `props`: the properties you sent;
- `meta`: metadata added by agg: `path`, `referrer`, `language`, `browser`, `os`, `device`, `bot`, `ip` (see [sdk.md](sdk.md#metadata));
- `item`: the current element when Explode is set;
- `visitor`: the visitor id (empty when disabled);
- `ts`: unix time (seconds) the server received the event.

Useful operators: `== != < > <= >=`, `&& || !`, `in` (`props.currency in ["EUR", "PLN"]`), `contains`,
`startsWith`, `matches` (regex), `+ - * /`, `??` (`item.quantity ?? 1`), indexing (`props.items[0].id`).

A missing field is `nil`. If Where errors or is not `true`, the event is not counted. Numeric strings (`"12.50"`) are
accepted by `sum`. Dimension values are strings: `73` and `"73"` are the same product; values are trimmed and capped
at 200 bytes.

## Operations

| Operation | Value | Windows |
|---|---|---|
| `count` | number of matching events (or exploded items) | yes, plus an all-time total |
| `sum` | sum of the Value expression | yes, plus an all-time total |
| `count_distinct` | exact number of distinct values of the Value expression (default: the visitor id) | yes |
| `last_value` | most recent value of the Value expression | no |
| `last_timestamp` | unix time of the most recent matching event | no |

## Windows and precision

Windows slide with the current time. Counts are stored in minute, hour and day buckets:

| Window | Computed from | Precision |
|---|---|---|
| `5m`, `1h`, `6h`, `24h` | minute buckets | exact to the minute |
| `7d` | hour buckets | exact to the hour |
| `30d` | day buckets (UTC days) | exact to the day |

Each window has a previous window of the same length right before it (`prev_24h` is the 24 hours before the last 24
hours), used for "change vs previous period".

## Variables

Every aggregate provides variables used by formulas, the values API and exports:

- `purchases_5m` … `purchases_30d`: window values;
- `purchases_prev_5m` … `purchases_prev_30d`: previous windows;
- `purchases_total`: all-time value since creation or the last reset (count and sum);
- `last_purchase`: for `last_value` / `last_timestamp` aggregates, the name alone.

Names must not end with a window suffix (`_24h`, `_total`…).

For a grouped aggregate, a variable is the site-wide value unless a dimension value is given, e.g.
`/v1/values?…&v=purchases_24h&product=73`.

## Formulas

Formulas combine variables and are evaluated when read, so they work over history and can be changed at any time:

```
revenue_24h / purchases_24h                       average order value
purchases_24h / product_viewers_24h * 100         conversion rate (%)
(orders_24h - orders_prev_24h) / orders_prev_24h  trend
coalesce(last_purchase, 0)
```

Allowed: `+ - * /`, parentheses, `round() abs() min() max() floor() ceil()`, `coalesce(x, 0)`. Division by zero and
missing values give `null`, never a guess or `Infinity`.

## Reset and rebuild

- **Reset data** deletes the collected values of one aggregate. New events keep being counted.
- **Rebuild** resets the aggregate and replays the stored raw events (default 7 days) through its current
  definition, keeping each event's original time. Use it after changing the definition. Data older than the raw
  retention cannot be rebuilt.

## Guards

The ingest endpoint is public, so:

- events with the same `id` are counted once per site (48 hours), e.g. a reloaded thank-you page;
- a visitor can send at most 300 events per minute, and an IP at most 50 requests per second;
- each aggregate stores at most 50,000 distinct group values; beyond that, new values still count towards the
  site-wide total but get no own series;
- personal-data property names are removed before storage (see [sdk.md](sdk.md#visitor-id-privacy-consent));
- automatic page views from bots are not stored.
