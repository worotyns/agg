// Command loadtest sends a large, deterministic stream of events to a running agg server over HTTP,
// measures ingest throughput and latency, then reads every aggregate back and checks it against the
// values computed locally from the same stream.
//
//	agg serve -db bench.db -admin-token bench -trust-proxy &
//	go run ./scripts/loadtest -url http://127.0.0.1:8080 -token bench -events 1000000
//
// The server must run with -trust-proxy: requests carry X-Forwarded-For from a pool of client IPs,
// so the per-IP ingest limit behaves as it would with many real browsers.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/cookiejar"
	"os"
	"slices"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

var (
	baseURL     = flag.String("url", "http://127.0.0.1:8080", "agg server URL")
	token       = flag.String("token", "", "admin token")
	total       = flag.Int("events", 1_000_000, "events to send")
	batch       = flag.Int("batch", 50, "events per request (the SDK sends at most 50)")
	workers     = flag.Int("concurrency", 32, "concurrent HTTP clients")
	visitors    = flag.Int("visitors", 100_000, "distinct visitor ids")
	products    = flag.Int("products", 200, "distinct products")
	clientIPs   = flag.Int("ips", 5_000, "distinct client IPs (X-Forwarded-For)")
	drainLimit  = flag.Duration("drain-timeout", 5*time.Minute, "how long to wait for buffered events to reach storage")
	readQueries = flag.Int("reads", 2_000, "read requests in the read benchmark")
)

const (
	purchaseEvery = 20 // every 20th event is a purchase, the rest are page views
	pathCount     = 50
)

// expected aggregate values, computed from the generated stream.
type expected struct {
	pageViews, purchases int
	revenue              float64
	visitors             map[string]struct{}
	perProduct           map[string]int
	perPath              map[string]int
	lastValue            float64
}

// event i is fully determined by i, so workers can generate in parallel and the totals stay exact.
func gen(i int) map[string]any {
	r := rand.New(rand.NewPCG(uint64(i), 42))
	visitor := "v" + strconv.Itoa(r.IntN(*visitors))
	if i%purchaseEvery == 0 {
		cents := 100 + r.IntN(50_000)
		return map[string]any{
			"name": "purchase", "visitorId": visitor,
			"props": map[string]any{"value": float64(cents) / 100, "product": "p" + strconv.Itoa(r.IntN(*products))},
		}
	}
	return map[string]any{
		"name": "page_view", "visitorId": visitor,
		"meta": map[string]any{"path": "/page/" + strconv.Itoa(r.IntN(pathCount))},
	}
}

func computeExpected() expected {
	e := expected{visitors: map[string]struct{}{}, perProduct: map[string]int{}, perPath: map[string]int{}}
	for i := 0; i < *total; i++ {
		ev := gen(i)
		e.visitors[ev["visitorId"].(string)] = struct{}{}
		if ev["name"] == "purchase" {
			p := ev["props"].(map[string]any)
			e.purchases++
			e.revenue += p["value"].(float64)
			e.perProduct[p["product"].(string)]++
		} else {
			e.pageViews++
			e.perPath[ev["meta"].(map[string]any)["path"].(string)]++
		}
	}
	return e
}

type client struct {
	http *http.Client
}

func (c *client) do(method, path string, body any, out any, hdr ...string) (int, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, *baseURL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 300 {
		return res.StatusCode, fmt.Errorf("%s %s: %d %s", method, path, res.StatusCode, b)
	}
	if out != nil {
		return res.StatusCode, json.Unmarshal(b, out)
	}
	return res.StatusCode, nil
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func main() {
	flag.Parse()
	jar, _ := cookiejar.New(nil)
	tr := &http.Transport{MaxIdleConns: *workers * 2, MaxIdleConnsPerHost: *workers * 2}
	admin := &client{&http.Client{Jar: jar, Transport: tr, Timeout: 60 * time.Second}}

	_, err := admin.do("POST", "/api/login", map[string]string{"token": *token}, nil)
	must(err)
	slug := fmt.Sprintf("bench_%d", time.Now().Unix())
	var site struct {
		ID        int64  `json:"id"`
		PublicKey string `json:"publicKey"`
	}
	_, err = admin.do("POST", "/api/sites", map[string]any{"name": slug, "slug": slug, "preset": "none", "timezone": "UTC"}, &site)
	must(err)
	aggs := []map[string]any{
		{"name": "page_views", "events": []string{"page_view"}, "op": "count", "visibility": "private"},
		{"name": "purchases", "events": []string{"purchase"}, "op": "count", "visibility": "private"},
		{"name": "revenue", "events": []string{"purchase"}, "op": "sum", "value": "props.value", "visibility": "private"},
		{"name": "visitors", "events": []string{"page_view", "purchase"}, "op": "count_distinct", "visibility": "private"},
		{"name": "per_product", "events": []string{"purchase"}, "op": "count", "visibility": "private",
			"groupBy": map[string]string{"dimension": "product", "expr": "props.product"}},
		{"name": "per_path", "events": []string{"page_view"}, "op": "count", "visibility": "private",
			"groupBy": map[string]string{"dimension": "page", "expr": "meta.path"}},
		{"name": "last_order", "events": []string{"purchase"}, "op": "last_value", "value": "props.value", "visibility": "private"},
	}
	ids := map[string]int64{}
	for _, a := range aggs {
		var out struct {
			Aggregate struct {
				ID int64 `json:"id"`
			} `json:"aggregate"`
		}
		_, err := admin.do("POST", fmt.Sprintf("/api/sites/%d/aggregates", site.ID), a, &out)
		must(err)
		ids[a["name"].(string)] = out.Aggregate.ID
	}
	fmt.Printf("site %s (id %d), %d aggregates\n", slug, site.ID, len(aggs))

	fmt.Printf("computing expected values for %d events…\n", *total)
	exp := computeExpected()

	// ---- write
	ingest := &client{&http.Client{Transport: tr, Timeout: 60 * time.Second}}
	nReq := (*total + *batch - 1) / *batch
	var next, accepted, dropped, failed, throttled atomic.Int64
	lat := make([][]time.Duration, *workers)
	var lastMu sync.Mutex
	lastPurchaseIdx, lastPurchaseAt := -1, time.Time{}
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for {
				r := int(next.Add(1) - 1)
				if r >= nReq {
					return
				}
				lo, hi := r**batch, min((r+1)**batch, *total)
				evs := make([]map[string]any, 0, hi-lo)
				lastP := -1
				for i := lo; i < hi; i++ {
					evs = append(evs, gen(i))
					if i%purchaseEvery == 0 {
						lastP = i
					}
				}
				ip := fmt.Sprintf("10.%d.%d.%d", r%*clientIPs/65536, r%*clientIPs/256%256, r%*clientIPs%256)
				for attempt := 0; ; attempt++ {
					var res struct{ Accepted, Dropped int }
					t0 := time.Now()
					code, err := ingest.do("POST", "/e", map[string]any{"site": site.PublicKey, "events": evs}, &res,
						"X-Forwarded-For", ip, "User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15")
					if code == 429 && attempt < 50 {
						throttled.Add(1)
						time.Sleep(20 * time.Millisecond)
						continue
					}
					lat[w] = append(lat[w], time.Since(t0))
					if err != nil {
						failed.Add(int64(len(evs)))
						fmt.Fprintln(os.Stderr, err)
						break
					}
					accepted.Add(int64(res.Accepted))
					dropped.Add(int64(res.Dropped))
					if lastP >= 0 {
						// The server orders last_value by arrival; remember the purchase that was accepted last.
						lastMu.Lock()
						if now := time.Now(); now.After(lastPurchaseAt) {
							lastPurchaseAt, lastPurchaseIdx = now, lastP
						}
						lastMu.Unlock()
					}
					break
				}
			}
		}(w)
	}
	wg.Wait()
	elapsed := time.Since(start)
	all := slices.Concat(lat...)
	slices.Sort(all)
	pct := func(p float64) time.Duration { return all[int(math.Min(float64(len(all)-1), p*float64(len(all))))] }

	fmt.Printf("\n== write: %d events in %d requests of %d, concurrency %d\n", *total, nReq, *batch, *workers)
	fmt.Printf("elapsed           %.2fs\n", elapsed.Seconds())
	fmt.Printf("throughput        %.0f events/s, %.0f requests/s\n", float64(*total)/elapsed.Seconds(), float64(nReq)/elapsed.Seconds())
	fmt.Printf("latency           p50 %v  p90 %v  p99 %v  max %v\n", pct(.5).Round(time.Microsecond), pct(.9).Round(time.Microsecond), pct(.99).Round(time.Microsecond), all[len(all)-1].Round(time.Microsecond))
	fmt.Printf("accepted %d, dropped %d, failed %d, 429 retries %d\n", accepted.Load(), dropped.Load(), failed.Load(), throttled.Load())

	if lastPurchaseIdx >= 0 {
		exp.lastValue = gen(lastPurchaseIdx)["props"].(map[string]any)["value"].(float64)
	}

	value24h := func(name, qs string) float64 {
		var v struct {
			Windows []struct {
				Window string  `json:"window"`
				Value  float64 `json:"value"`
			} `json:"windows"`
		}
		_, err := admin.do("GET", fmt.Sprintf("/api/sites/%d/aggregates/%d/values?%s", site.ID, ids[name], qs), nil, &v)
		must(err)
		for _, w := range v.Windows {
			if w.Window == "24h" {
				return w.Value
			}
		}
		return math.NaN()
	}
	// Accepted events are buffered in memory and written by the flush loop; wait until reads see all of them.
	fmt.Printf("\n== persistence\n")
	wantPV := float64(exp.pageViews)
	var persisted time.Duration
	for {
		if value24h("page_views", "") >= wantPV {
			persisted = time.Since(start)
			break
		}
		if time.Since(start) > elapsed+*drainLimit {
			fmt.Println("timed out waiting for flush")
			persisted = time.Since(start)
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Printf("all events readable %.2fs after the first request (%.2fs after the last response)\n", persisted.Seconds(), (persisted - elapsed).Seconds())
	fmt.Printf("end-to-end        %.0f events/s (send → stored and queryable)\n", float64(*total)/persisted.Seconds())
	time.Sleep(2 * time.Second) // let the last flush of the other aggregates land too

	// ---- consistency
	fmt.Printf("\n== consistency (24h window)\n")
	ok := true
	check := func(name string, got, want float64, tol float64) {
		status := "OK"
		if math.Abs(got-want) > tol {
			status, ok = "MISMATCH", false
		}
		fmt.Printf("%-28s got %-16s want %-16s %s\n", name, fmtNum(got), fmtNum(want), status)
	}
	check("page_views", value24h("page_views", ""), float64(exp.pageViews), 0)
	check("purchases", value24h("purchases", ""), float64(exp.purchases), 0)
	check("revenue", value24h("revenue", ""), exp.revenue, 0.01)
	check("visitors (distinct)", value24h("visitors", ""), float64(len(exp.visitors)), 0)
	check("accepted == sent", float64(accepted.Load()), float64(*total), 0)

	top := func(name string, limit int) map[string]float64 {
		var t struct {
			Items []struct {
				Key   string  `json:"key"`
				Value float64 `json:"value"`
			} `json:"items"`
		}
		_, err := admin.do("GET", fmt.Sprintf("/api/sites/%d/aggregates/%d/top?window=24h&limit=%d", site.ID, ids[name], limit), nil, &t)
		must(err)
		m := map[string]float64{}
		for _, it := range t.Items {
			m[it.Key] = it.Value
		}
		return m
	}
	// The top endpoint returns at most 100 items: every returned item must be exact, and when all groups fit,
	// the set must be complete.
	compareTop := func(label string, got map[string]float64, want map[string]int) {
		bad := 0
		for k, v := range got {
			if v != float64(want[k]) {
				bad++
			}
		}
		status := "OK"
		if bad > 0 || (len(want) <= 100 && len(got) != len(want)) {
			status, ok = "MISMATCH", false
		}
		fmt.Printf("%-28s %d/%d returned items exact (%d groups) %s\n", label, len(got)-bad, len(got), len(want), status)
	}
	compareTop("per_product (top 100)", top("per_product", 100), exp.perProduct)
	compareTop("per_path (top)", top("per_path", pathCount), exp.perPath)
	// Every group, read one by one through the dimension filter.
	keys := make([]string, 0, len(exp.perProduct))
	for k := range exp.perProduct {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	bad, sum := 0, 0.0
	for _, k := range keys {
		v := value24h("per_product", "product="+k)
		sum += v
		if v != float64(exp.perProduct[k]) {
			bad++
		}
	}
	status := "OK"
	if bad > 0 {
		status, ok = "MISMATCH", false
	}
	fmt.Printf("%-28s %d/%d groups exact, sum %s (want %d) %s\n", "per_product (each group)", len(keys)-bad, len(keys), fmtNum(sum), exp.purchases, status)

	var last struct {
		Last float64 `json:"last"`
	}
	_, err = admin.do("GET", fmt.Sprintf("/api/sites/%d/aggregates/%d/values", site.ID, ids["last_order"]), nil, &last)
	must(err)
	// Requests run concurrently, so "last" is only defined up to requests that finished at the same moment;
	// report it rather than fail on it.
	fmt.Printf("%-28s got %-16s last acked %s\n", "last_order (last_value)", fmtNum(last.Last), fmtNum(exp.lastValue))

	// ---- read
	fmt.Printf("\n== read: %d requests, concurrency %d\n", *readQueries, *workers)
	reads := []string{
		fmt.Sprintf("/api/sites/%d/aggregates/%d/values", site.ID, ids["page_views"]),
		fmt.Sprintf("/api/sites/%d/aggregates/%d/values", site.ID, ids["visitors"]),
		fmt.Sprintf("/api/sites/%d/aggregates/%d/top?window=24h&limit=20", site.ID, ids["per_product"]),
		fmt.Sprintf("/api/sites/%d/aggregates/%d/series?range=24h", site.ID, ids["page_views"]),
		fmt.Sprintf("/api/sites/%d/insights?window=24h", site.ID),
	}
	for _, path := range reads {
		var rl []time.Duration
		var mu sync.Mutex
		var rn atomic.Int64
		n := *readQueries / len(reads)
		t0 := time.Now()
		var rwg sync.WaitGroup
		for w := 0; w < *workers; w++ {
			rwg.Add(1)
			go func() {
				defer rwg.Done()
				for rn.Add(1) <= int64(n) {
					s := time.Now()
					_, err := admin.do("GET", path, nil, nil)
					must(err)
					mu.Lock()
					rl = append(rl, time.Since(s))
					mu.Unlock()
				}
			}()
		}
		rwg.Wait()
		el := time.Since(t0)
		slices.Sort(rl)
		fmt.Printf("%-62s %7.0f req/s  p50 %v  p99 %v\n", trimQuery(path), float64(n)/el.Seconds(),
			rl[len(rl)/2].Round(time.Microsecond), rl[len(rl)*99/100].Round(time.Microsecond))
	}

	if !ok {
		fmt.Println("\nRESULT: MISMATCH")
		os.Exit(1)
	}
	fmt.Println("\nRESULT: all aggregates consistent")
}

func trimQuery(p string) string {
	if len(p) > 62 {
		return p[:62]
	}
	return p
}

func fmtNum(f float64) string {
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'f', 2, 64)
}
