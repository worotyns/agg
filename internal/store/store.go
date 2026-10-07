// Package store persists configuration, aggregate state and raw events.
//
// Storage is an interface so other backends (Postgres, ClickHouse) can be added later;
// SQLite in WAL mode is the first and default implementation.
package store

import (
	"context"
	"errors"

	"github.com/worotyns/agg/internal/model"
)

var ErrNotFound = errors.New("not found")

// Counter is a count and a sum, the state of COUNT and SUM aggregates.
type Counter struct {
	Count int64   `json:"count"`
	Sum   float64 `json:"sum"`
}

type BucketKey struct {
	Agg    int64
	Gran   model.Gran
	Bucket int64
	Part   string
	Member string
}

type DistinctKey struct {
	Agg    int64
	Gran   model.Gran
	Bucket int64
	Part   string
	Hash   int64
}

type PartKey struct {
	Agg    int64
	Part   string
	Member string
}

type LastValue struct {
	TS    int64  `json:"ts"`
	Value string `json:"value"`
}

type LabelKey struct {
	Agg  int64
	Kind byte // 'p' for group (partition) values, 'm' for rank (member) values
	Key  string
}

type MatchedKey struct {
	Agg  int64
	Hour int64
}

type DedupeKey struct {
	Site int64
	ID   string
}

// Batch is a set of state changes applied atomically by ApplyBatch.
// EventQuery selects raw events, newest first. Empty fields do not filter.
type EventQuery struct {
	Limit     int
	Name      string
	VisitorID string
	BeforeID  int64 // only events with a raw id below this one (paging to older events)
}

type Batch struct {
	Buckets  map[BucketKey]*Counter
	Distinct map[DistinctKey]struct{}
	Last     map[PartKey]LastValue // Member is always ""
	Totals   map[PartKey]*Counter
	Labels   map[LabelKey]string
	Matched  map[MatchedKey]int64
	Dedupe   map[DedupeKey]int64 // expires at (unix s)
	Raw      []model.Event
}

func NewBatch() *Batch {
	return &Batch{
		Buckets:  map[BucketKey]*Counter{},
		Distinct: map[DistinctKey]struct{}{},
		Last:     map[PartKey]LastValue{},
		Totals:   map[PartKey]*Counter{},
		Labels:   map[LabelKey]string{},
		Matched:  map[MatchedKey]int64{},
		Dedupe:   map[DedupeKey]int64{},
	}
}

func (b *Batch) Empty() bool {
	return len(b.Buckets) == 0 && len(b.Distinct) == 0 && len(b.Last) == 0 && len(b.Totals) == 0 &&
		len(b.Labels) == 0 && len(b.Matched) == 0 && len(b.Dedupe) == 0 && len(b.Raw) == 0
}

// Level selects which keys a top-N query ranks.
type Level int

const (
	LevelPart   Level = iota // top partitions (group values) of the whole site
	LevelMember              // top members (rank values) inside one partition, or across all with Part ""
)

type TopRow struct {
	Key   string  `json:"key"`
	Label string  `json:"label,omitempty"`
	Count int64   `json:"count"`
	Sum   float64 `json:"sum"`
	TS    int64   `json:"ts,omitempty"`
}

type Point struct {
	Bucket int64   `json:"t"`
	Count  int64   `json:"count"`
	Sum    float64 `json:"sum"`
}

type EventNameCount struct {
	Name  string `json:"name"`
	Count int64  `json:"count"`
}

// Storage is everything the engine, the query layer and the HTTP API need from persistence.
type Storage interface {
	Close() error

	// settings
	GetSetting(ctx context.Context, key string) (string, error)
	SetSetting(ctx context.Context, key, value string) error

	// sites
	ListSites(ctx context.Context) ([]model.Site, error)
	GetSite(ctx context.Context, id int64) (model.Site, error)
	CreateSite(ctx context.Context, s *model.Site) error
	UpdateSite(ctx context.Context, s *model.Site) error
	DeleteSite(ctx context.Context, id int64) error

	// aggregates
	ListAggregates(ctx context.Context, siteID int64) ([]model.Aggregate, error)
	GetAggregate(ctx context.Context, id int64) (model.Aggregate, error)
	CreateAggregate(ctx context.Context, a *model.Aggregate) error
	UpdateAggregate(ctx context.Context, a *model.Aggregate) error
	DeleteAggregate(ctx context.Context, id int64) error
	ClearAggregateData(ctx context.Context, id int64, resetAt int64) error

	// formulas
	ListFormulas(ctx context.Context, siteID int64) ([]model.Formula, error)
	GetFormula(ctx context.Context, id int64) (model.Formula, error)
	CreateFormula(ctx context.Context, f *model.Formula) error
	UpdateFormula(ctx context.Context, f *model.Formula) error
	DeleteFormula(ctx context.Context, id int64) error

	// exports
	ListExports(ctx context.Context, siteID int64) ([]model.Export, error)
	GetExport(ctx context.Context, id string) (model.Export, error)
	CreateExport(ctx context.Context, e *model.Export) error
	UpdateExport(ctx context.Context, e *model.Export) error
	DeleteExport(ctx context.Context, id string) error
	RecordScrape(ctx context.Context, id string, at int64) error

	// aggregate state
	ApplyBatch(ctx context.Context, b *Batch) error
	SumBuckets(ctx context.Context, agg int64, g model.Gran, from, to int64, part, member string) (Counter, error)
	CountDistinct(ctx context.Context, agg int64, g model.Gran, from, to int64, part string) (int64, error)
	Last(ctx context.Context, agg int64, part string) (LastValue, bool, error)
	Total(ctx context.Context, agg int64, part, member string) (Counter, error)
	TopBuckets(ctx context.Context, agg int64, g model.Gran, from, to int64, level Level, part string, bySum bool, limit int) ([]TopRow, error)
	TopDistinct(ctx context.Context, agg int64, g model.Gran, from, to int64, limit int) ([]TopRow, error)
	TopLast(ctx context.Context, agg int64, limit int) ([]TopRow, error)
	TopTotals(ctx context.Context, agg int64, limit int) ([]TopRow, error)
	SeriesBuckets(ctx context.Context, agg int64, g model.Gran, from, to int64, part, member string) ([]Point, error)
	SeriesDistinct(ctx context.Context, agg int64, g model.Gran, from, to int64, part string) ([]Point, error)
	Labels(ctx context.Context, agg int64, kind byte, keys []string) (map[string]string, error)
	MatchedSince(ctx context.Context, siteID int64, fromHour int64) (map[int64]int64, error)
	KeyCount(ctx context.Context, agg int64) (int, error)
	KnownKeys(ctx context.Context, agg int64) ([]string, error)

	// raw events
	RecentEvents(ctx context.Context, siteID int64, q EventQuery) ([]model.Event, error)
	EventNameCounts(ctx context.Context, siteID int64, since int64) ([]EventNameCount, error)
	ScanRaw(ctx context.Context, siteID int64, upTo int64, fn func(model.Event) error) error
	LoadDedupe(ctx context.Context, now int64) (map[DedupeKey]int64, error)

	// alerts and notifications
	ListAlerts(ctx context.Context, siteID int64) ([]model.Alert, error)
	GetAlert(ctx context.Context, id int64) (model.Alert, error)
	CreateAlert(ctx context.Context, a *model.Alert) error
	UpdateAlert(ctx context.Context, a *model.Alert) error
	SaveAlertState(ctx context.Context, a *model.Alert) error
	DeleteAlert(ctx context.Context, id int64) error
	AddAlertEvent(ctx context.Context, e model.AlertEvent) error
	ListAlertEvents(ctx context.Context, alertID int64, limit int) ([]model.AlertEvent, error)
	AddNotification(ctx context.Context, n *model.Notification) error
	ListNotifications(ctx context.Context, limit int) ([]model.Notification, int, error)
	MarkNotificationsRead(ctx context.Context) error
	SavePushSubscription(ctx context.Context, p model.PushSubscription) error
	ListPushSubscriptions(ctx context.Context) ([]model.PushSubscription, error)
	DeletePushSubscription(ctx context.Context, endpoint string) error

	// API tokens
	ListAPITokens(ctx context.Context) ([]model.APIToken, error)
	CreateAPIToken(ctx context.Context, t *model.APIToken) error
	DeleteAPIToken(ctx context.Context, id int64) error
	APITokenByHash(ctx context.Context, hash string) (model.APIToken, error)
	TouchAPIToken(ctx context.Context, id int64, at int64) error

	// maintenance
	Cleanup(ctx context.Context, now int64, rawRetentionDays int) error
	DBSize(ctx context.Context) (int64, error)
}
