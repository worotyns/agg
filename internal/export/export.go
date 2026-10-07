// Package export renders aggregate values for Prometheus (text exposition format 0.0.4) and as JSON.
//
// Prometheus pulls ("scrapes") the current state of a target and keeps the history itself, so an export:
//   - is read-only and idempotent: a scrape never changes state;
//   - exposes current values without timestamps (Prometheus stamps samples at scrape time);
//   - uses gauges for sliding-window values (they go up and down) and a counter (_total) for
//     all-time counts, so rate()/increase() work; a data reset looks like a counter reset, which
//     Prometheus handles;
//   - keeps cardinality bounded: per-dimension series only with an explicit top-K or allowlist;
//   - keeps totals and per-dimension series in separate families, so sum() never double counts;
//   - never emits NaN/Inf for missing data: the series is left out instead.
package export

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/worotyns/agg/internal/model"
	"github.com/worotyns/agg/internal/query"
)

type Family struct {
	Name, Help, Type string
}

const (
	FamValue         = "agg_value"
	FamDimValue      = "agg_dimension_value"
	FamEventsTotal   = "agg_events_total"
	FamDimEvents     = "agg_dimension_events_total"
	FamLastTimestamp = "agg_last_timestamp_seconds"
	FamLastValue     = "agg_last_value"
	FamFormula       = "agg_formula_value"
)

// Families in output order.
var Families = []Family{
	{FamValue, "Aggregate value over a sliding time window: count, sum or distinct count.", "gauge"},
	{FamDimValue, "Aggregate value over a sliding time window for one value of its group-by dimension.", "gauge"},
	{FamEventsTotal, "Events counted by a count aggregate since it was created or last reset.", "counter"},
	{FamDimEvents, "Events counted by a count aggregate for one value of its group-by dimension.", "counter"},
	{FamLastTimestamp, "Unix time of the most recent event matched by a last_timestamp aggregate.", "gauge"},
	{FamLastValue, "Most recent numeric value recorded by a last_value aggregate.", "gauge"},
	{FamFormula, "Formula computed from aggregate values.", "gauge"},
}

type Label struct {
	Name, Value string
}

type Sample struct {
	Family string
	Labels []Label
	Value  float64
}

func (s Sample) key() string {
	var b strings.Builder
	for _, l := range s.Labels {
		b.WriteString(l.Name)
		b.WriteByte(0)
		b.WriteString(l.Value)
		b.WriteByte(0)
	}
	return b.String()
}

var reservedLabels = map[string]bool{
	"site": true, "aggregate": true, "window": true, "formula": true,
	"job": true, "instance": true, "le": true, "quantile": true,
}

// DimensionLabel is the Prometheus label name used for a dimension. Dimension names are already
// valid label names (lowercase slugs); names that clash with our own or Prometheus' labels get a dim_ prefix.
func DimensionLabel(dim string) string {
	if reservedLabels[dim] {
		return "dim_" + dim
	}
	return dim
}

// DefaultWindows is used when an export does not select windows.
var DefaultWindows = []string{"1h", "24h"}

// Collect computes the samples of an export.
func Collect(ctx context.Context, q *query.Querier, site model.Site, ex model.Export, aggs []model.Aggregate, formulas []model.Formula) ([]Sample, error) {
	byName := map[string]model.Aggregate{}
	for _, a := range aggs {
		byName[a.Name] = a
	}
	var windows []model.Window
	wnames := ex.Scope.Windows
	if len(wnames) == 0 {
		wnames = DefaultWindows
	}
	for _, n := range wnames {
		if w, ok := model.WindowByName(n); ok {
			windows = append(windows, w)
		}
	}
	var out []Sample
	add := func(fam string, v float64, labels ...Label) {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return
		}
		out = append(out, Sample{Family: fam, Labels: labels, Value: v})
	}
	siteL := Label{"site", site.Slug}
	topK := ex.Scope.TopK
	if topK <= 0 {
		topK = 10
	}
	mode := ex.Scope.Partitions
	for _, name := range uniqueSorted(ex.Scope.Aggregates) {
		a, ok := byName[name]
		if !ok {
			continue
		}
		aggL := Label{"aggregate", a.Name}
		grouped := a.GroupBy != nil && (mode == model.PartitionsTopK || mode == model.PartitionsAllowlist)
		var dimL string
		if a.GroupBy != nil {
			dimL = DimensionLabel(a.GroupBy.Dimension)
		}
		switch a.Op {
		case model.OpCount, model.OpSum, model.OpCountDistinct:
			for _, w := range windows {
				winL := Label{"window", w.Name}
				v, err := q.WindowValue(ctx, a, w, false, "", "")
				if err != nil {
					return nil, err
				}
				add(FamValue, v, siteL, aggL, winL)
				if !grouped {
					continue
				}
				if mode == model.PartitionsTopK {
					items, err := q.Top(ctx, a, w, a.GroupBy.Dimension, nil, topK)
					if err != nil {
						return nil, err
					}
					for _, it := range items {
						add(FamDimValue, it.Value, siteL, aggL, winL, Label{dimL, it.Key})
					}
				} else {
					for _, p := range uniqueSorted(ex.Scope.Allow) {
						v, err := q.WindowValue(ctx, a, w, false, p, "")
						if err != nil {
							return nil, err
						}
						add(FamDimValue, v, siteL, aggL, winL, Label{dimL, p})
					}
				}
			}
			if a.Op != model.OpCount {
				continue
			}
			t, err := q.TotalValue(ctx, a, "", "")
			if err != nil {
				return nil, err
			}
			add(FamEventsTotal, t, siteL, aggL)
			if !grouped {
				continue
			}
			if mode == model.PartitionsTopK {
				rows, err := q.St.TopTotals(ctx, a.ID, topK)
				if err != nil {
					return nil, err
				}
				for _, r := range rows {
					add(FamDimEvents, float64(r.Count), siteL, aggL, Label{dimL, r.Key})
				}
			} else {
				for _, p := range uniqueSorted(ex.Scope.Allow) {
					t, err := q.TotalValue(ctx, a, p, "")
					if err != nil {
						return nil, err
					}
					add(FamDimEvents, t, siteL, aggL, Label{dimL, p})
				}
			}
		case model.OpLastTimestamp, model.OpLastValue:
			fam := FamLastTimestamp
			if a.Op == model.OpLastValue {
				fam = FamLastValue
			}
			emit := func(part string, labels ...Label) error {
				v, err := q.LastValue(ctx, a, part)
				if err != nil {
					return err
				}
				if f, ok := numeric(v); ok {
					add(fam, f, labels...)
				}
				return nil
			}
			if err := emit("", siteL, aggL); err != nil {
				return nil, err
			}
			if !grouped {
				continue
			}
			parts := uniqueSorted(ex.Scope.Allow)
			if mode == model.PartitionsTopK {
				rows, err := q.St.TopLast(ctx, a.ID, topK)
				if err != nil {
					return nil, err
				}
				parts = parts[:0]
				for _, r := range rows {
					parts = append(parts, r.Key)
				}
			}
			for _, p := range parts {
				if err := emit(p, siteL, aggL, Label{dimL, p}); err != nil {
					return nil, err
				}
			}
		}
	}
	if len(ex.Scope.Formulas) > 0 {
		sc := query.NewScope(q, aggs, formulas, nil)
		for _, name := range uniqueSorted(ex.Scope.Formulas) {
			f, ok := sc.Formulas[name]
			if !ok {
				continue
			}
			v, err := sc.EvalFormula(ctx, f.Expr)
			if err != nil {
				continue // a formula broken by a deleted aggregate must not break the whole scrape
			}
			if fv, ok := v.(float64); ok {
				add(FamFormula, fv, siteL, Label{"formula", f.Name})
			}
		}
	}
	return dedupe(out), nil
}

func numeric(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int64:
		return float64(t), true
	case int:
		return float64(t), true
	case string:
		f, err := strconv.ParseFloat(t, 64)
		return f, err == nil
	}
	return 0, false
}

func uniqueSorted(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// dedupe sorts samples by family order and labels, dropping any repeated series.
func dedupe(in []Sample) []Sample {
	order := map[string]int{}
	for i, f := range Families {
		order[f.Name] = i
	}
	sort.SliceStable(in, func(i, j int) bool {
		if in[i].Family != in[j].Family {
			return order[in[i].Family] < order[in[j].Family]
		}
		return in[i].key() < in[j].key()
	})
	out := in[:0]
	for i, s := range in {
		if i > 0 && s.Family == in[i-1].Family && s.key() == in[i-1].key() {
			continue
		}
		out = append(out, s)
	}
	return out
}

// ContentTypePrometheus is the content type of the text exposition format.
const ContentTypePrometheus = "text/plain; version=0.0.4; charset=utf-8"

// WritePrometheus renders samples in the Prometheus text exposition format. Each family is written
// once, with its HELP and TYPE lines followed by all of its samples.
func WritePrometheus(samples []Sample) []byte {
	var b bytes.Buffer
	byFam := map[string][]Sample{}
	for _, s := range samples {
		byFam[s.Family] = append(byFam[s.Family], s)
	}
	for _, f := range Families {
		ss := byFam[f.Name]
		if len(ss) == 0 {
			continue
		}
		b.WriteString("# HELP " + f.Name + " " + escapeHelp(f.Help) + "\n")
		b.WriteString("# TYPE " + f.Name + " " + f.Type + "\n")
		for _, s := range ss {
			b.WriteString(f.Name)
			if len(s.Labels) > 0 {
				b.WriteByte('{')
				for i, l := range s.Labels {
					if i > 0 {
						b.WriteByte(',')
					}
					b.WriteString(l.Name + `="` + escapeLabel(l.Value) + `"`)
				}
				b.WriteByte('}')
			}
			b.WriteByte(' ')
			b.WriteString(FormatValue(s.Value))
			b.WriteByte('\n')
		}
	}
	return b.Bytes()
}

// FormatValue prints integers without exponent or decimals and other numbers in the shortest exact form.
func FormatValue(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

var (
	labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	helpEscaper  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
)

func escapeLabel(s string) string { return labelEscaper.Replace(strings.ToValidUTF8(s, "�")) }
func escapeHelp(s string) string  { return helpEscaper.Replace(s) }

// JSONSample is one sample in the JSON export, convenient for the Grafana Infinity datasource.
type JSONSample struct {
	Metric string            `json:"metric"`
	Labels map[string]string `json:"labels"`
	Value  float64           `json:"value"`
}

func WriteJSON(site model.Site, generatedAt int64, samples []Sample) []byte {
	out := struct {
		Site        string       `json:"site"`
		GeneratedAt int64        `json:"generatedAt"`
		Metrics     []JSONSample `json:"metrics"`
	}{site.Slug, generatedAt, make([]JSONSample, 0, len(samples))}
	for _, s := range samples {
		l := map[string]string{}
		for _, x := range s.Labels {
			l[x.Name] = x.Value
		}
		out.Metrics = append(out.Metrics, JSONSample{s.Family, l, s.Value})
	}
	b, _ := json.Marshal(out)
	return b
}
