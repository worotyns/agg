# Benchmarks

How fast agg ingests events, how long they take to become queryable, and whether the aggregates are
exactly right afterwards. Two tools:

- **Engine micro-benchmarks** (`internal/engine/bench_test.go`): ingestion in-process, without HTTP.
- **Load test** (`scripts/loadtest`): a real server over HTTP, 1,000,000 events, then a consistency check of every
  aggregate and a read benchmark.

## Setup

| | |
|---|---|
| Machine | Apple M1, 8 cores, 16 GB RAM, macOS 27 |
| Go | 1.27.1 |
| Server | `agg serve -trust-proxy`, default options (flush every 1 s, raw events kept 7 days), fresh SQLite file |
| Client | load generator on the **same machine**, so client and server share the CPU |
| Date | 2026-10-07 |

The workload, for both tools:

- 95% `page_view` (with `meta.path` from 50 paths), 5% `purchase` (`props.value`, `props.product` from 200 products);
- 100,000 visitor ids; 50 events per request (the SDK's maximum batch);
- 7 aggregates: `page_views` (count), `purchases` (count), `revenue` (sum), `visitors` (count distinct),
  `per_product` (count by product), `per_path` (count by page), `last_order` (last value).

Every event is processed against every matching aggregate and also stored as a raw event.

## Results

### Load test: 1,000,000 events over HTTP

```
== write: 1000000 events in 20000 requests of 50, concurrency 32
elapsed           4.71s
throughput        212467 events/s, 4249 requests/s
latency           p50 6.539ms  p90 11.084ms  p99 29.442ms  max 84.096ms
accepted 1000000, dropped 0, failed 0, 429 retries 0

== persistence
all events readable 12.05s after the first request (7.35s after the last response)
end-to-end        82963 events/s (send → stored and queryable)
```

| Metric | Value |
|---|---|
| Ingest (HTTP 200 returned) | **~212,000 events/s**, ~4,250 requests/s |
| Request latency | p50 6.5 ms, p99 29 ms |
| End-to-end (sent → stored → queryable) | **~83,000 events/s** |
| Peak server memory (RSS) | 1.33 GB, while ~600k events were waiting for the flush |
| Peak server CPU | ~7 of 8 cores |
| Database after the run | 145 MB + 114 MB WAL (raw events included) |

Over three runs the write throughput ranged from 212k to 258k events/s and end-to-end from 83k to 89k events/s.

Ingest is faster than storage: a request returns once its events are validated, evaluated and buffered in memory;
the flush loop then writes the buffer to SQLite through a single writer connection. At this load the flush fell about
7 s behind and caught up after the client stopped.

### Consistency after 1M events

Every number read back through the admin API equals the value computed by the load generator from the same
deterministic stream:

```
== consistency (24h window)
page_views                   got 950000           want 950000           OK
purchases                    got 50000            want 50000            OK
revenue                      got 12589140.41      want 12589140.41      OK
visitors (distinct)          got 99995            want 99995            OK
accepted == sent             got 1000000          want 1000000          OK
per_product (top 100)        100/100 returned items exact (200 groups) OK
per_path (top)               50/50 returned items exact (50 groups) OK
per_product (each group)     200/200 groups exact, sum 50000 (want 50000) OK
last_order (last_value)      got 338.09           last acked 121.58

RESULT: all aggregates consistent
```

No event was lost or counted twice, sums are exact to the cent, distinct counts are exact (agg stores member hashes,
not a sketch), and every group of the grouped aggregates matches.

`last_order` is informational: 32 requests run at the same time, so "the last purchase" depends on which request the
server finished last, which the client cannot observe exactly.

### Reads after 1M events (32 concurrent clients)

| Endpoint | Throughput | p50 | p99 |
|---|---|---|---|
| values of a count (`page_views`, 6 windows + previous) | 8,200 req/s | 3.6 ms | 6.7 ms |
| values of a distinct count (`visitors`, 100k members) | **14 req/s** | **2.27 s** | 3.46 s |
| top 20 (`per_product`) | 4,800 req/s | 6.1 ms | 13 ms |
| series, 24 h (`page_views`) | 19,000 req/s | 1.5 ms | 4.1 ms |
| insights page | 18,700 req/s | 1.4 ms | 3.9 ms |

A single request for the distinct count, with no other load, takes ~270 ms.

### Engine micro-benchmarks

```
go test ./internal/engine -run '^$' -bench . -benchtime 3s

BenchmarkIngest-8           17845    205069 ns/op   243820 events/s   182178 B/op   2062 allocs/op
BenchmarkIngestAndFlush-8    3225   2564086 ns/op    19500 events/s   300930 B/op   5550 allocs/op
```

- `BenchmarkIngest`: validation, enrichment (User-Agent parsing), rule evaluation and buffering: **~244k events/s**
  on one goroutine (~4 µs per event across 7 aggregates).
- `BenchmarkIngestAndFlush`: adds a SQLite flush every 1,000 events: ~19.5k events/s. The real server flushes once a
  second, so under load each flush carries many more events and costs less per event (see the 83k end-to-end figure
  above).

## Findings

1. **Correctness holds at 1M events.** Counts, sums, distinct counts and every group are exact.
2. **The SQLite flush is the bottleneck**, not HTTP or rule evaluation: ~83k events/s end-to-end against ~212k events/s
   accepted.
3. **The in-memory buffer has no upper bound.** When events arrive faster than the flush can write them for a long time,
   the backlog and memory keep growing (1.3 GB at a 600k-event backlog). Possible fix: stop accepting (HTTP 503 with
   `Retry-After`, which the SDK already retries) once pending raw events pass a limit.
4. **Distinct-count reads do not scale with many members.** A values request runs 12 `COUNT(DISTINCT hash)` scans
   (6 windows, current and previous); with 100k visitors that is ~270 ms per request, and the 8 read connections
   cap it at ~14 req/s. Possible fixes: cache values for a few seconds, compute all windows in one pass, or move
   long windows to a sketch (HyperLogLog), trading exactness for speed.
5. Everything else that the UI and the values API read stays in single-digit milliseconds.

## Reproduce

```sh
make build
./dist/agg serve -db bench.db -admin-token bench -trust-proxy &
go run ./scripts/loadtest -url http://127.0.0.1:8080 -token bench -events 1000000

go test ./internal/engine -run '^$' -bench . -benchtime 3s
```

`-trust-proxy` is needed because the load test sends each request with an `X-Forwarded-For` address from a pool of
5,000, so the per-IP ingest limit (50 requests/s) behaves as it would with many real browsers. Other options:
`-concurrency`, `-batch`, `-visitors`, `-products`, `-reads`; see `go run ./scripts/loadtest -h`.
