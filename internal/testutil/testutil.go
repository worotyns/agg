// Package testutil builds a real store + engine + querier with a controllable clock for tests.
package testutil

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/worotyns/agg/internal/engine"
	"github.com/worotyns/agg/internal/model"
	"github.com/worotyns/agg/internal/query"
	"github.com/worotyns/agg/internal/store"
)

type Fixture struct {
	T    testing.TB
	St   *store.SQLite
	Eng  *engine.Engine
	Q    *query.Querier
	Now  time.Time
	Site model.Site
	Log  *slog.Logger
}

// Start is a fixed point in time (a Wednesday, 12:30:00 UTC) so window maths is predictable.
var Start = time.Date(2026, 10, 7, 12, 30, 0, 0, time.UTC)

func New(t testing.TB) *Fixture {
	t.Helper()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "agg.db"))
	if err != nil {
		t.Fatal(err)
	}
	f := &Fixture{T: t, St: st, Now: Start, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	f.Eng = engine.New(st, f.Log, engine.Options{})
	f.Eng.Now = func() time.Time { return f.Now }
	f.Q = query.New(st)
	f.Q.Now = func() time.Time { return f.Now }
	t.Cleanup(func() { st.Close() })
	f.Site = f.AddSite("shop")
	return f
}

func (f *Fixture) AddSite(slug string) model.Site {
	f.T.Helper()
	s := model.Site{Slug: slug, Name: slug, PublicKey: "pk_" + slug, Config: model.DefaultSiteConfig()}
	if err := f.St.CreateSite(context.Background(), &s); err != nil {
		f.T.Fatal(err)
	}
	f.Reload()
	return s
}

func (f *Fixture) Reload() {
	f.T.Helper()
	if err := f.Eng.Reload(context.Background()); err != nil {
		f.T.Fatal(err)
	}
}

// Agg creates an aggregate on the fixture's default site.
func (f *Fixture) Agg(name string, def model.AggregateDef) model.Aggregate {
	return f.AggOn(f.Site, name, def)
}

func (f *Fixture) AggOn(site model.Site, name string, def model.AggregateDef) model.Aggregate {
	f.T.Helper()
	if _, err := engine.Validate(&def); err != nil {
		f.T.Fatalf("aggregate %s: %v", name, err)
	}
	a := model.Aggregate{SiteID: site.ID, Name: name, AggregateDef: def}
	if err := f.St.CreateAggregate(context.Background(), &a); err != nil {
		f.T.Fatal(err)
	}
	f.Reload()
	return a
}

func (f *Fixture) Formula(name, expr string) model.Formula {
	f.T.Helper()
	fm := model.Formula{SiteID: f.Site.ID, Name: name, Title: name, Expr: expr, Unit: "number", Visibility: model.VisibilityPrivate}
	if err := f.St.CreateFormula(context.Background(), &fm); err != nil {
		f.T.Fatal(err)
	}
	return fm
}

// Send ingests events on the default site at the current fixture time and flushes them.
func (f *Fixture) Send(events ...engine.IncomingEvent) engine.IngestResult {
	return f.SendTo(f.Site, events...)
}

func (f *Fixture) SendTo(site model.Site, events ...engine.IncomingEvent) engine.IngestResult {
	f.T.Helper()
	res, err := f.Eng.Ingest(site.PublicKey, "", engine.RequestInfo{UserAgent: TestUA}, events)
	if err != nil {
		f.T.Fatal(err)
	}
	if err := f.Eng.Flush(context.Background()); err != nil {
		f.T.Fatal(err)
	}
	return res
}

func (f *Fixture) Advance(d time.Duration) { f.Now = f.Now.Add(d) }

// TestUA is a desktop Chrome User-Agent.
const TestUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"

// Ev is a shorthand for an event.
func Ev(name string, props map[string]any) engine.IncomingEvent {
	return engine.IncomingEvent{Name: name, Props: props}
}

// Purchase builds a purchase event with items (id, name, category, price, quantity).
func Purchase(id string, value any, items ...map[string]any) engine.IncomingEvent {
	arr := make([]any, len(items))
	for i, it := range items {
		arr[i] = it
	}
	return engine.IncomingEvent{Name: "purchase", ID: id, Props: map[string]any{"order_id": id, "value": value, "currency": "PLN", "items": arr}}
}

func Item(id, category string, price, qty float64) map[string]any {
	return map[string]any{"id": id, "name": "Product " + id, "category": category, "price": price, "quantity": qty}
}

func (f *Fixture) WindowValue(a model.Aggregate, window, part, member string) float64 {
	f.T.Helper()
	w, ok := model.WindowByName(window)
	if !ok {
		f.T.Fatalf("unknown window %s", window)
	}
	v, err := f.Q.WindowValue(context.Background(), a, w, false, part, member)
	if err != nil {
		f.T.Fatal(err)
	}
	return v
}
