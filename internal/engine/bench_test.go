package engine_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/worotyns/agg/internal/engine"
	"github.com/worotyns/agg/internal/geo"
	"github.com/worotyns/agg/internal/model"
	"github.com/worotyns/agg/internal/testutil"
)

// benchFixture sets up a site with a realistic mix of aggregates: counts, a sum, distinct visitors,
// two grouped counts and a last value.
func benchFixture(b *testing.B) *testutil.Fixture {
	f := testutil.New(b)
	f.Agg("page_views", model.AggregateDef{Events: []string{"page_view"}, Op: model.OpCount})
	f.Agg("purchases", model.AggregateDef{Events: []string{"purchase"}, Op: model.OpCount})
	f.Agg("revenue", model.AggregateDef{Events: []string{"purchase"}, Op: model.OpSum, Value: "props.value"})
	f.Agg("visitors", model.AggregateDef{Events: []string{"page_view", "purchase"}, Op: model.OpCountDistinct})
	f.Agg("per_product", model.AggregateDef{Events: []string{"purchase"}, Op: model.OpCount,
		GroupBy: &model.Grouping{Dimension: "product", Expr: "props.product"}})
	f.Agg("per_path", model.AggregateDef{Events: []string{"page_view"}, Op: model.OpCount,
		GroupBy: &model.Grouping{Dimension: "page", Expr: "meta.path"}})
	f.Agg("last_order", model.AggregateDef{Events: []string{"purchase"}, Op: model.OpLastValue, Value: "props.value"})
	return f
}

// benchBatch builds the i-th batch of 50 events: 1 in 20 is a purchase, the rest page views,
// spread over 100k visitors so the per-visitor limit is never hit.
func benchBatch(i int) []engine.IncomingEvent {
	evs := make([]engine.IncomingEvent, 50)
	for j := range evs {
		n := i*50 + j
		v := "v" + strconv.Itoa(n*7919%100_000)
		if n%20 == 0 {
			evs[j] = engine.IncomingEvent{Name: "purchase", VisitorID: v,
				Props: map[string]any{"value": float64(n%500) + 0.99, "product": "p" + strconv.Itoa(n%200)}}
		} else {
			evs[j] = engine.IncomingEvent{Name: "page_view", VisitorID: v,
				Meta: map[string]any{"path": "/page/" + strconv.Itoa(n%50)}}
		}
	}
	return evs
}

// BenchmarkIngest measures the in-memory part of ingestion: validation, enrichment, rule evaluation
// and buffering of deltas. Storage writes happen on flush and are measured below.
func BenchmarkIngest(b *testing.B) {
	f := benchFixture(b)
	key := f.Site.PublicKey
	batches := make([][]engine.IncomingEvent, 1000)
	for i := range batches {
		batches[i] = benchBatch(i)
	}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := f.Eng.Ingest(key, "", engine.RequestInfo{UserAgent: testutil.TestUA}, batches[i%len(batches)]); err != nil {
			b.Fatal(err)
		}
		if i%1000 == 999 { // keep the buffer bounded as the 1 s flush loop would
			b.StopTimer()
			f.Eng.Flush(ctx)
			b.StartTimer()
		}
	}
	b.ReportMetric(float64(b.N*50)/b.Elapsed().Seconds(), "events/s")
}

// BenchmarkIngestAndFlush includes the SQLite writes: one flush per 20 batches (1000 events),
// roughly what a busy server does each second.
func BenchmarkIngestAndFlush(b *testing.B) {
	f := benchFixture(b)
	key := f.Site.PublicKey
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := f.Eng.Ingest(key, "", engine.RequestInfo{UserAgent: testutil.TestUA}, benchBatch(i)); err != nil {
			b.Fatal(err)
		}
		if i%20 == 19 {
			f.Advance(time.Second)
			if err := f.Eng.Flush(ctx); err != nil {
				b.Fatal(err)
			}
		}
	}
	if err := f.Eng.Flush(ctx); err != nil {
		b.Fatal(err)
	}
	b.ReportMetric(float64(b.N*50)/b.Elapsed().Seconds(), "events/s")
}

// BenchmarkIngestGeoCached is BenchmarkIngest with GeoIP on and the client IP in the cache (the steady state).
func BenchmarkIngestGeoCached(b *testing.B) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"city":"Lisbon","country_iso":"PT","has_data":true}`))
	}))
	defer srv.Close()
	g, err := geo.New(srv.URL, nil)
	if err != nil {
		b.Fatal(err)
	}
	f := benchFixture(b)
	f.Eng = engine.New(f.St, f.Log, engine.Options{Geo: g})
	f.Eng.Now = func() time.Time { return f.Now }
	f.Reload()
	key := f.Site.PublicKey
	batches := make([][]engine.IncomingEvent, 1000)
	for i := range batches {
		batches[i] = benchBatch(i)
	}
	ctx := context.Background()
	req := engine.RequestInfo{UserAgent: testutil.TestUA, IP: "85.0.0.1"}
	f.Eng.Ingest(key, "", req, batches[0]) // warm the cache
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := f.Eng.Ingest(key, "", req, batches[i%len(batches)]); err != nil {
			b.Fatal(err)
		}
		if i%1000 == 999 {
			b.StopTimer()
			f.Eng.Flush(ctx)
			b.StartTimer()
		}
	}
	b.ReportMetric(float64(b.N*50)/b.Elapsed().Seconds(), "events/s")
}
