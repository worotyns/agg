// Package engine turns incoming events into aggregate state: it validates and filters events,
// runs the compiled rules, buffers the resulting deltas in memory and flushes them to storage.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/worotyns/agg/internal/geo"
	"github.com/worotyns/agg/internal/model"
	"github.com/worotyns/agg/internal/privacy"
	"github.com/worotyns/agg/internal/store"
)

type Options struct {
	FlushInterval       time.Duration // default 1s
	RawRetentionDays    int           // default 7
	MaxKeysPerAggregate int           // distinct group/rank values per aggregate, default 50000
	VisitorEventsPerMin int           // per visitor and site, default 300
	MaxEventsPerRequest int           // default 100
	MaxPropsBytes       int           // default 8192
	DedupeTTL           time.Duration // default 48h
	Geo                 *geo.Client   // optional GeoIP lookup client; nil = no location meta
}

func (o *Options) defaults() {
	if o.FlushInterval <= 0 {
		o.FlushInterval = time.Second
	}
	if o.RawRetentionDays <= 0 {
		o.RawRetentionDays = 7
	}
	if o.MaxKeysPerAggregate <= 0 {
		o.MaxKeysPerAggregate = 50000
	}
	if o.VisitorEventsPerMin <= 0 {
		o.VisitorEventsPerMin = 300
	}
	if o.MaxEventsPerRequest <= 0 {
		o.MaxEventsPerRequest = 100
	}
	if o.MaxPropsBytes <= 0 {
		o.MaxPropsBytes = 8192
	}
	if o.DedupeTTL <= 0 {
		o.DedupeTTL = 48 * time.Hour
	}
}

var (
	ErrUnknownSite = errors.New("unknown site")
	ErrOrigin      = errors.New("origin not allowed")
)

type siteState struct {
	site    model.Site
	blocked privacy.Blocklist
	rules   map[string][]*Rule
	origins []string
}

type visitorKey struct {
	site    int64
	visitor string
}

// Stats are the engine's operational counters, exposed on /internal/metrics.
type Stats struct {
	Received      atomic.Int64
	Accepted      atomic.Int64
	Duplicates    atomic.Int64
	RateLimited   atomic.Int64
	Invalid       atomic.Int64
	Bots          atomic.Int64
	KeysCapped    atomic.Int64
	Flushes       atomic.Int64
	FlushErrors   atomic.Int64
	FlushMicros   atomic.Int64 // duration of the last flush
	PendingRaw    atomic.Int64
	RuleErrors    atomic.Int64
	LastFlushUnix atomic.Int64
}

type Engine struct {
	st    store.Storage
	log   *slog.Logger
	opts  Options
	Now   func() time.Time
	Stats Stats

	mu        sync.Mutex
	sites     map[string]*siteState // by public key
	byID      map[int64]*siteState
	buf       *store.Batch
	dedupe    map[store.DedupeKey]int64
	visitors  map[visitorKey]int
	visitMin  int64
	keys      map[int64]map[string]struct{}
	flushMu   sync.Mutex
	stop      chan struct{}
	stopped   chan struct{}
	closeOnce sync.Once
}

func New(st store.Storage, log *slog.Logger, opts Options) *Engine {
	opts.defaults()
	return &Engine{
		st: st, log: log, opts: opts, Now: time.Now,
		sites: map[string]*siteState{}, byID: map[int64]*siteState{},
		buf: store.NewBatch(), dedupe: map[store.DedupeKey]int64{}, visitors: map[visitorKey]int{},
		keys: map[int64]map[string]struct{}{},
	}
}

func (e *Engine) Options() Options { return e.opts }

// Start loads configuration and state, then runs the flush and cleanup loops until Close.
func (e *Engine) Start(ctx context.Context) error {
	if err := e.Reload(ctx); err != nil {
		return err
	}
	d, err := e.st.LoadDedupe(ctx, e.Now().Unix())
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.dedupe = d
	e.mu.Unlock()
	e.stop, e.stopped = make(chan struct{}), make(chan struct{})
	go e.loop()
	return nil
}

func (e *Engine) loop() {
	defer close(e.stopped)
	flush := time.NewTicker(e.opts.FlushInterval)
	clean := time.NewTicker(5 * time.Minute)
	defer flush.Stop()
	defer clean.Stop()
	e.cleanup()
	for {
		select {
		case <-e.stop:
			return
		case <-flush.C:
			if err := e.Flush(context.Background()); err != nil {
				e.log.Error("flush failed, will retry", "err", err)
			}
		case <-clean.C:
			e.cleanup()
		}
	}
}

func (e *Engine) cleanup() {
	now := e.Now()
	if err := e.st.Cleanup(context.Background(), now.Unix(), e.opts.RawRetentionDays); err != nil {
		e.log.Error("cleanup failed", "err", err)
	}
	e.mu.Lock()
	for k, exp := range e.dedupe {
		if exp < now.Unix() {
			delete(e.dedupe, k)
		}
	}
	e.mu.Unlock()
}

// Close stops the loops and flushes everything still buffered.
func (e *Engine) Close() error {
	e.closeOnce.Do(func() {
		if e.stop != nil {
			close(e.stop)
			<-e.stopped
		}
	})
	return e.Flush(context.Background())
}

// Reload recompiles all sites and aggregates. Call it after any configuration change.
func (e *Engine) Reload(ctx context.Context) error {
	sites, err := e.st.ListSites(ctx)
	if err != nil {
		return err
	}
	aggs, err := e.st.ListAggregates(ctx, 0)
	if err != nil {
		return err
	}
	bySite := map[int64][]model.Aggregate{}
	for _, a := range aggs {
		bySite[a.SiteID] = append(bySite[a.SiteID], a)
	}
	newSites := map[string]*siteState{}
	newByID := map[int64]*siteState{}
	var loadKeys []int64
	e.mu.Lock()
	for _, a := range aggs {
		if _, ok := e.keys[a.ID]; !ok {
			loadKeys = append(loadKeys, a.ID)
		}
	}
	e.mu.Unlock()
	keys := map[int64]map[string]struct{}{}
	for _, id := range loadKeys {
		ks, err := e.st.KnownKeys(ctx, id)
		if err != nil {
			return err
		}
		set := make(map[string]struct{}, len(ks))
		for _, k := range ks {
			set[k] = struct{}{}
		}
		keys[id] = set
	}
	for _, s := range sites {
		ss := &siteState{site: s, blocked: privacy.NewBlocklist(s.Config.BlockedFields), rules: map[string][]*Rule{}, origins: s.Config.AllowedOrigins}
		for _, a := range bySite[s.ID] {
			if a.Paused {
				continue
			}
			r, err := Compile(a)
			if err != nil {
				e.log.Warn("aggregate does not compile, skipped", "site", s.Slug, "aggregate", a.Name, "err", err)
				continue
			}
			for _, ev := range r.Agg.Events {
				ss.rules[ev] = append(ss.rules[ev], r)
			}
		}
		newSites[s.PublicKey] = ss
		newByID[s.ID] = ss
	}
	e.mu.Lock()
	e.sites, e.byID = newSites, newByID
	for id, set := range keys {
		if _, ok := e.keys[id]; !ok {
			e.keys[id] = set
		}
	}
	e.mu.Unlock()
	return nil
}

// SiteByKey returns the site for a public key.
func (e *Engine) SiteByKey(key string) (model.Site, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.sites[key]
	if !ok {
		return model.Site{}, false
	}
	return s.site, true
}

// IncomingEvent is one event as sent by the SDK or any HTTP client.
type IncomingEvent struct {
	Name      string         `json:"name"`
	TS        int64          `json:"ts"`
	ID        string         `json:"id"`
	VisitorID string         `json:"visitorId"`
	Props     map[string]any `json:"props"`
	Meta      map[string]any `json:"meta"` // page context from the SDK: path, referrer, language
}

// RequestInfo is what the server knows about the request that carried the events.
type RequestInfo struct {
	UserAgent string
	IP        string
}

// clientMeta are the page-context keys a client may send in "meta"; everything else belongs in props.
var clientMeta = []string{"path", "referrer", "language"}

type IngestResult struct {
	Accepted int `json:"accepted"`
	Dropped  int `json:"dropped"`
}

// OriginAllowed checks an Origin header against a site's allowed origins.
// An entry may be an exact origin (https://shop.example) or a wildcard host (https://*.example.com).
func OriginAllowed(allowed []string, origin string) bool {
	if len(allowed) == 0 {
		return true
	}
	origin = strings.TrimRight(strings.ToLower(origin), "/")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	for _, a := range allowed {
		if a == origin {
			return true
		}
		if i := strings.Index(a, "://*."); i >= 0 {
			scheme, suffix := a[:i], a[i+4:]
			if u.Scheme == scheme && strings.HasSuffix(u.Host, suffix) {
				return true
			}
		}
	}
	return false
}

// Ingest validates, enriches and processes a batch of events for a site.
//
// Every event gets meta: the client's page context (path, referrer, language) plus browser, os and device parsed
// from the User-Agent, country (and city) from the IP when a GeoIP client is set and the site's geo mode allows it,
// and the IP itself when the site collects it. Automatic page views from bots are dropped;
// other events from bots are kept with meta.bot = true.
func (e *Engine) Ingest(siteKey, origin string, req RequestInfo, events []IncomingEvent) (IngestResult, error) {
	var res IngestResult
	e.mu.Lock()
	ss, ok := e.sites[siteKey]
	e.mu.Unlock()
	if !ok {
		return res, ErrUnknownSite
	}
	if !OriginAllowed(ss.origins, origin) {
		return res, ErrOrigin
	}
	if len(events) > e.opts.MaxEventsPerRequest {
		res.Dropped += len(events) - e.opts.MaxEventsPerRequest
		events = events[:e.opts.MaxEventsPerRequest]
	}
	e.Stats.Received.Add(int64(len(events)))
	now := e.Now()
	ua := ParseUserAgent(req.UserAgent)
	var loc geo.Info
	if mode := ss.site.Config.Geo; mode != model.GeoOff && req.IP != "" {
		if loc = e.opts.Geo.Lookup(context.Background(), req.IP); mode != model.GeoCity {
			loc.City = ""
		}
	}
	clean := make([]model.Event, 0, len(events))
	for _, in := range events {
		ev, ok := e.sanitize(ss, in)
		if !ok {
			e.Stats.Invalid.Add(1)
			res.Dropped++
			continue
		}
		if ua.Bot && ev.Name == "page_view" {
			e.Stats.Bots.Add(1)
			res.Dropped++
			continue
		}
		ev.Meta = buildMeta(in.Meta, ua, req.IP, ss.site.Config.CollectIP, loc)
		ev.ReceivedAt = now.UnixMilli()
		clean = append(clean, ev)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	minute := now.Unix() / 60
	if minute != e.visitMin {
		e.visitMin = minute
		e.visitors = map[visitorKey]int{}
	}
	for _, ev := range clean {
		if ev.ID != "" {
			k := store.DedupeKey{Site: ss.site.ID, ID: ev.ID}
			if exp, seen := e.dedupe[k]; seen && exp >= now.Unix() {
				e.Stats.Duplicates.Add(1)
				res.Dropped++
				continue
			}
			exp := now.Add(e.opts.DedupeTTL).Unix()
			e.dedupe[k] = exp
			e.buf.Dedupe[k] = exp
		}
		if ev.VisitorID != "" {
			vk := visitorKey{ss.site.ID, ev.VisitorID}
			e.visitors[vk]++
			if e.visitors[vk] > e.opts.VisitorEventsPerMin {
				e.Stats.RateLimited.Add(1)
				res.Dropped++
				continue
			}
		}
		e.buf.Raw = append(e.buf.Raw, ev)
		for _, r := range ss.rules[ev.Name] {
			e.applyLocked(e.buf, r, ev, now)
		}
		res.Accepted++
	}
	e.Stats.Accepted.Add(int64(res.Accepted))
	e.Stats.PendingRaw.Store(int64(len(e.buf.Raw)))
	return res, nil
}

func (e *Engine) sanitize(ss *siteState, in IncomingEvent) (model.Event, bool) {
	name := model.NormalizeEventName(in.Name)
	if name == "" {
		return model.Event{}, false
	}
	ev := model.Event{SiteID: ss.site.ID, Name: name, TS: in.TS, ID: truncate(in.ID, 128)}
	if ss.site.Config.VisitorID {
		ev.VisitorID = truncate(in.VisitorID, 64)
	}
	if len(in.Props) > 0 {
		ss.blocked.Strip(in.Props)
		b, err := json.Marshal(in.Props)
		if err != nil || len(b) > e.opts.MaxPropsBytes {
			return model.Event{}, false
		}
		ev.Props = in.Props
	}
	return ev, true
}

func buildMeta(client map[string]any, ua UserAgent, ip string, collectIP bool, loc geo.Info) map[string]any {
	m := map[string]any{}
	for _, k := range clientMeta {
		if v, ok := client[k].(string); ok {
			if v = truncate(v, 200); v != "" {
				m[k] = v
			}
		}
	}
	if ua.Browser != "" {
		m["browser"], m["os"], m["device"] = ua.Browser, ua.OS, ua.Device
	}
	if ua.Bot {
		m["bot"] = true
	}
	if loc.Country != "" {
		m["country"] = loc.Country
	}
	if loc.City != "" {
		m["city"] = truncate(loc.City, 200)
	}
	if collectIP && ip != "" {
		m["ip"] = ip
	}
	return m
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

var grans = []model.Gran{model.Minute, model.Hour, model.Day}

// applyLocked evaluates rule r on ev and adds the deltas to b. e.mu must be held (it guards e.keys).
func (e *Engine) applyLocked(b *store.Batch, r *Rule, ev model.Event, at time.Time) {
	matched, contribs, err := r.Eval(ev)
	if err != nil {
		e.Stats.RuleErrors.Add(1)
	}
	if !matched {
		return
	}
	agg := r.Agg.ID
	b.Matched[store.MatchedKey{Agg: agg, Hour: model.Hour.Floor(at)}]++
	keys := e.keys[agg]
	if keys == nil {
		keys = map[string]struct{}{}
		e.keys[agg] = keys
	}
	for _, c := range contribs {
		if c.Part != "" {
			k := c.Part + "\x00" + c.Member
			if _, ok := keys[k]; !ok {
				// A new member also creates its site-wide ("", member) key; count both against the cap.
				if len(keys) >= e.opts.MaxKeysPerAggregate {
					e.Stats.KeysCapped.Add(1)
					c.Part, c.Member, c.PartLabel, c.MemberLabel = "", "", "", ""
				} else {
					keys[k] = struct{}{}
					keys[c.Part+"\x00"] = struct{}{}
					if c.Member != "" {
						keys["\x00"+c.Member] = struct{}{}
					}
				}
			}
		}
		levels := [][2]string{{"", ""}}
		if c.Part != "" {
			levels = append(levels, [2]string{c.Part, ""})
			if c.Member != "" {
				levels = append(levels, [2]string{c.Part, c.Member}, [2]string{"", c.Member})
			}
		}
		if c.PartLabel != "" {
			b.Labels[store.LabelKey{Agg: agg, Kind: 'p', Key: c.Part}] = c.PartLabel
		}
		if c.MemberLabel != "" {
			b.Labels[store.LabelKey{Agg: agg, Kind: 'm', Key: c.Member}] = c.MemberLabel
		}
		for _, l := range levels {
			tk := store.PartKey{Agg: agg, Part: l[0], Member: l[1]}
			t := b.Totals[tk]
			if t == nil {
				t = &store.Counter{}
				b.Totals[tk] = t
			}
			t.Count++
			t.Sum += c.Value
			switch r.Agg.Op {
			case model.OpCount, model.OpSum, model.OpAvg, model.OpMin, model.OpMax, model.OpP50, model.OpP95, model.OpP99:
				for _, g := range grans {
					bk := store.BucketKey{Agg: agg, Gran: g, Bucket: g.Floor(at), Part: l[0], Member: l[1]}
					cnt := b.Buckets[bk]
					if cnt == nil {
						cnt = &store.Counter{}
						b.Buckets[bk] = cnt
					}
					cnt.Add(&store.Counter{Count: 1, Sum: c.Value, Min: c.Value, Max: c.Value})
					if _, ok := r.Agg.Op.Quantile(); ok {
						b.Hist[store.HistKey{Agg: agg, Gran: g, Bucket: bk.Bucket, Part: l[0], Member: l[1], Idx: model.HistIndex(c.Value)}]++
					}
				}
			case model.OpCountDistinct:
				for _, g := range grans {
					b.Distinct[store.DistinctKey{Agg: agg, Gran: g, Bucket: g.Floor(at), Part: l[0], Hash: c.hash}] = struct{}{}
				}
			case model.OpLastValue, model.OpLastTimestamp:
				lk := store.PartKey{Agg: agg, Part: l[0]}
				if cur, ok := b.Last[lk]; !ok || cur.TS <= at.UnixMilli() {
					b.Last[lk] = store.LastValue{TS: at.UnixMilli(), Value: c.Raw}
				}
			}
		}
	}
}

// Flush writes buffered deltas to storage. On failure the deltas are kept and retried on the next flush.
func (e *Engine) Flush(ctx context.Context) error {
	e.flushMu.Lock()
	defer e.flushMu.Unlock()
	e.mu.Lock()
	b := e.buf
	e.buf = store.NewBatch()
	e.mu.Unlock()
	return e.writeBatch(ctx, b)
}

func (e *Engine) writeBatch(ctx context.Context, b *store.Batch) error {
	if b.Empty() {
		return nil
	}
	start := time.Now()
	err := e.st.ApplyBatch(ctx, b)
	e.Stats.FlushMicros.Store(time.Since(start).Microseconds())
	if err != nil {
		e.Stats.FlushErrors.Add(1)
		e.mu.Lock()
		merge(b, e.buf)
		e.buf = b
		e.mu.Unlock()
		return err
	}
	e.Stats.Flushes.Add(1)
	e.Stats.LastFlushUnix.Store(time.Now().Unix())
	e.mu.Lock()
	e.Stats.PendingRaw.Store(int64(len(e.buf.Raw)))
	e.mu.Unlock()
	return nil
}

// merge adds src into dst.
func merge(dst, src *store.Batch) {
	for k, c := range src.Buckets {
		if d := dst.Buckets[k]; d != nil {
			d.Add(c)
		} else {
			dst.Buckets[k] = c
		}
	}
	for k, n := range src.Hist {
		dst.Hist[k] += n
	}
	for k := range src.Distinct {
		dst.Distinct[k] = struct{}{}
	}
	for k, v := range src.Last {
		if cur, ok := dst.Last[k]; !ok || cur.TS <= v.TS {
			dst.Last[k] = v
		}
	}
	for k, c := range src.Totals {
		if d := dst.Totals[k]; d != nil {
			d.Add(c)
		} else {
			dst.Totals[k] = c
		}
	}
	for k, v := range src.Labels {
		dst.Labels[k] = v
	}
	for k, v := range src.Matched {
		dst.Matched[k] += v
	}
	for k, v := range src.Dedupe {
		dst.Dedupe[k] = v
	}
	dst.Raw = append(dst.Raw, src.Raw...)
}

// Reset deletes all collected data of an aggregate. New events keep being counted.
func (e *Engine) Reset(ctx context.Context, aggID int64) (time.Time, error) {
	e.flushMu.Lock()
	defer e.flushMu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	// Persist everything buffered so far (including raw events), then drop this aggregate's state.
	b := e.buf
	e.buf = store.NewBatch()
	if err := e.st.ApplyBatch(ctx, b); err != nil {
		merge(b, e.buf)
		e.buf = b
		return time.Time{}, err
	}
	at := e.Now()
	if err := e.st.ClearAggregateData(ctx, aggID, at.UnixMilli()); err != nil {
		return time.Time{}, err
	}
	e.keys[aggID] = map[string]struct{}{}
	return at, nil
}

// Rebuild resets an aggregate and replays the stored raw events through its current definition.
func (e *Engine) Rebuild(ctx context.Context, aggID int64) (int, error) {
	a, err := e.st.GetAggregate(ctx, aggID)
	if err != nil {
		return 0, err
	}
	r, err := Compile(a)
	if err != nil {
		return 0, err
	}
	at, err := e.Reset(ctx, aggID)
	if err != nil {
		return 0, err
	}
	n := 0
	b := store.NewBatch()
	var pending []model.Event
	process := func() error {
		e.mu.Lock()
		for _, ev := range pending {
			e.applyLocked(b, r, ev, time.UnixMilli(ev.ReceivedAt))
		}
		e.mu.Unlock()
		pending = pending[:0]
		if err := e.st.ApplyBatch(ctx, b); err != nil {
			return err
		}
		b = store.NewBatch()
		return nil
	}
	err = e.st.ScanRaw(ctx, a.SiteID, at.UnixMilli(), func(ev model.Event) error {
		if !r.Matches(ev.Name) {
			return nil
		}
		n++
		pending = append(pending, ev)
		if len(pending) >= 1000 {
			return process()
		}
		return nil
	})
	if err != nil {
		return n, err
	}
	return n, process()
}

// DryRunResult describes what one event would contribute to an aggregate.
type DryRunResult struct {
	Event         model.Event    `json:"event"`
	Matched       bool           `json:"matched"`
	Contributions []Contribution `json:"contributions"`
	Error         string         `json:"error,omitempty"`
}

// DryRun evaluates a definition against events without storing anything.
func DryRun(def model.AggregateDef, events []model.Event) ([]DryRunResult, []string, error) {
	warns, err := Validate(&def)
	if err != nil {
		return nil, warns, err
	}
	r, err := Compile(model.Aggregate{AggregateDef: def})
	if err != nil {
		return nil, warns, err
	}
	out := make([]DryRunResult, 0, len(events))
	for _, ev := range events {
		ev.Name = model.NormalizeEventName(ev.Name)
		m, cs, err := r.Eval(ev)
		res := DryRunResult{Event: ev, Matched: m, Contributions: cs}
		if res.Contributions == nil {
			res.Contributions = []Contribution{}
		}
		if err != nil {
			res.Error = err.Error()
		}
		out = append(out, res)
	}
	return out, warns, nil
}
