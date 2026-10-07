package server_test

import (
	"encoding/json"
	"strings"
	"testing"
)

func (a *app) rpc(token string, method string, params any) (int, map[string]any) {
	a.T.Helper()
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		body["params"] = params
	}
	res, out := a.do("POST", "/mcp", body, "Authorization", "Bearer "+token, "Accept", "application/json, text/event-stream")
	var m map[string]any
	json.Unmarshal(out, &m)
	return res.StatusCode, m
}

func (a *app) tool(token, name string, args map[string]any) (string, bool) {
	a.T.Helper()
	_, m := a.rpc(token, "tools/call", map[string]any{"name": name, "arguments": args})
	r, ok := m["result"].(map[string]any)
	if !ok {
		a.T.Fatalf("tools/call %s: %v", name, m)
	}
	text := r["content"].([]any)[0].(map[string]any)["text"].(string)
	return text, r["isError"].(bool)
}

func TestAPITokensAreRevocableAndCannotLogIn(t *testing.T) {
	a := newApp(t)
	m := a.admin("POST", "/api/tokens", map[string]string{"name": "claude"})
	token := m["token"].(string)
	if !strings.HasPrefix(token, "agg_api_") {
		t.Fatal(token)
	}
	if res, _ := a.do("GET", "/api/sites", nil, "Authorization", "Bearer "+token); res.StatusCode != 200 {
		t.Fatalf("api token on admin API: %d", res.StatusCode)
	}
	if res, _ := a.do("POST", "/api/login", map[string]string{"token": token}); res.StatusCode != 401 {
		t.Fatal("API tokens must not log in to the UI")
	}
	list := a.admin("GET", "/api/tokens", nil)
	_ = list
	id := int(m["apiToken"].(map[string]any)["id"].(float64))
	a.admin("DELETE", "/api/tokens/"+itoa(id), nil)
	if res, _ := a.do("GET", "/api/sites", nil, "Authorization", "Bearer "+token); res.StatusCode != 401 {
		t.Fatal("revoked token still works")
	}
	if code, _ := a.rpc(token, "tools/list", nil); code != 401 {
		t.Fatalf("revoked token on MCP: %d", code)
	}
}

func TestMCPProtocolAndSetupFlow(t *testing.T) {
	a := newApp(t)
	token := a.admin("POST", "/api/tokens", map[string]string{"name": "mcp"})["token"].(string)
	if code, _ := a.rpc("nope", "initialize", nil); code != 401 {
		t.Fatalf("MCP without a valid token: %d", code)
	}
	_, init := a.rpc(token, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test"}})
	r := init["result"].(map[string]any)
	if r["protocolVersion"] != "2025-06-18" || r["instructions"] == "" {
		t.Fatalf("initialize: %v", init)
	}
	if res, _ := a.do("POST", "/mcp", map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}, "Authorization", "Bearer "+token); res.StatusCode != 202 {
		t.Fatalf("notification: %d", res.StatusCode)
	}
	_, tl := a.rpc(token, "tools/list", nil)
	tools := tl["result"].(map[string]any)["tools"].([]any)
	if len(tools) < 20 {
		t.Fatalf("only %d tools", len(tools))
	}
	for _, x := range tools {
		tool := x.(map[string]any)
		if tool["description"] == "" || tool["inputSchema"].(map[string]any)["type"] != "object" {
			t.Errorf("tool %v has no description or object schema", tool["name"])
		}
	}

	// plan → create site with a preset → snippet → events → test → aggregate → values → alert
	text, isErr := a.tool(token, "create_site", map[string]any{"name": "App", "preset": "saas", "timezone": "Europe/Warsaw"})
	if isErr || !strings.Contains(text, "aggregate signups") {
		t.Fatalf("create_site: %s", text)
	}
	var site struct {
		ID        int    `json:"id"`
		PublicKey string `json:"publicKey"`
	}
	json.Unmarshal([]byte(text), &site)
	if text, _ = a.tool(token, "get_install_snippet", map[string]any{"siteId": site.ID}); !strings.Contains(text, site.PublicKey) {
		t.Fatalf("snippet: %s", text)
	}
	a.do("POST", "/e", `{"site":"`+site.PublicKey+`","events":[{"name":"feature_used","visitorId":"u1","props":{"feature":"export"}},{"name":"feature_used","visitorId":"u2","props":{"feature":"export"}}]}`)
	a.flush()
	if text, _ = a.tool(token, "list_event_names", map[string]any{"siteId": site.ID}); !strings.Contains(text, "feature_used") {
		t.Fatalf("event names: %s", text)
	}
	text, _ = a.tool(token, "test_aggregate", map[string]any{"siteId": site.ID, "def": map[string]any{"events": []string{"feature_used"}, "op": "count",
		"groupBy": map[string]any{"dimension": "feature", "expr": "props.feature"}}})
	if !strings.HasPrefix(text, "2 of 2 events matched") {
		t.Fatalf("test_aggregate: %s", text)
	}
	if text, isErr = a.tool(token, "create_aggregate", map[string]any{"siteId": site.ID, "name": "exports", "events": []string{"feature_used"}, "op": "count", "where": `props.feature == "export"`}); isErr {
		t.Fatalf("create_aggregate: %s", text)
	}
	if text, isErr = a.tool(token, "create_aggregate", map[string]any{"siteId": site.ID, "name": "bad", "events": []string{"x"}, "op": "sum"}); !isErr || !strings.Contains(text, "Value") {
		t.Fatalf("invalid aggregate must return a tool error: %s", text)
	}
	if text, _ = a.tool(token, "rebuild_aggregate", map[string]any{"siteId": site.ID, "aggregateId": 1}); !strings.Contains(text, `"events": 0`) {
		t.Logf("rebuild: %s", text)
	}
	text, _ = a.tool(token, "get_values", map[string]any{"siteId": site.ID, "v": []string{"feature_usage_24h", "active_users_24h"}, "dimensions": map[string]any{"feature": "export"}})
	if !strings.Contains(text, `"feature_usage_24h": 2`) || !strings.Contains(text, `"active_users_24h": 2`) {
		t.Fatalf("get_values: %s", text)
	}
	if text, _ = a.tool(token, "check_alert", map[string]any{"siteId": site.ID, "condition": "feature_usage_1h > 1"}); !strings.Contains(text, `"result": true`) {
		t.Fatalf("check_alert: %s", text)
	}
	if text, isErr = a.tool(token, "create_alert", map[string]any{"siteId": site.ID, "name": "busy", "condition": "feature_usage_1h > 100", "push": true}); isErr {
		t.Fatalf("create_alert: %s", text)
	}
	if text, _ = a.tool(token, "update_tracking", map[string]any{"siteId": site.ID, "requireConsent": true}); !strings.Contains(text, `"requireConsent": true`) || !strings.Contains(text, `"visitorId": true`) {
		t.Fatalf("update_tracking must merge: %s", text)
	}
	if text, _ = a.tool(token, "create_export", map[string]any{"siteId": site.ID, "name": "prom", "format": "prometheus", "scope": map[string]any{"aggregates": []string{"signups"}}}); !strings.Contains(text, "credentials: agg_exp_") {
		t.Fatalf("create_export: %s", text)
	}

	_, rl := a.rpc(token, "resources/list", nil)
	if n := len(rl["result"].(map[string]any)["resources"].([]any)); n < 5 {
		t.Fatalf("resources: %d", n)
	}
	_, rr := a.rpc(token, "resources/read", map[string]any{"uri": "agg://docs/overview"})
	if !strings.Contains(rr["result"].(map[string]any)["contents"].([]any)[0].(map[string]any)["text"].(string), "agg") {
		t.Fatal("overview resource empty")
	}
	_, pg := a.rpc(token, "prompts/get", map[string]any{"name": "plan_setup", "arguments": map[string]any{"goal": "feature adoption"}})
	if !strings.Contains(pg["result"].(map[string]any)["messages"].([]any)[0].(map[string]any)["content"].(map[string]any)["text"].(string), "feature adoption") {
		t.Fatal("prompt")
	}
	if _, m := a.rpc(token, "nope/nope", nil); m["error"] == nil {
		t.Fatal("unknown method must be a JSON-RPC error")
	}
}

func TestAlertsAPIAndPushSubscription(t *testing.T) {
	a := newApp(t)
	base := "/api/sites/" + itoa(int(a.Site.ID))
	a.admin("POST", base+"/aggregates", map[string]any{"name": "orders", "events": []string{"purchase"}, "op": "count"})
	if res, out := a.do("POST", base+"/alerts", map[string]any{"name": "x", "condition": "orderz_1h < 1"}, "Authorization", "Bearer "+adminToken); res.StatusCode != 400 {
		t.Fatalf("invalid condition accepted: %s", out)
	}
	al := a.admin("POST", base+"/alerts", map[string]any{"name": "no_orders", "condition": "orders_1h < 1", "enabled": true, "push": true})
	if al["state"] != "ok" {
		t.Fatal(al)
	}
	chk := a.admin("POST", base+"/alerts/check", map[string]any{"condition": "orders_1h < 1"})
	if chk["result"] != true || chk["values"].(map[string]any)["orders_1h"] != 0.0 {
		t.Fatal(chk)
	}
	push := a.admin("GET", "/api/push", nil)
	if len(push["publicKey"].(string)) != 87 {
		t.Fatalf("VAPID public key: %v", push["publicKey"])
	}
	if res, _ := a.do("POST", "/api/push/subscriptions", map[string]any{"endpoint": "http://insecure", "keys": map[string]string{"p256dh": "x", "auth": "y"}}, "Authorization", "Bearer "+adminToken); res.StatusCode != 400 {
		t.Fatal("non-https push endpoint accepted")
	}
	a.admin("POST", base+"/alerts/"+itoa(int(al["id"].(float64)))+"/test", nil)
	n := a.admin("GET", "/api/notifications", nil)
	if n["unread"] != 1.0 {
		t.Fatalf("test notification not stored: %v", n)
	}
	a.admin("POST", "/api/notifications/read", nil)
	if a.admin("GET", "/api/notifications", nil)["unread"] != 0.0 {
		t.Fatal("mark read")
	}
	if res, out := a.do("GET", "/sw.js", nil); res.StatusCode != 200 || !strings.Contains(string(out), "showNotification") {
		t.Fatal("service worker not served")
	}
	if res, out := a.do("GET", "/llms.txt", nil); res.StatusCode != 200 || len(out) < 500 {
		t.Fatal("llms.txt not served")
	}
}
