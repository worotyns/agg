package server_test

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/expfmt"
	pmodel "github.com/prometheus/common/model"
	"github.com/worotyns/agg/internal/alert"
	"github.com/worotyns/agg/internal/export"
	"github.com/worotyns/agg/internal/server"
	"github.com/worotyns/agg/internal/testutil"
	"github.com/worotyns/agg/internal/webpush"
)

const adminToken = "test-admin-token"

type app struct {
	*testutil.Fixture
	srv    *httptest.Server
	client *http.Client
}

func newApp(t *testing.T) *app {
	return newAppWith(t, server.Config{Version: "test", InternalMetricsToken: "imt"})
}

func newAppWith(t *testing.T, cfg server.Config) *app {
	f := testutil.New(t)
	if _, err := server.EnsureAdminToken(t.Context(), f.St, adminToken); err != nil {
		t.Fatal(err)
	}
	vapid, _ := webpush.GenerateVAPID()
	alerts := &alert.Engine{St: f.St, Push: &webpush.Sender{VAPID: vapid}, Log: f.Log, Now: func() time.Time { return f.Now }}
	s := server.New(cfg, f.St, f.Eng, alerts, f.Log)
	s.SetNow(func() time.Time { return f.Now })
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	return &app{Fixture: f, srv: srv, client: &http.Client{Jar: jar}}
}

func (a *app) do(method, path string, body any, hdr ...string) (*http.Response, []byte) {
	a.T.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	req, _ := http.NewRequest(method, a.srv.URL+path, rd)
	req.Header.Set("User-Agent", testutil.TestUA)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := a.client.Do(req)
	if err != nil {
		a.T.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return res, out
}

func (a *app) admin(method, path string, body any) map[string]any {
	a.T.Helper()
	res, out := a.do(method, path, body, "Authorization", "Bearer "+adminToken)
	if res.StatusCode >= 300 {
		a.T.Fatalf("%s %s: %d %s", method, path, res.StatusCode, out)
	}
	var m map[string]any
	json.Unmarshal(out, &m)
	return m
}

func (a *app) flush() {
	if err := a.Eng.Flush(a.T.(*testing.T).Context()); err != nil {
		a.T.Fatal(err)
	}
}

// The limit is 5 failed attempts per minute per client IP.
func TestAuthFailureRateLimit(t *testing.T) {
	a := newApp(t)
	// Successful logins do not count against the limit.
	for i := 0; i < 7; i++ {
		if res, _ := a.do("POST", "/api/login", map[string]string{"token": adminToken}); res.StatusCode != 200 {
			t.Fatalf("login %d: %d", i, res.StatusCode)
		}
	}
	// Failures across login, Bearer and MCP share one per-IP budget.
	for i := 0; i < 5; i++ {
		var res *http.Response
		switch i % 3 {
		case 0:
			res, _ = a.do("POST", "/api/login", map[string]string{"token": "wrong"})
		case 1:
			res, _ = a.do("GET", "/api/sites", nil, "Authorization", "Bearer wrong")
		default:
			res, _ = a.do("POST", "/mcp", `{}`, "Authorization", "Bearer wrong")
		}
		if res.StatusCode != 401 {
			t.Fatalf("failure %d: %d", i, res.StatusCode)
		}
	}
	// Once exhausted, even the correct token is refused until the bucket refills.
	res, _ := a.do("POST", "/api/login", map[string]string{"token": adminToken})
	if res.StatusCode != 429 || res.Header.Get("Retry-After") == "" {
		t.Fatalf("login after limit: %d", res.StatusCode)
	}
	if res, _ := a.do("GET", "/api/sites", nil, "Authorization", "Bearer "+adminToken); res.StatusCode != 429 {
		t.Fatalf("bearer after limit: %d", res.StatusCode)
	}
}

func TestLoginAndAdminAuth(t *testing.T) {
	a := newApp(t)
	if res, _ := a.do("GET", "/api/sites", nil); res.StatusCode != 401 {
		t.Fatalf("unauthenticated: %d", res.StatusCode)
	}
	if res, _ := a.do("POST", "/api/login", map[string]string{"token": "wrong"}); res.StatusCode != 401 {
		t.Fatalf("wrong token: %d", res.StatusCode)
	}
	if res, _ := a.do("POST", "/api/login", map[string]string{"token": adminToken}); res.StatusCode != 200 {
		t.Fatalf("login: %d", res.StatusCode)
	}
	if res, _ := a.do("GET", "/api/sites", nil); res.StatusCode != 200 {
		t.Fatalf("with session: %d", res.StatusCode)
	}
	if res, _ := a.do("POST", "/api/sites", map[string]string{"name": "x"}, "Origin", "https://evil.example"); res.StatusCode != 403 {
		t.Fatalf("cross-origin write with cookie: %d", res.StatusCode)
	}
	a.do("POST", "/api/logout", nil)
	if res, _ := a.do("GET", "/api/sites", nil); res.StatusCode != 401 {
		t.Fatalf("after logout: %d", res.StatusCode)
	}
}

func TestIngestToPublicValues(t *testing.T) {
	a := newApp(t)
	site := a.admin("POST", "/api/sites", map[string]string{"name": "My Shop"})
	id := int(site["id"].(float64))
	key := site["publicKey"].(string)
	base := "/api/sites/" + itoa(id)
	a.admin("POST", base+"/aggregates", map[string]any{"name": "purchases", "events": []string{"purchase"}, "op": "count",
		"visibility": "public", "explode": "props.items", "groupBy": map[string]string{"dimension": "product", "expr": "item.id"}})
	a.admin("POST", base+"/aggregates", map[string]any{"name": "revenue", "events": []string{"purchase"}, "op": "sum", "value": "props.value"})

	payload := `{"site":"` + key + `","events":[
		{"name":"page_view","visitorId":"v1","props":{"path":"/"}},
		{"name":"purchase","id":"o1","props":{"value":120,"items":[{"id":"73"},{"id":"12"}],"contact":{"email":"a@b.c"}}},
		{"name":"purchase","id":"o2","props":{"value":30,"items":[{"id":"73"}]}}]}`
	res, out := a.do("POST", "/e", payload, "Content-Type", "text/plain")
	if res.StatusCode != 202 || res.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("ingest: %d %s", res.StatusCode, out)
	}
	a.flush()

	res, out = a.do("GET", "/v1/values?site="+key+"&v=purchases_24h,revenue_24h,nope&product=73", nil)
	if res.StatusCode != 200 || !strings.Contains(res.Header.Get("Cache-Control"), "max-age") {
		t.Fatalf("values: %d", res.StatusCode)
	}
	var v struct {
		Values  map[string]any `json:"values"`
		Unknown []string       `json:"unknown"`
	}
	json.Unmarshal(out, &v)
	if v.Values["purchases_24h"] != 2.0 {
		t.Errorf("public purchases for product 73 = %v, want 2", v.Values["purchases_24h"])
	}
	if v.Values["revenue_24h"] != nil || len(v.Unknown) != 2 {
		t.Errorf("private aggregate leaked or unknown not reported: %s", out)
	}
	_, out = a.do("GET", "/v1/top?site="+key+"&aggregate=purchases&window=24h", nil)
	if !strings.Contains(string(out), `"key":"73","value":2`) {
		t.Errorf("top: %s", out)
	}
	// Starter aggregates exist and counted the page view.
	vals := a.admin("GET", base+"/values?v=page_views_24h,visitors_24h,revenue_24h", nil)["values"].(map[string]any)
	if vals["page_views_24h"] != 1.0 || vals["visitors_24h"] != 1.0 || vals["revenue_24h"] != 150.0 {
		t.Errorf("admin values: %v", vals)
	}
	// Personal data never reaches storage.
	_, evs := a.do("GET", base+"/events", nil, "Authorization", "Bearer "+adminToken)
	if strings.Contains(string(evs), "a@b.c") {
		t.Error("personal data stored")
	}
}

func TestSDKConfigAndScript(t *testing.T) {
	a := newApp(t)
	res, out := a.do("GET", "/v1/config?site="+a.Site.PublicKey, nil)
	if res.StatusCode != 200 || !strings.Contains(string(out), `"pageViews":true`) || !strings.Contains(string(out), `"pageTime":true`) || strings.Contains(string(out), "layers") {
		t.Errorf("config: %d %s", res.StatusCode, out)
	}
	if res, _ := a.do("GET", "/v1/config?site=pk_missing", nil); res.StatusCode != 404 {
		t.Error("unknown site config must 404")
	}
	res, out = a.do("GET", "/agg.js", nil)
	if res.StatusCode != 200 || !strings.Contains(res.Header.Get("Content-Type"), "javascript") || !bytes.Contains(out, []byte("sendBeacon")) {
		t.Error("sdk not served")
	}
}

// ---- export endpoint

func (a *app) createExport(format string) (id, token string) {
	a.T.Helper()
	base := "/api/sites/" + itoa(int(a.Site.ID))
	a.admin("POST", base+"/aggregates", map[string]any{"name": "purchases", "events": []string{"purchase"}, "op": "count"})
	a.Send(testutil.Purchase("o1", 10), testutil.Purchase("o2", 10))
	m := a.admin("POST", base+"/exports", map[string]any{"name": "prom", "format": format,
		"scope": map[string]any{"aggregates": []string{"purchases"}, "windows": []string{"1h", "24h"}}})
	return m["export"].(map[string]any)["id"].(string), m["token"].(string)
}

func TestExportEndpointAuth(t *testing.T) {
	a := newApp(t)
	id, token := a.createExport("prometheus")
	url := "/export/" + id + "/metrics"
	for name, hdr := range map[string][]string{
		"no token":    nil,
		"wrong token": {"Authorization", "Bearer agg_exp_wrong"},
		"admin token": {"Authorization", "Bearer " + adminToken},
	} {
		res, _ := a.do("GET", url, nil, hdr...)
		if res.StatusCode != 401 || res.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("%s: status %d, want 401 with WWW-Authenticate", name, res.StatusCode)
		}
	}
	if res, _ := a.do("GET", "/export/unknown/metrics", nil, "Authorization", "Bearer "+token); res.StatusCode != 401 {
		t.Errorf("unknown export with a valid token of another export: %d", res.StatusCode)
	}
	if res, _ := a.do("GET", url, nil, "Authorization", "Bearer "+token); res.StatusCode != 200 {
		t.Errorf("bearer: %d", res.StatusCode)
	}
	if res, _ := a.do("GET", url+"?token="+token, nil); res.StatusCode != 200 {
		t.Errorf("query token: %d", res.StatusCode)
	}
	if res, _ := a.do("POST", url, nil, "Authorization", "Bearer "+token); res.StatusCode != 405 {
		t.Errorf("export must be read-only, POST = %d", res.StatusCode)
	}
	// Rotating invalidates the old token immediately.
	m := a.admin("POST", "/api/sites/"+itoa(int(a.Site.ID))+"/exports/"+id+"/rotate", nil)
	if res, _ := a.do("GET", url, nil, "Authorization", "Bearer "+token); res.StatusCode != 401 {
		t.Errorf("old token after rotate: %d", res.StatusCode)
	}
	if res, _ := a.do("GET", url, nil, "Authorization", "Bearer "+m["token"].(string)); res.StatusCode != 200 {
		t.Errorf("new token after rotate: %d", res.StatusCode)
	}
	// Deleting revokes.
	a.admin("DELETE", "/api/sites/"+itoa(int(a.Site.ID))+"/exports/"+id, nil)
	if res, _ := a.do("GET", url, nil, "Authorization", "Bearer "+m["token"].(string)); res.StatusCode != 401 {
		t.Errorf("deleted export: %d", res.StatusCode)
	}
}

func TestExportEndpointLikeAPrometheusScrape(t *testing.T) {
	a := newApp(t)
	id, token := a.createExport("prometheus")
	url := "/export/" + id + "/metrics"
	// Headers sent by Prometheus 3 when scraping.
	hdr := []string{"Authorization", "Bearer " + token, "Accept-Encoding", "gzip",
		"Accept", "application/openmetrics-text;version=1.0.0;q=0.5,text/plain;version=0.0.4;q=0.3,*/*;q=0.1",
		"X-Prometheus-Scrape-Timeout-Seconds", "10"}
	req, _ := http.NewRequest("GET", a.srv.URL+url, nil)
	for i := 0; i < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	// Use a transport that does not transparently decompress, like Prometheus.
	res, err := (&http.Client{Transport: &http.Transport{DisableCompression: true}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); ct != export.ContentTypePrometheus {
		t.Errorf("Content-Type %q, want %q", ct, export.ContentTypePrometheus)
	}
	if expfmt.ResponseFormat(res.Header).FormatType() != expfmt.TypeTextPlain {
		t.Errorf("Prometheus does not recognise the format: %v", expfmt.ResponseFormat(res.Header))
	}
	body := res.Body.(io.Reader)
	if res.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		body = gz
	}
	p := expfmt.NewTextParser(pmodel.LegacyValidation)
	fams, err := p.TextToMetricFamilies(body)
	if err != nil {
		t.Fatal(err)
	}
	if v := fams["agg_value"]; v == nil || len(v.Metric) != 2 {
		t.Errorf("agg_value series: %v", v)
	}
	if fams["agg_events_total"].Metric[0].Counter.GetValue() != 2 {
		t.Error("counter value")
	}
	// Scrapes are recorded.
	_, list := a.do("GET", "/api/sites/"+itoa(int(a.Site.ID))+"/exports", nil, "Authorization", "Bearer "+adminToken)
	if !strings.Contains(string(list), `"scrapeCount":1`) {
		t.Errorf("scrape not recorded: %s", list)
	}
}

func TestExportGzipNegotiation(t *testing.T) {
	a := newApp(t)
	id, token := a.createExport("prometheus")
	// Make the body large enough to be compressed.
	for i := 0; i < 30; i++ {
		a.admin("POST", "/api/sites/"+itoa(int(a.Site.ID))+"/aggregates", map[string]any{"name": "agg" + itoa(i), "events": []string{"x"}, "op": "count"})
	}
	names := []string{}
	for i := 0; i < 30; i++ {
		names = append(names, "agg"+itoa(i))
	}
	a.admin("PUT", "/api/sites/"+itoa(int(a.Site.ID))+"/exports/"+id, map[string]any{"name": "prom", "format": "prometheus",
		"scope": map[string]any{"aggregates": names, "windows": []string{"1h", "24h"}}})
	req, _ := http.NewRequest("GET", a.srv.URL+"/export/"+id+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept-Encoding", "gzip")
	res, err := (&http.Client{Transport: &http.Transport{DisableCompression: true}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.Header.Get("Content-Encoding") != "gzip" {
		t.Error("large response must be gzip-compressed when the scraper accepts it")
	}
	req.Header.Del("Accept-Encoding")
	res, _ = (&http.Client{Transport: &http.Transport{DisableCompression: true}}).Do(req)
	res.Body.Close()
	if res.Header.Get("Content-Encoding") != "" {
		t.Error("must not compress without Accept-Encoding")
	}
}

func TestExportJSONFormat(t *testing.T) {
	a := newApp(t)
	id, token := a.createExport("json")
	res, out := a.do("GET", "/export/"+id+"/metrics", nil, "Authorization", "Bearer "+token)
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("json export: %d", res.StatusCode)
	}
	var body struct {
		Metrics []export.JSONSample `json:"metrics"`
	}
	if err := json.Unmarshal(out, &body); err != nil || len(body.Metrics) != 3 {
		t.Errorf("json export body: %v %s", err, out)
	}
}

func TestInternalMetrics(t *testing.T) {
	a := newApp(t)
	if res, _ := a.do("GET", "/internal/metrics", nil); res.StatusCode != 401 {
		t.Errorf("internal metrics without token: %d", res.StatusCode)
	}
	res, out := a.do("GET", "/internal/metrics", nil, "Authorization", "Bearer imt")
	if res.StatusCode != 200 {
		t.Fatal(res.StatusCode)
	}
	p := expfmt.NewTextParser(pmodel.LegacyValidation)
	if _, err := p.TextToMetricFamilies(bytes.NewReader(out)); err != nil {
		t.Errorf("internal metrics do not parse: %v\n%s", err, out)
	}
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

func TestRecentEventsFilters(t *testing.T) {
	a := newApp(t)
	site := a.admin("POST", "/api/sites", map[string]any{"name": "Audit", "slug": "audit", "preset": "none"})
	key, base := site["publicKey"].(string), "/api/sites/"+itoa(int(site["id"].(float64)))
	payload := `{"site":"` + key + `","events":[
		{"name":"sign_up","visitorId":"v1"},{"name":"page_view","visitorId":"v2"},
		{"name":"sign_up","visitorId":"v2"},{"name":"page_view","visitorId":"v1"}]}`
	if res, out := a.do("POST", "/e", payload, "Content-Type", "text/plain"); res.StatusCode != 202 {
		t.Fatalf("ingest: %d %s", res.StatusCode, out)
	}
	list := func(qs string) []map[string]any {
		res, out := a.do("GET", base+"/events?"+qs, nil, "Authorization", "Bearer "+adminToken)
		if res.StatusCode != 200 {
			t.Fatalf("events?%s: %d %s", qs, res.StatusCode, out)
		}
		var evs []map[string]any
		json.Unmarshal(out, &evs)
		return evs
	}
	if evs := list("limit=10"); len(evs) != 4 || evs[0]["visitorId"] != "v1" || evs[0]["name"] != "page_view" {
		t.Fatalf("all, newest first: %v", evs)
	}
	if evs := list("visitor=v2"); len(evs) != 2 {
		t.Fatalf("visitor filter: %v", evs)
	}
	if evs := list("visitor=v1&name=Sign%20Up"); len(evs) != 1 {
		t.Fatalf("visitor + normalized name: %v", evs)
	}
	page1 := list("limit=2")
	page2 := list("limit=2&before=" + itoa(int(page1[1]["rawId"].(float64))))
	if len(page2) != 2 || page2[0]["rawId"].(float64) >= page1[1]["rawId"].(float64) {
		t.Fatalf("paging: %v then %v", page1, page2)
	}
}

// With a client IP header, a forged X-Forwarded-For does not give an attacker fresh rate-limit buckets.
func TestClientIPHeaderBeatsForwardedFor(t *testing.T) {
	a := newAppWith(t, server.Config{Version: "test", TrustProxy: true, ClientIPHeader: "Fly-Client-IP"})
	login := func(token, xff string) int {
		res, _ := a.do("POST", "/api/login", map[string]string{"token": token}, "Fly-Client-IP", "203.0.113.7", "X-Forwarded-For", xff)
		return res.StatusCode
	}
	for i := 0; i < 5; i++ {
		if code := login("wrong", "10.0.0."+itoa(i)); code != 401 {
			t.Fatalf("failure %d: %d", i, code)
		}
	}
	if code := login("wrong", "10.0.0.99"); code != 429 {
		t.Fatalf("new X-Forwarded-For escaped the limit: %d", code)
	}
}

func TestPageTimeAggregatesEndToEnd(t *testing.T) {
	a := newApp(t)
	base := "/api/sites/" + itoa(int(a.Site.ID))
	by := map[string]string{"dimension": "page", "expr": "meta.path"}
	for _, op := range []string{"avg", "max", "p95"} {
		m := a.admin("POST", base+"/aggregates", map[string]any{"name": "time_" + op, "events": []string{"page_leave"}, "op": op,
			"value": "props.ms", "visibility": "public", "groupBy": by})
		if m["error"] != nil {
			t.Fatalf("create %s: %v", op, m)
		}
	}
	var evs []string
	for _, ms := range []int{1000, 2000, 3000, 4000, 90000} {
		evs = append(evs, `{"name":"page_leave","props":{"ms":`+itoa(ms)+`},"meta":{"path":"/docs"}}`)
	}
	res, out := a.do("POST", "/e", `{"site":"`+a.Site.PublicKey+`","events":[`+strings.Join(evs, ",")+`]}`, "Content-Type", "text/plain")
	if res.StatusCode != 202 {
		t.Fatalf("ingest: %d %s", res.StatusCode, out)
	}
	a.flush()

	res, out = a.do("GET", "/v1/values?site="+a.Site.PublicKey+"&v=time_avg_24h,time_max_24h,time_p95_24h,time_max_total", nil)
	var v struct {
		Values  map[string]float64 `json:"values"`
		Unknown []string           `json:"unknown"`
	}
	json.Unmarshal(out, &v)
	if v.Values["time_avg_24h"] != 20000 || v.Values["time_max_24h"] != 90000 {
		t.Errorf("values: %s", out)
	}
	if p := v.Values["time_p95_24h"]; p < 87000 || p > 93000 {
		t.Errorf("p95 = %v, want ≈90000", p)
	}
	if len(v.Unknown) != 1 || v.Unknown[0] != "time_max_total" {
		t.Errorf("max has no all-time variable, unknown = %v", v.Unknown)
	}
	_, out = a.do("GET", "/v1/top?site="+a.Site.PublicKey+"&aggregate=time_avg&window=24h", nil)
	if !strings.Contains(string(out), `"key":"/docs","value":20000`) {
		t.Errorf("top by average: %s", out)
	}
	_, out = a.do("GET", base+"/aggregates", nil, "Authorization", "Bearer "+adminToken)
	if !strings.Contains(string(out), "time_p95_24h") || strings.Contains(string(out), "time_p95_total") {
		t.Errorf("aggregate list must offer the p95 window variables and no total: %s", out)
	}

	m := a.admin("POST", base+"/exports", map[string]any{"name": "prom", "format": "prometheus",
		"scope": map[string]any{"aggregates": []string{"time_p95", "time_avg"}, "windows": []string{"24h"}}})
	exp := m["export"].(map[string]any)["id"].(string)
	_, out = a.do("GET", "/export/"+exp+"/metrics", nil, "Authorization", "Bearer "+m["token"].(string))
	if !strings.Contains(string(out), `agg_value{site="`+a.Site.Slug+`",aggregate="time_avg",window="24h"} 20000`) ||
		!strings.Contains(string(out), `aggregate="time_p95",window="24h"}`) {
		t.Errorf("prometheus export:\n%s", out)
	}
}
