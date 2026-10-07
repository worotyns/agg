package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/worotyns/agg/internal/engine"
	"github.com/worotyns/agg/internal/model"
	"github.com/worotyns/agg/internal/query"
	"github.com/worotyns/agg/web"
)

const maxIngestBody = 256 << 10

func cors(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Content-Type")
	h.Set("Access-Control-Max-Age", "86400")
}

func (s *Server) handlePreflight(w http.ResponseWriter, r *http.Request) {
	cors(w)
	w.WriteHeader(http.StatusNoContent)
}

type ingestPayload struct {
	Site   string                 `json:"site"`
	Events []engine.IncomingEvent `json:"events"`
}

// handleIngest accepts a batch of events. The SDK sends text/plain so browsers skip the CORS preflight.
func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if !s.ingestLimit.Allow(s.clientIP(r)) {
		writeErr(w, http.StatusTooManyRequests, "rate limited")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxIngestBody))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "payload too large")
		return
	}
	var p ingestPayload
	if err := json.Unmarshal(body, &p); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	if p.Site == "" {
		p.Site = r.URL.Query().Get("site")
	}
	res, err := s.eng.Ingest(p.Site, r.Header.Get("Origin"), engine.RequestInfo{UserAgent: r.UserAgent(), IP: s.clientIP(r)}, p.Events)
	switch {
	case errors.Is(err, engine.ErrUnknownSite):
		writeErr(w, 404, "unknown site")
	case errors.Is(err, engine.ErrOrigin):
		writeErr(w, 403, "origin not allowed")
	case err != nil:
		s.fail(w, err)
	default:
		writeJSON(w, http.StatusAccepted, res)
	}
}

// sdkConfig is the tracking configuration the SDK downloads on start.
type sdkConfig struct {
	VisitorID      bool `json:"visitorId"`
	PageViews      bool `json:"pageViews"`
	RequireConsent bool `json:"requireConsent"`
}

func sdkConfigFor(c model.SiteConfig) sdkConfig {
	return sdkConfig{c.VisitorID, c.PageViews, c.RequireConsent}
}

func (s *Server) handleSDKConfig(w http.ResponseWriter, r *http.Request) {
	cors(w)
	site, ok := s.eng.SiteByKey(r.URL.Query().Get("site"))
	if !ok {
		writeErr(w, 404, "unknown site")
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, 200, sdkConfigFor(site.Config))
}

func (s *Server) handleSDK(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Write(web.SDK)
}

// publicCached serves a cached public JSON response, computing it with fn on a miss.
func (s *Server) publicCached(w http.ResponseWriter, r *http.Request, fn func() (int, any)) {
	cors(w)
	key := r.URL.Path + "?" + r.URL.RawQuery
	if b, ok := s.cache.Get(key); ok {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=30")
		w.Write(b)
		return
	}
	status, v := fn()
	b, _ := json.Marshal(v)
	if status == 200 {
		s.cache.Set(key, b, 10*time.Second)
		w.Header().Set("Cache-Control", "public, max-age=30")
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	w.Write(b)
}

type valuesResponse struct {
	Values     map[string]any    `json:"values"`
	Dimensions map[string]string `json:"dimensions"`
	Unknown    []string          `json:"unknown,omitempty"`
}

// values resolves the requested variables. With public=true only public aggregates and formulas
// are returned; anything else is reported as unknown and its value is null.
func (s *Server) values(r *http.Request, site model.Site, names []string, dims map[string]string, public bool) (valuesResponse, error) {
	ctx := r.Context()
	aggs, err := s.st.ListAggregates(ctx, site.ID)
	if err != nil {
		return valuesResponse{}, err
	}
	formulas, err := s.st.ListFormulas(ctx, site.ID)
	if err != nil {
		return valuesResponse{}, err
	}
	sc := query.NewScope(s.q, aggs, formulas, dims)
	res := valuesResponse{Values: map[string]any{}, Dimensions: dims}
	for _, n := range names {
		if n == "" {
			continue
		}
		res.Values[n] = nil
		visibility := ""
		if f, ok := sc.Formulas[n]; ok {
			visibility = f.Visibility
		} else if a, _, ok := sc.AggregateFor(n); ok {
			visibility = a.Visibility
		} else {
			res.Unknown = append(res.Unknown, n)
			continue
		}
		if public && visibility != model.VisibilityPublic && visibility != model.VisibilityPublicBucketed {
			res.Unknown = append(res.Unknown, n)
			continue
		}
		v, err := sc.Resolve(ctx, n)
		if err != nil && !errors.Is(err, query.ErrUnknownVariable) {
			return res, err
		}
		if public && visibility == model.VisibilityPublicBucketed {
			v = query.BucketValue(v)
		}
		res.Values[n] = v
	}
	return res, nil
}

func splitList(s string) []string {
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) > 100 {
		out = out[:100]
	}
	return out
}

func (s *Server) handlePublicValues(w http.ResponseWriter, r *http.Request) {
	s.publicCached(w, r, func() (int, any) {
		site, ok := s.eng.SiteByKey(r.URL.Query().Get("site"))
		if !ok {
			return 404, apiError{Error: "unknown site"}
		}
		res, err := s.values(r, site, splitList(r.URL.Query().Get("v")), dimsFromQuery(r, "site", "v"), true)
		if err != nil {
			s.log.Error("values failed", "err", err)
			return 500, apiError{Error: "internal error"}
		}
		return 200, res
	})
}

type topResponse struct {
	Aggregate string          `json:"aggregate"`
	Window    string          `json:"window"`
	By        string          `json:"by"`
	Items     []query.TopItem `json:"items"`
}

type publicTopItem struct {
	Key   string `json:"key"`
	Label string `json:"label,omitempty"`
	Value any    `json:"value"`
}

func (s *Server) top(r *http.Request, a model.Aggregate) (int, any) {
	qv := r.URL.Query()
	win := qv.Get("window")
	if win == "" {
		win = "24h"
	}
	w, ok := model.WindowByName(win)
	if !ok {
		return 400, apiError{Error: "unknown window"}
	}
	limit, _ := strconv.Atoi(qv.Get("limit"))
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	by := qv.Get("by")
	if by == "" {
		by = query.TopDimension(a)
	}
	items, err := s.q.Top(r.Context(), a, w, by, dimsFromQuery(r, "site", "aggregate", "window", "limit", "by"), limit)
	if err != nil {
		return 400, apiError{Error: err.Error()}
	}
	return 200, topResponse{a.Name, win, by, items}
}

func (s *Server) handlePublicTop(w http.ResponseWriter, r *http.Request) {
	s.publicCached(w, r, func() (int, any) {
		site, ok := s.eng.SiteByKey(r.URL.Query().Get("site"))
		if !ok {
			return 404, apiError{Error: "unknown site"}
		}
		aggs, err := s.st.ListAggregates(r.Context(), site.ID)
		if err != nil {
			return 500, apiError{Error: "internal error"}
		}
		name := r.URL.Query().Get("aggregate")
		for _, a := range aggs {
			if a.Name != name || (a.Visibility != model.VisibilityPublic && a.Visibility != model.VisibilityPublicBucketed) {
				continue
			}
			status, v := s.top(r, a)
			if res, ok := v.(topResponse); ok {
				items := make([]publicTopItem, len(res.Items))
				for i, it := range res.Items {
					items[i] = publicTopItem{it.Key, it.Label, it.Value}
					if a.Visibility == model.VisibilityPublicBucketed {
						items[i].Value = query.BucketValue(it.Value)
					}
				}
				return status, map[string]any{"aggregate": res.Aggregate, "window": res.Window, "by": res.By, "items": items}
			}
			return status, v
		}
		return 404, apiError{Error: "unknown aggregate"}
	})
}
