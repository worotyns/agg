// Package geo looks up country and city for an IP address by calling an external GeoIP lookup service
// (GET {base}/lookup?ip=… → JSON with "country_iso" and "city"). The IP is never stored here.
package geo

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	cacheTTL   = time.Hour
	cacheMax   = 10000
	breakerFor = 30 * time.Second
)

// Info is the only location data agg keeps: ISO 3166-1 alpha-2 country code and city name.
type Info struct{ Country, City string }

type entry struct {
	info Info
	exp  time.Time
}

// Client talks to the lookup service. A nil *Client is valid and returns empty results.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	Log     *slog.Logger

	Lookups  atomic.Int64 // requests sent to the service (cache hits are not counted)
	Failures atomic.Int64 // requests that failed (transport, timeout, unexpected status)

	now func() time.Time
	mu  sync.Mutex
	// cache holds results by IP; openUntil is the end of the window in which calls are skipped.
	cache     map[string]entry
	openUntil time.Time
	open      bool
}

// New validates the base URL (http or https, with a host) and returns a client.
func New(base string, log *slog.Logger) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid GeoIP URL %q: want http(s)://host[:port]", base)
	}
	return &Client{BaseURL: strings.TrimRight(base, "/"), HTTP: &http.Client{Timeout: 300 * time.Millisecond}, Log: log, now: time.Now, cache: map[string]entry{}}, nil
}

// Lookup returns the location of ip, or an empty Info when unknown, not routable or the service is unavailable.
func (c *Client) Lookup(ctx context.Context, ip string) Info {
	if c == nil {
		return Info{}
	}
	a, err := netip.ParseAddr(ip)
	if err != nil || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsUnspecified() {
		return Info{}
	}
	now := c.now()
	c.mu.Lock()
	if e, ok := c.cache[ip]; ok && now.Before(e.exp) {
		c.mu.Unlock()
		return e.info
	}
	skip := now.Before(c.openUntil)
	c.mu.Unlock()
	if skip {
		return Info{}
	}
	c.Lookups.Add(1)
	info, ok := c.fetch(ctx, ip)
	c.mu.Lock()
	defer c.mu.Unlock()
	if !ok {
		c.Failures.Add(1)
		c.openUntil = now.Add(breakerFor)
		if !c.open {
			c.open = true
			c.log().Warn("geoip lookups failing, pausing for 30s", "url", c.BaseURL)
		}
		return Info{}
	}
	if c.open {
		c.open = false
		c.log().Info("geoip lookups working again")
	}
	if len(c.cache) >= cacheMax {
		c.cache = map[string]entry{}
	}
	c.cache[ip] = entry{info, now.Add(cacheTTL)}
	return info
}

// fetch reports ok=false on transport errors and on statuses other than 200 and 400.
func (c *Client) fetch(ctx context.Context, ip string) (Info, bool) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.BaseURL+"/lookup?ip="+url.QueryEscape(ip), nil)
	if err != nil {
		return Info{}, false
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Info{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusBadRequest {
		return Info{}, true
	}
	if resp.StatusCode != http.StatusOK {
		return Info{}, false
	}
	var r struct {
		City       string `json:"city"`
		CountryISO string `json:"country_iso"`
		HasData    bool   `json:"has_data"`
	}
	if json.NewDecoder(resp.Body).Decode(&r) != nil {
		return Info{}, false
	}
	if !r.HasData {
		return Info{}, true
	}
	return Info{Country: r.CountryISO, City: r.City}, true
}

func (c *Client) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}
