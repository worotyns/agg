// Package query reads aggregate values: windows, variables, formulas, top-N and series.
package query

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/worotyns/agg/internal/model"
	"github.com/worotyns/agg/internal/store"
)

type Querier struct {
	St  store.Storage
	Now func() time.Time
}

func New(st store.Storage) *Querier { return &Querier{St: st, Now: time.Now} }

var ErrUnknownVariable = errors.New("unknown variable")

// WindowValue returns a COUNT / SUM / COUNT_DISTINCT aggregate over a window (or the window before it).
func (q *Querier) WindowValue(ctx context.Context, a model.Aggregate, w model.Window, prev bool, part, member string) (float64, error) {
	from, to := w.Range(q.Now(), prev)
	switch a.Op {
	case model.OpCountDistinct:
		n, err := q.St.CountDistinct(ctx, a.ID, w.Gran, from, to, part)
		return float64(n), err
	case model.OpSum:
		c, err := q.St.SumBuckets(ctx, a.ID, w.Gran, from, to, part, member)
		return c.Sum, err
	case model.OpCount:
		c, err := q.St.SumBuckets(ctx, a.ID, w.Gran, from, to, part, member)
		return float64(c.Count), err
	case model.OpAvg, model.OpMin, model.OpMax:
		c, err := q.St.SumBuckets(ctx, a.ID, w.Gran, from, to, part, member)
		return statValue(a.Op, c), err
	}
	if p, ok := a.Op.Quantile(); ok {
		bins, err := q.St.Hist(ctx, a.ID, w.Gran, from, to, part, member)
		return model.Quantile(bins, p), err
	}
	return 0, fmt.Errorf("%s has no windows", a.Op)
}

// statValue is avg, min or max of a counter; an empty counter (no events in the window) is 0.
func statValue(op model.Op, c store.Counter) float64 {
	if c.Count == 0 {
		return 0
	}
	switch op {
	case model.OpMin:
		return c.Min
	case model.OpMax:
		return c.Max
	}
	return c.Sum / float64(c.Count)
}

// TotalValue is the all-time value (since creation or the last reset) of a COUNT, SUM or AVG aggregate.
func (q *Querier) TotalValue(ctx context.Context, a model.Aggregate, part, member string) (float64, error) {
	c, err := q.St.Total(ctx, a.ID, part, member)
	switch a.Op {
	case model.OpSum:
		return c.Sum, err
	case model.OpAvg:
		return statValue(a.Op, c), err
	}
	return float64(c.Count), err
}

// LastValue returns the value of a LAST_VALUE aggregate (decoded JSON) or, for LAST_TIMESTAMP, unix seconds.
// It returns nil when nothing was recorded yet.
func (q *Querier) LastValue(ctx context.Context, a model.Aggregate, part string) (any, error) {
	v, ok, err := q.St.Last(ctx, a.ID, part)
	if err != nil || !ok {
		return nil, err
	}
	if a.Op == model.OpLastTimestamp {
		return v.TS / 1000, nil
	}
	var out any
	if err := json.Unmarshal([]byte(v.Value), &out); err != nil {
		return v.Value, nil
	}
	return out, nil
}

// Variable is a parsed variable name: purchases_24h, purchases_prev_24h, purchases_total or last_order.
type Variable struct {
	Name      string
	Aggregate string
	Window    string
	Prev      bool
	Total     bool
}

var varRe = regexp.MustCompile(`^([a-z][a-z0-9_]*?)_(prev_)?(5m|1h|6h|24h|7d|30d)$`)

// ParseVariable splits a variable name. It does not check that the aggregate exists.
func ParseVariable(name string) Variable {
	if m := varRe.FindStringSubmatch(name); m != nil {
		return Variable{Name: name, Aggregate: m[1], Prev: m[2] != "", Window: m[3]}
	}
	if strings.HasSuffix(name, "_total") {
		return Variable{Name: name, Aggregate: strings.TrimSuffix(name, "_total"), Total: true}
	}
	return Variable{Name: name, Aggregate: name}
}

// VariableNames lists every variable an aggregate provides.
func VariableNames(a model.Aggregate) []string {
	if !a.Op.Windowed() {
		return []string{a.Name}
	}
	var out []string
	for _, w := range model.Windows {
		out = append(out, a.Name+"_"+w.Name)
	}
	for _, w := range model.Windows {
		out = append(out, a.Name+"_prev_"+w.Name)
	}
	if a.Op.HasTotal() {
		out = append(out, a.Name+"_total")
	}
	return out
}

// Scope resolves variables for one site, optionally narrowed by dimension values (product=73).
type Scope struct {
	Q          *Querier
	Aggregates map[string]model.Aggregate
	Formulas   map[string]model.Formula
	Dims       map[string]string
	cache      map[string]any
}

func NewScope(q *Querier, aggs []model.Aggregate, formulas []model.Formula, dims map[string]string) *Scope {
	s := &Scope{Q: q, Aggregates: map[string]model.Aggregate{}, Formulas: map[string]model.Formula{}, Dims: dims, cache: map[string]any{}}
	for _, a := range aggs {
		s.Aggregates[a.Name] = a
	}
	for _, f := range formulas {
		s.Formulas[f.Name] = f
	}
	if s.Dims == nil {
		s.Dims = map[string]string{}
	}
	return s
}

// Keys returns the partition and member selected by the scope's dimensions for an aggregate.
func (s *Scope) Keys(a model.Aggregate) (part, member string) {
	if a.GroupBy != nil {
		part = s.Dims[a.GroupBy.Dimension]
	}
	if a.RankBy != nil {
		member = s.Dims[a.RankBy.Dimension]
	}
	return
}

// AggregateFor returns the aggregate a variable refers to.
func (s *Scope) AggregateFor(name string) (model.Aggregate, Variable, bool) {
	v := ParseVariable(name)
	a, ok := s.Aggregates[v.Aggregate]
	if !ok {
		return a, v, false
	}
	if a.Op.Windowed() == (v.Window == "" && !v.Total) {
		return a, v, false
	}
	if v.Total && !a.Op.HasTotal() {
		return a, v, false
	}
	return a, v, true
}

// Value resolves an aggregate variable (not a formula). Unknown names return ErrUnknownVariable.
func (s *Scope) Value(ctx context.Context, name string) (any, error) {
	if v, ok := s.cache[name]; ok {
		return v, nil
	}
	a, v, ok := s.AggregateFor(name)
	if !ok {
		return nil, ErrUnknownVariable
	}
	part, member := s.Keys(a)
	var out any
	var err error
	switch {
	case v.Total:
		out, err = s.Q.TotalValue(ctx, a, part, member)
	case v.Window != "":
		w, _ := model.WindowByName(v.Window)
		if a.Op == model.OpCountDistinct {
			member = ""
		}
		out, err = s.Q.WindowValue(ctx, a, w, v.Prev, part, member)
	default:
		out, err = s.Q.LastValue(ctx, a, part)
	}
	if err != nil {
		return nil, err
	}
	s.cache[name] = out
	return out, nil
}

// Resolve resolves an aggregate variable or a formula name.
func (s *Scope) Resolve(ctx context.Context, name string) (any, error) {
	if f, ok := s.Formulas[name]; ok {
		return s.EvalFormula(ctx, f.Expr)
	}
	return s.Value(ctx, name)
}

// BucketValue hides exact numbers for public_bucketed aggregates: 0–9 exact, then "10+", "100+", …
func BucketValue(v any) any {
	f, ok := v.(float64)
	if !ok || f < 10 {
		return v
	}
	p := math.Pow(10, math.Floor(math.Log10(f)))
	return fmt.Sprintf("%.0f+", p)
}

// ---- top-N

type TopItem struct {
	Key   string  `json:"key"`
	Label string  `json:"label,omitempty"`
	Value float64 `json:"value"`
}

// TopDimension returns the dimension a top-N query ranks by default: the rank dimension, else the group dimension.
func TopDimension(a model.Aggregate) string {
	if a.RankBy != nil {
		return a.RankBy.Dimension
	}
	if a.GroupBy != nil {
		return a.GroupBy.Dimension
	}
	return ""
}

// Top ranks the values of dimension `by` within a window. For aggregates with Rank within group,
// ranking by the rank dimension is done inside the selected group value (or across all groups if none).
func (q *Querier) Top(ctx context.Context, a model.Aggregate, w model.Window, by string, dims map[string]string, limit int) ([]TopItem, error) {
	if a.GroupBy == nil {
		return nil, fmt.Errorf("aggregate %s is not grouped", a.Name)
	}
	if limit <= 0 || limit > 1000 {
		limit = 10
	}
	if by == "" {
		by = TopDimension(a)
	}
	from, to := w.Range(q.Now(), false)
	var rows []store.TopRow
	var err error
	kind := byte('p')
	switch {
	case a.Op == model.OpLastValue || a.Op == model.OpLastTimestamp:
		rows, err = q.St.TopLast(ctx, a.ID, limit)
	case a.Op == model.OpCountDistinct:
		rows, err = q.St.TopDistinct(ctx, a.ID, w.Gran, from, to, limit)
	case a.RankBy != nil && by == a.RankBy.Dimension:
		kind = 'm'
		if _, ok := a.Op.Quantile(); ok {
			return q.topQuantile(ctx, a, w, store.LevelMember, dims[a.GroupBy.Dimension], limit)
		}
		rows, err = q.St.TopBuckets(ctx, a.ID, w.Gran, from, to, store.LevelMember, dims[a.GroupBy.Dimension], topBy(a.Op), limit)
	case by == a.GroupBy.Dimension:
		if _, ok := a.Op.Quantile(); ok {
			return q.topQuantile(ctx, a, w, store.LevelPart, "", limit)
		}
		rows, err = q.St.TopBuckets(ctx, a.ID, w.Gran, from, to, store.LevelPart, "", topBy(a.Op), limit)
	default:
		return nil, fmt.Errorf("aggregate %s has no dimension %q", a.Name, by)
	}
	if err != nil {
		return nil, err
	}
	keys := make([]string, len(rows))
	for i, r := range rows {
		keys[i] = r.Key
	}
	labels, err := q.St.Labels(ctx, a.ID, kind, keys)
	if err != nil {
		return nil, err
	}
	out := make([]TopItem, 0, len(rows))
	for _, r := range rows {
		it := TopItem{Key: r.Key, Label: labels[r.Key], Value: float64(r.Count)}
		switch a.Op {
		case model.OpSum:
			it.Value = r.Sum
		case model.OpAvg, model.OpMin, model.OpMax:
			it.Value = statValue(a.Op, store.Counter{Count: r.Count, Sum: r.Sum, Min: r.Min, Max: r.Max})
		case model.OpLastValue, model.OpLastTimestamp:
			it.Value = float64(r.TS / 1000)
		}
		out = append(out, it)
	}
	return out, nil
}

func topBy(op model.Op) store.TopBy {
	switch op {
	case model.OpSum:
		return store.BySum
	case model.OpAvg:
		return store.ByAvg
	case model.OpMin:
		return store.ByMin
	case model.OpMax:
		return store.ByMax
	}
	return store.ByCount
}

// topQuantile ranks the group (or rank) values of a percentile aggregate by their percentile, highest first.
func (q *Querier) topQuantile(ctx context.Context, a model.Aggregate, w model.Window, level store.Level, part string, limit int) ([]TopItem, error) {
	from, to := w.Range(q.Now(), false)
	hists, err := q.St.TopHist(ctx, a.ID, w.Gran, from, to, level, part)
	if err != nil {
		return nil, err
	}
	p, _ := a.Op.Quantile()
	out := make([]TopItem, 0, len(hists))
	for key, bins := range hists {
		out = append(out, TopItem{Key: key, Value: model.Quantile(bins, p)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Value != out[j].Value {
			return out[i].Value > out[j].Value
		}
		return out[i].Key < out[j].Key
	})
	if len(out) > limit {
		out = out[:limit]
	}
	kind := byte('p')
	if level == store.LevelMember {
		kind = 'm'
	}
	keys := make([]string, len(out))
	for i, it := range out {
		keys[i] = it.Key
	}
	labels, err := q.St.Labels(ctx, a.ID, kind, keys)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Label = labels[out[i].Key]
	}
	return out, nil
}

// ---- series

type Range struct {
	Name     string
	Duration time.Duration
	Gran     model.Gran
}

var Ranges = []Range{
	{"1h", time.Hour, model.Minute},
	{"24h", 24 * time.Hour, model.Hour},
	{"7d", 7 * 24 * time.Hour, model.Hour},
	{"30d", 30 * 24 * time.Hour, model.Day},
	{"90d", 90 * 24 * time.Hour, model.Day},
}

func RangeByName(name string) (Range, bool) {
	for _, r := range Ranges {
		if r.Name == name {
			return r, true
		}
	}
	return Range{}, false
}

// RangeForWindow picks the series range shown next to a window value (sparklines).
func RangeForWindow(w string) Range {
	switch w {
	case "5m", "1h":
		r, _ := RangeByName("1h")
		return r
	case "6h", "24h":
		r, _ := RangeByName("24h")
		return r
	case "7d":
		r, _ := RangeByName("7d")
		return r
	}
	r, _ := RangeByName("30d")
	return r
}

type Point struct {
	T int64   `json:"t"`
	V float64 `json:"v"`
}

// Series returns one point per bucket (zero filled) for a windowed aggregate.
func (q *Querier) Series(ctx context.Context, a model.Aggregate, r Range, part, member string) ([]Point, error) {
	if !a.Op.Windowed() {
		return []Point{}, nil
	}
	step := int64(r.Gran.Duration() / time.Second)
	n := int64(r.Duration / r.Gran.Duration())
	to := r.Gran.Floor(q.Now())
	from := to - (n-1)*step
	var pts []store.Point
	var err error
	var hists map[int64][]model.HistBin
	if a.Op == model.OpCountDistinct {
		pts, err = q.St.SeriesDistinct(ctx, a.ID, r.Gran, from, to, part)
	} else if _, ok := a.Op.Quantile(); ok {
		var rows []store.HistRow
		if rows, err = q.St.SeriesHist(ctx, a.ID, r.Gran, from, to, part, member); err == nil {
			hists = map[int64][]model.HistBin{}
			for _, h := range rows {
				hists[h.Bucket] = append(hists[h.Bucket], model.HistBin{Idx: h.Idx, Count: h.Count})
				if len(hists[h.Bucket]) == 1 {
					pts = append(pts, store.Point{Bucket: h.Bucket})
				}
			}
		}
	} else {
		pts, err = q.St.SeriesBuckets(ctx, a.ID, r.Gran, from, to, part, member)
	}
	if err != nil {
		return nil, err
	}
	byT := map[int64]store.Point{}
	for _, p := range pts {
		byT[p.Bucket] = p
	}
	out := make([]Point, 0, n)
	for t := from; t <= to; t += step {
		p := byT[t]
		v := float64(p.Count)
		switch a.Op {
		case model.OpSum:
			v = p.Sum
		case model.OpAvg, model.OpMin, model.OpMax:
			v = statValue(a.Op, store.Counter{Count: p.Count, Sum: p.Sum, Min: p.Min, Max: p.Max})
		default:
			if pq, ok := a.Op.Quantile(); ok {
				v = model.Quantile(hists[t], pq)
			}
		}
		out = append(out, Point{T: t, V: v})
	}
	return out, nil
}

// ---- helpers

// SortedNames returns the keys of a map sorted.
func SortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
