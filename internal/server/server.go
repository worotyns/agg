// Package server is the HTTP layer: public ingest and values API, admin API, exports and the embedded UI.
package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/worotyns/agg/internal/alert"
	"github.com/worotyns/agg/internal/engine"
	"github.com/worotyns/agg/internal/model"
	"github.com/worotyns/agg/internal/query"
	"github.com/worotyns/agg/internal/store"
	"github.com/worotyns/agg/web"
)

type Config struct {
	PublicURL            string // optional, used in snippets and scrape configs; derived from the request otherwise
	TrustProxy           bool   // use X-Forwarded-For / X-Forwarded-Proto
	ClientIPHeader       string // header a proxy sets to the client IP (Fly-Client-IP, CF-Connecting-IP); wins over X-Forwarded-For
	InternalMetricsToken string // enables /internal/metrics when set
	Version              string
}

type Server struct {
	cfg    Config
	st     store.Storage
	eng    *engine.Engine
	alerts *alert.Engine
	q      *query.Querier
	log    *slog.Logger
	now    func() time.Time
	mux    *http.ServeMux
	cache  *ttlCache

	ingestLimit *limiter
	authLimit   *limiter // failed admin/API token attempts per client IP
	exportLimit *limiter

	secretOnce sync.Once
	secret     []byte
}

// New builds the HTTP server. alerts may be nil (alerting disabled).
func New(cfg Config, st store.Storage, eng *engine.Engine, alerts *alert.Engine, log *slog.Logger) *Server {
	s := &Server{
		cfg: cfg, st: st, eng: eng, alerts: alerts, q: query.New(st), log: log, now: time.Now,
		cache:       newTTLCache(20000),
		ingestLimit: newLimiter(50, 200),
		authLimit:   newLimiter(authFailuresPerMinute/60.0, authFailuresPerMinute),
		exportLimit: newLimiter(5, 20),
	}
	s.q.Now = func() time.Time { return s.now() }
	if s.alerts != nil {
		if s.alerts.Q == nil {
			s.alerts.Q = s.q
		}
		if s.alerts.BaseURL == nil {
			s.alerts.BaseURL = s.storedBaseURL
		}
	}
	s.routes()
	return s
}

// SetNow overrides the clock (tests).
func (s *Server) SetNow(f func() time.Time) {
	s.now = f
	s.eng.Now = f
	if s.alerts != nil {
		s.alerts.Now = f
	}
}

const settingBaseURL = "base_url"

// storedBaseURL is the configured public URL, or the last URL the admin UI was opened with (for links in alerts).
func (s *Server) storedBaseURL() string {
	if s.cfg.PublicURL != "" {
		return strings.TrimRight(s.cfg.PublicURL, "/")
	}
	v, _ := s.st.GetSetting(context.Background(), settingBaseURL)
	return v
}

func (s *Server) Handler() http.Handler { return s }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	m := http.NewServeMux()
	s.mux = m

	// public
	m.HandleFunc("POST /e", s.handleIngest)
	m.HandleFunc("OPTIONS /e", s.handlePreflight)
	m.HandleFunc("GET /v1/config", s.handleSDKConfig)
	m.HandleFunc("GET /v1/values", s.handlePublicValues)
	m.HandleFunc("GET /v1/top", s.handlePublicTop)
	m.HandleFunc("OPTIONS /v1/", s.handlePreflight)
	m.HandleFunc("GET /agg.js", s.handleSDK)
	m.HandleFunc("GET /sw.js", s.handleServiceWorker)
	m.HandleFunc("GET /llms.txt", s.handleLLMsTxt)
	m.HandleFunc("POST /mcp", s.handleMCP)
	m.HandleFunc("GET /mcp", s.handleMCPGet)
	m.HandleFunc("DELETE /mcp", s.handleMCPGet)
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })

	// exports
	m.HandleFunc("GET /export/{id}/metrics", s.handleExport)
	m.HandleFunc("/export/{id}/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	})
	m.HandleFunc("GET /internal/metrics", s.handleInternalMetrics)

	// admin
	m.HandleFunc("POST /api/login", s.handleLogin)
	m.HandleFunc("POST /api/logout", s.handleLogout)
	a := func(pattern string, h func(http.ResponseWriter, *http.Request)) { m.Handle(pattern, s.requireAdmin(h)) }
	a("GET /api/me", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]bool{"ok": true}) })
	a("GET /api/meta", s.handleMeta)
	a("GET /api/sites", s.listSites)
	a("POST /api/sites", s.createSite)
	a("GET /api/sites/{site}", s.getSite)
	a("PUT /api/sites/{site}", s.updateSite)
	a("DELETE /api/sites/{site}", s.deleteSite)
	a("GET /api/sites/{site}/aggregates", s.listAggregates)
	a("POST /api/sites/{site}/aggregates", s.createAggregate)
	a("GET /api/sites/{site}/aggregates/{id}", s.getAggregate)
	a("PUT /api/sites/{site}/aggregates/{id}", s.updateAggregate)
	a("DELETE /api/sites/{site}/aggregates/{id}", s.deleteAggregate)
	a("POST /api/sites/{site}/aggregates/{id}/reset", s.resetAggregate)
	a("POST /api/sites/{site}/aggregates/{id}/rebuild", s.rebuildAggregate)
	a("GET /api/sites/{site}/aggregates/{id}/values", s.aggregateValues)
	a("GET /api/sites/{site}/aggregates/{id}/top", s.aggregateTop)
	a("GET /api/sites/{site}/aggregates/{id}/series", s.aggregateSeries)
	a("POST /api/sites/{site}/dry-run", s.dryRun)
	a("GET /api/sites/{site}/events", s.recentEvents)
	a("GET /api/sites/{site}/event-names", s.eventNames)
	a("GET /api/sites/{site}/formulas", s.listFormulas)
	a("POST /api/sites/{site}/formulas", s.createFormula)
	a("POST /api/sites/{site}/formulas/preview", s.previewFormula)
	a("PUT /api/sites/{site}/formulas/{id}", s.updateFormula)
	a("DELETE /api/sites/{site}/formulas/{id}", s.deleteFormula)
	a("GET /api/sites/{site}/exports", s.listExports)
	a("POST /api/sites/{site}/exports", s.createExport)
	a("PUT /api/sites/{site}/exports/{id}", s.updateExport)
	a("DELETE /api/sites/{site}/exports/{id}", s.deleteExport)
	a("POST /api/sites/{site}/exports/{id}/rotate", s.rotateExport)
	a("GET /api/sites/{site}/exports/{id}/preview", s.previewExport)
	a("POST /api/sites/{site}/exports/preview", s.previewExportDraft)
	a("GET /api/sites/{site}/insights", s.insights)
	a("GET /api/sites/{site}/variables", s.variables)
	a("GET /api/sites/{site}/values", s.adminValues)
	a("GET /api/presets", s.listPresets)
	a("POST /api/sites/{site}/presets/{id}", s.applyPreset)
	a("GET /api/sites/{site}/alerts", s.listAlerts)
	a("POST /api/sites/{site}/alerts", s.createAlert)
	a("POST /api/sites/{site}/alerts/check", s.checkAlert)
	a("PUT /api/sites/{site}/alerts/{id}", s.updateAlert)
	a("DELETE /api/sites/{site}/alerts/{id}", s.deleteAlert)
	a("GET /api/sites/{site}/alerts/{id}/events", s.alertEvents)
	a("POST /api/sites/{site}/alerts/{id}/test", s.testAlert)
	a("GET /api/notifications", s.listNotifications)
	a("POST /api/notifications/read", s.readNotifications)
	a("GET /api/push", s.pushInfo)
	a("POST /api/push/subscriptions", s.subscribePush)
	a("DELETE /api/push/subscriptions", s.unsubscribePush)
	a("POST /api/push/test", s.testPush)
	a("GET /api/tokens", s.listTokens)
	a("POST /api/tokens", s.createToken)
	a("DELETE /api/tokens/{id}", s.deleteToken)

	// UI
	ui, _ := fs.Sub(web.UI, "ui")
	files := http.FileServer(http.FS(ui))
	m.Handle("GET /ui/", http.StripPrefix("/ui/", noCache(files)))
	m.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(web.Index())
	})
}

// noCache makes browsers revalidate UI files, so an upgraded binary never runs with a stale UI.
func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		h.ServeHTTP(w, r)
	})
}

// ---- helpers

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

type apiError struct {
	Error    string   `json:"error"`
	Warnings []string `json:"warnings,omitempty"`
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, apiError{Error: msg})
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	var conflict store.ErrConflict
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, 404, "not found")
	case errors.As(err, &conflict):
		writeErr(w, 409, conflict.Msg)
	default:
		s.log.Error("request failed", "err", err)
		writeErr(w, 500, "internal error")
	}
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		writeErr(w, 400, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func (s *Server) clientIP(r *http.Request) string {
	// A dedicated header is overwritten by the proxy on every request, unlike the first X-Forwarded-For entry,
	// which a client can send itself.
	if s.cfg.ClientIPHeader != "" {
		if ip := strings.TrimSpace(r.Header.Get(s.cfg.ClientIPHeader)); ip != "" {
			return ip
		}
	}
	if s.cfg.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			return strings.TrimSpace(strings.Split(xff, ",")[0])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// baseURL is the public URL of this server, used in install snippets and scrape configs.
func (s *Server) baseURL(r *http.Request) string {
	if s.cfg.PublicURL != "" {
		return strings.TrimRight(s.cfg.PublicURL, "/")
	}
	scheme := "http"
	if r.TLS != nil || (s.cfg.TrustProxy && r.Header.Get("X-Forwarded-Proto") == "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func randomString(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))[:n]
}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

// ---- small rate limiter (token bucket per key)

type bucket struct {
	tokens float64
	last   time.Time
}

type limiter struct {
	mu      sync.Mutex
	rate    float64
	burst   float64
	buckets map[string]*bucket
	sweep   time.Time
}

func newLimiter(rate, burst float64) *limiter {
	return &limiter{rate: rate, burst: burst, buckets: map[string]*bucket{}}
}

func (l *limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.refill(key)
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Blocked reports whether key has no tokens left, without consuming one.
func (l *limiter) Blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.refill(key).tokens < 1
}

// refill returns the bucket for key with tokens added for the time since its last use. Callers hold l.mu.
func (l *limiter) refill(key string) *bucket {
	now := time.Now()
	if now.Sub(l.sweep) > time.Minute {
		for k, b := range l.buckets {
			if now.Sub(b.last) > 10*time.Minute {
				delete(l.buckets, k)
			}
		}
		l.sweep = now
	}
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	return b
}

// ---- small TTL cache for public responses

type cacheEntry struct {
	body    []byte
	expires time.Time
}

type ttlCache struct {
	mu  sync.Mutex
	max int
	m   map[string]cacheEntry
}

func newTTLCache(max int) *ttlCache { return &ttlCache{max: max, m: map[string]cacheEntry{}} }

func (c *ttlCache) Get(k string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if !ok || time.Now().After(e.expires) {
		return nil, false
	}
	return e.body, true
}

func (c *ttlCache) Set(k string, b []byte, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= c.max {
		now := time.Now()
		for k, e := range c.m {
			if now.After(e.expires) {
				delete(c.m, k)
			}
		}
		if len(c.m) >= c.max {
			c.m = map[string]cacheEntry{}
		}
	}
	c.m[k] = cacheEntry{b, time.Now().Add(ttl)}
}

func (c *ttlCache) Clear() {
	c.mu.Lock()
	c.m = map[string]cacheEntry{}
	c.mu.Unlock()
}

// reload recompiles the engine after a configuration change and drops cached public responses.
func (s *Server) reload(ctx context.Context) {
	if err := s.eng.Reload(ctx); err != nil {
		s.log.Error("reload failed", "err", err)
	}
	s.cache.Clear()
}

func (s *Server) site(w http.ResponseWriter, r *http.Request) (model.Site, bool) {
	id, err := strconv.ParseInt(r.PathValue("site"), 10, 64)
	if err != nil {
		writeErr(w, 404, "not found")
		return model.Site{}, false
	}
	st, err := s.st.GetSite(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return st, false
	}
	return st, true
}

// dimsFromQuery collects dimension values from query parameters, ignoring reserved parameter names.
func dimsFromQuery(r *http.Request, reserved ...string) map[string]string {
	skip := map[string]bool{}
	for _, k := range reserved {
		skip[k] = true
	}
	out := map[string]string{}
	for k, v := range r.URL.Query() {
		if skip[k] || len(v) == 0 || v[0] == "" {
			continue
		}
		if model.ValidateSlug(k) == nil {
			out[k] = v[0]
		}
	}
	return out
}
