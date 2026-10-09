// Package model holds the domain types shared by the store, the engine and the HTTP layer.
package model

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Site is one tracked website. Its PublicKey is embedded in the install snippet and is not a secret.
type Site struct {
	ID        int64      `json:"id"`
	Slug      string     `json:"slug"`
	Name      string     `json:"name"`
	PublicKey string     `json:"publicKey"`
	Config    SiteConfig `json:"config"`
	CreatedAt int64      `json:"createdAt"`
}

// SiteConfig is what the browser SDK downloads on start (except AllowedOrigins and CollectIP) and what ingest enforces.
type SiteConfig struct {
	AllowedOrigins []string `json:"allowedOrigins"`
	BlockedFields  []string `json:"blockedFields"`
	VisitorID      bool     `json:"visitorId"`
	PageViews      bool     `json:"pageViews"`
	PageTime       bool     `json:"pageTime"`
	RequireConsent bool     `json:"requireConsent"`
	CollectIP      bool     `json:"collectIp"`
	Geo            string   `json:"geo"` // location from the client IP: GeoOff, GeoCountry or GeoCity (needs AGG_GEOIP_URL)
}

const (
	GeoOff     = "off"
	GeoCountry = "country"
	GeoCity    = "city"
)

func DefaultSiteConfig() SiteConfig {
	return SiteConfig{AllowedOrigins: []string{}, BlockedFields: []string{}, VisitorID: true, PageViews: true, PageTime: true, Geo: GeoCountry}
}

// Normalize fills nil slices so the JSON never contains null lists, and defaults an unknown geo mode to country.
func (c *SiteConfig) Normalize() {
	clean := func(in []string, f func(string) string) []string {
		out := []string{}
		seen := map[string]bool{}
		for _, s := range in {
			s = f(strings.TrimSpace(s))
			if s != "" && !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
		return out
	}
	c.AllowedOrigins = clean(c.AllowedOrigins, func(s string) string { return strings.TrimRight(strings.ToLower(s), "/") })
	c.BlockedFields = clean(c.BlockedFields, strings.ToLower)
	if c.Geo != GeoOff && c.Geo != GeoCity {
		c.Geo = GeoCountry
	}
}

type Op string

const (
	OpCount         Op = "count"
	OpSum           Op = "sum"
	OpCountDistinct Op = "count_distinct"
	OpLastValue     Op = "last_value"
	OpLastTimestamp Op = "last_timestamp"
	OpAvg           Op = "avg"
	OpMin           Op = "min"
	OpMax           Op = "max"
	OpP50           Op = "p50"
	OpP95           Op = "p95"
	OpP99           Op = "p99"
)

// Quantile returns the quantile of a percentile operation (p95 → 0.95).
func (o Op) Quantile() (float64, bool) {
	switch o {
	case OpP50:
		return 0.50, true
	case OpP95:
		return 0.95, true
	case OpP99:
		return 0.99, true
	}
	return 0, false
}

// Stat reports whether the operation summarizes a numeric Value expression per window (avg, min, max, percentiles).
func (o Op) Stat() bool {
	_, q := o.Quantile()
	return q || o == OpAvg || o == OpMin || o == OpMax
}

// Windowed reports whether the operation has window variables (<name>_24h, <name>_prev_24h, …).
func (o Op) Windowed() bool { return o == OpCount || o == OpSum || o == OpCountDistinct || o.Stat() }

// HasTotal reports whether the operation also has an all-time variable (<name>_total).
func (o Op) HasTotal() bool { return o == OpCount || o == OpSum || o == OpAvg }

const (
	VisibilityPrivate        = "private"
	VisibilityPublic         = "public"
	VisibilityPublicBucketed = "public_bucketed"
)

// Grouping splits an aggregate by a dimension. Expr computes the dimension value from the event,
// Label optionally computes a human readable name (e.g. product name for a product id).
type Grouping struct {
	Dimension string `json:"dimension"`
	Expr      string `json:"expr"`
	Label     string `json:"label,omitempty"`
}

// AggregateDef is the user-editable definition of an aggregate.
type AggregateDef struct {
	Title       string    `json:"title"`
	Description string    `json:"description,omitempty"`
	Events      []string  `json:"events"`
	Where       string    `json:"where,omitempty"`
	Explode     string    `json:"explode,omitempty"`
	GroupBy     *Grouping `json:"groupBy,omitempty"`
	RankBy      *Grouping `json:"rankBy,omitempty"`
	Op          Op        `json:"op"`
	Value       string    `json:"value,omitempty"`
	Visibility  string    `json:"visibility"`
	Paused      bool      `json:"paused"`
	Pinned      bool      `json:"pinned"`
}

type Aggregate struct {
	ID          int64  `json:"id"`
	SiteID      int64  `json:"siteId"`
	Name        string `json:"name"`
	CreatedAt   int64  `json:"createdAt"`
	UpdatedAt   int64  `json:"updatedAt"`
	DataResetAt int64  `json:"dataResetAt"`
	AggregateDef
}

// Formula is a derived metric evaluated at read time from aggregate variables.
type Formula struct {
	ID         int64  `json:"id"`
	SiteID     int64  `json:"siteId"`
	Name       string `json:"name"`
	Title      string `json:"title"`
	Expr       string `json:"expr"`
	Unit       string `json:"unit"`
	Visibility string `json:"visibility"`
	Pinned     bool   `json:"pinned"`
	CreatedAt  int64  `json:"createdAt"`
	UpdatedAt  int64  `json:"updatedAt"`
}

const (
	FormatPrometheus = "prometheus"
	FormatJSON       = "json"

	PartitionsNone      = "none"
	PartitionsTopK      = "topk"
	PartitionsAllowlist = "allowlist"
)

type ExportScope struct {
	Aggregates []string `json:"aggregates"`
	Formulas   []string `json:"formulas"`
	Windows    []string `json:"windows"`
	Partitions string   `json:"partitions"`
	TopK       int      `json:"topK"`
	Allow      []string `json:"allow"`
}

// Export is a token protected, read-only export endpoint (Prometheus or JSON).
type Export struct {
	ID            string      `json:"id"`
	SiteID        int64       `json:"siteId"`
	Name          string      `json:"name"`
	Format        string      `json:"format"`
	Scope         ExportScope `json:"scope"`
	CacheSeconds  int         `json:"cacheSeconds"`
	TokenHash     string      `json:"-"`
	CreatedAt     int64       `json:"createdAt"`
	LastScrapedAt int64       `json:"lastScrapedAt"`
	ScrapeCount   int64       `json:"scrapeCount"`
}

// Event is one collected event. Times are unix milliseconds.
type Event struct {
	SiteID     int64          `json:"-"`
	RawID      int64          `json:"rawId,omitempty"`
	Name       string         `json:"name"`
	TS         int64          `json:"ts,omitempty"`
	ID         string         `json:"id,omitempty"`
	VisitorID  string         `json:"visitorId,omitempty"`
	Props      map[string]any `json:"props,omitempty"`
	Meta       map[string]any `json:"meta,omitempty"`
	ReceivedAt int64          `json:"receivedAt,omitempty"`
}

// Granularity of time buckets.
type Gran byte

const (
	Minute Gran = 'm'
	Hour   Gran = 'h'
	Day    Gran = 'd'
)

func (g Gran) Duration() time.Duration {
	switch g {
	case Minute:
		return time.Minute
	case Hour:
		return time.Hour
	default:
		return 24 * time.Hour
	}
}

// Floor returns the start (unix seconds) of the bucket containing t.
func (g Gran) Floor(t time.Time) int64 {
	s := t.Unix()
	d := int64(g.Duration() / time.Second)
	return s - ((s%d)+d)%d
}

// Window is a sliding time window. Windows up to 24h are computed from minute buckets
// (exact to the minute), 7d from hour buckets, 30d from day buckets (UTC days).
type Window struct {
	Name     string
	Duration time.Duration
	Gran     Gran
}

var Windows = []Window{
	{"5m", 5 * time.Minute, Minute},
	{"1h", time.Hour, Minute},
	{"6h", 6 * time.Hour, Minute},
	{"24h", 24 * time.Hour, Minute},
	{"7d", 7 * 24 * time.Hour, Hour},
	{"30d", 30 * 24 * time.Hour, Day},
}

func WindowByName(name string) (Window, bool) {
	for _, w := range Windows {
		if w.Name == name {
			return w, true
		}
	}
	return Window{}, false
}

// Range returns the inclusive bucket range [from, to] of the window ending at now.
// With prev=true it returns the same-length window immediately before it.
func (w Window) Range(now time.Time, prev bool) (from, to int64) {
	step := int64(w.Gran.Duration() / time.Second)
	n := int64(w.Duration / w.Gran.Duration())
	to = w.Gran.Floor(now)
	if prev {
		to -= n * step
	}
	return to - (n-1)*step, to
}

// Retention per granularity: enough for every window and its previous window, plus series.
var Retention = map[Gran]time.Duration{
	Minute: 49 * time.Hour,
	Hour:   15 * 24 * time.Hour,
	Day:    400 * 24 * time.Hour,
}

var (
	slugRe        = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
	reservedEndRe = regexp.MustCompile(`_(5m|1h|6h|24h|7d|30d|total)$`)
)

// ValidateName checks names of aggregates and formulas. They become variable prefixes
// (purchases → purchases_24h), so window suffixes are not allowed in the name itself.
func ValidateName(name string) error {
	if !slugRe.MatchString(name) {
		return fmt.Errorf("name must start with a letter and contain only a-z, 0-9 and _ (max 63 chars)")
	}
	if reservedEndRe.MatchString(name) || strings.Contains(name, "_prev_") {
		return fmt.Errorf("name must not end with a window suffix (_5m, _1h, _24h, …, _total) or contain _prev_")
	}
	return nil
}

func ValidateSlug(s string) error {
	if !slugRe.MatchString(s) {
		return fmt.Errorf("must start with a letter and contain only a-z, 0-9 and _")
	}
	return nil
}

var (
	camelRe    = regexp.MustCompile(`([a-z0-9])([A-Z])`)
	nonAlnumRe = regexp.MustCompile(`[^a-z0-9]+`)
)

// NormalizeEventName turns "Add To Cart" or "addToCart" into "add_to_cart". The SDK uses the same rules.
func NormalizeEventName(s string) string {
	s = camelRe.ReplaceAllString(strings.TrimSpace(s), "${1}_${2}")
	s = nonAlnumRe.ReplaceAllString(strings.ToLower(s), "_")
	s = strings.Trim(s, "_")
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

// AlertSchedule limits when an alert notifies. Empty Days means every day; FromHour == ToHour means all day.
type AlertSchedule struct {
	Days     []int  `json:"days"`
	FromHour int    `json:"fromHour"`
	ToHour   int    `json:"toHour"`
	Timezone string `json:"timezone"`
}

// AlertDef is the user-editable part of an alert.
type AlertDef struct {
	Title           string            `json:"title"`
	Condition       string            `json:"condition"`
	Dims            map[string]string `json:"dims"`
	ForMinutes      int               `json:"forMinutes"`
	CooldownMinutes int               `json:"cooldownMinutes"`
	NotifyResolved  bool              `json:"notifyResolved"`
	Push            bool              `json:"push"`
	Webhook         string            `json:"webhook"`
	Schedule        AlertSchedule     `json:"schedule"`
	Enabled         bool              `json:"enabled"`
}

const (
	AlertOK      = "ok"
	AlertPending = "pending"
	AlertFiring  = "firing"
)

type Alert struct {
	ID           int64  `json:"id"`
	SiteID       int64  `json:"siteId"`
	Name         string `json:"name"`
	State        string `json:"state"`
	StateSince   int64  `json:"stateSince"`
	LastEval     int64  `json:"lastEval"`
	LastNotified int64  `json:"lastNotified"`
	LastValue    string `json:"lastValue"`
	LastError    string `json:"lastError"`
	CreatedAt    int64  `json:"createdAt"`
	UpdatedAt    int64  `json:"updatedAt"`
	AlertDef
}

type AlertEvent struct {
	ID      int64  `json:"id"`
	AlertID int64  `json:"alertId"`
	TS      int64  `json:"ts"`
	State   string `json:"state"`
	Message string `json:"message"`
}

type Notification struct {
	ID      int64  `json:"id"`
	SiteID  int64  `json:"siteId"`
	AlertID int64  `json:"alertId"`
	TS      int64  `json:"ts"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	URL     string `json:"url"`
	Read    bool   `json:"read"`
}

type PushSubscription struct {
	Endpoint  string `json:"endpoint"`
	P256dh    string `json:"p256dh"`
	Auth      string `json:"auth"`
	UserAgent string `json:"userAgent"`
	CreatedAt int64  `json:"createdAt"`
}

// APIToken is a named, revocable token for the admin API and the MCP server.
type APIToken struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Prefix     string `json:"prefix"`
	TokenHash  string `json:"-"`
	CreatedAt  int64  `json:"createdAt"`
	LastUsedAt int64  `json:"lastUsedAt"`
}
