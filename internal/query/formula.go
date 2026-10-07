package query

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/vm"
	"github.com/worotyns/agg/internal/model"
)

var FormulaUnits = []string{"number", "percent", "currency", "duration"}

type identCollector struct{ names map[string]bool }

func (c *identCollector) Visit(n *ast.Node) {
	if id, ok := (*n).(*ast.IdentifierNode); ok {
		c.names[id.Value] = true
	}
}

func coalesce(params ...any) (any, error) {
	for _, p := range params {
		if p != nil {
			return p, nil
		}
	}
	return nil, nil
}

// CompileFormula compiles a formula against the variables provided by the given aggregates
// and returns the variables it references.
func CompileFormula(src string, aggs []model.Aggregate) (*vm.Program, []string, error) {
	if strings.TrimSpace(src) == "" {
		return nil, nil, fmt.Errorf("expression is required")
	}
	env := map[string]any{}
	for _, a := range aggs {
		for _, n := range VariableNames(a) {
			env[n] = 0.0
		}
	}
	p, err := expr.Compile(src, expr.Env(env), expr.MaxNodes(200),
		expr.Function("coalesce", coalesce, new(func(...any) any)))
	if err != nil {
		return nil, nil, err
	}
	c := &identCollector{names: map[string]bool{}}
	node := p.Node()
	ast.Walk(&node, c)
	refs := []string{}
	for n := range c.names {
		if _, ok := env[n]; ok {
			refs = append(refs, n)
		}
	}
	return p, refs, nil
}

// ValidateFormula checks name, expression and unit.
func ValidateFormula(f *model.Formula, aggs []model.Aggregate) error {
	if err := model.ValidateName(f.Name); err != nil {
		return err
	}
	for _, a := range aggs {
		for _, n := range VariableNames(a) {
			if n == f.Name {
				return fmt.Errorf("name %q is already a variable of aggregate %s", f.Name, a.Name)
			}
		}
	}
	if f.Unit == "" {
		f.Unit = "number"
	}
	unitOK := false
	for _, u := range FormulaUnits {
		if f.Unit == u || strings.HasPrefix(f.Unit, "currency:") && len(f.Unit) == 12 {
			unitOK = true
		}
	}
	if !unitOK {
		return fmt.Errorf("unit must be number, percent, duration or currency:XXX")
	}
	switch f.Visibility {
	case "":
		f.Visibility = model.VisibilityPrivate
	case model.VisibilityPrivate, model.VisibilityPublic:
	default:
		return fmt.Errorf("visibility must be private or public")
	}
	_, _, err := CompileFormula(f.Expr, aggs)
	return err
}

// EvalFormula evaluates a formula expression in this scope. Missing values, division by zero
// and other arithmetic errors give nil: a formula never returns a guess, NaN or Infinity.
func (s *Scope) EvalFormula(ctx context.Context, src string) (any, error) {
	aggs := make([]model.Aggregate, 0, len(s.Aggregates))
	for _, a := range s.Aggregates {
		aggs = append(aggs, a)
	}
	p, refs, err := CompileFormula(src, aggs)
	if err != nil {
		return nil, err
	}
	env := map[string]any{}
	for _, r := range refs {
		v, err := s.Value(ctx, r)
		if err != nil {
			return nil, err
		}
		if v != nil {
			if f, ok := toFloat(v); ok {
				v = f
			} else {
				v = nil
			}
		}
		env[r] = v
	}
	out, err := expr.Run(p, env)
	if err != nil {
		return nil, nil
	}
	f, ok := toFloat(out)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, nil
	}
	return f, nil
}

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	}
	return 0, false
}

// conditionEnv lists every name a condition may use: aggregate variables and formula names.
func conditionEnv(aggs []model.Aggregate, formulas []model.Formula) map[string]any {
	env := map[string]any{}
	for _, a := range aggs {
		for _, n := range VariableNames(a) {
			env[n] = 0.0
		}
	}
	for _, f := range formulas {
		env[f.Name] = 0.0
	}
	env["now"] = 0.0 // unix seconds, e.g. now - last_purchase > 7200
	return env
}

// CompileCondition compiles an alert condition, which must be a boolean expression, e.g. orders_1h < 1.
func CompileCondition(src string, aggs []model.Aggregate, formulas []model.Formula) (*vm.Program, []string, error) {
	if strings.TrimSpace(src) == "" {
		return nil, nil, fmt.Errorf("condition is required")
	}
	env := conditionEnv(aggs, formulas)
	p, err := expr.Compile(src, expr.Env(env), expr.AsBool(), expr.MaxNodes(200),
		expr.Function("coalesce", coalesce, new(func(...any) any)))
	if err != nil {
		return nil, nil, err
	}
	c := &identCollector{names: map[string]bool{}}
	node := p.Node()
	ast.Walk(&node, c)
	refs := []string{}
	for n := range c.names {
		if _, ok := env[n]; ok {
			refs = append(refs, n)
		}
	}
	sort.Strings(refs)
	return p, refs, nil
}

// EvalCondition evaluates an alert condition in this scope. It returns the referenced values for display.
// A missing value (e.g. no last purchase yet) or an arithmetic error is reported as an error, not as false.
func (s *Scope) EvalCondition(ctx context.Context, src string) (bool, map[string]any, error) {
	aggs := make([]model.Aggregate, 0, len(s.Aggregates))
	for _, a := range s.Aggregates {
		aggs = append(aggs, a)
	}
	formulas := make([]model.Formula, 0, len(s.Formulas))
	for _, f := range s.Formulas {
		formulas = append(formulas, f)
	}
	p, refs, err := CompileCondition(src, aggs, formulas)
	if err != nil {
		return false, nil, err
	}
	env := map[string]any{}
	for _, r := range refs {
		if r == "now" {
			env[r] = float64(s.Q.Now().Unix())
			continue
		}
		v, err := s.Resolve(ctx, r)
		if err != nil {
			return false, nil, err
		}
		if f, ok := toFloat(v); ok {
			v = f
		} else {
			v = nil
		}
		env[r] = v
	}
	out, err := expr.Run(p, env)
	if err != nil {
		missing := []string{}
		for _, r := range refs {
			if env[r] == nil {
				missing = append(missing, r)
			}
		}
		if len(missing) > 0 {
			return false, env, fmt.Errorf("no value for %s", strings.Join(missing, ", "))
		}
		return false, env, err
	}
	b, _ := out.(bool)
	return b, env, nil
}
