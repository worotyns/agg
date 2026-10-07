package server

// MCP server (Model Context Protocol, Streamable HTTP transport) on POST /mcp.
//
// Tools wrap the admin API: each call is dispatched in-process to the same handlers the UI uses,
// with the caller's bearer token, so validation and permissions are identical.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/worotyns/agg"
)

var mcpVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (s *Server) handleMCPGet(w http.ResponseWriter, r *http.Request) {
	// No server-initiated stream and no sessions: everything is request/response.
	w.Header().Set("Allow", "POST")
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	t := bearer(r)
	if t == "" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="agg mcp"`)
		writeErr(w, http.StatusUnauthorized, "create an API token in agg (Settings → API & MCP) and send it as Authorization: Bearer <token>")
		return
	}
	if !s.checkToken(w, r, func() bool { return s.validToken(r.Context(), t) }) {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeErr(w, 413, "request too large")
		return
	}
	body = bytes.TrimSpace(body)
	if len(body) > 0 && body[0] == '[' {
		var reqs []rpcRequest
		if err := json.Unmarshal(body, &reqs); err != nil {
			writeJSON(w, 200, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}})
			return
		}
		out := []rpcResponse{}
		for _, q := range reqs {
			if res, ok := s.mcpDispatch(r, q); ok {
				out = append(out, res)
			}
		}
		if len(out) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeJSON(w, 200, out)
		return
	}
	var q rpcRequest
	if err := json.Unmarshal(body, &q); err != nil {
		writeJSON(w, 200, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}})
		return
	}
	res, ok := s.mcpDispatch(r, q)
	if !ok {
		w.WriteHeader(http.StatusAccepted) // notification
		return
	}
	writeJSON(w, 200, res)
}

func (s *Server) mcpDispatch(r *http.Request, q rpcRequest) (rpcResponse, bool) {
	if len(q.ID) == 0 || string(q.ID) == "null" {
		return rpcResponse{}, false // notifications (e.g. notifications/initialized) get no response
	}
	res := rpcResponse{JSONRPC: "2.0", ID: q.ID}
	fail := func(code int, msg string) (rpcResponse, bool) {
		res.Error = &rpcError{code, msg}
		return res, true
	}
	switch q.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(q.Params, &p)
		v := mcpVersions[0]
		for _, x := range mcpVersions {
			if x == p.ProtocolVersion {
				v = x
			}
		}
		res.Result = map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{}, "resources": map[string]any{}, "prompts": map[string]any{}},
			"serverInfo":      map[string]any{"name": "agg", "title": "agg", "version": s.cfg.Version},
			"instructions":    mcpInstructions,
		}
	case "ping":
		res.Result = map[string]any{}
	case "tools/list":
		list := make([]map[string]any, 0, len(mcpTools))
		for _, t := range mcpTools {
			list = append(list, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.Schema})
		}
		res.Result = map[string]any{"tools": list}
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(q.Params, &p); err != nil {
			return fail(-32602, "invalid params")
		}
		for _, t := range mcpTools {
			if t.Name == p.Name {
				if p.Arguments == nil {
					p.Arguments = map[string]any{}
				}
				text, isErr := t.Call(s, r, p.Arguments)
				res.Result = map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isErr}
				return res, true
			}
		}
		return fail(-32602, "unknown tool "+p.Name)
	case "resources/list":
		list := []map[string]any{}
		for _, d := range mcpDocs {
			list = append(list, map[string]any{"uri": d.URI, "name": d.Name, "description": d.Description, "mimeType": "text/markdown"})
		}
		res.Result = map[string]any{"resources": list}
	case "resources/read":
		var p struct {
			URI string `json:"uri"`
		}
		json.Unmarshal(q.Params, &p)
		for _, d := range mcpDocs {
			if d.URI == p.URI {
				res.Result = map[string]any{"contents": []map[string]any{{"uri": d.URI, "mimeType": "text/markdown", "text": *d.Text}}}
				return res, true
			}
		}
		return fail(-32002, "resource not found")
	case "prompts/list":
		res.Result = map[string]any{"prompts": []map[string]any{{
			"name":        "plan_setup",
			"title":       "Plan an agg setup",
			"description": "Plan tracking, aggregates, formulas and alerts for a website or product, then apply it.",
			"arguments": []map[string]any{
				{"name": "goal", "description": "What you want to know, e.g. 'which features paying users use' or 'bestsellers per category'", "required": true},
				{"name": "site", "description": "Site name or id, if it already exists", "required": false},
			},
		}}}
	case "prompts/get":
		var p struct {
			Name      string            `json:"name"`
			Arguments map[string]string `json:"arguments"`
		}
		json.Unmarshal(q.Params, &p)
		if p.Name != "plan_setup" {
			return fail(-32602, "unknown prompt")
		}
		text := fmt.Sprintf(planPrompt, p.Arguments["goal"], p.Arguments["site"])
		res.Result = map[string]any{"description": "Plan an agg setup", "messages": []map[string]any{
			{"role": "user", "content": map[string]any{"type": "text", "text": text}}}}
	default:
		return fail(-32601, "method not found: "+q.Method)
	}
	return res, true
}

const mcpInstructions = `agg computes real-time aggregates (count, sum, distinct count, last value/time; grouped and ranked; windows 5m..30d + previous period) from website and product events, with alerts, a JSON values API and Prometheus exports.

Typical flow:
1. list_sites, or create_site with a preset (website, shop, saas, publisher). list_presets shows what each creates and what the site must send.
2. get_install_snippet: the <script> tag for the site, plus agg.track() examples for custom events.
3. list_event_names / recent_events: see which events and properties actually arrive before defining aggregates.
4. test_aggregate (dry run on real recent events) before create_aggregate. Expressions use expr syntax: props.<field> (what the developer sent), meta.<field> (path, referrer, language, browser, os, device, bot, ip), item.<field> when explode is set, visitor, event.
5. Variables are <aggregate>_<window> (5m 1h 6h 24h 7d 30d), <aggregate>_prev_<window>, <aggregate>_total, or the name alone for last_value/last_timestamp. Use them in formulas and alert conditions; check_alert evaluates a condition now.
6. get_values, get_top, get_series, get_insights read the numbers.
Ask before deleting anything. Read the agg://docs/* resources for details.`

const planPrompt = `Help me plan an agg setup.

Goal: %s
Existing site (if any): %s

Steps:
1. Ask what kind of project it is (website, online shop, SaaS/app, blog) and what decisions the numbers should support, if not clear.
2. Call list_presets and list_sites. If the site exists, call list_event_names and recent_events to see what it already sends.
3. Propose a plan as a short table: events to send (exact agg.track() calls with their properties), aggregates (name, operation, group by, windows used), formulas, alerts (condition, schedule, channels). Prefer a preset plus a few additions over many custom aggregates. Keep it small.
4. After I confirm: create the site or apply the preset, create the remaining aggregates (test_aggregate first when events exist), formulas and alerts, then give me the install snippet and the code to add.`

type mcpDoc struct {
	URI, Name, Description string
	Text                   *string
}

var mcpDocs = []mcpDoc{
	{"agg://docs/overview", "Overview (llms.txt)", "What agg does, setup, concepts, MCP", &agg.LLMsTxt},
	{"agg://docs/aggregates", "Aggregates", "Events, where, explode, operations, group by, windows, variables, formulas", &agg.DocAggregates},
	{"agg://docs/sdk", "Browser SDK", "Snippet, agg.track(), retries, metadata, privacy, consent", &agg.DocSDK},
	{"agg://docs/alerts", "Alerts", "Conditions, states, schedule, Web Push, webhooks", &agg.DocAlerts},
	{"agg://docs/api", "HTTP API", "Ingest, values API, admin API", &agg.DocAPI},
	{"agg://docs/prometheus", "Prometheus and Grafana", "Export endpoints, metrics, PromQL", &agg.DocPrometheus},
	{"agg://docs/mcp", "MCP", "Connecting AI assistants, tools", &agg.DocMCP},
}

func (s *Server) handleLLMsTxt(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Write([]byte(agg.LLMsTxt))
}

// ---- in-process API calls

type recorder struct {
	h      http.Header
	status int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header         { return r.h }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }
func (r *recorder) WriteHeader(s int)           { r.status = s }

// api calls an admin API endpoint with the MCP caller's credentials.
func (s *Server) api(r *http.Request, method, path string, body any) (int, []byte) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(r.Context(), method, path, rd)
	req.Host = r.Host
	req.RemoteAddr = r.RemoteAddr
	req.Header.Set("Authorization", r.Header.Get("Authorization"))
	if fp := r.Header.Get("X-Forwarded-Proto"); fp != "" {
		req.Header.Set("X-Forwarded-Proto", fp)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := &recorder{h: http.Header{}, status: 200}
	s.mux.ServeHTTP(rec, req)
	return rec.status, rec.body.Bytes()
}

// ---- tools

type mcpTool struct {
	Name        string
	Description string
	Schema      map[string]any
	Call        func(s *Server, r *http.Request, args map[string]any) (string, bool)
}

func obj(props map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func num(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }
func boo(desc string) map[string]any { return map[string]any{"type": "boolean", "description": desc} }
func strs(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}
func enum(desc string, v ...string) map[string]any {
	return map[string]any{"type": "string", "enum": v, "description": desc}
}

var siteID = num("Site id (from list_sites)")
var aggID = num("Aggregate id (from list_aggregates)")

var groupingSchema = map[string]any{"type": "object", "description": "Split by a dimension", "properties": map[string]any{
	"dimension": str("Dimension name used in the API and Prometheus, lowercase, e.g. product"),
	"expr":      str("Expression for the value, e.g. item.id, props.plan or meta.path"),
	"label":     str("Optional expression for a readable label, e.g. item.name"),
}, "required": []string{"dimension", "expr"}}

var aggregateProps = map[string]any{
	"name":       str("Variable prefix: lowercase letters, digits, _; must not end with a window suffix"),
	"title":      str("Readable title"),
	"events":     strs("Event names it listens to, e.g. [\"purchase\"]"),
	"where":      str("Optional filter expression, e.g. props.value > 100"),
	"explode":    str("Optional array to iterate, e.g. props.items; each element is `item`"),
	"op":         enum("Operation", "count", "sum", "count_distinct", "last_value", "last_timestamp"),
	"value":      str("Value expression: required for sum and last_value; for count_distinct defaults to the visitor id"),
	"groupBy":    groupingSchema,
	"rankBy":     groupingSchema,
	"visibility": enum("Values API visibility", "private", "public", "public_bucketed"),
	"pinned":     boo("Show on the Insights page (default true)"),
	"paused":     boo("Ignore new events"),
}

func with(base map[string]any, extra map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func argInt(args map[string]any, k string) string {
	switch v := args[k].(type) {
	case float64:
		return fmt.Sprintf("%d", int64(v))
	case string:
		return url.PathEscape(v)
	}
	return ""
}

func argStr(args map[string]any, k string) string {
	v, _ := args[k].(string)
	return v
}

// without returns args minus the given keys (path parameters).
func without(args map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	for k, v := range args {
		out[k] = v
	}
	for _, k := range keys {
		delete(out, k)
	}
	return out
}

func result(status int, body []byte) (string, bool) {
	if status >= 400 {
		var e apiError
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			msg := "Error: " + e.Error
			if len(e.Warnings) > 0 {
				msg += "\nWarnings: " + strings.Join(e.Warnings, "; ")
			}
			return msg, true
		}
		return fmt.Sprintf("Error: HTTP %d %s", status, body), true
	}
	var v any
	if json.Unmarshal(body, &v) != nil {
		return string(body), false
	}
	b, _ := json.MarshalIndent(v, "", " ")
	if len(b) > 60000 {
		b = append(b[:60000], []byte("\n… (truncated)")...)
	}
	return string(b), false
}

func call(method string, path func(a map[string]any) string, pathKeys ...string) func(*Server, *http.Request, map[string]any) (string, bool) {
	return func(s *Server, r *http.Request, args map[string]any) (string, bool) {
		var body any
		if method != http.MethodGet && method != http.MethodDelete {
			body = without(args, pathKeys...)
		}
		return result(s.api(r, method, path(args), body))
	}
}

func site(a map[string]any) string { return "/api/sites/" + argInt(a, "siteId") }

func qs(a map[string]any, keys ...string) string {
	v := url.Values{}
	for _, k := range keys {
		switch x := a[k].(type) {
		case string:
			v.Set(k, x)
		case float64:
			v.Set(k, fmt.Sprintf("%d", int64(x)))
		case []any:
			parts := []string{}
			for _, p := range x {
				parts = append(parts, fmt.Sprint(p))
			}
			v.Set(k, strings.Join(parts, ","))
		}
	}
	if d, ok := a["dimensions"].(map[string]any); ok {
		for k, x := range d {
			v.Set(k, fmt.Sprint(x))
		}
	}
	if len(v) == 0 {
		return ""
	}
	return "?" + v.Encode()
}

var mcpTools = []mcpTool{
	{"list_sites", "List sites with their id, public key and tracking settings.", obj(map[string]any{}),
		call("GET", func(a map[string]any) string { return "/api/sites" })},
	{"create_site", "Create a site. A preset adds ready aggregates, formulas and alerts (default: website).",
		obj(map[string]any{"name": str("Site name"), "slug": str("Optional id used as the Prometheus site label"),
			"preset": enum("Preset", "website", "shop", "saas", "publisher", "none"), "timezone": str("IANA timezone for alert schedules, e.g. Europe/Warsaw")}, "name"),
		call("POST", func(a map[string]any) string { return "/api/sites" })},
	{"get_install_snippet", "Get the script tag to install on the site and examples of custom events.",
		obj(map[string]any{"siteId": siteID}, "siteId"), installSnippet},
	{"update_tracking", "Change tracking settings. Only the given fields change.",
		obj(map[string]any{"siteId": siteID, "name": str("Site name"),
			"blockedFields": strs("Extra property names to strip"), "visitorId": boo("Random visitor id in localStorage (needed for distinct visitors)"),
			"collectIp": boo("Add the client IP to event meta (personal data; off by default)"),
			"pageViews": boo("Send page_view automatically"), "requireConsent": boo("Wait for agg.consent({analytics:true})"),
			"allowedOrigins": strs("Origins allowed to send events, e.g. https://shop.example.com; empty = any")}, "siteId"), updateTracking},
	{"list_presets", "List presets: what each creates and what the site has to send.", obj(map[string]any{}),
		call("GET", func(a map[string]any) string { return "/api/presets" })},
	{"apply_preset", "Add a preset's missing aggregates, formulas, alerts and allowed events to a site. Existing names are kept.",
		obj(map[string]any{"siteId": siteID, "preset": enum("Preset", "website", "shop", "saas", "publisher"), "timezone": str("IANA timezone for alert schedules")}, "siteId", "preset"),
		call("POST", func(a map[string]any) string { return site(a) + "/presets/" + argStr(a, "preset") }, "siteId", "preset")},
	{"list_event_names", "Event names received in the last 24 hours with counts.", obj(map[string]any{"siteId": siteID}, "siteId"),
		call("GET", func(a map[string]any) string { return site(a) + "/event-names" })},
	{"recent_events", "Recent raw events (after the privacy filter) with their properties.",
		obj(map[string]any{"siteId": siteID, "name": str("Only this event name"), "limit": num("Max events, default 20")}, "siteId"),
		func(s *Server, r *http.Request, a map[string]any) (string, bool) {
			if _, ok := a["limit"]; !ok {
				a["limit"] = float64(20)
			}
			return result(s.api(r, "GET", site(a)+"/events"+qs(a, "name", "limit"), nil))
		}},
	{"list_aggregates", "List aggregates with their definition, matched events (24h), current 24h value and variables.",
		obj(map[string]any{"siteId": siteID}, "siteId"), call("GET", func(a map[string]any) string { return site(a) + "/aggregates" })},
	{"test_aggregate", "Dry-run an aggregate definition on recent stored events (or on given events) without saving.",
		obj(map[string]any{"siteId": siteID, "def": obj(aggregateProps), "events": map[string]any{"type": "array", "description": "Optional events [{name, props, visitorId}]", "items": map[string]any{"type": "object"}}}, "siteId", "def"),
		testAggregate},
	{"create_aggregate", "Create an aggregate.", obj(with(aggregateProps, map[string]any{"siteId": siteID}), "siteId", "name", "events", "op"),
		call("POST", func(a map[string]any) string { return site(a) + "/aggregates" }, "siteId")},
	{"update_aggregate", "Update an aggregate. Only the given fields change. Use rebuild_aggregate afterwards to recompute stored events.",
		obj(with(aggregateProps, map[string]any{"siteId": siteID, "aggregateId": aggID}), "siteId", "aggregateId"), updateAggregate},
	{"rebuild_aggregate", "Clear an aggregate and recompute it from the stored raw events (default 7 days).",
		obj(map[string]any{"siteId": siteID, "aggregateId": aggID}, "siteId", "aggregateId"),
		call("POST", func(a map[string]any) string { return site(a) + "/aggregates/" + argInt(a, "aggregateId") + "/rebuild" }, "siteId", "aggregateId")},
	{"delete_aggregate", "Delete an aggregate and its data. Ask the user first.",
		obj(map[string]any{"siteId": siteID, "aggregateId": aggID}, "siteId", "aggregateId"),
		call("DELETE", func(a map[string]any) string { return site(a) + "/aggregates/" + argInt(a, "aggregateId") })},
	{"list_variables", "All variables (aggregate windows, totals, formulas) usable in formulas, alerts and the values API.",
		obj(map[string]any{"siteId": siteID}, "siteId"), call("GET", func(a map[string]any) string { return site(a) + "/variables" })},
	{"get_values", "Current values of variables, optionally for one dimension value (e.g. {\"product\": \"73\"}).",
		obj(map[string]any{"siteId": siteID, "v": strs("Variable names, e.g. [\"purchases_24h\", \"aov\"]"),
			"dimensions": map[string]any{"type": "object", "description": "Dimension values", "additionalProperties": map[string]any{"type": "string"}}}, "siteId", "v"),
		func(s *Server, r *http.Request, a map[string]any) (string, bool) {
			return result(s.api(r, "GET", site(a)+"/values"+qs(a, "v"), nil))
		}},
	{"get_top", "Top values of a grouped aggregate in a window.",
		obj(map[string]any{"siteId": siteID, "aggregateId": aggID, "window": enum("Window", "5m", "1h", "6h", "24h", "7d", "30d"),
			"by": str("Dimension to rank (default: rank dimension, else group dimension)"), "limit": num("Default 10"),
			"dimensions": map[string]any{"type": "object", "description": "e.g. {\"category\": \"shoes\"} to rank inside a group"}}, "siteId", "aggregateId"),
		func(s *Server, r *http.Request, a map[string]any) (string, bool) {
			return result(s.api(r, "GET", site(a)+"/aggregates/"+argInt(a, "aggregateId")+"/top"+qs(a, "window", "by", "limit"), nil))
		}},
	{"get_series", "History of a windowed aggregate: 1h by minute, 24h/7d by hour, 30d/90d by day.",
		obj(map[string]any{"siteId": siteID, "aggregateId": aggID, "range": enum("Range", "1h", "24h", "7d", "30d", "90d")}, "siteId", "aggregateId"),
		func(s *Server, r *http.Request, a map[string]any) (string, bool) {
			return result(s.api(r, "GET", site(a)+"/aggregates/"+argInt(a, "aggregateId")+"/series"+qs(a, "range"), nil))
		}},
	{"get_insights", "The Insights page: pinned values with change vs the previous period, top lists and formulas.",
		obj(map[string]any{"siteId": siteID, "window": enum("Window", "1h", "24h", "7d", "30d")}, "siteId"),
		func(s *Server, r *http.Request, a map[string]any) (string, bool) {
			return result(s.api(r, "GET", site(a)+"/insights"+qs(a, "window"), nil))
		}},
	{"list_formulas", "List formulas with their current value.", obj(map[string]any{"siteId": siteID}, "siteId"),
		call("GET", func(a map[string]any) string { return site(a) + "/formulas" })},
	{"create_formula", "Create a formula from variables, e.g. revenue_24h / purchases_24h.",
		obj(map[string]any{"siteId": siteID, "name": str("Name"), "title": str("Title"), "expr": str("Expression"),
			"unit": str("number, percent, duration or currency:EUR"), "visibility": enum("Visibility", "private", "public"), "pinned": boo("Show on Insights")}, "siteId", "name", "expr"),
		call("POST", func(a map[string]any) string { return site(a) + "/formulas" }, "siteId")},
	{"list_alerts", "List alerts with state (ok, pending, firing), last values and errors.", obj(map[string]any{"siteId": siteID}, "siteId"),
		call("GET", func(a map[string]any) string { return site(a) + "/alerts" })},
	{"check_alert", "Evaluate an alert condition now without saving, e.g. orders_1h < 1 or now - last_purchase > 7200.",
		obj(map[string]any{"siteId": siteID, "condition": str("Boolean expression over variables and `now` (unix seconds)"),
			"dims": map[string]any{"type": "object", "description": "Dimension values"}}, "siteId", "condition"),
		call("POST", func(a map[string]any) string { return site(a) + "/alerts/check" }, "siteId")},
	{"create_alert", "Create an alert. Notifies by browser push (devices subscribed in the UI) and/or a webhook (Slack/Discord/Mattermost compatible).",
		obj(map[string]any{"siteId": siteID, "name": str("Name (lowercase, _)"), "title": str("Title shown in notifications"),
			"condition":  str("Boolean expression, e.g. revenue_prev_24h > 0 && revenue_24h < 0.5 * revenue_prev_24h"),
			"forMinutes": num("Condition must hold this long before firing"), "cooldownMinutes": num("Minimum minutes between notifications"),
			"notifyResolved": boo("Notify when it resolves"), "push": boo("Browser push"), "webhook": str("Webhook URL"), "enabled": boo("Default true"),
			"dims": map[string]any{"type": "object", "description": "Dimension values"},
			"schedule": map[string]any{"type": "object", "description": "When to notify", "properties": map[string]any{
				"days":     map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "0=Sunday..6; empty = every day"},
				"fromHour": num("0-24"), "toHour": num("0-24; equal to fromHour = all day"), "timezone": str("IANA timezone")}}}, "siteId", "name", "condition"),
		func(s *Server, r *http.Request, a map[string]any) (string, bool) {
			if _, ok := a["enabled"]; !ok {
				a["enabled"] = true
			}
			return result(s.api(r, "POST", site(a)+"/alerts", without(a, "siteId")))
		}},
	{"delete_alert", "Delete an alert. Ask the user first.", obj(map[string]any{"siteId": siteID, "alertId": num("Alert id")}, "siteId", "alertId"),
		call("DELETE", func(a map[string]any) string { return site(a) + "/alerts/" + argInt(a, "alertId") })},
	{"create_export", "Create a token-protected Prometheus or JSON export endpoint. Returns the token once and a scrape config.",
		obj(map[string]any{"siteId": siteID, "name": str("Name"), "format": enum("Format", "prometheus", "json"),
			"scope": map[string]any{"type": "object", "properties": map[string]any{
				"aggregates": strs("Aggregate names"), "formulas": strs("Formula names"), "windows": strs("Windows, e.g. [\"1h\",\"24h\"]"),
				"partitions": enum("Per-dimension series", "none", "topk", "allowlist"), "topK": num("K for topk"), "allow": strs("Values for allowlist")}}}, "siteId", "name", "format", "scope"),
		createExport},
}

func installSnippet(s *Server, r *http.Request, a map[string]any) (string, bool) {
	status, body := s.api(r, "GET", site(a), nil)
	if status >= 400 {
		return result(status, body)
	}
	var st struct {
		PublicKey string `json:"publicKey"`
	}
	json.Unmarshal(body, &st)
	base := s.baseURL(r)
	return fmt.Sprintf(`Add before </head> on every page:

<script async src="%[1]s/agg.js" data-site="%[2]s"></script>

Custom events from your code (names are normalized to snake_case; the third argument is an optional dedupe id):

agg.track('sign_up', { plan: 'pro' })
agg.track('feature_used', { feature: 'export_pdf' })
agg.track('purchase', { order_id: 'A-1', value: 120, currency: 'EUR', items: [{ id: '73', name: 'Shoe', category: 'shoes', price: 60, quantity: 2 }] }, 'A-1')

Page views are sent automatically (unless disabled). Every event gets meta: path, referrer, language, browser, os, device.
From a backend, POST %[1]s/e with {"site": "%[2]s", "events": [{"name": "...", "props": {...}}]}.
Public values API: %[1]s/v1/values?site=%[2]s&v=<variable>,<variable>`, base, st.PublicKey), false
}

func updateTracking(s *Server, r *http.Request, a map[string]any) (string, bool) {
	status, body := s.api(r, "GET", site(a), nil)
	if status >= 400 {
		return result(status, body)
	}
	var cur struct {
		Name   string         `json:"name"`
		Config map[string]any `json:"config"`
	}
	json.Unmarshal(body, &cur)
	for k, v := range without(a, "siteId", "name") {
		cur.Config[k] = v
	}
	if n := argStr(a, "name"); n != "" {
		cur.Name = n
	}
	return result(s.api(r, "PUT", site(a), map[string]any{"name": cur.Name, "config": cur.Config}))
}

func updateAggregate(s *Server, r *http.Request, a map[string]any) (string, bool) {
	path := site(a) + "/aggregates/" + argInt(a, "aggregateId")
	status, body := s.api(r, "GET", path, nil)
	if status >= 400 {
		return result(status, body)
	}
	var cur map[string]any
	json.Unmarshal(body, &cur)
	for k, v := range without(a, "siteId", "aggregateId", "name") {
		cur[k] = v
	}
	return result(s.api(r, "PUT", path, cur))
}

// testAggregate returns a compact summary: how many events matched and what they contributed.
func testAggregate(s *Server, r *http.Request, a map[string]any) (string, bool) {
	status, body := s.api(r, "POST", site(a)+"/dry-run", without(a, "siteId"))
	if status >= 400 {
		return result(status, body)
	}
	var res struct {
		Results []struct {
			Event         map[string]any   `json:"event"`
			Matched       bool             `json:"matched"`
			Contributions []map[string]any `json:"contributions"`
			Error         string           `json:"error"`
		} `json:"results"`
		Warnings []string `json:"warnings"`
	}
	json.Unmarshal(body, &res)
	matched, contribs := 0, 0
	var b strings.Builder
	for _, x := range res.Results {
		if x.Matched {
			matched++
			contribs += len(x.Contributions)
		}
	}
	fmt.Fprintf(&b, "%d of %d events matched, %d contributions.\n", matched, len(res.Results), contribs)
	for _, w := range res.Warnings {
		fmt.Fprintf(&b, "Warning: %s\n", w)
	}
	shown := 0
	for _, x := range res.Results {
		if shown >= 10 {
			break
		}
		if !x.Matched && x.Error == "" {
			continue
		}
		shown++
		c, _ := json.Marshal(x.Contributions)
		p, _ := json.Marshal(x.Event["props"])
		if len(p) > 400 {
			p = append(p[:400], []byte("…")...)
		}
		fmt.Fprintf(&b, "- %v props=%s → %s %s\n", x.Event["name"], p, c, x.Error)
	}
	if len(res.Results) == 0 {
		b.WriteString("No stored events with these names yet. Pass `events` to test with examples.\n")
	}
	return b.String(), false
}

func createExport(s *Server, r *http.Request, a map[string]any) (string, bool) {
	status, body := s.api(r, "POST", site(a)+"/exports", without(a, "siteId"))
	if status >= 400 {
		return result(status, body)
	}
	var res struct {
		Export struct {
			ID     string `json:"id"`
			Format string `json:"format"`
		} `json:"export"`
		Token string `json:"token"`
	}
	json.Unmarshal(body, &res)
	base := s.baseURL(r)
	u, _ := url.Parse(base)
	return fmt.Sprintf(`Export created. The token is shown only now.
URL: %s/export/%s/metrics
Token: %s

prometheus.yml:
scrape_configs:
  - job_name: agg
    scrape_interval: 60s
    scheme: %s
    metrics_path: /export/%s/metrics
    authorization:
      type: Bearer
      credentials: %s
    static_configs:
      - targets: ['%s']`, base, res.Export.ID, res.Token, u.Scheme, res.Export.ID, res.Token, u.Host), false
}
