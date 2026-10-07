// Package preset holds ready-made setups (aggregates, formulas, alerts, tracking events) for common use cases.
// Applying a preset only adds what is missing: existing names are kept untouched.
// Alert conditions only become true after the first data arrived, so a new site does not alert before install.
package preset

import (
	"context"
	"errors"

	"github.com/worotyns/agg/internal/alert"
	"github.com/worotyns/agg/internal/engine"
	"github.com/worotyns/agg/internal/model"
	"github.com/worotyns/agg/internal/query"
	"github.com/worotyns/agg/internal/store"
)

type Aggregate struct {
	Name string             `json:"name"`
	Def  model.AggregateDef `json:"def"`
}

type Formula struct {
	Name  string `json:"name"`
	Title string `json:"title"`
	Expr  string `json:"expr"`
	Unit  string `json:"unit"`
}

type Alert struct {
	Name string         `json:"name"`
	Def  model.AlertDef `json:"def"`
}

type Preset struct {
	ID          string      `json:"id"`
	Title       string      `json:"title"`
	Description string      `json:"description"`
	Includes    []string    `json:"includes,omitempty"`
	Setup       []string    `json:"setup"` // what the site has to send, for humans and LLMs
	Aggregates  []Aggregate `json:"aggregates"`
	Formulas    []Formula   `json:"formulas"`
	Alerts      []Alert     `json:"alerts"`
}

func pinned(title string, events []string, op model.Op) model.AggregateDef {
	return model.AggregateDef{Title: title, Events: events, Op: op, Pinned: true, Visibility: model.VisibilityPrivate}
}

func grouped(d model.AggregateDef, dim, expr, label string) model.AggregateDef {
	d.GroupBy = &model.Grouping{Dimension: dim, Expr: expr, Label: label}
	return d
}

var pv = []string{"page_view"}

var website = Preset{
	ID:          "website",
	Title:       "Website analytics",
	Description: "Page views, visitors, top pages, referrers, browsers and devices, with an alert when traffic stops.",
	Setup:       []string{"Install the snippet on every page. Page views (with path, referrer, browser and device) are sent automatically."},
	Aggregates: []Aggregate{
		{"page_views", pinned("Page views", pv, model.OpCount)},
		{"visitors", pinned("Visitors", pv, model.OpCountDistinct)},
		{"pages", grouped(pinned("Top pages", pv, model.OpCount), "page", "meta.path", "")},
		{"referrers", func() model.AggregateDef {
			d := grouped(pinned("Referrers", pv, model.OpCount), "referrer", "meta.referrer", "")
			d.Where = `meta.referrer != nil`
			return d
		}()},
		{"browsers", grouped(pinned("Browsers", pv, model.OpCountDistinct), "browser", "meta.browser", "")},
		{"devices", grouped(pinned("Devices", pv, model.OpCountDistinct), "device", "meta.device", "")},
	},
	Formulas: []Formula{{"views_per_visitor", "Page views per visitor", "round(page_views_24h / visitors_24h * 10) / 10", "number"}},
	Alerts: []Alert{{"no_traffic", model.AlertDef{Title: "No page views for 6 hours", Condition: "page_views_total > 0 && page_views_6h < 1",
		ForMinutes: 15, CooldownMinutes: 360, NotifyResolved: true, Push: true, Enabled: true}}},
}

var shop = Preset{
	ID:          "shop",
	Title:       "Online shop",
	Description: "Website analytics plus purchases, revenue, purchases per product, bestsellers per category, product viewers, conversion and order alerts.",
	Includes:    []string{"website"},
	Setup: []string{
		"Product page: agg.track('product_view', { id: '73', name: 'Trail shoe', category: 'shoes', price: 129 })",
		"Add to cart: agg.track('add_to_cart', { id: '73', name: 'Trail shoe', category: 'shoes', price: 129, quantity: 1 })",
		"Order confirmation: agg.track('purchase', { order_id: 'A-1001', value: 258, currency: 'EUR', items: [{ id: '73', name: 'Trail shoe', category: 'shoes', price: 129, quantity: 2 }] }, 'A-1001')  (the last argument makes a reloaded page count once)",
	},
	Aggregates: []Aggregate{
		{"purchases", pinned("Purchases", []string{"purchase"}, model.OpCount)},
		{"revenue", func() model.AggregateDef {
			d := pinned("Revenue", []string{"purchase"}, model.OpSum)
			d.Value = "props.value"
			return d
		}()},
		{"product_purchases", func() model.AggregateDef {
			d := grouped(pinned("Purchases per product", []string{"purchase"}, model.OpSum), "product", "item.id", "item.name")
			d.Explode, d.Value, d.Pinned = "props.items", "item.quantity ?? 1", false
			return d
		}()},
		{"bestsellers", func() model.AggregateDef {
			d := grouped(pinned("Bestsellers per category", []string{"purchase"}, model.OpSum), "category", "item.category", "")
			d.Explode, d.Value = "props.items", "item.quantity ?? 1"
			d.RankBy = &model.Grouping{Dimension: "product", Expr: "item.id", Label: "item.name"}
			return d
		}()},
		{"product_viewers", func() model.AggregateDef {
			d := grouped(pinned("Product viewers", []string{"product_view"}, model.OpCountDistinct), "product", "props.id", "props.name")
			d.Pinned = false
			return d
		}()},
		{"add_to_carts", func() model.AggregateDef {
			d := grouped(pinned("Add to carts", []string{"add_to_cart"}, model.OpCount), "product", "props.id", "props.name")
			d.Pinned = false
			return d
		}()},
		{"last_purchase", pinned("Last purchase", []string{"purchase"}, model.OpLastTimestamp)},
	},
	Formulas: []Formula{
		{"aov", "Average order value", "revenue_24h / purchases_24h", "number"},
		{"conversion_rate", "Conversion rate", "purchases_24h / visitors_24h * 100", "percent"},
	},
	Alerts: []Alert{
		{"no_orders", model.AlertDef{Title: "No orders for 3 hours", Condition: "now - last_purchase > 3 * 3600",
			CooldownMinutes: 180, NotifyResolved: true, Push: true, Enabled: true,
			Schedule: model.AlertSchedule{FromHour: 9, ToHour: 21, Timezone: "UTC"}}},
		{"revenue_drop", model.AlertDef{Title: "Revenue down 50% vs the previous 24 hours",
			Condition: "revenue_prev_24h > 0 && revenue_24h < 0.5 * revenue_prev_24h", ForMinutes: 60, CooldownMinutes: 720,
			NotifyResolved: true, Push: true, Enabled: true}},
	},
}

var saas = Preset{
	ID:          "saas",
	Title:       "SaaS / product insights",
	Description: "Website analytics plus sign-ups per plan, active users, feature usage and users per feature.",
	Includes:    []string{"website"},
	Setup: []string{
		"Sign-ups: agg.track('sign_up', { plan: 'pro', source: 'landing' }, userIdOrEmailHash).",
		"Feature usage: agg.track('feature_used', { feature: 'export_pdf' }) wherever a feature is used.",
	},
	Aggregates: []Aggregate{
		{"signups", grouped(pinned("Sign-ups", []string{"sign_up"}, model.OpCount), "plan", "props.plan", "")},
		{"active_users", pinned("Active users", []string{"page_view", "feature_used"}, model.OpCountDistinct)},
		{"feature_usage", grouped(pinned("Feature usage", []string{"feature_used"}, model.OpCount), "feature", "props.feature", "")},
		{"feature_users", func() model.AggregateDef {
			d := grouped(pinned("Users per feature", []string{"feature_used"}, model.OpCountDistinct), "feature", "props.feature", "")
			d.Pinned = false
			return d
		}()},
	},
	Formulas: []Formula{{"signup_rate", "Sign-up rate", "signups_24h / visitors_24h * 100", "percent"}},
	Alerts: []Alert{{"no_signups", model.AlertDef{Title: "No sign-ups for 24 hours", Condition: "signups_total > 0 && signups_24h < 1",
		ForMinutes: 30, CooldownMinutes: 1440, NotifyResolved: true, Push: true, Enabled: true}}},
}

var publisher = Preset{
	ID:          "publisher",
	Title:       "Blog / publisher",
	Description: "Website analytics plus most read articles and readers per section.",
	Includes:    []string{"website"},
	Setup:       []string{"On article pages: agg.track('article_read', { article: slug, title, section }) once the reader scrolled or stayed long enough."},
	Aggregates: []Aggregate{
		{"articles", grouped(pinned("Most read articles", []string{"article_read"}, model.OpCount), "article", "props.article", "props.title")},
		{"sections", grouped(pinned("Readers per section", []string{"article_read"}, model.OpCountDistinct), "section", "props.section", "")},
	},
}

// All presets in display order.
var All = []Preset{website, shop, saas, publisher}

func Get(id string) (Preset, bool) {
	for _, p := range All {
		if p.ID == id {
			return p, true
		}
	}
	return Preset{}, false
}

// Expanded returns the preset with included presets merged in.
func Expanded(p Preset) Preset {
	out := Preset{ID: p.ID, Title: p.Title, Description: p.Description, Includes: p.Includes}
	for _, inc := range p.Includes {
		if q, ok := Get(inc); ok {
			q = Expanded(q)
			out.Setup = append(out.Setup, q.Setup...)
			out.Aggregates = append(out.Aggregates, q.Aggregates...)
			out.Formulas = append(out.Formulas, q.Formulas...)
			out.Alerts = append(out.Alerts, q.Alerts...)
		}
	}
	out.Setup = append(out.Setup, p.Setup...)
	out.Aggregates = append(out.Aggregates, p.Aggregates...)
	out.Formulas = append(out.Formulas, p.Formulas...)
	out.Alerts = append(out.Alerts, p.Alerts...)
	return out
}

type Result struct {
	Created []string `json:"created"`
	Skipped []string `json:"skipped"`
	Setup   []string `json:"setup"`
}

// Apply adds the preset's missing aggregates, formulas and alerts to a site.
// timezone sets the alert schedules (IANA name, e.g. Europe/Warsaw); empty keeps UTC.
func Apply(ctx context.Context, st store.Storage, site model.Site, id, timezone string) (Result, error) {
	p, ok := Get(id)
	if !ok {
		return Result{}, errors.New("unknown preset " + id)
	}
	p = Expanded(p)
	res := Result{Created: []string{}, Skipped: []string{}, Setup: p.Setup}
	aggs, err := st.ListAggregates(ctx, site.ID)
	if err != nil {
		return res, err
	}
	have := map[string]bool{}
	for _, a := range aggs {
		have[a.Name] = true
	}
	for _, pa := range p.Aggregates {
		if have[pa.Name] {
			res.Skipped = append(res.Skipped, "aggregate "+pa.Name)
			continue
		}
		def := pa.Def
		if _, err := engine.Validate(&def); err != nil {
			return res, err
		}
		a := model.Aggregate{SiteID: site.ID, Name: pa.Name, AggregateDef: def}
		if err := st.CreateAggregate(ctx, &a); err != nil {
			return res, err
		}
		have[pa.Name] = true
		aggs = append(aggs, a)
		res.Created = append(res.Created, "aggregate "+pa.Name)
	}
	formulas, err := st.ListFormulas(ctx, site.ID)
	if err != nil {
		return res, err
	}
	for _, pf := range p.Formulas {
		exists := have[pf.Name]
		for _, f := range formulas {
			exists = exists || f.Name == pf.Name
		}
		if exists {
			res.Skipped = append(res.Skipped, "formula "+pf.Name)
			continue
		}
		f := model.Formula{SiteID: site.ID, Name: pf.Name, Title: pf.Title, Expr: pf.Expr, Unit: pf.Unit, Pinned: true}
		if err := query.ValidateFormula(&f, aggs); err != nil {
			return res, err
		}
		if err := st.CreateFormula(ctx, &f); err != nil {
			return res, err
		}
		formulas = append(formulas, f)
		res.Created = append(res.Created, "formula "+pf.Name)
	}
	alerts, err := st.ListAlerts(ctx, site.ID)
	if err != nil {
		return res, err
	}
	for _, pa := range p.Alerts {
		exists := false
		for _, a := range alerts {
			exists = exists || a.Name == pa.Name
		}
		if exists {
			res.Skipped = append(res.Skipped, "alert "+pa.Name)
			continue
		}
		def := pa.Def
		if timezone != "" {
			def.Schedule.Timezone = timezone
		}
		if err := alert.Validate(&def, aggs, formulas); err != nil {
			return res, err
		}
		a := model.Alert{SiteID: site.ID, Name: pa.Name, AlertDef: def}
		if err := st.CreateAlert(ctx, &a); err != nil {
			return res, err
		}
		res.Created = append(res.Created, "alert "+pa.Name)
	}
	return res, nil
}
