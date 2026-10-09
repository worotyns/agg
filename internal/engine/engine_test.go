package engine_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/worotyns/agg/internal/engine"
	"github.com/worotyns/agg/internal/geo"
	"github.com/worotyns/agg/internal/model"
	"github.com/worotyns/agg/internal/query"
	"github.com/worotyns/agg/internal/store"
	"github.com/worotyns/agg/internal/testutil"
)

func TestCountSumAndWindows(t *testing.T) {
	f := testutil.New(t)
	count := f.Agg("purchases", model.AggregateDef{Events: []string{"purchase"}, Op: model.OpCount})
	sum := f.Agg("revenue", model.AggregateDef{Events: []string{"purchase"}, Op: model.OpSum, Value: "props.value"})

	f.Send(testutil.Purchase("a", 10))
	f.Advance(3 * time.Minute)
	f.Send(testutil.Purchase("b", "15.5"))
	f.Advance(3 * time.Minute) // a is 6 minutes old, b is 3

	if v := f.WindowValue(count, "5m", "", ""); v != 1 {
		t.Errorf("5m = %v, want 1", v)
	}
	if v := f.WindowValue(count, "1h", "", ""); v != 2 {
		t.Errorf("1h = %v, want 2", v)
	}
	if v := f.WindowValue(sum, "24h", "", ""); v != 25.5 {
		t.Errorf("sum 24h = %v, want 25.5 (numeric strings are accepted)", v)
	}
	f.Advance(25 * time.Hour)
	if v := f.WindowValue(count, "24h", "", ""); v != 0 {
		t.Errorf("24h after 25h = %v, want 0", v)
	}
	for _, w := range []string{"7d", "30d"} {
		if v := f.WindowValue(count, w, "", ""); v != 2 {
			t.Errorf("%s = %v, want 2", w, v)
		}
	}
}

func TestPreviousWindow(t *testing.T) {
	f := testutil.New(t)
	a := f.Agg("orders", model.AggregateDef{Events: []string{"purchase"}, Op: model.OpCount})
	f.Send(testutil.Purchase("", 1), testutil.Purchase("", 1))
	f.Advance(90 * time.Minute)
	f.Send(testutil.Purchase("", 1))
	w, _ := model.WindowByName("1h")
	cur, _ := f.Q.WindowValue(context.Background(), a, w, false, "", "")
	prev, _ := f.Q.WindowValue(context.Background(), a, w, true, "", "")
	if cur != 1 || prev != 2 {
		t.Errorf("1h = %v, prev_1h = %v; want 1 and 2", cur, prev)
	}
}

func TestWindowRangeBoundaries(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 30, 45, 0, time.UTC)
	w, _ := model.WindowByName("5m")
	from, to := w.Range(now, false)
	if to != time.Date(2026, 10, 7, 12, 30, 0, 0, time.UTC).Unix() || to-from != 4*60 {
		t.Errorf("5m range = %d..%d", from, to)
	}
	pf, pt := w.Range(now, true)
	if pt != from-60 || pt-pf != 4*60 {
		t.Errorf("prev 5m range = %d..%d, must end right before %d", pf, pt, from)
	}
	d, _ := model.WindowByName("30d")
	from, to = d.Range(now, false)
	if to != time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC).Unix() || (to-from)/86400 != 29 {
		t.Errorf("30d range = %d..%d", from, to)
	}
}

func TestExplodeGroupAndRank(t *testing.T) {
	f := testutil.New(t)
	a := f.Agg("bestsellers", model.AggregateDef{Events: []string{"purchase"}, Op: model.OpSum, Explode: "props.items",
		Value:   "item.price * item.quantity",
		GroupBy: &model.Grouping{Dimension: "category", Expr: "item.category"},
		RankBy:  &model.Grouping{Dimension: "product", Expr: "item.id", Label: "item.name"}})
	f.Send(
		testutil.Purchase("1", 0, testutil.Item("73", "shoes", 100, 2), testutil.Item("12", "hats", 20, 1)),
		testutil.Purchase("2", 0, testutil.Item("74", "shoes", 50, 1), testutil.Item(" 73", "shoes", 100, 1)),
	)
	if v := f.WindowValue(a, "24h", "", ""); v != 370 {
		t.Errorf("site total = %v, want 370", v)
	}
	if v := f.WindowValue(a, "24h", "shoes", ""); v != 350 {
		t.Errorf("shoes = %v, want 350", v)
	}
	if v := f.WindowValue(a, "24h", "shoes", "73"); v != 300 {
		t.Errorf("shoes/73 = %v, want 300 (ids are trimmed)", v)
	}
	w, _ := model.WindowByName("24h")
	top, err := f.Q.Top(context.Background(), a, w, "product", map[string]string{"category": "shoes"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 2 || top[0].Key != "73" || top[0].Value != 300 || !strings.HasSuffix(top[0].Label, "73") || top[1].Key != "74" {
		t.Errorf("top products in shoes = %+v", top)
	}
	cats, _ := f.Q.Top(context.Background(), a, w, "category", nil, 10)
	if len(cats) != 2 || cats[0].Key != "shoes" {
		t.Errorf("top categories = %+v", cats)
	}
	all, _ := f.Q.Top(context.Background(), a, w, "product", nil, 10)
	if len(all) != 3 || all[0].Key != "73" {
		t.Errorf("top products overall = %+v", all)
	}
}

func TestCountDistinctAndWhere(t *testing.T) {
	f := testutil.New(t)
	a := f.Agg("viewing", model.AggregateDef{Events: []string{"view_item"}, Op: model.OpCountDistinct,
		Where: "props.items[0].price > 10", GroupBy: &model.Grouping{Dimension: "product", Expr: "props.items[0].id"}})
	view := func(visitor, id string, price float64) engine.IncomingEvent {
		return engine.IncomingEvent{Name: "view_item", VisitorID: visitor, Props: map[string]any{"items": []any{testutil.Item(id, "c", price, 1)}}}
	}
	f.Send(view("v1", "73", 20), view("v1", "73", 20), view("v2", "73", 20), view("v3", "12", 20), view("v4", "73", 5))
	if v := f.WindowValue(a, "5m", "", ""); v != 3 {
		t.Errorf("distinct visitors = %v, want 3 (v4 filtered by where)", v)
	}
	if v := f.WindowValue(a, "5m", "73", ""); v != 2 {
		t.Errorf("distinct on 73 = %v, want 2", v)
	}
}

func TestLastValueAndTimestamp(t *testing.T) {
	f := testutil.New(t)
	lv := f.Agg("last_city", model.AggregateDef{Events: []string{"purchase"}, Op: model.OpLastValue, Value: "props.currency"})
	lt := f.Agg("last_order", model.AggregateDef{Events: []string{"purchase"}, Op: model.OpLastTimestamp})
	ctx := context.Background()
	if v, _ := f.Q.LastValue(ctx, lv, ""); v != nil {
		t.Errorf("no data must be nil, got %v", v)
	}
	f.Send(testutil.Purchase("1", 1))
	f.Advance(time.Minute)
	f.Send(testutil.Purchase("2", 1))
	v, _ := f.Q.LastValue(ctx, lv, "")
	ts, _ := f.Q.LastValue(ctx, lt, "")
	if v != "PLN" || ts != f.Now.Unix() {
		t.Errorf("last value %v, last ts %v (want %d)", v, ts, f.Now.Unix())
	}
}

func TestDedupeByEventID(t *testing.T) {
	f := testutil.New(t)
	a := f.Agg("purchases", model.AggregateDef{Events: []string{"purchase"}, Op: model.OpCount})
	res := f.Send(testutil.Purchase("order-1", 1), testutil.Purchase("order-1", 1))
	f.Send(testutil.Purchase("order-1", 1))
	if res.Accepted != 1 || res.Dropped != 1 {
		t.Errorf("result %+v", res)
	}
	if v := f.WindowValue(a, "24h", "", ""); v != 1 {
		t.Errorf("count = %v, want 1", v)
	}
	// Dedupe survives a restart.
	e2 := engine.New(f.St, f.Log, engine.Options{})
	e2.Now = func() time.Time { return f.Now }
	if err := e2.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer e2.Close()
	if r, _ := e2.Ingest(f.Site.PublicKey, "", engine.RequestInfo{}, []engine.IncomingEvent{testutil.Purchase("order-1", 1)}); r.Accepted != 0 {
		t.Error("duplicate accepted after restart")
	}
}

func TestPrivacyFieldsAreStrippedServerSide(t *testing.T) {
	f := testutil.New(t)
	f.Send(engine.IncomingEvent{Name: "purchase", Props: map[string]any{
		"value": 1, "contact": map[string]any{"email": "a@b.c", "phone": "+48 600"},
		"items": []any{map[string]any{"id": "1", "Email": "x@y.z", "address": "street 1"}},
	}})
	evs, err := f.St.RecentEvents(context.Background(), f.Site.ID, store.EventQuery{Limit: 10})
	if err != nil || len(evs) != 1 {
		t.Fatal(err, len(evs))
	}
	b, _ := json.Marshal(evs[0].Props)
	for _, bad := range []string{"a@b.c", "+48", "x@y.z", "street"} {
		if strings.Contains(string(b), bad) {
			t.Errorf("stored props contain %q: %s", bad, b)
		}
	}
}

func TestEventNamesAreNormalized(t *testing.T) {
	f := testutil.New(t)
	a := f.Agg("carts", model.AggregateDef{Events: []string{"Add To Cart"}, Op: model.OpCount})
	if a.Events[0] != "add_to_cart" {
		t.Fatalf("definition events not normalized: %v", a.Events)
	}
	res := f.Send(testutil.Ev("addToCart", nil), testutil.Ev("Add to cart", nil), testutil.Ev("  ", nil), testutil.Ev("", nil))
	if res.Accepted != 2 || res.Dropped != 2 {
		t.Errorf("result %+v", res)
	}
	if v := f.WindowValue(a, "1h", "", ""); v != 2 {
		t.Errorf("count = %v", v)
	}
}

func TestOriginsAndUnknownSite(t *testing.T) {
	f := testutil.New(t)
	f.Site.Config.AllowedOrigins = []string{"https://shop.example.com", "https://*.example.org"}
	if err := f.St.UpdateSite(context.Background(), &f.Site); err != nil {
		t.Fatal(err)
	}
	f.Reload()
	ok := func(origin string) bool {
		_, err := f.Eng.Ingest(f.Site.PublicKey, origin, engine.RequestInfo{}, []engine.IncomingEvent{testutil.Ev("x", nil)})
		return err == nil
	}
	if !ok("https://shop.example.com") || !ok("https://a.example.org") || ok("https://evil.com") || ok("") || ok("http://shop.example.com") {
		t.Error("origin checks wrong")
	}
	if _, err := f.Eng.Ingest("pk_nope", "", engine.RequestInfo{}, nil); err != engine.ErrUnknownSite {
		t.Errorf("err = %v", err)
	}
}

func TestVisitorRateLimit(t *testing.T) {
	f := testutil.New(t)
	evs := make([]engine.IncomingEvent, 100)
	for i := range evs {
		evs[i] = engine.IncomingEvent{Name: "click", VisitorID: "bot"}
	}
	total := 0
	for i := 0; i < 4; i++ {
		total += f.Send(evs...).Accepted
	}
	if total != 300 {
		t.Errorf("accepted %d events from one visitor in a minute, want 300", total)
	}
	f.Advance(time.Minute)
	if r := f.Send(evs[:1]...); r.Accepted != 1 {
		t.Error("limit must reset every minute")
	}
}

func TestResetAndRebuild(t *testing.T) {
	f := testutil.New(t)
	a := f.Agg("orders", model.AggregateDef{Events: []string{"purchase"}, Op: model.OpCount})
	f.Send(testutil.Purchase("1", 10), testutil.Purchase("2", 30))
	ctx := context.Background()
	if _, err := f.Eng.Reset(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if v := f.WindowValue(a, "24h", "", ""); v != 0 {
		t.Errorf("after reset = %v", v)
	}
	// Change the definition to a sum and rebuild from raw events.
	a.Op, a.Value = model.OpSum, "props.value"
	if err := f.St.UpdateAggregate(ctx, &a); err != nil {
		t.Fatal(err)
	}
	f.Reload()
	f.Advance(time.Hour)
	n, err := f.Eng.Rebuild(ctx, a.ID)
	if err != nil || n != 2 {
		t.Fatalf("rebuild: %d events, %v", n, err)
	}
	if v := f.WindowValue(a, "24h", "", ""); v != 40 {
		t.Errorf("after rebuild = %v, want 40", v)
	}
	if v := f.WindowValue(a, "5m", "", ""); v != 0 {
		t.Errorf("rebuilt events must keep their original time; 5m = %v", v)
	}
}

func TestCardinalityCap(t *testing.T) {
	f := testutil.New(t)
	f.Eng = engine.New(f.St, f.Log, engine.Options{MaxKeysPerAggregate: 4})
	f.Eng.Now = func() time.Time { return f.Now }
	a := f.Agg("pages", model.AggregateDef{Events: []string{"page_view"}, Op: model.OpCount, GroupBy: &model.Grouping{Dimension: "page", Expr: "props.path"}})
	for _, p := range []string{"/a", "/b", "/c", "/d", "/e", "/a"} {
		f.Send(testutil.Ev("page_view", map[string]any{"path": p}))
	}
	if v := f.WindowValue(a, "1h", "", ""); v != 6 {
		t.Errorf("site total must count everything: %v", v)
	}
	if v := f.WindowValue(a, "1h", "/a", ""); v != 2 {
		t.Errorf("/a = %v", v)
	}
	if v := f.WindowValue(a, "1h", "/e", ""); v != 0 {
		t.Errorf("/e is over the cap but was stored: %v", v)
	}
}

func TestValidation(t *testing.T) {
	cases := []struct {
		def  model.AggregateDef
		want string
	}{
		{model.AggregateDef{Op: model.OpCount}, "at least one event"},
		{model.AggregateDef{Events: []string{"x"}, Op: "avg"}, "unknown operation"},
		{model.AggregateDef{Events: []string{"x"}, Op: model.OpSum}, "Value"},
		{model.AggregateDef{Events: []string{"x"}, Op: model.OpCount, Where: "item.price > 1"}, "only available when Explode"},
		{model.AggregateDef{Events: []string{"x"}, Op: model.OpCount, Explode: "props.items", Where: "items.price > 1"}, "item.<field>"},
		{model.AggregateDef{Events: []string{"x"}, Op: model.OpCount, Where: "props.a >"}, "Where"},
		{model.AggregateDef{Events: []string{"x"}, Op: model.OpCount, RankBy: &model.Grouping{Dimension: "p", Expr: "props.p"}}, "needs Group by"},
		{model.AggregateDef{Events: []string{"x"}, Op: model.OpCount, GroupBy: &model.Grouping{Dimension: "Bad Name", Expr: "props.p"}}, "dimension"},
	}
	for _, c := range cases {
		_, err := engine.Validate(&c.def)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: err %v, want %q", c.def, err, c.want)
		}
	}
	if err := model.ValidateName("orders_24h"); err == nil {
		t.Error("names with window suffix must be rejected")
	}
}

func TestDryRun(t *testing.T) {
	def := model.AggregateDef{Events: []string{"purchase"}, Op: model.OpSum, Explode: "props.items", Value: "item.price",
		GroupBy: &model.Grouping{Dimension: "product", Expr: "item.id"}}
	p := testutil.Purchase("1", 0, testutil.Item("73", "c", 10, 1), testutil.Item("12", "c", 5, 1))
	res, _, err := engine.DryRun(def, []model.Event{{Name: p.Name, Props: p.Props}, {Name: "page_view"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res[0].Matched || len(res[0].Contributions) != 2 || res[0].Contributions[1].Part != "12" || res[1].Matched {
		t.Errorf("dry run = %+v", res)
	}
}

func TestFormulas(t *testing.T) {
	f := testutil.New(t)
	rev := f.Agg("revenue", model.AggregateDef{Events: []string{"purchase"}, Op: model.OpSum, Value: "props.value"})
	ord := f.Agg("orders", model.AggregateDef{Events: []string{"purchase"}, Op: model.OpCount})
	last := f.Agg("last_order", model.AggregateDef{Events: []string{"purchase"}, Op: model.OpLastTimestamp})
	aggs := []model.Aggregate{rev, ord, last}
	sc := func() *query.Scope { return query.NewScope(f.Q, aggs, nil, nil) }
	ctx := context.Background()
	if v, _ := sc().EvalFormula(ctx, "revenue_24h / orders_24h"); v != nil {
		t.Errorf("0/0 must be nil, got %v", v)
	}
	if v, _ := sc().EvalFormula(ctx, "coalesce(last_order, 0)"); v != 0.0 {
		t.Errorf("coalesce = %v", v)
	}
	f.Send(testutil.Purchase("1", 100), testutil.Purchase("2", 50))
	if v, _ := sc().EvalFormula(ctx, "round(revenue_24h / orders_24h)"); v != 75.0 {
		t.Errorf("aov = %v", v)
	}
	if v, _ := sc().EvalFormula(ctx, "revenue_total"); v != 150.0 {
		t.Errorf("total = %v", v)
	}
	if _, _, err := query.CompileFormula("revenu_24h * 2", aggs); err == nil {
		t.Error("unknown variable must fail to compile")
	}
	if got := query.BucketValue(float64(1234)); got != "1000+" {
		t.Errorf("bucket = %v", got)
	}
	if got := query.BucketValue(float64(7)); got != 7.0 {
		t.Errorf("bucket = %v", got)
	}
}

func TestUserAgentParsing(t *testing.T) {
	cases := map[string]engine.UserAgent{
		testutil.TestUA: {Browser: "Chrome", OS: "macOS", Device: "desktop"},
		"Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1": {Browser: "Safari", OS: "iOS", Device: "mobile"},
		"Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0 Mobile Safari/537.36":                       {Browser: "Chrome", OS: "Android", Device: "mobile"},
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0 Safari/537.36 Edg/141.0":                   {Browser: "Edge", OS: "Windows", Device: "desktop"},
		"Mozilla/5.0 (X11; Linux x86_64; rv:131.0) Gecko/20100101 Firefox/131.0":                                                                  {Browser: "Firefox", OS: "Linux", Device: "desktop"},
		"Mozilla/5.0 (iPad; CPU OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/604.1":                        {Browser: "Safari", OS: "iOS", Device: "tablet"},
		"Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)":                                                                {Browser: "Other", OS: "Other", Device: "desktop", Bot: true},
		"curl/8.4.0": {Browser: "Other", OS: "Other", Device: "desktop", Bot: true},
		"":           {},
	}
	for ua, want := range cases {
		if got := engine.ParseUserAgent(ua); got != want {
			t.Errorf("%q: %+v, want %+v", ua, got, want)
		}
	}
}

func TestMetaEnrichmentAndBots(t *testing.T) {
	f := testutil.New(t)
	a := f.Agg("by_browser", model.AggregateDef{Events: []string{"page_view", "sign_up"}, Op: model.OpCount,
		GroupBy: &model.Grouping{Dimension: "browser", Expr: "meta.browser"}})
	ingest := func(ua string, ev engine.IncomingEvent) engine.IngestResult {
		res, err := f.Eng.Ingest(f.Site.PublicKey, "", engine.RequestInfo{UserAgent: ua, IP: "203.0.113.7"}, []engine.IncomingEvent{ev})
		if err != nil {
			t.Fatal(err)
		}
		f.Eng.Flush(context.Background())
		return res
	}
	pv := engine.IncomingEvent{Name: "page_view", Meta: map[string]any{"path": "/a", "referrer": "news.example", "language": "pl", "evil": "x"}}
	ingest(testutil.TestUA, pv)
	if r := ingest("Mozilla/5.0 (compatible; Googlebot/2.1)", pv); r.Accepted != 0 {
		t.Error("page views from bots must be dropped")
	}
	if r := ingest("python-requests/2.32", engine.IncomingEvent{Name: "sign_up"}); r.Accepted != 1 {
		t.Error("custom events from scripts must be kept")
	}
	evs, _ := f.St.RecentEvents(context.Background(), f.Site.ID, store.EventQuery{Limit: 10})
	got := map[string]any{}
	for k, v := range evs[1].Meta {
		got[k] = v
	}
	want := map[string]any{"path": "/a", "referrer": "news.example", "language": "pl", "browser": "Chrome", "os": "macOS", "device": "desktop"}
	if len(got) != len(want) {
		t.Fatalf("meta %v, want %v (no ip unless enabled, no unknown client keys)", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("meta.%s = %v, want %v", k, got[k], v)
		}
	}
	if evs[0].Meta["bot"] != true {
		t.Errorf("script event meta %v must have bot=true", evs[0].Meta)
	}
	if v := f.WindowValue(a, "1h", "Chrome", ""); v != 1 {
		t.Errorf("group by meta.browser = %v", v)
	}
	f.Site.Config.CollectIP = true
	f.St.UpdateSite(context.Background(), &f.Site)
	f.Reload()
	ingest(testutil.TestUA, engine.IncomingEvent{Name: "sign_up"})
	evs, _ = f.St.RecentEvents(context.Background(), f.Site.ID, store.EventQuery{Limit: 1})
	if evs[0].Meta["ip"] != "203.0.113.7" {
		t.Errorf("collectIp: meta %v", evs[0].Meta)
	}
}

// geoFixture is a fixture whose engine looks locations up in a fake GeoIP service; n counts its requests.
func geoFixture(t *testing.T) (f *testutil.Fixture, n *atomic.Int64) {
	n = new(atomic.Int64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.Write([]byte(`{"city":"Lisbon","country_iso":"PT","country":"Portugal","latitude":38.7,"has_data":true}`))
	}))
	t.Cleanup(srv.Close)
	g, err := geo.New(srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	f = testutil.New(t)
	f.Eng = engine.New(f.St, f.Log, engine.Options{Geo: g})
	f.Eng.Now = func() time.Time { return f.Now }
	f.Reload()
	return f, n
}

func lastMeta(t *testing.T, f *testutil.Fixture, ip string) map[string]any {
	t.Helper()
	if _, err := f.Eng.Ingest(f.Site.PublicKey, "", engine.RequestInfo{UserAgent: testutil.TestUA, IP: ip}, []engine.IncomingEvent{{Name: "sign_up"}}); err != nil {
		t.Fatal(err)
	}
	f.Eng.Flush(context.Background())
	evs, _ := f.St.RecentEvents(context.Background(), f.Site.ID, store.EventQuery{Limit: 1})
	return evs[0].Meta
}

func setGeo(f *testutil.Fixture, mode string, collectIP bool) {
	f.Site.Config.Geo, f.Site.Config.CollectIP = mode, collectIP
	f.St.UpdateSite(context.Background(), &f.Site)
	f.Reload()
}

func TestGeoMeta(t *testing.T) {
	f, n := geoFixture(t)
	m := lastMeta(t, f, "85.0.0.1") // default mode: country, and no ip because collectIp is off
	if m["country"] != "PT" || m["city"] != nil || m["ip"] != nil {
		t.Errorf("country mode: %v", m)
	}
	setGeo(f, model.GeoCity, false)
	if m = lastMeta(t, f, "85.0.0.1"); m["country"] != "PT" || m["city"] != "Lisbon" || m["ip"] != nil {
		t.Errorf("city mode: %v", m)
	}
	setGeo(f, model.GeoCountry, true)
	if m = lastMeta(t, f, "85.0.0.1"); m["country"] != "PT" || m["ip"] != "85.0.0.1" {
		t.Errorf("collectIp: %v", m)
	}
	before := n.Load()
	setGeo(f, model.GeoOff, false)
	if m = lastMeta(t, f, "85.0.0.9"); m["country"] != nil || m["city"] != nil || n.Load() != before {
		t.Errorf("off must add nothing and call nothing: %v (%d requests)", m, n.Load()-before)
	}
}

func TestNoGeoClientLeavesMetaUnchanged(t *testing.T) {
	f := testutil.New(t) // default site config has geo "country", but there is no client
	want := map[string]any{"browser": "Chrome", "os": "macOS", "device": "desktop"}
	m := lastMeta(t, f, "85.0.0.1")
	if len(m) != len(want) {
		t.Errorf("meta %v, want %v", m, want)
	}
}

func TestGeoModeNormalization(t *testing.T) {
	for in, want := range map[string]string{"": "country", "bogus": "country", "off": "off", "city": "city", "country": "country"} {
		c := model.SiteConfig{Geo: in}
		c.Normalize()
		if c.Geo != want {
			t.Errorf("geo %q normalized to %q, want %q", in, c.Geo, want)
		}
	}
	if model.DefaultSiteConfig().Geo != model.GeoCountry {
		t.Error("default must be country")
	}
}
