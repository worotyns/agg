package export_test

// These tests check the Prometheus export against how Prometheus consumes it:
// the official text parser must accept it, promlint must find nothing, the exposition rules
// the parser does not enforce (one HELP/TYPE per family, contiguous families, unique series,
// no timestamps, no NaN/Inf) are checked line by line, and the semantics of each metric type
// (gauges follow sliding windows, counters only go up until a reset) are checked across scrapes.

import (
	"bufio"
	"bytes"
	"context"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil/promlint"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	pmodel "github.com/prometheus/common/model"
	"github.com/worotyns/agg/internal/engine"
	"github.com/worotyns/agg/internal/export"
	"github.com/worotyns/agg/internal/model"
	"github.com/worotyns/agg/internal/testutil"
)

type env struct {
	*testutil.Fixture
	purchases, revenue, byProduct, visitors, lastOrder, lastValue model.Aggregate
}

func setup(t *testing.T) *env {
	f := testutil.New(t)
	e := &env{Fixture: f}
	e.purchases = f.Agg("purchases", model.AggregateDef{Events: []string{"purchase"}, Op: model.OpCount})
	e.revenue = f.Agg("revenue", model.AggregateDef{Events: []string{"purchase"}, Op: model.OpSum, Value: "props.value"})
	e.byProduct = f.Agg("product_purchases", model.AggregateDef{Events: []string{"purchase"}, Op: model.OpCount,
		Explode: "props.items", GroupBy: &model.Grouping{Dimension: "product", Expr: "item.id", Label: "item.name"}})
	e.visitors = f.Agg("visitors", model.AggregateDef{Events: []string{"page_view"}, Op: model.OpCountDistinct})
	e.lastOrder = f.Agg("last_order", model.AggregateDef{Events: []string{"purchase"}, Op: model.OpLastTimestamp})
	e.lastValue = f.Agg("last_order_value", model.AggregateDef{Events: []string{"purchase"}, Op: model.OpLastValue, Value: "props.value"})
	f.Formula("aov", "revenue_24h / purchases_24h")
	return e
}

func (e *env) seed() {
	e.Send(
		testutil.Purchase("o1", 100, testutil.Item("73", "shoes", 50, 2)),
		testutil.Purchase("o2", 30, testutil.Item("73", "shoes", 30, 1), testutil.Item("12", "hats", 0, 1)),
		testutil.Purchase("o3", 20, testutil.Item("5", "socks", 20, 1)),
	)
	for _, v := range []string{"v1", "v2", "v2", "v3"} {
		e.Send(engine.IncomingEvent{Name: "page_view", VisitorID: v})
	}
}

func allExport(partitions string) model.Export {
	return model.Export{
		ID: "x", Name: "all", Format: model.FormatPrometheus,
		Scope: model.ExportScope{
			Aggregates: []string{"purchases", "revenue", "product_purchases", "visitors", "last_order", "last_order_value"},
			Formulas:   []string{"aov"},
			Windows:    []string{"5m", "1h", "24h", "7d", "30d"},
			Partitions: partitions, TopK: 2,
		},
	}
}

func (e *env) scrape(ex model.Export) string {
	e.T.Helper()
	ctx := context.Background()
	aggs, err := e.St.ListAggregates(ctx, e.Site.ID)
	if err != nil {
		e.T.Fatal(err)
	}
	fs, err := e.St.ListFormulas(ctx, e.Site.ID)
	if err != nil {
		e.T.Fatal(err)
	}
	samples, err := export.Collect(ctx, e.Q, e.Site, ex, aggs, fs)
	if err != nil {
		e.T.Fatal(err)
	}
	return string(export.WritePrometheus(samples))
}

func parse(t *testing.T, text string) map[string]*dto.MetricFamily {
	t.Helper()
	// Legacy validation is the strictest scheme: names must be valid for every Prometheus version.
	p := expfmt.NewTextParser(pmodel.LegacyValidation)
	fams, err := p.TextToMetricFamilies(strings.NewReader(text))
	if err != nil {
		t.Fatalf("Prometheus text parser rejected the export: %v\n%s", err, text)
	}
	return fams
}

// value returns the sample value of family fam with exactly the given labels.
func value(t *testing.T, fams map[string]*dto.MetricFamily, fam string, labels map[string]string) (float64, bool) {
	t.Helper()
	mf, ok := fams[fam]
	if !ok {
		return 0, false
	}
	for _, m := range mf.Metric {
		if len(m.Label) != len(labels) {
			continue
		}
		match := true
		for _, l := range m.Label {
			if labels[l.GetName()] != l.GetValue() {
				match = false
			}
		}
		if !match {
			continue
		}
		switch mf.GetType() {
		case dto.MetricType_COUNTER:
			return m.Counter.GetValue(), true
		case dto.MetricType_GAUGE:
			return m.Gauge.GetValue(), true
		default:
			return m.Untyped.GetValue(), true
		}
	}
	return 0, false
}

func lbl(kv ...string) map[string]string {
	m := map[string]string{"site": "shop"}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

func TestExportParsesWithPrometheusParserAndHasCorrectTypes(t *testing.T) {
	e := setup(t)
	e.seed()
	fams := parse(t, e.scrape(allExport(model.PartitionsTopK)))

	want := map[string]dto.MetricType{
		export.FamValue:         dto.MetricType_GAUGE,
		export.FamDimValue:      dto.MetricType_GAUGE,
		export.FamEventsTotal:   dto.MetricType_COUNTER,
		export.FamDimEvents:     dto.MetricType_COUNTER,
		export.FamLastTimestamp: dto.MetricType_GAUGE,
		export.FamLastValue:     dto.MetricType_GAUGE,
		export.FamFormula:       dto.MetricType_GAUGE,
	}
	for name, typ := range want {
		mf, ok := fams[name]
		if !ok {
			t.Fatalf("family %s missing", name)
		}
		if mf.GetType() != typ {
			t.Errorf("%s: type %v, want %v", name, mf.GetType(), typ)
		}
		if mf.GetHelp() == "" {
			t.Errorf("%s: no HELP", name)
		}
	}
	if len(fams) != len(want) {
		t.Errorf("unexpected families: %d", len(fams))
	}
}

func TestExportPassesPromlint(t *testing.T) {
	e := setup(t)
	e.seed()
	for _, mode := range []string{model.PartitionsNone, model.PartitionsTopK, model.PartitionsAllowlist} {
		ex := allExport(mode)
		ex.Scope.Allow = []string{"73", "999"}
		problems, err := promlint.New(strings.NewReader(e.scrape(ex))).Lint()
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range problems {
			t.Errorf("%s: promlint: %s: %s", mode, p.Metric, p.Text)
		}
	}
}

var sampleLine = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{.*\})? (\S+)$`)

// TestExpositionRules checks the rules of the text format that the parser does not enforce.
func TestExpositionRules(t *testing.T) {
	e := setup(t)
	e.seed()
	text := e.scrape(allExport(model.PartitionsTopK))
	if !strings.HasSuffix(text, "\n") {
		t.Error("output must end with a newline")
	}
	help, typ := map[string]int{}, map[string]int{}
	finished := map[string]bool{}
	seen := map[string]bool{}
	current := ""
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			t.Error("empty line in output")
			continue
		}
		if strings.HasPrefix(line, "# HELP ") || strings.HasPrefix(line, "# TYPE ") {
			name := strings.Fields(line)[2]
			if strings.HasPrefix(line, "# HELP ") {
				help[name]++
			} else {
				typ[name]++
				if help[name] == 0 {
					t.Errorf("%s: TYPE before HELP", name)
				}
			}
			if name != current {
				if current != "" {
					finished[current] = true
				}
				if finished[name] {
					t.Errorf("%s: family is split into several blocks", name)
				}
				current = name
			}
			continue
		}
		m := sampleLine.FindStringSubmatch(line)
		if m == nil {
			t.Errorf("not a plain sample line (timestamps are not allowed): %q", line)
			continue
		}
		if m[1] != current {
			t.Errorf("sample %q outside of its family block %q", m[1], current)
		}
		if typ[m[1]] != 1 {
			t.Errorf("sample of %s without a preceding TYPE", m[1])
		}
		if seen[m[1]+m[2]] {
			t.Errorf("duplicate series %s%s", m[1], m[2])
		}
		seen[m[1]+m[2]] = true
		v, err := strconv.ParseFloat(m[3], 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			t.Errorf("value must be a finite number: %q", line)
		}
	}
	for name, n := range help {
		if n != 1 || typ[name] != 1 {
			t.Errorf("%s: HELP x%d, TYPE x%d, want exactly one of each", name, n, typ[name])
		}
	}
}

func TestGaugesMatchAggregateValues(t *testing.T) {
	e := setup(t)
	e.seed()
	fams := parse(t, e.scrape(allExport(model.PartitionsTopK)))
	cases := []struct {
		labels map[string]string
		want   float64
	}{
		{lbl("aggregate", "purchases", "window", "24h"), 3},
		{lbl("aggregate", "revenue", "window", "1h"), 150},
		{lbl("aggregate", "visitors", "window", "24h"), 3},
		{lbl("aggregate", "product_purchases", "window", "24h"), 4},
	}
	for _, c := range cases {
		got, ok := value(t, fams, export.FamValue, c.labels)
		if !ok || got != c.want {
			t.Errorf("agg_value%v = %v (present %v), want %v", c.labels, got, ok, c.want)
		}
	}
	if v, _ := value(t, fams, export.FamDimValue, lbl("aggregate", "product_purchases", "window", "24h", "product", "73")); v != 2 {
		t.Errorf("agg_dimension_value product 73 = %v, want 2", v)
	}
	if v, _ := value(t, fams, export.FamEventsTotal, lbl("aggregate", "purchases")); v != 3 {
		t.Errorf("agg_events_total = %v, want 3", v)
	}
	if v, _ := value(t, fams, export.FamLastTimestamp, lbl("aggregate", "last_order")); int64(v) != testutil.Start.Unix() {
		t.Errorf("last timestamp = %v, want %d", v, testutil.Start.Unix())
	}
	if v, _ := value(t, fams, export.FamLastValue, lbl("aggregate", "last_order_value")); v != 20 {
		t.Errorf("last value = %v, want 20", v)
	}
	if v, _ := value(t, fams, export.FamFormula, lbl("formula", "aov")); v != 50 {
		t.Errorf("formula aov = %v, want 50", v)
	}
	// Every window value in the export equals what the values API computes.
	for _, w := range []string{"5m", "1h", "24h", "7d", "30d"} {
		got, _ := value(t, fams, export.FamValue, lbl("aggregate", "revenue", "window", w))
		if want := e.WindowValue(e.revenue, w, "", ""); got != want {
			t.Errorf("revenue %s: export %v, values %v", w, got, want)
		}
	}
}

func TestGaugesFollowSlidingWindowsButCountersDoNot(t *testing.T) {
	e := setup(t)
	e.seed()
	ex := allExport(model.PartitionsNone)
	e.Advance(2 * time.Hour)
	fams := parse(t, e.scrape(ex))
	if v, ok := value(t, fams, export.FamValue, lbl("aggregate", "purchases", "window", "1h")); !ok || v != 0 {
		t.Errorf("1h window after 2h = %v (present %v), want an explicit 0", v, ok)
	}
	if v, _ := value(t, fams, export.FamValue, lbl("aggregate", "purchases", "window", "24h")); v != 3 {
		t.Errorf("24h window after 2h = %v, want 3", v)
	}
	e.Advance(48 * time.Hour)
	fams = parse(t, e.scrape(ex))
	if v, _ := value(t, fams, export.FamValue, lbl("aggregate", "purchases", "window", "24h")); v != 0 {
		t.Errorf("24h window after 50h = %v, want 0", v)
	}
	if v, _ := value(t, fams, export.FamEventsTotal, lbl("aggregate", "purchases")); v != 3 {
		t.Errorf("counter must not follow windows: %v, want 3", v)
	}
}

func TestCounterIsMonotonicAcrossScrapesAndResetsToZero(t *testing.T) {
	e := setup(t)
	ex := allExport(model.PartitionsTopK)
	prev := -1.0
	for i := 0; i < 5; i++ {
		e.Send(testutil.Purchase("", 10, testutil.Item("73", "shoes", 10, 1)))
		e.Advance(17 * time.Minute)
		fams := parse(t, e.scrape(ex))
		v, ok := value(t, fams, export.FamEventsTotal, lbl("aggregate", "purchases"))
		if !ok || v < prev {
			t.Fatalf("scrape %d: counter %v after %v, must never decrease", i, v, prev)
		}
		if v != float64(i+1) {
			t.Fatalf("scrape %d: counter %v, want %d", i, v, i+1)
		}
		prev = v
	}
	// A data reset is the only way a counter goes down; Prometheus treats it as a counter reset.
	if _, err := e.Eng.Reset(context.Background(), e.purchases.ID); err != nil {
		t.Fatal(err)
	}
	fams := parse(t, e.scrape(ex))
	if v, _ := value(t, fams, export.FamEventsTotal, lbl("aggregate", "purchases")); v != 0 {
		t.Errorf("after reset counter = %v, want 0", v)
	}
}

func TestPartitionsNoneHasNoDimensionLabels(t *testing.T) {
	e := setup(t)
	e.seed()
	fams := parse(t, e.scrape(allExport(model.PartitionsNone)))
	for _, mf := range fams {
		for _, m := range mf.Metric {
			for _, l := range m.Label {
				if l.GetName() == "product" {
					t.Fatalf("%s has a product label with partitions=none", mf.GetName())
				}
			}
		}
	}
}

func seriesFor(fams map[string]*dto.MetricFamily, fam, aggregate, window string) map[string]float64 {
	out := map[string]float64{}
	mf, ok := fams[fam]
	if !ok {
		return out
	}
	for _, m := range mf.Metric {
		var agg, win, product string
		for _, l := range m.Label {
			switch l.GetName() {
			case "aggregate":
				agg = l.GetValue()
			case "window":
				win = l.GetValue()
			case "product":
				product = l.GetValue()
			}
		}
		if agg == aggregate && win == window && product != "" {
			if m.Gauge != nil {
				out[product] = m.Gauge.GetValue()
			} else {
				out[product] = m.Counter.GetValue()
			}
		}
	}
	return out
}

func TestPartitionsTopKBoundsCardinality(t *testing.T) {
	e := setup(t)
	for i := 0; i < 20; i++ {
		var items []map[string]any
		for j := 0; j <= i%5; j++ {
			items = append(items, testutil.Item(strconv.Itoa(j), "c", 1, 1))
		}
		e.Send(testutil.Purchase("", 1, items...))
	}
	ex := allExport(model.PartitionsTopK)
	ex.Scope.TopK = 3
	fams := parse(t, e.scrape(ex))
	for _, w := range ex.Scope.Windows {
		got := seriesFor(fams, export.FamDimValue, "product_purchases", w)
		// product 0 is in all 20 orders, 1 in 16, 2 in 12, 3 in 8, 4 in 4
		want := map[string]float64{"0": 20, "1": 16, "2": 12}
		if len(got) != 3 {
			t.Fatalf("window %s: %d product series, want top 3: %v", w, len(got), got)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("window %s product %s = %v, want %v", w, k, got[k], v)
			}
		}
	}
}

func TestPartitionsAllowlistExportsExactlyTheListWithZeros(t *testing.T) {
	e := setup(t)
	e.seed()
	ex := allExport(model.PartitionsAllowlist)
	ex.Scope.Allow = []string{"73", "999"}
	fams := parse(t, e.scrape(ex))
	got := seriesFor(fams, export.FamDimValue, "product_purchases", "24h")
	if len(got) != 2 || got["73"] != 2 || got["999"] != 0 {
		t.Errorf("allowlist series = %v, want 73=2 and an explicit 999=0", got)
	}
	if _, ok := got["12"]; ok {
		t.Error("product 12 is not on the allowlist")
	}
}

func TestLabelValuesAreEscapedAndRoundTrip(t *testing.T) {
	f := testutil.New(t)
	f.Agg("clicks", model.AggregateDef{Events: []string{"click"}, Op: model.OpCount,
		GroupBy: &model.Grouping{Dimension: "target", Expr: "props.target"}})
	tricky := []string{`say "hi"`, `C:\path\to`, "two\nlines", "zażółć 🚀", `\"`, strings.Repeat("ą", 150)}
	for _, v := range tricky {
		f.Send(testutil.Ev("click", map[string]any{"target": v}))
	}
	e := &env{Fixture: f}
	ex := model.Export{Format: model.FormatPrometheus, Scope: model.ExportScope{
		Aggregates: []string{"clicks"}, Windows: []string{"24h"}, Partitions: model.PartitionsTopK, TopK: 100}}
	text := e.scrape(ex)
	fams := parse(t, text)
	got := []string{}
	for _, m := range fams[export.FamDimValue].Metric {
		for _, l := range m.Label {
			if l.GetName() == "target" {
				got = append(got, l.GetValue())
			}
		}
	}
	want := []string{}
	for _, v := range tricky {
		want = append(want, engine.KeyString(v)) // trimmed and capped at 200 bytes on a rune boundary
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("label values did not round-trip\n got %q\nwant %q", got, want)
	}
	if strings.Count(text, "\n") != bytes.Count([]byte(text), []byte("\n")) || strings.Contains(text, "two\nlines") {
		t.Error("raw newline inside a label value")
	}
}

func TestReservedDimensionNamesGetPrefixed(t *testing.T) {
	f := testutil.New(t)
	f.Agg("views", model.AggregateDef{Events: []string{"page_view"}, Op: model.OpCount,
		GroupBy: &model.Grouping{Dimension: "window", Expr: "props.w"}})
	f.Send(testutil.Ev("page_view", map[string]any{"w": "main"}))
	e := &env{Fixture: f}
	fams := parse(t, e.scrape(model.Export{Format: model.FormatPrometheus, Scope: model.ExportScope{
		Aggregates: []string{"views"}, Windows: []string{"24h"}, Partitions: model.PartitionsTopK, TopK: 5}}))
	if v, ok := value(t, fams, export.FamDimValue, lbl("aggregate", "views", "window", "24h", "dim_window", "main")); !ok || v != 1 {
		t.Errorf("want dim_window label, got %v %v", v, ok)
	}
}

func TestMissingDataIsOmittedNotNaN(t *testing.T) {
	e := setup(t) // no events at all
	e.Agg("last_note", model.AggregateDef{Events: []string{"note"}, Op: model.OpLastValue, Value: "props.text"})
	e.Send(testutil.Ev("note", map[string]any{"text": "not a number"}))
	ex := allExport(model.PartitionsTopK)
	ex.Scope.Aggregates = append(ex.Scope.Aggregates, "last_note")
	text := e.scrape(ex)
	fams := parse(t, text)
	for _, name := range []string{export.FamLastTimestamp, export.FamLastValue, export.FamFormula} {
		if _, ok := fams[name]; ok {
			t.Errorf("%s must be absent when there is no numeric value (aov is 0/0)", name)
		}
	}
	if v, ok := value(t, fams, export.FamValue, lbl("aggregate", "purchases", "window", "24h")); !ok || v != 0 {
		t.Errorf("counts without events are a real 0, got %v %v", v, ok)
	}
	if strings.Contains(text, "NaN") || strings.Contains(text, "Inf") {
		t.Error("NaN/Inf in output")
	}
}

func TestOutputIsDeterministic(t *testing.T) {
	e := setup(t)
	e.seed()
	ex := allExport(model.PartitionsTopK)
	a, b := e.scrape(ex), e.scrape(ex)
	if a != b {
		t.Error("two scrapes without new data differ")
	}
}

func TestScopeSelectsOnlyChosenDataAndSite(t *testing.T) {
	e := setup(t)
	other := e.AddSite("blog")
	e.AggOn(other, "purchases", model.AggregateDef{Events: []string{"purchase"}, Op: model.OpCount})
	e.SendTo(other, testutil.Purchase("x", 1), testutil.Purchase("y", 1))
	e.seed()
	ex := model.Export{Format: model.FormatPrometheus, Scope: model.ExportScope{Aggregates: []string{"purchases", "deleted_one"}, Windows: []string{"24h"}}}
	text := e.scrape(ex)
	fams := parse(t, text)
	if strings.Contains(text, `site="blog"`) {
		t.Error("data of another site leaked into the export")
	}
	if strings.Contains(text, "revenue") || strings.Contains(text, `window="1h"`) {
		t.Error("export contains aggregates or windows outside its scope")
	}
	if v, _ := value(t, fams, export.FamValue, lbl("aggregate", "purchases", "window", "24h")); v != 3 {
		t.Errorf("purchases = %v, want 3 (only this site)", v)
	}
}

func TestFormatValue(t *testing.T) {
	for v, want := range map[float64]string{0: "0", 3: "3", -2: "-2", 1234567: "1234567", 0.5: "0.5", 182.4: "182.4", 1e20: "1e+20"} {
		if got := export.FormatValue(v); got != want {
			t.Errorf("FormatValue(%v) = %s, want %s", v, got, want)
		}
	}
}

// Totals and per-dimension series live in separate families, so summing a family never double counts.
func TestTotalsAndDimensionsDoNotMix(t *testing.T) {
	e := setup(t)
	e.seed()
	fams := parse(t, e.scrape(allExport(model.PartitionsTopK)))
	for _, name := range []string{export.FamValue, export.FamEventsTotal} {
		for _, m := range fams[name].Metric {
			for _, l := range m.Label {
				if l.GetName() == "product" {
					t.Errorf("%s contains a per-product series", name)
				}
			}
		}
	}
	for _, name := range []string{export.FamDimValue, export.FamDimEvents} {
		for _, m := range fams[name].Metric {
			has := false
			for _, l := range m.Label {
				has = has || l.GetName() == "product"
			}
			if !has {
				t.Errorf("%s contains a series without its dimension label", name)
			}
		}
	}
}
