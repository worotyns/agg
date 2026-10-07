// Package agg embeds the documentation served by the binary (/llms.txt and MCP resources).
package agg

import _ "embed"

//go:embed site/llms.txt
var LLMsTxt string

//go:embed docs/aggregates.md
var DocAggregates string

//go:embed docs/sdk.md
var DocSDK string

//go:embed docs/api.md
var DocAPI string

//go:embed docs/prometheus-grafana.md
var DocPrometheus string

//go:embed docs/alerts.md
var DocAlerts string

//go:embed docs/mcp.md
var DocMCP string
