package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/worotyns/agg/internal/alert"
	"github.com/worotyns/agg/internal/model"
	"github.com/worotyns/agg/internal/preset"
	"github.com/worotyns/agg/internal/store"
	"github.com/worotyns/agg/internal/webpush"
	"github.com/worotyns/agg/web"
)

const settingVAPID = "vapid_keys"

// EnsureVAPID loads the Web Push key pair, generating it on first start.
func EnsureVAPID(ctx context.Context, st store.Storage) (*webpush.VAPID, error) {
	if v, err := st.GetSetting(ctx, settingVAPID); err == nil {
		var k webpush.VAPID
		if err := json.Unmarshal([]byte(v), &k); err != nil {
			return nil, err
		}
		return &k, k.Load()
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	k, err := webpush.GenerateVAPID()
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(k)
	return k, st.SetSetting(ctx, settingVAPID, string(b))
}

func (s *Server) handleServiceWorker(w http.ResponseWriter, r *http.Request) {
	b, _ := web.UI.ReadFile("ui/sw.js")
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(b)
}

// ---- presets

func (s *Server) listPresets(w http.ResponseWriter, r *http.Request) {
	out := make([]preset.Preset, 0, len(preset.All))
	for _, p := range preset.All {
		out = append(out, preset.Expanded(p))
	}
	writeJSON(w, 200, out)
}

func (s *Server) applyPreset(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	var body struct {
		Timezone string `json:"timezone"`
	}
	if r.ContentLength > 0 && !readJSON(w, r, &body) {
		return
	}
	res, err := preset.Apply(r.Context(), s.st, site, r.PathValue("id"), body.Timezone)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	s.reload(r.Context())
	writeJSON(w, 200, res)
}

// ---- alerts

type alertRow struct {
	model.Alert
	Events []model.AlertEvent `json:"events,omitempty"`
}

func (s *Server) alertOf(w http.ResponseWriter, r *http.Request, site model.Site) (model.Alert, bool) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	a, err := s.st.GetAlert(r.Context(), id)
	if err == nil && a.SiteID != site.ID {
		err = store.ErrNotFound
	}
	if err != nil {
		s.fail(w, err)
		return a, false
	}
	return a, true
}

func (s *Server) listAlerts(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	as, err := s.st.ListAlerts(r.Context(), site.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, as)
}

func (s *Server) alertEvents(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	a, ok := s.alertOf(w, r, site)
	if !ok {
		return
	}
	evs, err := s.st.ListAlertEvents(r.Context(), a.ID, 50)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, evs)
}

func (s *Server) siteDefs(ctx context.Context, siteID int64) ([]model.Aggregate, []model.Formula, error) {
	aggs, err := s.st.ListAggregates(ctx, siteID)
	if err != nil {
		return nil, nil, err
	}
	fs, err := s.st.ListFormulas(ctx, siteID)
	return aggs, fs, err
}

type alertBody struct {
	Name string `json:"name"`
	model.AlertDef
}

func (s *Server) readAlert(w http.ResponseWriter, r *http.Request, site model.Site) (alertBody, bool) {
	var b alertBody
	if !readJSON(w, r, &b) {
		return b, false
	}
	aggs, fs, err := s.siteDefs(r.Context(), site.ID)
	if err != nil {
		s.fail(w, err)
		return b, false
	}
	if err := alert.Validate(&b.AlertDef, aggs, fs); err != nil {
		writeErr(w, 400, err.Error())
		return b, false
	}
	return b, true
}

func (s *Server) createAlert(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	b, ok := s.readAlert(w, r, site)
	if !ok {
		return
	}
	b.Name = strings.TrimSpace(b.Name)
	if err := model.ValidateSlug(b.Name); err != nil {
		writeErr(w, 400, "name "+err.Error())
		return
	}
	a := model.Alert{SiteID: site.ID, Name: b.Name, AlertDef: b.AlertDef}
	if err := s.st.CreateAlert(r.Context(), &a); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 201, a)
}

func (s *Server) updateAlert(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	a, ok := s.alertOf(w, r, site)
	if !ok {
		return
	}
	b, ok := s.readAlert(w, r, site)
	if !ok {
		return
	}
	a.AlertDef = b.AlertDef
	if err := s.st.UpdateAlert(r.Context(), &a); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, a)
}

func (s *Server) deleteAlert(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	a, ok := s.alertOf(w, r, site)
	if !ok {
		return
	}
	if err := s.st.DeleteAlert(r.Context(), a.ID); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// checkAlert evaluates a condition now, without saving, for the editor preview.
func (s *Server) checkAlert(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	var def model.AlertDef
	if !readJSON(w, r, &def) {
		return
	}
	aggs, fs, err := s.siteDefs(r.Context(), site.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	res, vals, err := s.alerts.Check(r.Context(), def, aggs, fs)
	delete(vals, "now")
	out := map[string]any{"result": res, "values": vals}
	if err != nil {
		out["error"] = err.Error()
	}
	writeJSON(w, 200, out)
}

// testAlert sends a test notification through the alert's channels.
func (s *Server) testAlert(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	a, ok := s.alertOf(w, r, site)
	if !ok {
		return
	}
	title := a.Title
	if title == "" {
		title = a.Name
	}
	s.alerts.Notify(r.Context(), site, a, "Test: "+title, "This is a test notification for "+a.Condition)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---- notifications and push

func (s *Server) listNotifications(w http.ResponseWriter, r *http.Request) {
	ns, unread, err := s.st.ListNotifications(r.Context(), 50)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": ns, "unread": unread})
}

func (s *Server) readNotifications(w http.ResponseWriter, r *http.Request) {
	if err := s.st.MarkNotificationsRead(r.Context()); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) pushInfo(w http.ResponseWriter, r *http.Request) {
	subs, err := s.st.ListPushSubscriptions(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	pub := ""
	if s.alerts != nil && s.alerts.Push != nil {
		pub = s.alerts.Push.VAPID.PublicKey
	}
	writeJSON(w, 200, map[string]any{"publicKey": pub, "subscriptions": subs})
}

type pushSubBody struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

func (s *Server) subscribePush(w http.ResponseWriter, r *http.Request) {
	var b pushSubBody
	if !readJSON(w, r, &b) {
		return
	}
	if !strings.HasPrefix(b.Endpoint, "https://") || b.Keys.P256dh == "" || b.Keys.Auth == "" {
		writeErr(w, 400, "invalid subscription")
		return
	}
	sub := model.PushSubscription{Endpoint: b.Endpoint, P256dh: b.Keys.P256dh, Auth: b.Keys.Auth, UserAgent: r.UserAgent()}
	if err := s.st.SavePushSubscription(r.Context(), sub); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 201, map[string]bool{"ok": true})
}

func (s *Server) unsubscribePush(w http.ResponseWriter, r *http.Request) {
	var b pushSubBody
	if !readJSON(w, r, &b) {
		return
	}
	if err := s.st.DeletePushSubscription(r.Context(), b.Endpoint); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// testPush sends a test message to every subscribed browser and reports per-device results.
func (s *Server) testPush(w http.ResponseWriter, r *http.Request) {
	subs, err := s.st.ListPushSubscriptions(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	type result struct {
		Endpoint string `json:"endpoint"`
		Error    string `json:"error,omitempty"`
	}
	out := []result{}
	for _, sub := range subs {
		err := s.alerts.Push.Send(r.Context(), webpush.Subscription{Endpoint: sub.Endpoint, P256dh: sub.P256dh, Auth: sub.Auth},
			webpush.Message{Title: "agg: test notification", Body: "Browser notifications work on this device.", URL: s.baseURL(r) + "/", Tag: "agg-test"})
		res := result{Endpoint: sub.Endpoint}
		if errors.Is(err, webpush.ErrGone) {
			s.st.DeletePushSubscription(r.Context(), sub.Endpoint)
			res.Error = "subscription expired, removed"
		} else if err != nil {
			res.Error = err.Error()
		}
		out = append(out, res)
	}
	writeJSON(w, 200, out)
}

// ---- API tokens

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	ts, err := s.st.ListAPITokens(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, ts)
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	b.Name = strings.TrimSpace(b.Name)
	if b.Name == "" {
		writeErr(w, 400, "name is required")
		return
	}
	token := "agg_api_" + randomString(32)
	t := model.APIToken{Name: b.Name, Prefix: token[:13], TokenHash: hashToken(token)}
	if err := s.st.CreateAPIToken(r.Context(), &t); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{"token": token, "apiToken": t})
}

func (s *Server) deleteToken(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.st.DeleteAPIToken(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// apiTokenValid checks a named API token and records its use (at most once a minute).
func (s *Server) apiTokenValid(ctx context.Context, token string) bool {
	if !strings.HasPrefix(token, "agg_api_") {
		return false
	}
	t, err := s.st.APITokenByHash(ctx, hashToken(token))
	if err != nil {
		return false
	}
	now := time.Now().UnixMilli()
	if now-t.LastUsedAt > 60000 {
		s.st.TouchAPIToken(ctx, t.ID, now)
	}
	return true
}
