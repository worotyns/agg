package geo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func newTest(t *testing.T, h http.HandlerFunc) (*Client, *atomic.Int64, *time.Time) {
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	return c, &n, &now
}

const lisbon = `{"ip":"85.0.0.1","city":"Lisbon","country":"Portugal","country_iso":"PT","timezone":"Europe/Lisbon","latitude":38.7,"longitude":-9.1,"has_data":true,"error":""}`

func ok(w http.ResponseWriter, r *http.Request) { w.Write([]byte(lisbon)) }

func TestLookupAndCache(t *testing.T) {
	c, n, now := newTest(t, ok)
	ctx := context.Background()
	if got := c.Lookup(ctx, "85.0.0.1"); got != (Info{"PT", "Lisbon"}) {
		t.Fatalf("got %+v", got)
	}
	c.Lookup(ctx, "85.0.0.1")
	if n.Load() != 1 || c.Lookups.Load() != 1 {
		t.Errorf("cache hit must not call the service: %d requests", n.Load())
	}
	*now = now.Add(2 * time.Hour)
	c.Lookup(ctx, "85.0.0.1")
	if n.Load() != 2 {
		t.Errorf("expired entry must be fetched again: %d requests", n.Load())
	}
}

func TestSkipsUnroutableAndBadIPs(t *testing.T) {
	c, n, _ := newTest(t, ok)
	for _, ip := range []string{"", "nope", "10.0.0.1", "192.168.1.1", "127.0.0.1", "::1", "169.254.1.1", "0.0.0.0", "fe80::1"} {
		if got := c.Lookup(context.Background(), ip); got != (Info{}) {
			t.Errorf("%q: %+v", ip, got)
		}
	}
	if n.Load() != 0 {
		t.Errorf("%d requests for unroutable IPs", n.Load())
	}
}

func TestNoDataAndBadRequestAreCached(t *testing.T) {
	c, n, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ip") == "85.0.0.2" {
			w.WriteHeader(400)
			return
		}
		w.Write([]byte(`{"has_data":false}`))
	})
	for i := 0; i < 2; i++ {
		c.Lookup(context.Background(), "85.0.0.2")
		c.Lookup(context.Background(), "85.0.0.3")
	}
	if n.Load() != 2 || c.Failures.Load() != 0 {
		t.Errorf("requests %d failures %d", n.Load(), c.Failures.Load())
	}
}

func TestBreakerOpensOn503(t *testing.T) {
	status := 503
	c, n, now := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if status != 200 {
			w.WriteHeader(status)
			return
		}
		ok(w, r)
	})
	ctx := context.Background()
	c.Lookup(ctx, "85.0.0.1")
	c.Lookup(ctx, "85.0.0.4")
	if n.Load() != 1 || c.Failures.Load() != 1 {
		t.Fatalf("breaker must skip calls while open: %d requests, %d failures", n.Load(), c.Failures.Load())
	}
	status = 200
	*now = now.Add(31 * time.Second)
	if got := c.Lookup(ctx, "85.0.0.1"); got.Country != "PT" {
		t.Errorf("must recover after the window: %+v", got)
	}
}

func TestTimeout(t *testing.T) {
	c, _, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) { time.Sleep(600 * time.Millisecond) })
	start := time.Now()
	if got := c.Lookup(context.Background(), "85.0.0.1"); got != (Info{}) {
		t.Errorf("got %+v", got)
	}
	if time.Since(start) > 550*time.Millisecond || c.Failures.Load() != 1 {
		t.Errorf("timeout not enforced (%v) or failure not counted", time.Since(start))
	}
}

func TestNilClientAndValidation(t *testing.T) {
	var c *Client
	if c.Lookup(context.Background(), "85.0.0.1") != (Info{}) {
		t.Error("nil client must return empty Info")
	}
	for _, u := range []string{"", "geoip:8082", "ftp://geoip", "http://", "://x"} {
		if _, err := New(u, nil); err == nil {
			t.Errorf("%q must be rejected", u)
		}
	}
	if _, err := New("http://geoip:8082", nil); err != nil {
		t.Error(err)
	}
}
