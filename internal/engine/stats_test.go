package engine_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/worotyns/agg/internal/engine"
	"github.com/worotyns/agg/internal/model"
	"github.com/worotyns/agg/internal/query"
	"github.com/worotyns/agg/internal/testutil"
)

func leave(path string, ms float64) engine.IncomingEvent {
	return testutil.Ev("page_leave", map[string]any{"path": path, "ms": ms})
}

func near(got, want, tol float64) bool { return math.Abs(got-want) <= tol*want }

func TestAvgMinMaxWindows(t *testing.T) {
	f := testutil.New(t)
	def := func(op model.Op) model.AggregateDef {
		return model.AggregateDef{Events: []string{"page_leave"}, Op: op, Value: "props.ms"}
	}
	avg := f.Agg("t_avg", def(model.OpAvg))
	lo := f.Agg("t_min", def(model.OpMin))
	hi := f.Agg("t_max", def(model.OpMax))

	if v := f.WindowValue(avg, "24h", "", ""); v != 0 {
		t.Errorf("avg without events = %v, want 0", v)
	}
	f.Send(leave("/a", 100), leave("/a", 300))
	f.Advance(10 * time.Minute)
	f.Send(leave("/a", 200), leave("/a", 400))

	for _, c := range []struct {
		name string
		a    model.Aggregate
		w    string
		want float64
	}{
		{"avg 1h", avg, "1h", 250}, {"min 1h", lo, "1h", 100}, {"max 1h", hi, "1h", 400},
		{"avg 5m", avg, "5m", 300}, {"min 5m", lo, "5m", 200}, {"max 5m", hi, "5m", 400},
		{"avg 7d", avg, "7d", 250}, {"min 30d", lo, "30d", 100},
	} {
		if got := f.WindowValue(c.a, c.w, "", ""); got != c.want {
			t.Errorf("%s = %v, want %v", c.name, got, c.want)
		}
	}
	if v, err := f.Q.TotalValue(context.Background(), avg, "", ""); err != nil || v != 250 {
		t.Errorf("avg total = %v, %v, want 250", v, err)
	}
	// min and max have no all-time variable; avg has.
	if got := len(query.VariableNames(lo)); got != 12 {
		t.Errorf("min variables = %d, want 12 (6 windows + 6 previous, no _total)", got)
	}
	f.Advance(70 * time.Minute) // everything left the 1h window
	if v := f.WindowValue(hi, "1h", "", ""); v != 0 {
		t.Errorf("max of an empty window = %v, want 0", v)
	}
	if v := f.WindowValue(hi, "24h", "", ""); v != 400 {
		t.Errorf("max 24h = %v, want 400", v)
	}
}

func TestPercentiles(t *testing.T) {
	f := testutil.New(t)
	p50 := f.Agg("t_p50", model.AggregateDef{Events: []string{"page_leave"}, Op: model.OpP50, Value: "props.ms"})
	p95 := f.Agg("t_p95", model.AggregateDef{Events: []string{"page_leave"}, Op: model.OpP95, Value: "props.ms"})
	p99 := f.Agg("t_p99", model.AggregateDef{Events: []string{"page_leave"}, Op: model.OpP99, Value: "props.ms"})

	var evs []engine.IncomingEvent
	for i := 1; i <= 100; i++ {
		evs = append(evs, leave("/a", float64(i*1000)))
	}
	f.Send(evs...)
	for _, c := range []struct {
		a    model.Aggregate
		want float64
	}{{p50, 50000}, {p95, 95000}, {p99, 99000}} {
		for _, w := range []string{"1h", "24h", "7d", "30d"} {
			if got := f.WindowValue(c.a, w, "", ""); !near(got, c.want, 0.03) {
				t.Errorf("%s %s = %v, want %v ±3%%", c.a.Name, w, got, c.want)
			}
		}
	}
	// A slow outlier moves p99 but not the median.
	f.Send(leave("/a", 3_600_000), leave("/a", 3_600_000))
	if got := f.WindowValue(p50, "1h", "", ""); !near(got, 51000, 0.04) {
		t.Errorf("p50 with outliers = %v, want ≈51000", got)
	}
	if got := f.WindowValue(p99, "1h", "", ""); !near(got, 3_600_000, 0.03) {
		t.Errorf("p99 with outliers = %v, want ≈3600000", got)
	}
}

func TestPercentileZeroAndNegativeValues(t *testing.T) {
	f := testutil.New(t)
	p95 := f.Agg("t_p95", model.AggregateDef{Events: []string{"page_leave"}, Op: model.OpP95, Value: "props.ms"})
	f.Send(leave("/a", 0), leave("/a", -5), leave("/a", 0))
	if got := f.WindowValue(p95, "1h", "", ""); got != 0 {
		t.Errorf("p95 of zero and negative values = %v, want 0", got)
	}
}

func TestStatsPerGroupTopAndSeries(t *testing.T) {
	f := testutil.New(t)
	group := &model.Grouping{Dimension: "page", Expr: "props.path"}
	avg := f.Agg("t_avg", model.AggregateDef{Events: []string{"page_leave"}, Op: model.OpAvg, Value: "props.ms", GroupBy: group})
	p95 := f.Agg("t_p95", model.AggregateDef{Events: []string{"page_leave"}, Op: model.OpP95, Value: "props.ms", GroupBy: group})
	hi := f.Agg("t_max", model.AggregateDef{Events: []string{"page_leave"}, Op: model.OpMax, Value: "props.ms", GroupBy: group})

	f.Send(leave("/fast", 1000), leave("/fast", 3000), leave("/slow", 40000), leave("/slow", 60000), leave("/slow", 20000))
	f.Advance(2 * time.Hour)
	f.Send(leave("/fast", 5000))

	if got := f.WindowValue(avg, "24h", "/slow", ""); got != 40000 {
		t.Errorf("avg of /slow = %v, want 40000", got)
	}
	if got := f.WindowValue(p95, "24h", "/fast", ""); !near(got, 5000, 0.03) {
		t.Errorf("p95 of /fast = %v, want ≈5000", got)
	}
	w, _ := model.WindowByName("24h")
	for _, c := range []struct {
		a         model.Aggregate
		first     string
		firstWant float64
	}{{avg, "/slow", 40000}, {p95, "/slow", 60000}, {hi, "/slow", 60000}} {
		top, err := f.Q.Top(context.Background(), c.a, w, "page", nil, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(top) != 2 || top[0].Key != c.first || !near(top[0].Value, c.firstWant, 0.03) {
			t.Errorf("top %s = %+v, want %s first with ≈%v", c.a.Name, top, c.first, c.firstWant)
		}
	}

	rg, _ := query.RangeByName("24h")
	for _, a := range []model.Aggregate{avg, p95} {
		pts, err := f.Q.Series(context.Background(), a, rg, "/fast", "")
		if err != nil {
			t.Fatal(err)
		}
		want := 2000.0 // /fast earlier: 1000 and 3000
		if a.Op == model.OpP95 {
			want = 3000
		}
		last, prev := pts[len(pts)-1], pts[len(pts)-3]
		if !near(last.V, 5000, 0.03) || !near(prev.V, want, 0.03) {
			t.Errorf("series %s: last %v, two hours earlier %v", a.Name, last.V, prev.V)
		}
	}
}
