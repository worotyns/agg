package server

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/worotyns/agg/internal/engine"
	"github.com/worotyns/agg/internal/export"
	"github.com/worotyns/agg/internal/model"
	"github.com/worotyns/agg/internal/preset"
	"github.com/worotyns/agg/internal/privacy"
	"github.com/worotyns/agg/internal/query"
	"github.com/worotyns/agg/internal/store"
)

func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	if s.cfg.PublicURL == "" {
		if cur, _ := s.st.GetSetting(r.Context(), settingBaseURL); cur != s.baseURL(r) {
			s.st.SetSetting(r.Context(), settingBaseURL, s.baseURL(r))
		}
	}
	windows := []string{}
	for _, w := range model.Windows {
		windows = append(windows, w.Name)
	}
	ranges := []string{}
	for _, r := range query.Ranges {
		ranges = append(ranges, r.Name)
	}
	writeJSON(w, 200, map[string]any{
		"version":           s.cfg.Version,
		"baseUrl":           s.baseURL(r),
		"windows":           windows,
		"ranges":            ranges,
		"baseBlockedFields": privacy.BaseBlockedFields,
		"rawRetentionDays":  s.eng.Options().RawRetentionDays,
		"units":             query.FormulaUnits,
		"geo":               map[string]any{"enabled": s.eng.Options().Geo != nil},
	})
}

// ---- sites

func (s *Server) listSites(w http.ResponseWriter, r *http.Request) {
	sites, err := s.st.ListSites(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, sites)
}

func (s *Server) getSite(w http.ResponseWriter, r *http.Request) {
	if site, ok := s.site(w, r); ok {
		writeJSON(w, 200, site)
	}
}

func (s *Server) createSite(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     string `json:"name"`
		Slug     string `json:"slug"`
		Preset   string `json:"preset"`   // default "website"; "none" for an empty site
		Timezone string `json:"timezone"` // for preset alert schedules
		Starters *bool  `json:"starters"` // deprecated: false = preset "none"
	}
	if !readJSON(w, r, &body) {
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		writeErr(w, 400, "name is required")
		return
	}
	if body.Slug == "" {
		body.Slug = model.NormalizeEventName(body.Name)
		if body.Slug == "" || body.Slug[0] < 'a' || body.Slug[0] > 'z' {
			body.Slug = "site_" + body.Slug
		}
	}
	if err := model.ValidateSlug(body.Slug); err != nil {
		writeErr(w, 400, "id "+err.Error())
		return
	}
	site := model.Site{Slug: body.Slug, Name: body.Name, PublicKey: "pk_" + randomString(20), Config: model.DefaultSiteConfig()}
	if err := s.st.CreateSite(r.Context(), &site); err != nil {
		s.fail(w, err)
		return
	}
	if body.Preset == "" {
		body.Preset = "website"
		if body.Starters != nil && !*body.Starters {
			body.Preset = "none"
		}
	}
	var applied *preset.Result
	if body.Preset != "none" {
		res, err := preset.Apply(r.Context(), s.st, site, body.Preset, body.Timezone)
		if err != nil {
			s.st.DeleteSite(r.Context(), site.ID)
			writeErr(w, 400, err.Error())
			return
		}
		applied = &res
		site, _ = s.st.GetSite(r.Context(), site.ID)
	}
	s.reload(r.Context())
	writeJSON(w, 201, struct {
		model.Site
		Preset *preset.Result `json:"preset,omitempty"`
	}{site, applied})
}

func (s *Server) updateSite(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	var body struct {
		Name   string           `json:"name"`
		Config model.SiteConfig `json:"config"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if n := strings.TrimSpace(body.Name); n != "" {
		site.Name = n
	}
	body.Config.Normalize()
	for _, o := range body.Config.AllowedOrigins {
		if !strings.HasPrefix(o, "http://") && !strings.HasPrefix(o, "https://") {
			writeErr(w, 400, "allowed origins must start with http:// or https:// (e.g. https://shop.example.com)")
			return
		}
	}
	site.Config = body.Config
	if err := s.st.UpdateSite(r.Context(), &site); err != nil {
		s.fail(w, err)
		return
	}
	s.reload(r.Context())
	writeJSON(w, 200, site)
}

func (s *Server) deleteSite(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	if err := s.eng.Flush(r.Context()); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.st.DeleteSite(r.Context(), site.ID); err != nil {
		s.fail(w, err)
		return
	}
	s.reload(r.Context())
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---- aggregates

type aggregateRow struct {
	model.Aggregate
	Matched24h int64    `json:"matched24h"`
	Value24h   any      `json:"value24h"`
	Variables  []string `json:"variables"`
	Error      string   `json:"error,omitempty"`
}

func (s *Server) aggregate(w http.ResponseWriter, r *http.Request, site model.Site) (model.Aggregate, bool) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	a, err := s.st.GetAggregate(r.Context(), id)
	if err == nil && a.SiteID != site.ID {
		err = store.ErrNotFound
	}
	if err != nil {
		s.fail(w, err)
		return a, false
	}
	return a, true
}

func (s *Server) listAggregates(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	aggs, err := s.st.ListAggregates(ctx, site.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	matched, err := s.st.MatchedSince(ctx, site.ID, model.Hour.Floor(s.now().Add(-23*time.Hour)))
	if err != nil {
		s.fail(w, err)
		return
	}
	w24, _ := model.WindowByName("24h")
	out := make([]aggregateRow, 0, len(aggs))
	for _, a := range aggs {
		row := aggregateRow{Aggregate: a, Matched24h: matched[a.ID], Variables: query.VariableNames(a)}
		if _, err := engine.Compile(a); err != nil {
			row.Error = err.Error()
		}
		if a.Op.Windowed() {
			row.Value24h, err = s.q.WindowValue(ctx, a, w24, false, "", "")
		} else {
			row.Value24h, err = s.q.LastValue(ctx, a, "")
		}
		if err != nil {
			s.fail(w, err)
			return
		}
		out = append(out, row)
	}
	writeJSON(w, 200, out)
}

func (s *Server) getAggregate(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	if a, ok := s.aggregate(w, r, site); ok {
		writeJSON(w, 200, aggregateRow{Aggregate: a, Variables: query.VariableNames(a)})
	}
}

type aggregateBody struct {
	Name string `json:"name"`
	model.AggregateDef
}

func (s *Server) validateAggregate(w http.ResponseWriter, def *model.AggregateDef) ([]string, bool) {
	def.Title = strings.TrimSpace(def.Title)
	warns, err := engine.Validate(def)
	if err != nil {
		writeJSON(w, 400, apiError{Error: err.Error(), Warnings: warns})
		return nil, false
	}
	return warns, true
}

func (s *Server) createAggregate(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	var body aggregateBody
	if !readJSON(w, r, &body) {
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if err := model.ValidateName(body.Name); err != nil {
		writeErr(w, 400, "name: "+err.Error())
		return
	}
	if s.nameTaken(r, site.ID, body.Name, 0) {
		writeErr(w, 409, "a formula with this name already exists")
		return
	}
	warns, ok := s.validateAggregate(w, &body.AggregateDef)
	if !ok {
		return
	}
	if body.Title == "" {
		body.Title = body.Name
	}
	a := model.Aggregate{SiteID: site.ID, Name: body.Name, AggregateDef: body.AggregateDef}
	if err := s.st.CreateAggregate(r.Context(), &a); err != nil {
		s.fail(w, err)
		return
	}
	s.reload(r.Context())
	writeJSON(w, 201, map[string]any{"aggregate": a, "warnings": warns})
}

// nameTaken reports whether a formula already uses the name (aggregate names are unique in the table).
func (s *Server) nameTaken(r *http.Request, siteID int64, name string, exceptFormula int64) bool {
	fs, _ := s.st.ListFormulas(r.Context(), siteID)
	for _, f := range fs {
		if f.Name == name && f.ID != exceptFormula {
			return true
		}
	}
	return false
}

func (s *Server) updateAggregate(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	a, ok := s.aggregate(w, r, site)
	if !ok {
		return
	}
	var body aggregateBody
	if !readJSON(w, r, &body) {
		return
	}
	warns, ok := s.validateAggregate(w, &body.AggregateDef)
	if !ok {
		return
	}
	if body.Title == "" {
		body.Title = a.Name
	}
	logicChanged := defLogic(a.AggregateDef) != defLogic(body.AggregateDef)
	a.AggregateDef = body.AggregateDef
	if err := s.st.UpdateAggregate(r.Context(), &a); err != nil {
		s.fail(w, err)
		return
	}
	s.reload(r.Context())
	writeJSON(w, 200, map[string]any{"aggregate": a, "warnings": warns, "logicChanged": logicChanged})
}

// defLogic is the part of a definition that changes computed values.
func defLogic(d model.AggregateDef) string {
	g := func(x *model.Grouping) string {
		if x == nil {
			return ""
		}
		return x.Dimension + "\x00" + x.Expr
	}
	return strings.Join([]string{strings.Join(d.Events, ","), d.Where, d.Explode, g(d.GroupBy), g(d.RankBy), string(d.Op), d.Value}, "\x01")
}

func (s *Server) deleteAggregate(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	a, ok := s.aggregate(w, r, site)
	if !ok {
		return
	}
	if err := s.eng.Flush(r.Context()); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.st.DeleteAggregate(r.Context(), a.ID); err != nil {
		s.fail(w, err)
		return
	}
	s.reload(r.Context())
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) resetAggregate(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	a, ok := s.aggregate(w, r, site)
	if !ok {
		return
	}
	if _, err := s.eng.Reset(r.Context(), a.ID); err != nil {
		s.fail(w, err)
		return
	}
	s.cache.Clear()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) rebuildAggregate(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	a, ok := s.aggregate(w, r, site)
	if !ok {
		return
	}
	n, err := s.eng.Rebuild(r.Context(), a.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.cache.Clear()
	writeJSON(w, 200, map[string]any{"ok": true, "events": n})
}

type windowValue struct {
	Window string   `json:"window"`
	Value  float64  `json:"value"`
	Prev   float64  `json:"prev"`
	Change *float64 `json:"change"`
}

func change(cur, prev float64) *float64 {
	if prev == 0 {
		return nil
	}
	c := (cur - prev) / prev * 100
	return &c
}

func (s *Server) aggregateValues(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	a, ok := s.aggregate(w, r, site)
	if !ok {
		return
	}
	ctx := r.Context()
	sc := query.NewScope(s.q, []model.Aggregate{a}, nil, dimsFromQuery(r))
	part, member := sc.Keys(a)
	out := map[string]any{"dimensions": sc.Dims}
	if a.Op.Windowed() {
		if a.Op == model.OpCountDistinct {
			member = ""
		}
		ws := []windowValue{}
		for _, win := range model.Windows {
			cur, err := s.q.WindowValue(ctx, a, win, false, part, member)
			if err != nil {
				s.fail(w, err)
				return
			}
			prev, err := s.q.WindowValue(ctx, a, win, true, part, member)
			if err != nil {
				s.fail(w, err)
				return
			}
			ws = append(ws, windowValue{win.Name, cur, prev, change(cur, prev)})
		}
		out["windows"] = ws
		if a.Op.HasTotal() {
			t, err := s.q.TotalValue(ctx, a, part, member)
			if err != nil {
				s.fail(w, err)
				return
			}
			out["total"] = t
		}
	} else {
		v, err := s.q.LastValue(ctx, a, part)
		if err != nil {
			s.fail(w, err)
			return
		}
		out["last"] = v
		if lv, ok, err := s.st.Last(ctx, a.ID, part); err == nil && ok {
			out["lastAt"] = lv.TS / 1000
		}
	}
	writeJSON(w, 200, out)
}

func (s *Server) aggregateTop(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	a, ok := s.aggregate(w, r, site)
	if !ok {
		return
	}
	status, v := s.top(r, a)
	writeJSON(w, status, v)
}

func (s *Server) aggregateSeries(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	a, ok := s.aggregate(w, r, site)
	if !ok {
		return
	}
	rg, ok := query.RangeByName(r.URL.Query().Get("range"))
	if !ok {
		rg, _ = query.RangeByName("24h")
	}
	sc := query.NewScope(s.q, []model.Aggregate{a}, nil, dimsFromQuery(r, "range"))
	part, member := sc.Keys(a)
	pts, err := s.q.Series(r.Context(), a, rg, part, member)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"range": rg.Name, "step": int64(rg.Gran.Duration().Seconds()), "points": pts})
}

func (s *Server) dryRun(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	var body struct {
		Def    model.AggregateDef     `json:"def"`
		Events []engine.IncomingEvent `json:"events"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	var events []model.Event
	if body.Events != nil {
		blocked := privacy.NewBlocklist(site.Config.BlockedFields)
		for _, e := range body.Events {
			if e.Props != nil {
				blocked.Strip(e.Props)
			}
			events = append(events, model.Event{Name: e.Name, VisitorID: e.VisitorID, ID: e.ID, Props: e.Props, Meta: e.Meta, ReceivedAt: s.now().UnixMilli()})
		}
	} else {
		if err := s.eng.Flush(r.Context()); err != nil {
			s.fail(w, err)
			return
		}
		seen := map[string]bool{}
		for _, n := range body.Def.Events {
			n = model.NormalizeEventName(n)
			if seen[n] || n == "" {
				continue
			}
			seen[n] = true
			evs, err := s.st.RecentEvents(r.Context(), site.ID, store.EventQuery{Limit: 50, Name: n})
			if err != nil {
				s.fail(w, err)
				return
			}
			events = append(events, evs...)
		}
		sort.Slice(events, func(i, j int) bool { return events[i].RawID > events[j].RawID })
	}
	res, warns, err := engine.DryRun(body.Def, events)
	if err != nil {
		writeJSON(w, 400, apiError{Error: err.Error(), Warnings: warns})
		return
	}
	writeJSON(w, 200, map[string]any{"results": res, "warnings": warns})
}

func (s *Server) recentEvents(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	qv := r.URL.Query()
	limit, _ := strconv.Atoi(qv.Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 50
	}
	before, _ := strconv.ParseInt(qv.Get("before"), 10, 64)
	if err := s.eng.Flush(r.Context()); err != nil {
		s.fail(w, err)
		return
	}
	evs, err := s.st.RecentEvents(r.Context(), site.ID, store.EventQuery{
		Limit: limit, Name: model.NormalizeEventName(qv.Get("name")), VisitorID: strings.TrimSpace(qv.Get("visitor")), BeforeID: before,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, evs)
}

func (s *Server) eventNames(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	counts, err := s.st.EventNameCounts(r.Context(), site.ID, s.now().Add(-24*time.Hour).UnixMilli())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, counts)
}

// ---- formulas

func (s *Server) listFormulas(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	fs, err := s.st.ListFormulas(ctx, site.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	aggs, err := s.st.ListAggregates(ctx, site.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	sc := query.NewScope(s.q, aggs, fs, nil)
	type row struct {
		model.Formula
		Value any    `json:"value"`
		Error string `json:"error,omitempty"`
	}
	out := []row{}
	for _, f := range fs {
		v, err := sc.EvalFormula(ctx, f.Expr)
		rw := row{Formula: f, Value: v}
		if err != nil {
			rw.Error = err.Error()
		}
		out = append(out, rw)
	}
	writeJSON(w, 200, out)
}

func (s *Server) formulaFromBody(w http.ResponseWriter, r *http.Request, site model.Site, f *model.Formula) bool {
	if !readJSON(w, r, f) {
		return false
	}
	aggs, err := s.st.ListAggregates(r.Context(), site.ID)
	if err != nil {
		s.fail(w, err)
		return false
	}
	f.Name = strings.TrimSpace(f.Name)
	f.Title = strings.TrimSpace(f.Title)
	if f.Title == "" {
		f.Title = f.Name
	}
	for _, a := range aggs {
		if a.Name == f.Name {
			writeErr(w, 409, "an aggregate with this name already exists")
			return false
		}
	}
	if err := query.ValidateFormula(f, aggs); err != nil {
		writeErr(w, 400, err.Error())
		return false
	}
	return true
}

func (s *Server) createFormula(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	f := model.Formula{}
	if !s.formulaFromBody(w, r, site, &f) {
		return
	}
	f.SiteID = site.ID
	if err := s.st.CreateFormula(r.Context(), &f); err != nil {
		s.fail(w, err)
		return
	}
	s.cache.Clear()
	writeJSON(w, 201, f)
}

func (s *Server) formula(w http.ResponseWriter, r *http.Request, site model.Site) (model.Formula, bool) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	f, err := s.st.GetFormula(r.Context(), id)
	if err == nil && f.SiteID != site.ID {
		err = store.ErrNotFound
	}
	if err != nil {
		s.fail(w, err)
		return f, false
	}
	return f, true
}

func (s *Server) updateFormula(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	cur, ok := s.formula(w, r, site)
	if !ok {
		return
	}
	f := model.Formula{}
	if !s.formulaFromBody(w, r, site, &f) {
		return
	}
	f.ID, f.SiteID, f.Name, f.CreatedAt = cur.ID, cur.SiteID, cur.Name, cur.CreatedAt
	if err := s.st.UpdateFormula(r.Context(), &f); err != nil {
		s.fail(w, err)
		return
	}
	s.cache.Clear()
	writeJSON(w, 200, f)
}

func (s *Server) deleteFormula(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	f, ok := s.formula(w, r, site)
	if !ok {
		return
	}
	if err := s.st.DeleteFormula(r.Context(), f.ID); err != nil {
		s.fail(w, err)
		return
	}
	s.cache.Clear()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) previewFormula(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	var body struct {
		Expr string            `json:"expr"`
		Dims map[string]string `json:"dims"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	aggs, err := s.st.ListAggregates(r.Context(), site.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	_, refs, err := query.CompileFormula(body.Expr, aggs)
	if err != nil {
		writeJSON(w, 200, map[string]any{"error": err.Error()})
		return
	}
	sc := query.NewScope(s.q, aggs, nil, body.Dims)
	v, err := sc.EvalFormula(r.Context(), body.Expr)
	if err != nil {
		writeJSON(w, 200, map[string]any{"error": err.Error()})
		return
	}
	vars := map[string]any{}
	sort.Strings(refs)
	for _, n := range refs {
		vars[n], _ = sc.Value(r.Context(), n)
	}
	writeJSON(w, 200, map[string]any{"value": v, "variables": vars})
}

// ---- variables & values (API explorer)

func (s *Server) variables(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	aggs, err := s.st.ListAggregates(r.Context(), site.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	fs, err := s.st.ListFormulas(r.Context(), site.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	type v struct {
		Name       string   `json:"name"`
		Kind       string   `json:"kind"`
		Source     string   `json:"source"`
		Visibility string   `json:"visibility"`
		Dimensions []string `json:"dimensions"`
	}
	out := []v{}
	for _, a := range aggs {
		dims := []string{}
		if a.GroupBy != nil {
			dims = append(dims, a.GroupBy.Dimension)
		}
		if a.RankBy != nil {
			dims = append(dims, a.RankBy.Dimension)
		}
		for _, n := range query.VariableNames(a) {
			out = append(out, v{n, "aggregate", a.Name, a.Visibility, dims})
		}
	}
	for _, f := range fs {
		out = append(out, v{f.Name, "formula", f.Name, f.Visibility, []string{}})
	}
	writeJSON(w, 200, out)
}

func (s *Server) adminValues(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	res, err := s.values(r, site, splitList(r.URL.Query().Get("v")), dimsFromQuery(r, "v", "public"), r.URL.Query().Get("public") == "1")
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, res)
}

// ---- insights

type tile struct {
	ID     int64         `json:"id"`
	Name   string        `json:"name"`
	Title  string        `json:"title"`
	Op     model.Op      `json:"op"`
	Value  any           `json:"value"`
	Prev   *float64      `json:"prev,omitempty"`
	Change *float64      `json:"change,omitempty"`
	Series []query.Point `json:"series,omitempty"`
	Step   int64         `json:"step,omitempty"`
}

type topTile struct {
	ID        int64           `json:"id"`
	Name      string          `json:"name"`
	Title     string          `json:"title"`
	Op        model.Op        `json:"op"`
	Dimension string          `json:"dimension"`
	Items     []query.TopItem `json:"items"`
}

type formulaTile struct {
	Name  string `json:"name"`
	Title string `json:"title"`
	Unit  string `json:"unit"`
	Value any    `json:"value"`
}

func (s *Server) insights(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	wname := r.URL.Query().Get("window")
	win, ok := model.WindowByName(wname)
	if !ok {
		win, _ = model.WindowByName("24h")
	}
	aggs, err := s.st.ListAggregates(ctx, site.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	fs, err := s.st.ListFormulas(ctx, site.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	rg := query.RangeForWindow(win.Name)
	tiles, tops, ftiles := []tile{}, []topTile{}, []formulaTile{}
	for _, a := range aggs {
		if !a.Pinned {
			continue
		}
		if a.GroupBy != nil {
			items, err := s.q.Top(ctx, a, win, "", nil, 10)
			if err != nil {
				s.fail(w, err)
				return
			}
			tops = append(tops, topTile{a.ID, a.Name, a.Title, a.Op, query.TopDimension(a), items})
			continue
		}
		t := tile{ID: a.ID, Name: a.Name, Title: a.Title, Op: a.Op}
		if a.Op.Windowed() {
			cur, err := s.q.WindowValue(ctx, a, win, false, "", "")
			if err != nil {
				s.fail(w, err)
				return
			}
			prev, err := s.q.WindowValue(ctx, a, win, true, "", "")
			if err != nil {
				s.fail(w, err)
				return
			}
			t.Value, t.Prev, t.Change = cur, &prev, change(cur, prev)
			if t.Series, err = s.q.Series(ctx, a, rg, "", ""); err != nil {
				s.fail(w, err)
				return
			}
			t.Step = int64(rg.Gran.Duration().Seconds())
		} else if t.Value, err = s.q.LastValue(ctx, a, ""); err != nil {
			s.fail(w, err)
			return
		}
		tiles = append(tiles, t)
	}
	sc := query.NewScope(s.q, aggs, fs, nil)
	for _, f := range fs {
		if !f.Pinned {
			continue
		}
		v, _ := sc.EvalFormula(ctx, f.Expr)
		ftiles = append(ftiles, formulaTile{f.Name, f.Title, f.Unit, v})
	}
	writeJSON(w, 200, map[string]any{"window": win.Name, "range": rg.Name, "tiles": tiles, "tops": tops, "formulas": ftiles})
}

// ---- exports

type exportBody struct {
	Name         string            `json:"name"`
	Format       string            `json:"format"`
	Scope        model.ExportScope `json:"scope"`
	CacheSeconds int               `json:"cacheSeconds"`
}

func (b *exportBody) validate() string {
	b.Name = strings.TrimSpace(b.Name)
	if b.Name == "" {
		return "name is required"
	}
	if b.Format != model.FormatPrometheus && b.Format != model.FormatJSON {
		return "format must be prometheus or json"
	}
	if len(b.Scope.Aggregates) == 0 && len(b.Scope.Formulas) == 0 {
		return "select at least one aggregate or formula"
	}
	for _, wn := range b.Scope.Windows {
		if _, ok := model.WindowByName(wn); !ok {
			return "unknown window " + wn
		}
	}
	if len(b.Scope.Windows) == 0 {
		b.Scope.Windows = export.DefaultWindows
	}
	switch b.Scope.Partitions {
	case "":
		b.Scope.Partitions = model.PartitionsNone
	case model.PartitionsNone:
	case model.PartitionsTopK:
		if b.Scope.TopK <= 0 || b.Scope.TopK > 1000 {
			return "top K must be between 1 and 1000"
		}
	case model.PartitionsAllowlist:
		if len(b.Scope.Allow) == 0 || len(b.Scope.Allow) > 1000 {
			return "allowlist must contain 1 to 1000 values"
		}
	default:
		return "unknown partitions mode"
	}
	if b.CacheSeconds < 0 || b.CacheSeconds > 3600 {
		return "cache must be between 0 and 3600 seconds"
	}
	if b.Scope.Allow == nil {
		b.Scope.Allow = []string{}
	}
	if b.Scope.Formulas == nil {
		b.Scope.Formulas = []string{}
	}
	if b.Scope.Aggregates == nil {
		b.Scope.Aggregates = []string{}
	}
	return ""
}

func (s *Server) exportOf(w http.ResponseWriter, r *http.Request, site model.Site) (model.Export, bool) {
	e, err := s.st.GetExport(r.Context(), r.PathValue("id"))
	if err == nil && e.SiteID != site.ID {
		err = store.ErrNotFound
	}
	if err != nil {
		s.fail(w, err)
		return e, false
	}
	return e, true
}

func (s *Server) listExports(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	es, err := s.st.ListExports(r.Context(), site.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, es)
}

func newExportToken() string { return "agg_exp_" + randomString(32) }

func (s *Server) createExport(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	var b exportBody
	if !readJSON(w, r, &b) {
		return
	}
	if msg := b.validate(); msg != "" {
		writeErr(w, 400, msg)
		return
	}
	token := newExportToken()
	e := model.Export{ID: randomString(12), SiteID: site.ID, Name: b.Name, Format: b.Format, Scope: b.Scope,
		CacheSeconds: b.CacheSeconds, TokenHash: hashToken(token)}
	if err := s.st.CreateExport(r.Context(), &e); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{"export": e, "token": token})
}

func (s *Server) updateExport(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	e, ok := s.exportOf(w, r, site)
	if !ok {
		return
	}
	var b exportBody
	if !readJSON(w, r, &b) {
		return
	}
	if msg := b.validate(); msg != "" {
		writeErr(w, 400, msg)
		return
	}
	e.Name, e.Format, e.Scope, e.CacheSeconds = b.Name, b.Format, b.Scope, b.CacheSeconds
	if err := s.st.UpdateExport(r.Context(), &e); err != nil {
		s.fail(w, err)
		return
	}
	s.cache.Clear()
	writeJSON(w, 200, e)
}

func (s *Server) rotateExport(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	e, ok := s.exportOf(w, r, site)
	if !ok {
		return
	}
	token := newExportToken()
	e.TokenHash = hashToken(token)
	if err := s.st.UpdateExport(r.Context(), &e); err != nil {
		s.fail(w, err)
		return
	}
	s.cache.Clear()
	writeJSON(w, 200, map[string]any{"export": e, "token": token})
}

func (s *Server) deleteExport(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	e, ok := s.exportOf(w, r, site)
	if !ok {
		return
	}
	if err := s.st.DeleteExport(r.Context(), e.ID); err != nil {
		s.fail(w, err)
		return
	}
	s.cache.Clear()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) renderPreview(w http.ResponseWriter, r *http.Request, site model.Site, e model.Export) {
	samples, err := s.collect(r, site, e)
	if err != nil {
		s.fail(w, err)
		return
	}
	var body []byte
	if e.Format == model.FormatJSON {
		body = export.WriteJSON(site, s.now().Unix(), samples)
	} else {
		body = export.WritePrometheus(samples)
	}
	writeJSON(w, 200, map[string]any{"series": len(samples), "body": string(body)})
}

func (s *Server) previewExport(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	if e, ok := s.exportOf(w, r, site); ok {
		s.renderPreview(w, r, site, e)
	}
}

func (s *Server) previewExportDraft(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	var b exportBody
	if !readJSON(w, r, &b) {
		return
	}
	if b.Name == "" {
		b.Name = "preview"
	}
	if msg := b.validate(); msg != "" {
		writeJSON(w, 200, map[string]any{"error": msg, "series": 0, "body": ""})
		return
	}
	s.renderPreview(w, r, site, model.Export{SiteID: site.ID, Name: b.Name, Format: b.Format, Scope: b.Scope})
}
