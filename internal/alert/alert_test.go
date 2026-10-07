package alert_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/worotyns/agg/internal/alert"
	"github.com/worotyns/agg/internal/model"
	"github.com/worotyns/agg/internal/testutil"
	"github.com/worotyns/agg/internal/webpush"
)

type hooks struct {
	mu   sync.Mutex
	got  []alert.WebhookPayload
	push int
}

func setup(t *testing.T, def model.AlertDef) (*testutil.Fixture, *alert.Engine, *hooks, func() model.Alert) {
	f := testutil.New(t)
	f.Agg("orders", model.AggregateDef{Events: []string{"purchase"}, Op: model.OpCount})
	h := &hooks{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if r.URL.Path == "/push" {
			h.push++
			w.WriteHeader(201)
			return
		}
		var p alert.WebhookPayload
		json.NewDecoder(r.Body).Decode(&p)
		h.got = append(h.got, p)
	}))
	t.Cleanup(srv.Close)
	ctx := context.Background()
	vapid, _ := webpush.GenerateVAPID()
	e := &alert.Engine{St: f.St, Q: f.Q, Push: &webpush.Sender{VAPID: vapid}, Log: f.Log, Now: func() time.Time { return f.Now },
		BaseURL: func() string { return "https://agg.example.com" }}
	// a subscribed browser
	sub := model.PushSubscription{Endpoint: srv.URL + "/push", P256dh: "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4", Auth: "BTBZMqHH6r4Tts7J_aSIgg"}
	if err := f.St.SavePushSubscription(ctx, sub); err != nil {
		t.Fatal(err)
	}
	def.Webhook = srv.URL + "/hook"
	def.Enabled = true
	def.Push = true
	aggs, _ := f.St.ListAggregates(ctx, f.Site.ID)
	if err := alert.Validate(&def, aggs, nil); err != nil {
		t.Fatal(err)
	}
	a := model.Alert{SiteID: f.Site.ID, Name: "no_orders", AlertDef: def}
	if err := f.St.CreateAlert(ctx, &a); err != nil {
		t.Fatal(err)
	}
	get := func() model.Alert {
		x, err := f.St.GetAlert(ctx, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		return x
	}
	return f, e, h, get
}

func tick(t *testing.T, f *testutil.Fixture, e *alert.Engine, d time.Duration) {
	f.Advance(d)
	if err := e.EvaluateAll(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPendingFiringResolved(t *testing.T) {
	f, e, h, get := setup(t, model.AlertDef{Condition: "orders_1h < 1", ForMinutes: 10, NotifyResolved: true})
	tick(t, f, e, time.Minute)
	if a := get(); a.State != model.AlertPending || a.LastValue != "orders_1h = 0" {
		t.Fatalf("after 1 min: %s %q", a.State, a.LastValue)
	}
	tick(t, f, e, 5*time.Minute)
	if get().State != model.AlertPending || len(h.got) != 0 {
		t.Fatal("must stay pending until `for` has passed")
	}
	tick(t, f, e, 5*time.Minute)
	if get().State != model.AlertFiring {
		t.Fatalf("state %s, want firing", get().State)
	}
	if len(h.got) != 1 || h.got[0].Status != "firing" || h.got[0].Text == "" || h.push != 1 {
		t.Fatalf("webhook %+v push %d", h.got, h.push)
	}
	tick(t, f, e, time.Minute)
	if len(h.got) != 1 {
		t.Fatal("firing must notify once, not every minute")
	}
	f.Send(testutil.Purchase("", 1))
	tick(t, f, e, time.Minute)
	if get().State != model.AlertOK || len(h.got) != 2 || h.got[1].Status != "resolved" {
		t.Fatalf("resolve: %s %+v", get().State, h.got)
	}
	notes, unread, _ := f.St.ListNotifications(context.Background(), 10)
	if len(notes) != 2 || unread != 2 || notes[1].URL != "https://agg.example.com/#/s/1/alerts" {
		t.Fatalf("notifications %+v", notes)
	}
	evs, _ := f.St.ListAlertEvents(context.Background(), get().ID, 10)
	if len(evs) != 3 { // pending, firing, ok
		t.Fatalf("history %+v", evs)
	}
}

func TestPendingClearsWithoutNotification(t *testing.T) {
	f, e, h, get := setup(t, model.AlertDef{Condition: "orders_1h < 1", ForMinutes: 30})
	tick(t, f, e, time.Minute)
	f.Send(testutil.Purchase("", 1))
	tick(t, f, e, time.Minute)
	if get().State != model.AlertOK || len(h.got) != 0 {
		t.Fatal("a short dip must not notify")
	}
}

func TestCooldownLimitsRepeatedNotifications(t *testing.T) {
	f, e, h, _ := setup(t, model.AlertDef{Condition: "orders_5m < 1", CooldownMinutes: 60})
	tick(t, f, e, time.Minute) // fires immediately (for = 0)
	f.Send(testutil.Purchase("", 1))
	tick(t, f, e, time.Minute)    // resolved (no resolve notification configured)
	tick(t, f, e, 10*time.Minute) // fires again, inside cooldown: no notification
	if len(h.got) != 1 {
		t.Fatalf("notifications %d, want 1 within cooldown", len(h.got))
	}
	f.Send(testutil.Purchase("", 1))
	tick(t, f, e, time.Minute)
	tick(t, f, e, 70*time.Minute)
	if len(h.got) != 2 {
		t.Fatalf("notifications %d, want 2 after cooldown", len(h.got))
	}
}

func TestScheduleSuppressesNotifications(t *testing.T) {
	// testutil.Start is 12:30 UTC on a Wednesday
	f, e, h, get := setup(t, model.AlertDef{Condition: "orders_1h < 1", Schedule: model.AlertSchedule{FromHour: 18, ToHour: 22, Timezone: "UTC"}})
	tick(t, f, e, time.Minute)
	if get().State != model.AlertFiring || len(h.got) != 0 {
		t.Fatal("outside the schedule the alert fires silently")
	}
	if !alert.Active(model.AlertSchedule{FromHour: 22, ToHour: 6}, time.Date(2026, 1, 1, 23, 0, 0, 0, time.UTC)) ||
		alert.Active(model.AlertSchedule{Days: []int{0, 6}}, testutil.Start) {
		t.Error("overnight hours or weekdays wrong")
	}
	if !alert.Active(model.AlertSchedule{FromHour: 14, ToHour: 15, Timezone: "Europe/Warsaw"}, testutil.Start) {
		t.Error("timezone not applied (12:30 UTC is 14:30 in Warsaw)")
	}
}

func TestMissingValueKeepsState(t *testing.T) {
	f := testutil.New(t)
	f.Agg("last_order", model.AggregateDef{Events: []string{"purchase"}, Op: model.OpLastTimestamp})
	aggs, _ := f.St.ListAggregates(context.Background(), f.Site.ID)
	def := model.AlertDef{Condition: "now - last_order > 7200", Enabled: true}
	if err := alert.Validate(&def, aggs, nil); err != nil {
		t.Fatal(err)
	}
	a := model.Alert{SiteID: f.Site.ID, Name: "stale", AlertDef: def}
	f.St.CreateAlert(context.Background(), &a)
	e := &alert.Engine{St: f.St, Q: f.Q, Log: f.Log, Now: func() time.Time { return f.Now }}
	tick(t, f, e, time.Minute)
	got, _ := f.St.GetAlert(context.Background(), a.ID)
	if got.State != model.AlertOK || got.LastError != "no value for last_order" {
		t.Fatalf("state %s error %q", got.State, got.LastError)
	}
	f.Send(testutil.Purchase("", 1))
	tick(t, f, e, 3*time.Hour)
	got, _ = f.St.GetAlert(context.Background(), a.ID)
	if got.State != model.AlertFiring {
		t.Fatalf("state %s (%s), want firing 3h after the last order", got.State, got.LastError)
	}
}

func TestValidate(t *testing.T) {
	for _, c := range []model.AlertDef{
		{Condition: "orders_1h"},     // not boolean
		{Condition: "orderz_1h < 1"}, // unknown variable
		{Condition: "orders_1h < 1", Webhook: "ftp://x"},
		{Condition: "orders_1h < 1", Schedule: model.AlertSchedule{Timezone: "Mars/Base"}},
	} {
		def := c
		aggs := []model.Aggregate{{Name: "orders", AggregateDef: model.AggregateDef{Op: model.OpCount}}}
		if err := alert.Validate(&def, aggs, nil); err == nil {
			t.Errorf("%+v must be rejected", c)
		}
	}
}
