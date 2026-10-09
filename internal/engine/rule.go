package engine

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"strconv"
	"strings"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
	"github.com/worotyns/agg/internal/model"
)

// Rule is a compiled aggregate definition.
type Rule struct {
	Agg        model.Aggregate
	events     map[string]bool
	where      *vm.Program
	explode    *vm.Program
	group      *vm.Program
	groupLabel *vm.Program
	rank       *vm.Program
	rankLabel  *vm.Program
	value      *vm.Program
}

// Contribution is what one event (or one exploded item) adds to an aggregate.
type Contribution struct {
	Part        string  `json:"group,omitempty"`
	PartLabel   string  `json:"groupLabel,omitempty"`
	Member      string  `json:"rank,omitempty"`
	MemberLabel string  `json:"rankLabel,omitempty"`
	Value       float64 `json:"value,omitempty"`
	Raw         string  `json:"raw,omitempty"`
	Distinct    string  `json:"distinct,omitempty"`
	hash        int64
}

const maxKeyLen = 200

func exprEnv(withItem bool) map[string]any {
	env := map[string]any{"event": "", "props": map[string]any{}, "meta": map[string]any{}, "visitor": "", "ts": 0}
	if withItem {
		env["item"] = map[string]any{}
	}
	return env
}

func compileExpr(field, src string, withItem bool) (*vm.Program, error) {
	if strings.TrimSpace(src) == "" {
		return nil, nil
	}
	p, err := expr.Compile(src, expr.Env(exprEnv(withItem)), expr.MaxNodes(500))
	if err != nil {
		msg := err.Error()
		if !withItem && strings.Contains(msg, "unknown name item") {
			msg = "`item` is only available when Explode is set"
		} else if withItem && strings.Contains(msg, "unknown name items") {
			msg = "use `item.<field>` for the exploded element, not `items.`"
		}
		return nil, fmt.Errorf("%s: %s", field, msg)
	}
	return p, nil
}

// Validate normalizes and checks a definition. It returns non-fatal warnings.
func Validate(def *model.AggregateDef) ([]string, error) {
	_, warns, err := compile(model.Aggregate{AggregateDef: *def}, def)
	return warns, err
}

// Compile compiles an aggregate. The definition is normalized in place on a.
func Compile(a model.Aggregate) (*Rule, error) {
	r, _, err := compile(a, &a.AggregateDef)
	return r, err
}

func compile(a model.Aggregate, def *model.AggregateDef) (*Rule, []string, error) {
	var warns []string
	events := []string{}
	set := map[string]bool{}
	for _, e := range def.Events {
		if n := model.NormalizeEventName(e); n != "" && !set[n] {
			set[n] = true
			events = append(events, n)
		}
	}
	def.Events = events
	if len(events) == 0 {
		return nil, nil, fmt.Errorf("choose at least one event")
	}
	switch def.Op {
	case model.OpCount, model.OpSum, model.OpCountDistinct, model.OpLastValue, model.OpLastTimestamp:
	case model.OpAvg, model.OpMin, model.OpMax, model.OpP50, model.OpP95, model.OpP99:
	default:
		return nil, nil, fmt.Errorf("unknown operation %q", def.Op)
	}
	switch def.Visibility {
	case "":
		def.Visibility = model.VisibilityPrivate
	case model.VisibilityPrivate, model.VisibilityPublic, model.VisibilityPublicBucketed:
	default:
		return nil, nil, fmt.Errorf("unknown visibility %q", def.Visibility)
	}
	withItem := strings.TrimSpace(def.Explode) != ""
	r := &Rule{events: set}
	var err error
	if r.where, err = compileExpr("Where", def.Where, withItem); err != nil {
		return nil, nil, err
	}
	if r.explode, err = compileExpr("Explode", def.Explode, false); err != nil {
		return nil, nil, err
	}
	checkGroup := func(name string, g *model.Grouping) (*vm.Program, *vm.Program, error) {
		g.Dimension = strings.TrimSpace(g.Dimension)
		if err := model.ValidateSlug(g.Dimension); err != nil {
			return nil, nil, fmt.Errorf("%s dimension %s", name, err)
		}
		if strings.TrimSpace(g.Expr) == "" {
			return nil, nil, fmt.Errorf("%s: expression is required", name)
		}
		p, err := compileExpr(name, g.Expr, withItem)
		if err != nil {
			return nil, nil, err
		}
		l, err := compileExpr(name+" label", g.Label, withItem)
		return p, l, err
	}
	if def.GroupBy != nil && strings.TrimSpace(def.GroupBy.Dimension+def.GroupBy.Expr) == "" {
		def.GroupBy = nil
	}
	if def.RankBy != nil && strings.TrimSpace(def.RankBy.Dimension+def.RankBy.Expr) == "" {
		def.RankBy = nil
	}
	if def.GroupBy != nil {
		if r.group, r.groupLabel, err = checkGroup("Group by", def.GroupBy); err != nil {
			return nil, nil, err
		}
	}
	if def.RankBy != nil {
		if def.GroupBy == nil {
			return nil, nil, fmt.Errorf("Rank within group needs Group by")
		}
		if def.Op != model.OpCount && def.Op != model.OpSum {
			return nil, nil, fmt.Errorf("Rank within group is only available for count and sum")
		}
		if def.RankBy.Dimension == def.GroupBy.Dimension {
			return nil, nil, fmt.Errorf("Rank within group must use a different dimension than Group by")
		}
		if r.rank, r.rankLabel, err = checkGroup("Rank within group", def.RankBy); err != nil {
			return nil, nil, err
		}
	}
	switch {
	case def.Op == model.OpSum || def.Op == model.OpLastValue || def.Op.Stat():
		if strings.TrimSpace(def.Value) == "" {
			return nil, nil, fmt.Errorf("Value: an expression is required for %s", def.Op)
		}
	case def.Op == model.OpCount || def.Op == model.OpLastTimestamp:
		def.Value = ""
	}
	if r.value, err = compileExpr("Value", def.Value, withItem); err != nil {
		return nil, nil, err
	}
	if withItem {
		uses := def.Where + def.Value
		if def.GroupBy != nil {
			uses += def.GroupBy.Expr
		}
		if !strings.Contains(uses, "item") {
			warns = append(warns, "Explode is set but no expression uses `item`: every element is counted the same way")
		}
	}
	if def.Op == model.OpCountDistinct && def.Value == "" {
		warns = append(warns, "Counting distinct visitors requires the visitor id to be enabled in tracking settings")
	}
	a.AggregateDef = *def
	r.Agg = a
	return r, warns, nil
}

// Matches reports whether the rule listens to the event name.
func (r *Rule) Matches(name string) bool { return r.events[name] }

func run(p *vm.Program, env map[string]any) (any, error) {
	if p == nil {
		return nil, nil
	}
	return expr.Run(p, env)
}

// Eval applies the rule to one event. matched is true when the event passed the event name and Where filters.
func (r *Rule) Eval(ev model.Event) (matched bool, out []Contribution, err error) {
	if !r.events[ev.Name] {
		return false, nil, nil
	}
	props := ev.Props
	if props == nil {
		props = map[string]any{}
	}
	meta := ev.Meta
	if meta == nil {
		meta = map[string]any{}
	}
	env := map[string]any{"event": ev.Name, "props": props, "meta": meta, "visitor": ev.VisitorID, "ts": ev.ReceivedAt / 1000, "item": nil}
	items := []any{nil}
	if r.explode != nil {
		v, err := run(r.explode, env)
		if err != nil {
			return false, nil, fmt.Errorf("explode: %w", err)
		}
		arr, _ := v.([]any)
		items = arr
	}
	for _, item := range items {
		env["item"] = item
		if r.where != nil {
			v, err := run(r.where, env)
			if err != nil || v != true {
				continue
			}
		}
		matched = true
		c, ok, err := r.contribution(env)
		if err != nil {
			return matched, out, err
		}
		if ok {
			out = append(out, c)
		}
	}
	return matched, out, nil
}

func (r *Rule) contribution(env map[string]any) (Contribution, bool, error) {
	var c Contribution
	if r.group != nil {
		v, err := run(r.group, env)
		if err != nil {
			return c, false, fmt.Errorf("group by: %w", err)
		}
		c.Part = KeyString(v)
		if c.Part != "" {
			c.PartLabel = labelOf(r.groupLabel, env)
		}
	}
	if r.rank != nil && c.Part != "" {
		v, err := run(r.rank, env)
		if err != nil {
			return c, false, fmt.Errorf("rank: %w", err)
		}
		c.Member = KeyString(v)
		if c.Member != "" {
			c.MemberLabel = labelOf(r.rankLabel, env)
		}
	}
	switch op := r.Agg.Op; {
	case op == model.OpSum || op.Stat():
		v, err := run(r.value, env)
		if err != nil {
			return c, false, fmt.Errorf("value: %w", err)
		}
		f, ok := ToFloat(v)
		if !ok {
			return c, false, nil
		}
		c.Value = f
	case op == model.OpCountDistinct:
		var v any = env["visitor"]
		if r.value != nil {
			var err error
			if v, err = run(r.value, env); err != nil {
				return c, false, fmt.Errorf("value: %w", err)
			}
		}
		c.Distinct = KeyString(v)
		if c.Distinct == "" {
			return c, false, nil
		}
		h := fnv.New64a()
		h.Write([]byte(c.Distinct))
		c.hash = int64(h.Sum64())
	case op == model.OpLastValue:
		v, err := run(r.value, env)
		if err != nil {
			return c, false, fmt.Errorf("value: %w", err)
		}
		if v == nil {
			return c, false, nil
		}
		b, err := json.Marshal(v)
		if err != nil {
			return c, false, nil
		}
		if len(b) > 1024 {
			b = b[:0]
			b, _ = json.Marshal(KeyString(v))
		}
		c.Raw = string(b)
	}
	return c, true, nil
}

func labelOf(p *vm.Program, env map[string]any) string {
	if p == nil {
		return ""
	}
	v, err := run(p, env)
	if err != nil {
		return ""
	}
	return KeyString(v)
}

// KeyString turns a dimension value into a stable string key: 73 and "73" are the same product.
func KeyString(v any) string {
	var s string
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		s = strings.TrimSpace(t)
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return ""
		}
		if t == math.Trunc(t) && math.Abs(t) < 1e15 {
			s = strconv.FormatInt(int64(t), 10)
		} else {
			s = strconv.FormatFloat(t, 'f', -1, 64)
		}
	case int:
		s = strconv.Itoa(t)
	case int64:
		s = strconv.FormatInt(t, 10)
	case bool:
		s = strconv.FormatBool(t)
	default:
		return ""
	}
	if len(s) > maxKeyLen {
		s = strings.ToValidUTF8(s[:maxKeyLen], "")
	}
	return s
}

// ToFloat converts numbers and numeric strings ("12.50") to float64.
func ToFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, !math.IsNaN(t) && !math.IsInf(t, 0)
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
	}
	return 0, false
}
