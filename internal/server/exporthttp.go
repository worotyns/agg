package server

import (
	"bytes"
	"compress/gzip"
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/worotyns/agg/internal/export"
	"github.com/worotyns/agg/internal/model"
	"github.com/worotyns/agg/internal/store"
)

func (s *Server) collect(r *http.Request, site model.Site, e model.Export) ([]export.Sample, error) {
	ctx := r.Context()
	aggs, err := s.st.ListAggregates(ctx, site.ID)
	if err != nil {
		return nil, err
	}
	fs, err := s.st.ListFormulas(ctx, site.ID)
	if err != nil {
		return nil, err
	}
	return export.Collect(ctx, s.q, site, e, aggs, fs)
}

// handleExport serves an export endpoint. Authentication: "Authorization: Bearer <token>" or ?token=.
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	token := bearer(r)
	if token == "" {
		token = r.URL.Query().Get("token")
	}
	unauthorized := func() {
		w.Header().Set("WWW-Authenticate", `Bearer realm="agg export"`)
		writeErr(w, http.StatusUnauthorized, "invalid or missing token")
	}
	if token == "" {
		unauthorized()
		return
	}
	if !s.exportLimit.Allow(id + "|" + s.clientIP(r)) {
		writeErr(w, http.StatusTooManyRequests, "rate limited")
		return
	}
	e, err := s.st.GetExport(ctx, id)
	if err == store.ErrNotFound {
		unauthorized()
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	if subtle.ConstantTimeCompare([]byte(hashToken(token)), []byte(e.TokenHash)) != 1 {
		unauthorized()
		return
	}
	site, err := s.st.GetSite(ctx, e.SiteID)
	if err != nil {
		s.fail(w, err)
		return
	}
	contentType := export.ContentTypePrometheus
	if e.Format == model.FormatJSON {
		contentType = "application/json; charset=utf-8"
	}
	cacheKey := "export|" + e.ID + "|" + e.TokenHash
	body, ok := s.cache.Get(cacheKey)
	if !ok {
		samples, err := s.collect(r, site, e)
		if err != nil {
			s.fail(w, err)
			return
		}
		if e.Format == model.FormatJSON {
			body = export.WriteJSON(site, s.now().Unix(), samples)
		} else {
			body = export.WritePrometheus(samples)
		}
		if e.CacheSeconds > 0 {
			s.cache.Set(cacheKey, body, time.Duration(e.CacheSeconds)*time.Second)
		}
	}
	if err := s.st.RecordScrape(ctx, e.ID, s.now().UnixMilli()); err != nil {
		s.log.Warn("cannot record scrape", "err", err)
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Vary", "Accept-Encoding")
	writeMaybeGzip(w, r, body)
}

func writeMaybeGzip(w http.ResponseWriter, r *http.Request, body []byte) {
	if len(body) > 512 && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		var b bytes.Buffer
		gz := gzip.NewWriter(&b)
		gz.Write(body)
		gz.Close()
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(b.Bytes())
		return
	}
	w.Write(body)
}

// handleInternalMetrics exposes operational metrics of this instance (not site data).
func (s *Server) handleInternalMetrics(w http.ResponseWriter, r *http.Request) {
	if s.cfg.InternalMetricsToken == "" {
		http.NotFound(w, r)
		return
	}
	t := bearer(r)
	if subtle.ConstantTimeCompare([]byte(t), []byte(s.cfg.InternalMetricsToken)) != 1 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="agg internal"`)
		writeErr(w, http.StatusUnauthorized, "invalid or missing token")
		return
	}
	st := &s.eng.Stats
	var b bytes.Buffer
	metric := func(name, typ, help string, v float64, labels string) {
		if !strings.Contains(b.String(), "# TYPE "+name+" ") {
			fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
		}
		fmt.Fprintf(&b, "%s%s %s\n", name, labels, export.FormatValue(v))
	}
	metric("agg_ingest_events_received_total", "counter", "Events received on /e.", float64(st.Received.Load()), "")
	metric("agg_ingest_events_accepted_total", "counter", "Events accepted and stored.", float64(st.Accepted.Load()), "")
	metric("agg_ingest_events_dropped_total", "counter", "Events dropped, by reason.", float64(st.Duplicates.Load()), `{reason="duplicate"}`)
	metric("agg_ingest_events_dropped_total", "counter", "Events dropped, by reason.", float64(st.RateLimited.Load()), `{reason="visitor_rate"}`)
	metric("agg_ingest_events_dropped_total", "counter", "Events dropped, by reason.", float64(st.Invalid.Load()), `{reason="invalid"}`)
	metric("agg_aggregate_keys_capped_total", "counter", "Contributions whose new dimension value was not stored because of the per-aggregate cap.", float64(st.KeysCapped.Load()), "")
	metric("agg_rule_errors_total", "counter", "Expression evaluation errors.", float64(st.RuleErrors.Load()), "")
	metric("agg_flushes_total", "counter", "Successful flushes to storage.", float64(st.Flushes.Load()), "")
	metric("agg_flush_errors_total", "counter", "Failed flushes to storage.", float64(st.FlushErrors.Load()), "")
	metric("agg_flush_duration_seconds", "gauge", "Duration of the last flush.", float64(st.FlushMicros.Load())/1e6, "")
	metric("agg_pending_raw_events", "gauge", "Raw events buffered in memory, not yet flushed.", float64(st.PendingRaw.Load()), "")
	if size, err := s.st.DBSize(r.Context()); err == nil {
		metric("agg_db_size_bytes", "gauge", "Size of the SQLite database file.", float64(size), "")
	}
	w.Header().Set("Content-Type", export.ContentTypePrometheus)
	w.Write(b.Bytes())
}
