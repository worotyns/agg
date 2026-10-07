# MCP server: use agg from an AI assistant

agg includes a [Model Context Protocol](https://modelcontextprotocol.io) server at `/mcp` (Streamable HTTP,
JSON responses). An assistant can read your numbers and plan and apply configuration: sites, presets, tracking
settings, aggregates (dry-run on real events first), formulas, alerts and exports.

## Connect

1. In agg: **API & MCP → Create token** (e.g. "Claude on my laptop"). The token is shown once; revoke it there any
   time. API tokens work for the MCP server and the admin API, but cannot log in to the UI.
2. Add the server to your client.

Claude Code:

```sh
claude mcp add --transport http agg https://agg.example.com/mcp --header "Authorization: Bearer agg_api_…"
```

Clients with a JSON config (Claude Desktop, Cursor, VS Code…):

```json
{
  "mcpServers": {
    "agg": {
      "type": "http",
      "url": "https://agg.example.com/mcp",
      "headers": { "Authorization": "Bearer agg_api_…" }
    }
  }
}
```

Use HTTPS for anything but `localhost`: the token travels in a header.

## What to ask

- "Plan analytics for my SaaS: I want to know which features paying users use." (the `plan_setup` prompt walks
  through goal → preset → events to send → aggregates → alerts, and applies it after you confirm)
- "Set up my shop: bestsellers per category, revenue, and alert me when there are no orders for 3 hours. Tell me which events to send."
- "Which pages got the most views this week compared with last week?"
- "Why is the `bestsellers` aggregate empty?" (the assistant checks event names, recent events and dry-runs the
  definition)
- "Create a Prometheus export for purchases and revenue and give me the scrape config."

## Tools

| Tool | |
|---|---|
| `list_sites`, `create_site` (with preset), `get_install_snippet`, `update_tracking` | sites and installation |
| `list_presets`, `apply_preset` | ready setups: `website`, `shop`, `saas`, `publisher` |
| `list_event_names`, `recent_events` | what the site actually sends |
| `list_aggregates`, `test_aggregate`, `create_aggregate`, `update_aggregate`, `rebuild_aggregate`, `delete_aggregate` | aggregates |
| `list_variables`, `get_values`, `get_top`, `get_series`, `get_insights` | numbers |
| `list_formulas`, `create_formula` | formulas |
| `list_alerts`, `check_alert`, `create_alert`, `delete_alert` | alerts |
| `create_export` | Prometheus / JSON export with scrape config |

Resources `agg://docs/*` contain this documentation; prompt `plan_setup` takes a `goal` and an optional `site`.

Every tool call goes through the same admin API and validation as the UI. Deleting tools are described as "ask the
user first"; your client also asks you before calling tools unless you allow them.
