// Package alert evaluates alert rules every minute and sends notifications:
// in-app notifications, Web Push to subscribed browsers and an optional webhook.
//
// States: ok → pending (condition true, waiting for "for") → firing → ok (resolved).
// A condition that cannot be evaluated (e.g. no value yet) leaves the state unchanged.
package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/worotyns/agg/internal/export"
	"github.com/worotyns/agg/internal/model"
	"github.com/worotyns/agg/internal/query"
	"github.com/worotyns/agg/internal/store"
	"github.com/worotyns/agg/internal/webpush"
)

type Engine struct {
	St      store.Storage
	Q       *query.Querier
	Push    *webpush.Sender
	Log     *slog.Logger
	Now     func() time.Time
	BaseURL func() string
	HTTP    *http.Client

	mu sync.Mutex // one evaluation pass at a time
}

// Validate checks and normalizes an alert definition against the site's aggregates and formulas.
func Validate(def *model.AlertDef, aggs []model.Aggregate, formulas []model.Formula) error {
	def.Title = strings.TrimSpace(def.Title)
	if _, _, err := query.CompileCondition(def.Condition, aggs, formulas); err != nil {
		return fmt.Errorf("condition: %w", err)
	}
	if def.ForMinutes < 0 || def.ForMinutes > 7*24*60 {
		return errors.New("for must be between 0 and 10080 minutes")
	}
	if def.CooldownMinutes < 0 || def.CooldownMinutes > 7*24*60 {
		return errors.New("cooldown must be between 0 and 10080 minutes")
	}
	if def.Webhook != "" && !strings.HasPrefix(def.Webhook, "https://") && !strings.HasPrefix(def.Webhook, "http://") {
		return errors.New("webhook must be an http(s) URL")
	}
	sc := &def.Schedule
	if sc.FromHour < 0 || sc.FromHour > 24 || sc.ToHour < 0 || sc.ToHour > 24 {
		return errors.New("schedule hours must be between 0 and 24")
	}
	for _, d := range sc.Days {
		if d < 0 || d > 6 {
			return errors.New("schedule days must be 0 (Sunday) to 6 (Saturday)")
		}
	}
	if sc.Days == nil {
		sc.Days = []int{}
	}
	if sc.Timezone == "" {
		sc.Timezone = "UTC"
	}
	if _, err := time.LoadLocation(sc.Timezone); err != nil {
		return fmt.Errorf("unknown timezone %q", sc.Timezone)
	}
	if def.Dims == nil {
		def.Dims = map[string]string{}
	}
	return nil
}

// Active reports whether notifications are allowed at t.
func Active(s model.AlertSchedule, t time.Time) bool {
	if loc, err := time.LoadLocation(s.Timezone); err == nil && s.Timezone != "" {
		t = t.In(loc)
	}
	if len(s.Days) > 0 {
		ok := false
		for _, d := range s.Days {
			ok = ok || int(t.Weekday()) == d
		}
		if !ok {
			return false
		}
	}
	h := t.Hour()
	switch {
	case s.FromHour == s.ToHour:
		return true
	case s.FromHour < s.ToHour:
		return h >= s.FromHour && h < s.ToHour
	default: // overnight, e.g. 22 → 6
		return h >= s.FromHour || h < s.ToHour
	}
}

// Run evaluates all alerts every interval until ctx is done.
func (e *Engine) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := e.EvaluateAll(ctx); err != nil {
				e.Log.Error("alert evaluation failed", "err", err)
			}
		}
	}
}

// EvaluateAll evaluates every enabled alert once.
func (e *Engine) EvaluateAll(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	alerts, err := e.St.ListAlerts(ctx, 0)
	if err != nil {
		return err
	}
	type siteData struct {
		site     model.Site
		aggs     []model.Aggregate
		formulas []model.Formula
	}
	cache := map[int64]*siteData{}
	for _, a := range alerts {
		if !a.Enabled {
			continue
		}
		sd := cache[a.SiteID]
		if sd == nil {
			site, err := e.St.GetSite(ctx, a.SiteID)
			if err != nil {
				return err
			}
			aggs, err := e.St.ListAggregates(ctx, a.SiteID)
			if err != nil {
				return err
			}
			fs, err := e.St.ListFormulas(ctx, a.SiteID)
			if err != nil {
				return err
			}
			sd = &siteData{site, aggs, fs}
			cache[a.SiteID] = sd
		}
		if err := e.evaluate(ctx, sd.site, a, sd.aggs, sd.formulas); err != nil {
			e.Log.Error("alert failed", "alert", a.Name, "err", err)
		}
	}
	return nil
}

// Check evaluates a condition without changing any state (used by the editor preview).
func (e *Engine) Check(ctx context.Context, def model.AlertDef, aggs []model.Aggregate, formulas []model.Formula) (bool, map[string]any, error) {
	sc := query.NewScope(e.Q, aggs, formulas, def.Dims)
	return sc.EvalCondition(ctx, def.Condition)
}

func describe(vals map[string]any) string {
	keys := make([]string, 0, len(vals))
	for k := range vals {
		if k != "now" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	parts := []string{}
	for _, k := range keys {
		v := "no value"
		if f, ok := vals[k].(float64); ok {
			v = export.FormatValue(f)
		}
		parts = append(parts, k+" = "+v)
	}
	return strings.Join(parts, ", ")
}

func (e *Engine) evaluate(ctx context.Context, site model.Site, a model.Alert, aggs []model.Aggregate, formulas []model.Formula) error {
	now := e.Now()
	nowMs := now.UnixMilli()
	res, vals, err := e.Check(ctx, a.AlertDef, aggs, formulas)
	desc := describe(vals)
	a.LastEval = nowMs
	if err != nil {
		a.LastError = err.Error()
		return e.St.SaveAlertState(ctx, &a)
	}
	a.LastError, a.LastValue = "", desc
	event := func(state, msg string) error {
		a.State, a.StateSince = state, nowMs
		return e.St.AddAlertEvent(ctx, model.AlertEvent{AlertID: a.ID, TS: nowMs, State: state, Message: msg})
	}
	active := Active(a.Schedule, now)
	title := a.Title
	if title == "" {
		title = a.Name
	}
	if res {
		if a.State == model.AlertOK {
			if err := event(model.AlertPending, desc); err != nil {
				return err
			}
		}
		if a.State == model.AlertPending && nowMs-a.StateSince >= int64(a.ForMinutes)*60000 {
			if err := event(model.AlertFiring, desc); err != nil {
				return err
			}
			cooled := nowMs-a.LastNotified >= int64(a.CooldownMinutes)*60000
			if active && cooled {
				e.Notify(ctx, site, a, "Alert: "+title, a.Condition+" · "+desc)
				a.LastNotified = nowMs
			}
		}
	} else {
		switch a.State {
		case model.AlertPending:
			if err := event(model.AlertOK, "condition cleared before firing: "+desc); err != nil {
				return err
			}
		case model.AlertFiring:
			notifiedThisTime := a.LastNotified >= a.StateSince
			if err := event(model.AlertOK, "resolved: "+desc); err != nil {
				return err
			}
			if a.NotifyResolved && active && notifiedThisTime {
				e.Notify(ctx, site, a, "Resolved: "+title, desc)
			}
		}
	}
	return e.St.SaveAlertState(ctx, &a)
}

// Notify records an in-app notification and delivers it by Web Push and webhook.
// Delivery errors are logged; they never block the evaluation of other alerts.
func (e *Engine) Notify(ctx context.Context, site model.Site, a model.Alert, title, body string) {
	url := ""
	if e.BaseURL != nil {
		if b := e.BaseURL(); b != "" {
			url = fmt.Sprintf("%s/#/s/%d/alerts", b, site.ID)
		}
	}
	body = body + " · " + site.Name
	n := model.Notification{SiteID: site.ID, AlertID: a.ID, TS: e.Now().UnixMilli(), Title: title, Body: body, URL: url}
	if err := e.St.AddNotification(ctx, &n); err != nil {
		e.Log.Error("cannot store notification", "err", err)
	}
	if a.Push && e.Push != nil {
		subs, err := e.St.ListPushSubscriptions(ctx)
		if err != nil {
			e.Log.Error("cannot list push subscriptions", "err", err)
		}
		for _, s := range subs {
			err := e.Push.Send(ctx, webpush.Subscription{Endpoint: s.Endpoint, P256dh: s.P256dh, Auth: s.Auth},
				webpush.Message{Title: title, Body: body, URL: url, Tag: fmt.Sprintf("agg-alert-%d", a.ID)})
			if errors.Is(err, webpush.ErrGone) {
				e.St.DeletePushSubscription(ctx, s.Endpoint)
			} else if err != nil {
				e.Log.Warn("web push failed", "endpoint", s.Endpoint, "err", err)
			}
		}
	}
	if a.Webhook != "" {
		if err := e.webhook(ctx, site, a, title, body, url); err != nil {
			e.Log.Warn("webhook failed", "alert", a.Name, "err", err)
		}
	}
}

// WebhookPayload is posted as JSON. "text" and "content" make it work directly with Slack, Mattermost and
// Discord incoming webhooks.
type WebhookPayload struct {
	Status    string `json:"status"`
	Alert     string `json:"alert"`
	Title     string `json:"title"`
	Site      string `json:"site"`
	Condition string `json:"condition"`
	Values    string `json:"values"`
	URL       string `json:"url,omitempty"`
	Time      string `json:"time"`
	Text      string `json:"text"`
	Content   string `json:"content"`
}

func (e *Engine) webhook(ctx context.Context, site model.Site, a model.Alert, title, body, url string) error {
	status := "firing"
	if strings.HasPrefix(title, "Resolved") {
		status = "resolved"
	} else if strings.HasPrefix(title, "Test") {
		status = "test"
	}
	text := title + ": " + body
	p := WebhookPayload{Status: status, Alert: a.Name, Title: title, Site: site.Slug, Condition: a.Condition, Values: a.LastValue,
		URL: url, Time: e.Now().UTC().Format(time.RFC3339), Text: text, Content: text}
	b, _ := json.Marshal(p)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.Webhook, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "agg-alerts")
	client := e.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	res.Body.Close()
	if res.StatusCode >= 300 {
		return fmt.Errorf("webhook returned %s", res.Status)
	}
	return nil
}
