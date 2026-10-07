package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/worotyns/agg/internal/model"
	_ "modernc.org/sqlite"
)

// SQLite implements Storage. Writes go through a single connection (SQLite has one writer);
// reads use a separate pool, which WAL mode lets run concurrently with the writer.
type SQLite struct {
	w    *sql.DB
	r    *sql.DB
	path string
}

var _ Storage = (*SQLite)(nil)

func OpenSQLite(path string) (*SQLite, error) {
	q := url.Values{}
	for _, p := range []string{"journal_mode(WAL)", "busy_timeout(10000)", "synchronous(NORMAL)", "foreign_keys(ON)"} {
		q.Add("_pragma", p)
	}
	dsn := "file:" + path + "?" + q.Encode()
	w, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	if err := w.Ping(); err != nil {
		w.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	s := &SQLite{w: w, path: path}
	if err := s.migrate(); err != nil {
		w.Close()
		return nil, err
	}
	q.Add("_pragma", "query_only(1)")
	r, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(8)
	s.r = r
	return s, nil
}

func (s *SQLite) Close() error {
	s.r.Close()
	return s.w.Close()
}

var migrations = []string{
	`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
	CREATE TABLE sites (
		id INTEGER PRIMARY KEY, slug TEXT NOT NULL UNIQUE, name TEXT NOT NULL,
		public_key TEXT NOT NULL UNIQUE, config TEXT NOT NULL, created_at INTEGER NOT NULL);
	CREATE TABLE aggregates (
		id INTEGER PRIMARY KEY, site_id INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
		name TEXT NOT NULL, def TEXT NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
		data_reset_at INTEGER NOT NULL DEFAULT 0, UNIQUE(site_id, name));
	CREATE TABLE formulas (
		id INTEGER PRIMARY KEY, site_id INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
		name TEXT NOT NULL, def TEXT NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
		UNIQUE(site_id, name));
	CREATE TABLE exports (
		id TEXT PRIMARY KEY, site_id INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
		name TEXT NOT NULL, def TEXT NOT NULL, token_hash TEXT NOT NULL, created_at INTEGER NOT NULL,
		last_scraped_at INTEGER NOT NULL DEFAULT 0, scrape_count INTEGER NOT NULL DEFAULT 0);
	CREATE TABLE buckets (
		agg_id INTEGER NOT NULL, gran TEXT NOT NULL, part TEXT NOT NULL, member TEXT NOT NULL, bucket INTEGER NOT NULL,
		cnt INTEGER NOT NULL, sum REAL NOT NULL, PRIMARY KEY (agg_id, gran, part, member, bucket)) WITHOUT ROWID;
	CREATE INDEX buckets_time ON buckets (agg_id, gran, bucket);
	CREATE TABLE distinct_members (
		agg_id INTEGER NOT NULL, gran TEXT NOT NULL, part TEXT NOT NULL, bucket INTEGER NOT NULL, hash INTEGER NOT NULL,
		PRIMARY KEY (agg_id, gran, part, bucket, hash)) WITHOUT ROWID;
	CREATE INDEX distinct_time ON distinct_members (agg_id, gran, bucket);
	CREATE TABLE last_values (
		agg_id INTEGER NOT NULL, part TEXT NOT NULL, ts INTEGER NOT NULL, value TEXT NOT NULL,
		PRIMARY KEY (agg_id, part)) WITHOUT ROWID;
	CREATE TABLE totals (
		agg_id INTEGER NOT NULL, part TEXT NOT NULL, member TEXT NOT NULL, cnt INTEGER NOT NULL, sum REAL NOT NULL,
		PRIMARY KEY (agg_id, part, member)) WITHOUT ROWID;
	CREATE TABLE labels (
		agg_id INTEGER NOT NULL, kind TEXT NOT NULL, key TEXT NOT NULL, label TEXT NOT NULL,
		PRIMARY KEY (agg_id, kind, key)) WITHOUT ROWID;
	CREATE TABLE matched (
		agg_id INTEGER NOT NULL, hour INTEGER NOT NULL, cnt INTEGER NOT NULL, PRIMARY KEY (agg_id, hour)) WITHOUT ROWID;
	CREATE TABLE dedupe (
		site_id INTEGER NOT NULL, event_id TEXT NOT NULL, expires_at INTEGER NOT NULL,
		PRIMARY KEY (site_id, event_id)) WITHOUT ROWID;
	CREATE TABLE events_raw (
		id INTEGER PRIMARY KEY, site_id INTEGER NOT NULL, received_at INTEGER NOT NULL, ts INTEGER NOT NULL,
		name TEXT NOT NULL, event_id TEXT NOT NULL, visitor_id TEXT NOT NULL, props TEXT NOT NULL);
	CREATE INDEX events_raw_site ON events_raw (site_id, received_at);`,
	`CREATE TABLE alerts (
		id INTEGER PRIMARY KEY, site_id INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
		name TEXT NOT NULL, def TEXT NOT NULL, state TEXT NOT NULL DEFAULT 'ok', state_since INTEGER NOT NULL DEFAULT 0,
		last_eval INTEGER NOT NULL DEFAULT 0, last_notified INTEGER NOT NULL DEFAULT 0, last_value TEXT NOT NULL DEFAULT '',
		last_error TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, UNIQUE(site_id, name));
	CREATE TABLE alert_events (
		id INTEGER PRIMARY KEY, alert_id INTEGER NOT NULL, ts INTEGER NOT NULL, state TEXT NOT NULL, message TEXT NOT NULL);
	CREATE INDEX alert_events_alert ON alert_events (alert_id, id);
	CREATE TABLE notifications (
		id INTEGER PRIMARY KEY, site_id INTEGER NOT NULL, alert_id INTEGER NOT NULL, ts INTEGER NOT NULL,
		title TEXT NOT NULL, body TEXT NOT NULL, url TEXT NOT NULL, read INTEGER NOT NULL DEFAULT 0);
	CREATE TABLE push_subscriptions (
		endpoint TEXT PRIMARY KEY, p256dh TEXT NOT NULL, auth TEXT NOT NULL, user_agent TEXT NOT NULL, created_at INTEGER NOT NULL);
	CREATE TABLE api_tokens (
		id INTEGER PRIMARY KEY, name TEXT NOT NULL, prefix TEXT NOT NULL, token_hash TEXT NOT NULL UNIQUE,
		created_at INTEGER NOT NULL, last_used_at INTEGER NOT NULL DEFAULT 0);`,
	`ALTER TABLE events_raw ADD COLUMN meta TEXT NOT NULL DEFAULT '{}';`,
}

func (s *SQLite) migrate() error {
	var v int
	if err := s.w.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.w.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func now() int64 { return time.Now().UnixMilli() }

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// ---- settings

func (s *SQLite) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.r.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	return v, notFound(err)
}

func (s *SQLite) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.w.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// ---- sites

func scanSite(sc interface{ Scan(...any) error }) (model.Site, error) {
	var st model.Site
	var cfg string
	if err := sc.Scan(&st.ID, &st.Slug, &st.Name, &st.PublicKey, &cfg, &st.CreatedAt); err != nil {
		return st, err
	}
	st.Config = model.DefaultSiteConfig()
	if err := json.Unmarshal([]byte(cfg), &st.Config); err != nil {
		return st, err
	}
	st.Config.Normalize()
	return st, nil
}

const siteCols = `id, slug, name, public_key, config, created_at`

func (s *SQLite) ListSites(ctx context.Context) ([]model.Site, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT `+siteCols+` FROM sites ORDER BY name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Site{}
	for rows.Next() {
		st, err := scanSite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

func (s *SQLite) GetSite(ctx context.Context, id int64) (model.Site, error) {
	st, err := scanSite(s.r.QueryRowContext(ctx, `SELECT `+siteCols+` FROM sites WHERE id = ?`, id))
	return st, notFound(err)
}

func (s *SQLite) CreateSite(ctx context.Context, st *model.Site) error {
	cfg, _ := json.Marshal(st.Config)
	st.CreatedAt = now()
	res, err := s.w.ExecContext(ctx, `INSERT INTO sites (slug, name, public_key, config, created_at) VALUES (?, ?, ?, ?, ?)`,
		st.Slug, st.Name, st.PublicKey, string(cfg), st.CreatedAt)
	if err != nil {
		return uniqueErr(err, "a site with this id already exists")
	}
	st.ID, err = res.LastInsertId()
	return err
}

func (s *SQLite) UpdateSite(ctx context.Context, st *model.Site) error {
	cfg, _ := json.Marshal(st.Config)
	res, err := s.w.ExecContext(ctx, `UPDATE sites SET name = ?, config = ? WHERE id = ?`, st.Name, string(cfg), st.ID)
	return affected(res, err)
}

func (s *SQLite) DeleteSite(ctx context.Context, id int64) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := clearData(ctx, tx, `IN (SELECT id FROM aggregates WHERE site_id = ?)`, id); err != nil {
		return err
	}
	for _, q := range []string{
		`DELETE FROM events_raw WHERE site_id = ?`, `DELETE FROM dedupe WHERE site_id = ?`,
		`DELETE FROM aggregates WHERE site_id = ?`, `DELETE FROM formulas WHERE site_id = ?`,
		`DELETE FROM exports WHERE site_id = ?`, `DELETE FROM alert_events WHERE alert_id IN (SELECT id FROM alerts WHERE site_id = ?)`,
		`DELETE FROM alerts WHERE site_id = ?`, `DELETE FROM notifications WHERE site_id = ?`, `DELETE FROM sites WHERE id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func affected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ErrConflict is returned when a unique name is already taken.
type ErrConflict struct{ Msg string }

func (e ErrConflict) Error() string { return e.Msg }

func uniqueErr(err error, msg string) error {
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return ErrConflict{msg}
	}
	return err
}

// ---- aggregates

const aggCols = `id, site_id, name, def, created_at, updated_at, data_reset_at`

func scanAgg(sc interface{ Scan(...any) error }) (model.Aggregate, error) {
	var a model.Aggregate
	var def string
	if err := sc.Scan(&a.ID, &a.SiteID, &a.Name, &def, &a.CreatedAt, &a.UpdatedAt, &a.DataResetAt); err != nil {
		return a, err
	}
	err := json.Unmarshal([]byte(def), &a.AggregateDef)
	if a.Events == nil {
		a.Events = []string{}
	}
	return a, err
}

func (s *SQLite) ListAggregates(ctx context.Context, siteID int64) ([]model.Aggregate, error) {
	q := `SELECT ` + aggCols + ` FROM aggregates`
	args := []any{}
	if siteID > 0 {
		q += ` WHERE site_id = ?`
		args = append(args, siteID)
	}
	rows, err := s.r.QueryContext(ctx, q+` ORDER BY name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Aggregate{}
	for rows.Next() {
		a, err := scanAgg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *SQLite) GetAggregate(ctx context.Context, id int64) (model.Aggregate, error) {
	a, err := scanAgg(s.r.QueryRowContext(ctx, `SELECT `+aggCols+` FROM aggregates WHERE id = ?`, id))
	return a, notFound(err)
}

func (s *SQLite) CreateAggregate(ctx context.Context, a *model.Aggregate) error {
	def, _ := json.Marshal(a.AggregateDef)
	a.CreatedAt, a.UpdatedAt = now(), now()
	res, err := s.w.ExecContext(ctx, `INSERT INTO aggregates (site_id, name, def, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		a.SiteID, a.Name, string(def), a.CreatedAt, a.UpdatedAt)
	if err != nil {
		return uniqueErr(err, "an aggregate with this name already exists")
	}
	a.ID, err = res.LastInsertId()
	return err
}

func (s *SQLite) UpdateAggregate(ctx context.Context, a *model.Aggregate) error {
	def, _ := json.Marshal(a.AggregateDef)
	a.UpdatedAt = now()
	res, err := s.w.ExecContext(ctx, `UPDATE aggregates SET def = ?, updated_at = ? WHERE id = ?`, string(def), a.UpdatedAt, a.ID)
	return affected(res, err)
}

func clearData(ctx context.Context, tx *sql.Tx, cond string, arg any) error {
	for _, t := range []string{"buckets", "distinct_members", "last_values", "totals", "labels", "matched"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+t+` WHERE agg_id `+cond, arg); err != nil {
			return err
		}
	}
	return nil
}

func (s *SQLite) DeleteAggregate(ctx context.Context, id int64) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := clearData(ctx, tx, `= ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM aggregates WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLite) ClearAggregateData(ctx context.Context, id int64, resetAt int64) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := clearData(ctx, tx, `= ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE aggregates SET data_reset_at = ? WHERE id = ?`, resetAt, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- formulas

type formulaDef struct {
	Title      string `json:"title"`
	Expr       string `json:"expr"`
	Unit       string `json:"unit"`
	Visibility string `json:"visibility"`
	Pinned     bool   `json:"pinned"`
}

func scanFormula(sc interface{ Scan(...any) error }) (model.Formula, error) {
	var f model.Formula
	var def string
	if err := sc.Scan(&f.ID, &f.SiteID, &f.Name, &def, &f.CreatedAt, &f.UpdatedAt); err != nil {
		return f, err
	}
	var d formulaDef
	err := json.Unmarshal([]byte(def), &d)
	f.Title, f.Expr, f.Unit, f.Visibility, f.Pinned = d.Title, d.Expr, d.Unit, d.Visibility, d.Pinned
	return f, err
}

func formulaJSON(f *model.Formula) string {
	b, _ := json.Marshal(formulaDef{f.Title, f.Expr, f.Unit, f.Visibility, f.Pinned})
	return string(b)
}

const formulaCols = `id, site_id, name, def, created_at, updated_at`

func (s *SQLite) ListFormulas(ctx context.Context, siteID int64) ([]model.Formula, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT `+formulaCols+` FROM formulas WHERE site_id = ? ORDER BY name`, siteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Formula{}
	for rows.Next() {
		f, err := scanFormula(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *SQLite) GetFormula(ctx context.Context, id int64) (model.Formula, error) {
	f, err := scanFormula(s.r.QueryRowContext(ctx, `SELECT `+formulaCols+` FROM formulas WHERE id = ?`, id))
	return f, notFound(err)
}

func (s *SQLite) CreateFormula(ctx context.Context, f *model.Formula) error {
	f.CreatedAt, f.UpdatedAt = now(), now()
	res, err := s.w.ExecContext(ctx, `INSERT INTO formulas (site_id, name, def, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		f.SiteID, f.Name, formulaJSON(f), f.CreatedAt, f.UpdatedAt)
	if err != nil {
		return uniqueErr(err, "a formula with this name already exists")
	}
	f.ID, err = res.LastInsertId()
	return err
}

func (s *SQLite) UpdateFormula(ctx context.Context, f *model.Formula) error {
	f.UpdatedAt = now()
	res, err := s.w.ExecContext(ctx, `UPDATE formulas SET def = ?, updated_at = ? WHERE id = ?`, formulaJSON(f), f.UpdatedAt, f.ID)
	return affected(res, err)
}

func (s *SQLite) DeleteFormula(ctx context.Context, id int64) error {
	res, err := s.w.ExecContext(ctx, `DELETE FROM formulas WHERE id = ?`, id)
	return affected(res, err)
}

// ---- exports

type exportDef struct {
	Format       string            `json:"format"`
	Scope        model.ExportScope `json:"scope"`
	CacheSeconds int               `json:"cacheSeconds"`
}

const exportCols = `id, site_id, name, def, token_hash, created_at, last_scraped_at, scrape_count`

func scanExport(sc interface{ Scan(...any) error }) (model.Export, error) {
	var e model.Export
	var def string
	if err := sc.Scan(&e.ID, &e.SiteID, &e.Name, &def, &e.TokenHash, &e.CreatedAt, &e.LastScrapedAt, &e.ScrapeCount); err != nil {
		return e, err
	}
	var d exportDef
	err := json.Unmarshal([]byte(def), &d)
	e.Format, e.Scope, e.CacheSeconds = d.Format, d.Scope, d.CacheSeconds
	return e, err
}

func exportJSON(e *model.Export) string {
	b, _ := json.Marshal(exportDef{e.Format, e.Scope, e.CacheSeconds})
	return string(b)
}

func (s *SQLite) ListExports(ctx context.Context, siteID int64) ([]model.Export, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT `+exportCols+` FROM exports WHERE site_id = ? ORDER BY name`, siteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Export{}
	for rows.Next() {
		e, err := scanExport(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *SQLite) GetExport(ctx context.Context, id string) (model.Export, error) {
	e, err := scanExport(s.r.QueryRowContext(ctx, `SELECT `+exportCols+` FROM exports WHERE id = ?`, id))
	return e, notFound(err)
}

func (s *SQLite) CreateExport(ctx context.Context, e *model.Export) error {
	e.CreatedAt = now()
	_, err := s.w.ExecContext(ctx, `INSERT INTO exports (id, site_id, name, def, token_hash, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		e.ID, e.SiteID, e.Name, exportJSON(e), e.TokenHash, e.CreatedAt)
	return err
}

func (s *SQLite) UpdateExport(ctx context.Context, e *model.Export) error {
	res, err := s.w.ExecContext(ctx, `UPDATE exports SET name = ?, def = ?, token_hash = ? WHERE id = ?`,
		e.Name, exportJSON(e), e.TokenHash, e.ID)
	return affected(res, err)
}

func (s *SQLite) DeleteExport(ctx context.Context, id string) error {
	res, err := s.w.ExecContext(ctx, `DELETE FROM exports WHERE id = ?`, id)
	return affected(res, err)
}

func (s *SQLite) RecordScrape(ctx context.Context, id string, at int64) error {
	_, err := s.w.ExecContext(ctx, `UPDATE exports SET last_scraped_at = ?, scrape_count = scrape_count + 1 WHERE id = ?`, at, id)
	return err
}

// ---- aggregate state

func (s *SQLite) ApplyBatch(ctx context.Context, b *Batch) error {
	if b.Empty() {
		return nil
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	exec := func(q string, each func(st *sql.Stmt) error) error {
		st, err := tx.PrepareContext(ctx, q)
		if err != nil {
			return err
		}
		defer st.Close()
		return each(st)
	}
	err = exec(`INSERT INTO buckets (agg_id, gran, part, member, bucket, cnt, sum) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT DO UPDATE SET cnt = cnt + excluded.cnt, sum = sum + excluded.sum`, func(st *sql.Stmt) error {
		for k, c := range b.Buckets {
			if _, err := st.ExecContext(ctx, k.Agg, string(k.Gran), k.Part, k.Member, k.Bucket, c.Count, c.Sum); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	err = exec(`INSERT OR IGNORE INTO distinct_members (agg_id, gran, part, bucket, hash) VALUES (?, ?, ?, ?, ?)`, func(st *sql.Stmt) error {
		for k := range b.Distinct {
			if _, err := st.ExecContext(ctx, k.Agg, string(k.Gran), k.Part, k.Bucket, k.Hash); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	err = exec(`INSERT INTO last_values (agg_id, part, ts, value) VALUES (?, ?, ?, ?)
		ON CONFLICT DO UPDATE SET ts = excluded.ts, value = excluded.value WHERE excluded.ts >= ts`, func(st *sql.Stmt) error {
		for k, v := range b.Last {
			if _, err := st.ExecContext(ctx, k.Agg, k.Part, v.TS, v.Value); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	err = exec(`INSERT INTO totals (agg_id, part, member, cnt, sum) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT DO UPDATE SET cnt = cnt + excluded.cnt, sum = sum + excluded.sum`, func(st *sql.Stmt) error {
		for k, c := range b.Totals {
			if _, err := st.ExecContext(ctx, k.Agg, k.Part, k.Member, c.Count, c.Sum); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	err = exec(`INSERT INTO labels (agg_id, kind, key, label) VALUES (?, ?, ?, ?)
		ON CONFLICT DO UPDATE SET label = excluded.label`, func(st *sql.Stmt) error {
		for k, l := range b.Labels {
			if _, err := st.ExecContext(ctx, k.Agg, string(k.Kind), k.Key, l); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	err = exec(`INSERT INTO matched (agg_id, hour, cnt) VALUES (?, ?, ?)
		ON CONFLICT DO UPDATE SET cnt = cnt + excluded.cnt`, func(st *sql.Stmt) error {
		for k, c := range b.Matched {
			if _, err := st.ExecContext(ctx, k.Agg, k.Hour, c); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	err = exec(`INSERT OR REPLACE INTO dedupe (site_id, event_id, expires_at) VALUES (?, ?, ?)`, func(st *sql.Stmt) error {
		for k, exp := range b.Dedupe {
			if _, err := st.ExecContext(ctx, k.Site, k.ID, exp); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	err = exec(`INSERT INTO events_raw (site_id, received_at, ts, name, event_id, visitor_id, props, meta) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, func(st *sql.Stmt) error {
		for _, e := range b.Raw {
			props, meta := "{}", "{}"
			if len(e.Props) > 0 {
				p, _ := json.Marshal(e.Props)
				props = string(p)
			}
			if len(e.Meta) > 0 {
				m, _ := json.Marshal(e.Meta)
				meta = string(m)
			}
			if _, err := st.ExecContext(ctx, e.SiteID, e.ReceivedAt, e.TS, e.Name, e.ID, e.VisitorID, props, meta); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLite) SumBuckets(ctx context.Context, agg int64, g model.Gran, from, to int64, part, member string) (Counter, error) {
	var c Counter
	err := s.r.QueryRowContext(ctx, `SELECT COALESCE(SUM(cnt), 0), COALESCE(SUM(sum), 0) FROM buckets
		WHERE agg_id = ? AND gran = ? AND part = ? AND member = ? AND bucket BETWEEN ? AND ?`,
		agg, string(g), part, member, from, to).Scan(&c.Count, &c.Sum)
	return c, err
}

func (s *SQLite) CountDistinct(ctx context.Context, agg int64, g model.Gran, from, to int64, part string) (int64, error) {
	var n int64
	err := s.r.QueryRowContext(ctx, `SELECT COUNT(DISTINCT hash) FROM distinct_members
		WHERE agg_id = ? AND gran = ? AND part = ? AND bucket BETWEEN ? AND ?`,
		agg, string(g), part, from, to).Scan(&n)
	return n, err
}

func (s *SQLite) Last(ctx context.Context, agg int64, part string) (LastValue, bool, error) {
	var v LastValue
	err := s.r.QueryRowContext(ctx, `SELECT ts, value FROM last_values WHERE agg_id = ? AND part = ?`, agg, part).Scan(&v.TS, &v.Value)
	if errors.Is(err, sql.ErrNoRows) {
		return v, false, nil
	}
	return v, err == nil, err
}

func (s *SQLite) Total(ctx context.Context, agg int64, part, member string) (Counter, error) {
	var c Counter
	err := s.r.QueryRowContext(ctx, `SELECT cnt, sum FROM totals WHERE agg_id = ? AND part = ? AND member = ?`, agg, part, member).Scan(&c.Count, &c.Sum)
	if errors.Is(err, sql.ErrNoRows) {
		return c, nil
	}
	return c, err
}

func (s *SQLite) queryTop(ctx context.Context, q string, args ...any) ([]TopRow, error) {
	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TopRow{}
	for rows.Next() {
		var t TopRow
		if err := rows.Scan(&t.Key, &t.Count, &t.Sum, &t.TS); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *SQLite) TopBuckets(ctx context.Context, agg int64, g model.Gran, from, to int64, level Level, part string, bySum bool, limit int) ([]TopRow, error) {
	order := "2 DESC"
	if bySum {
		order = "3 DESC"
	}
	if level == LevelPart {
		return s.queryTop(ctx, `SELECT part, SUM(cnt), SUM(sum), 0 FROM buckets
			WHERE agg_id = ? AND gran = ? AND bucket BETWEEN ? AND ? AND part <> '' AND member = ''
			GROUP BY part ORDER BY `+order+`, 1 LIMIT ?`, agg, string(g), from, to, limit)
	}
	return s.queryTop(ctx, `SELECT member, SUM(cnt), SUM(sum), 0 FROM buckets
		WHERE agg_id = ? AND gran = ? AND bucket BETWEEN ? AND ? AND part = ? AND member <> ''
		GROUP BY member ORDER BY `+order+`, 1 LIMIT ?`, agg, string(g), from, to, part, limit)
}

func (s *SQLite) TopDistinct(ctx context.Context, agg int64, g model.Gran, from, to int64, limit int) ([]TopRow, error) {
	return s.queryTop(ctx, `SELECT part, COUNT(DISTINCT hash), 0, 0 FROM distinct_members
		WHERE agg_id = ? AND gran = ? AND bucket BETWEEN ? AND ? AND part <> ''
		GROUP BY part ORDER BY 2 DESC, 1 LIMIT ?`, agg, string(g), from, to, limit)
}

func (s *SQLite) TopLast(ctx context.Context, agg int64, limit int) ([]TopRow, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT part, ts, value FROM last_values WHERE agg_id = ? AND part <> ''
		ORDER BY ts DESC, part LIMIT ?`, agg, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TopRow{}
	for rows.Next() {
		var t TopRow
		var v string
		if err := rows.Scan(&t.Key, &t.TS, &v); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *SQLite) TopTotals(ctx context.Context, agg int64, limit int) ([]TopRow, error) {
	return s.queryTop(ctx, `SELECT part, cnt, sum, 0 FROM totals WHERE agg_id = ? AND part <> '' AND member = ''
		ORDER BY cnt DESC, part LIMIT ?`, agg, limit)
}

func (s *SQLite) SeriesBuckets(ctx context.Context, agg int64, g model.Gran, from, to int64, part, member string) ([]Point, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT bucket, cnt, sum FROM buckets
		WHERE agg_id = ? AND gran = ? AND part = ? AND member = ? AND bucket BETWEEN ? AND ? ORDER BY bucket`,
		agg, string(g), part, member, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Point{}
	for rows.Next() {
		var p Point
		if err := rows.Scan(&p.Bucket, &p.Count, &p.Sum); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *SQLite) SeriesDistinct(ctx context.Context, agg int64, g model.Gran, from, to int64, part string) ([]Point, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT bucket, COUNT(*) FROM distinct_members
		WHERE agg_id = ? AND gran = ? AND part = ? AND bucket BETWEEN ? AND ? GROUP BY bucket ORDER BY bucket`,
		agg, string(g), part, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Point{}
	for rows.Next() {
		var p Point
		if err := rows.Scan(&p.Bucket, &p.Count); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *SQLite) Labels(ctx context.Context, agg int64, kind byte, keys []string) (map[string]string, error) {
	out := map[string]string{}
	if len(keys) == 0 {
		return out, nil
	}
	args := []any{agg, string(kind)}
	for _, k := range keys {
		args = append(args, k)
	}
	rows, err := s.r.QueryContext(ctx, `SELECT key, label FROM labels WHERE agg_id = ? AND kind = ? AND key IN (?`+
		strings.Repeat(",?", len(keys)-1)+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, l string
		if err := rows.Scan(&k, &l); err != nil {
			return nil, err
		}
		out[k] = l
	}
	return out, rows.Err()
}

func (s *SQLite) MatchedSince(ctx context.Context, siteID int64, fromHour int64) (map[int64]int64, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT m.agg_id, SUM(m.cnt) FROM matched m JOIN aggregates a ON a.id = m.agg_id
		WHERE a.site_id = ? AND m.hour >= ? GROUP BY m.agg_id`, siteID, fromHour)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var id, n int64
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

func (s *SQLite) KeyCount(ctx context.Context, agg int64) (int, error) {
	var n int
	err := s.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM totals WHERE agg_id = ? AND NOT (part = '' AND member = '')`, agg).Scan(&n)
	return n, err
}

// KnownKeys returns part+"\x00"+member for every key the aggregate has state for (except the site total).
func (s *SQLite) KnownKeys(ctx context.Context, agg int64) ([]string, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT part, member FROM totals WHERE agg_id = ? AND NOT (part = '' AND member = '')`, agg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var p, m string
		if err := rows.Scan(&p, &m); err != nil {
			return nil, err
		}
		out = append(out, p+"\x00"+m)
	}
	return out, rows.Err()
}

// ---- raw events

func scanEvents(rows *sql.Rows) ([]model.Event, error) {
	defer rows.Close()
	out := []model.Event{}
	for rows.Next() {
		var e model.Event
		var props, meta string
		if err := rows.Scan(&e.RawID, &e.SiteID, &e.ReceivedAt, &e.TS, &e.Name, &e.ID, &e.VisitorID, &props, &meta); err != nil {
			return nil, err
		}
		if props != "" && props != "{}" {
			if err := json.Unmarshal([]byte(props), &e.Props); err != nil {
				return nil, err
			}
		}
		if meta != "" && meta != "{}" {
			if err := json.Unmarshal([]byte(meta), &e.Meta); err != nil {
				return nil, err
			}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

const rawCols = `id, site_id, received_at, ts, name, event_id, visitor_id, props, meta`

func (s *SQLite) RecentEvents(ctx context.Context, siteID int64, eq EventQuery) ([]model.Event, error) {
	q := `SELECT ` + rawCols + ` FROM events_raw WHERE site_id = ?`
	args := []any{siteID}
	if eq.Name != "" {
		q += ` AND name = ?`
		args = append(args, eq.Name)
	}
	if eq.VisitorID != "" {
		q += ` AND visitor_id = ?`
		args = append(args, eq.VisitorID)
	}
	if eq.BeforeID > 0 {
		q += ` AND id < ?`
		args = append(args, eq.BeforeID)
	}
	rows, err := s.r.QueryContext(ctx, q+` ORDER BY id DESC LIMIT ?`, append(args, eq.Limit)...)
	if err != nil {
		return nil, err
	}
	return scanEvents(rows)
}

func (s *SQLite) EventNameCounts(ctx context.Context, siteID int64, since int64) ([]EventNameCount, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT name, COUNT(*) FROM events_raw WHERE site_id = ? AND received_at >= ?
		GROUP BY name ORDER BY 2 DESC, 1`, siteID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EventNameCount{}
	for rows.Next() {
		var c EventNameCount
		if err := rows.Scan(&c.Name, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ScanRaw calls fn for every stored event of the site received at or before upTo (unix ms), oldest first.
func (s *SQLite) ScanRaw(ctx context.Context, siteID int64, upTo int64, fn func(model.Event) error) error {
	var lastID int64
	for {
		rows, err := s.r.QueryContext(ctx, `SELECT `+rawCols+` FROM events_raw
			WHERE site_id = ? AND received_at <= ? AND id > ? ORDER BY id LIMIT 2000`, siteID, upTo, lastID)
		if err != nil {
			return err
		}
		evs, err := scanEvents(rows)
		if err != nil {
			return err
		}
		for _, e := range evs {
			if err := fn(e); err != nil {
				return err
			}
			lastID = e.RawID
		}
		if len(evs) < 2000 {
			return nil
		}
	}
}

func (s *SQLite) LoadDedupe(ctx context.Context, nowS int64) (map[DedupeKey]int64, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT site_id, event_id, expires_at FROM dedupe WHERE expires_at > ?`, nowS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[DedupeKey]int64{}
	for rows.Next() {
		var k DedupeKey
		var exp int64
		if err := rows.Scan(&k.Site, &k.ID, &exp); err != nil {
			return nil, err
		}
		out[k] = exp
	}
	return out, rows.Err()
}

// ---- maintenance

func (s *SQLite) Cleanup(ctx context.Context, nowS int64, rawRetentionDays int) error {
	for g, keep := range model.Retention {
		cut := nowS - int64(keep.Seconds())
		if _, err := s.w.ExecContext(ctx, `DELETE FROM buckets WHERE gran = ? AND bucket < ?`, string(g), cut); err != nil {
			return err
		}
		if _, err := s.w.ExecContext(ctx, `DELETE FROM distinct_members WHERE gran = ? AND bucket < ?`, string(g), cut); err != nil {
			return err
		}
	}
	if _, err := s.w.ExecContext(ctx, `DELETE FROM matched WHERE hour < ?`, nowS-40*86400); err != nil {
		return err
	}
	if _, err := s.w.ExecContext(ctx, `DELETE FROM notifications WHERE ts < ?`, (nowS-90*86400)*1000); err != nil {
		return err
	}
	if _, err := s.w.ExecContext(ctx, `DELETE FROM alert_events WHERE ts < ?`, (nowS-90*86400)*1000); err != nil {
		return err
	}
	if _, err := s.w.ExecContext(ctx, `DELETE FROM dedupe WHERE expires_at < ?`, nowS); err != nil {
		return err
	}
	_, err := s.w.ExecContext(ctx, `DELETE FROM events_raw WHERE received_at < ?`, (nowS-int64(rawRetentionDays)*86400)*1000)
	return err
}

func (s *SQLite) DBSize(ctx context.Context) (int64, error) {
	var pages, size int64
	if err := s.r.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages); err != nil {
		return 0, err
	}
	if err := s.r.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&size); err != nil {
		return 0, err
	}
	return pages * size, nil
}

// ---- alerts

const alertCols = `id, site_id, name, def, state, state_since, last_eval, last_notified, last_value, last_error, created_at, updated_at`

func scanAlert(sc interface{ Scan(...any) error }) (model.Alert, error) {
	var a model.Alert
	var def string
	if err := sc.Scan(&a.ID, &a.SiteID, &a.Name, &def, &a.State, &a.StateSince, &a.LastEval, &a.LastNotified,
		&a.LastValue, &a.LastError, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return a, err
	}
	err := json.Unmarshal([]byte(def), &a.AlertDef)
	if a.Dims == nil {
		a.Dims = map[string]string{}
	}
	if a.Schedule.Days == nil {
		a.Schedule.Days = []int{}
	}
	return a, err
}

func (s *SQLite) ListAlerts(ctx context.Context, siteID int64) ([]model.Alert, error) {
	q := `SELECT ` + alertCols + ` FROM alerts`
	args := []any{}
	if siteID > 0 {
		q += ` WHERE site_id = ?`
		args = append(args, siteID)
	}
	rows, err := s.r.QueryContext(ctx, q+` ORDER BY name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Alert{}
	for rows.Next() {
		a, err := scanAlert(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *SQLite) GetAlert(ctx context.Context, id int64) (model.Alert, error) {
	a, err := scanAlert(s.r.QueryRowContext(ctx, `SELECT `+alertCols+` FROM alerts WHERE id = ?`, id))
	return a, notFound(err)
}

func (s *SQLite) CreateAlert(ctx context.Context, a *model.Alert) error {
	def, _ := json.Marshal(a.AlertDef)
	a.CreatedAt, a.UpdatedAt = now(), now()
	if a.State == "" {
		a.State = model.AlertOK
	}
	res, err := s.w.ExecContext(ctx, `INSERT INTO alerts (site_id, name, def, state, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		a.SiteID, a.Name, string(def), a.State, a.CreatedAt, a.UpdatedAt)
	if err != nil {
		return uniqueErr(err, "an alert with this name already exists")
	}
	a.ID, err = res.LastInsertId()
	return err
}

// UpdateAlert saves the definition and resets the state, so a changed condition starts fresh.
func (s *SQLite) UpdateAlert(ctx context.Context, a *model.Alert) error {
	def, _ := json.Marshal(a.AlertDef)
	a.UpdatedAt = now()
	a.State, a.StateSince, a.LastError = model.AlertOK, 0, ""
	res, err := s.w.ExecContext(ctx, `UPDATE alerts SET def = ?, updated_at = ?, state = 'ok', state_since = 0, last_error = '' WHERE id = ?`,
		string(def), a.UpdatedAt, a.ID)
	return affected(res, err)
}

func (s *SQLite) SaveAlertState(ctx context.Context, a *model.Alert) error {
	_, err := s.w.ExecContext(ctx, `UPDATE alerts SET state = ?, state_since = ?, last_eval = ?, last_notified = ?,
		last_value = ?, last_error = ? WHERE id = ?`, a.State, a.StateSince, a.LastEval, a.LastNotified, a.LastValue, a.LastError, a.ID)
	return err
}

func (s *SQLite) DeleteAlert(ctx context.Context, id int64) error {
	if _, err := s.w.ExecContext(ctx, `DELETE FROM alert_events WHERE alert_id = ?`, id); err != nil {
		return err
	}
	res, err := s.w.ExecContext(ctx, `DELETE FROM alerts WHERE id = ?`, id)
	return affected(res, err)
}

func (s *SQLite) AddAlertEvent(ctx context.Context, e model.AlertEvent) error {
	_, err := s.w.ExecContext(ctx, `INSERT INTO alert_events (alert_id, ts, state, message) VALUES (?, ?, ?, ?)`, e.AlertID, e.TS, e.State, e.Message)
	return err
}

func (s *SQLite) ListAlertEvents(ctx context.Context, alertID int64, limit int) ([]model.AlertEvent, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT id, alert_id, ts, state, message FROM alert_events WHERE alert_id = ? ORDER BY id DESC LIMIT ?`, alertID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.AlertEvent{}
	for rows.Next() {
		var e model.AlertEvent
		if err := rows.Scan(&e.ID, &e.AlertID, &e.TS, &e.State, &e.Message); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *SQLite) AddNotification(ctx context.Context, n *model.Notification) error {
	res, err := s.w.ExecContext(ctx, `INSERT INTO notifications (site_id, alert_id, ts, title, body, url) VALUES (?, ?, ?, ?, ?, ?)`,
		n.SiteID, n.AlertID, n.TS, n.Title, n.Body, n.URL)
	if err != nil {
		return err
	}
	n.ID, err = res.LastInsertId()
	return err
}

// ListNotifications returns the newest notifications and the number of unread ones.
func (s *SQLite) ListNotifications(ctx context.Context, limit int) ([]model.Notification, int, error) {
	var unread int
	if err := s.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM notifications WHERE read = 0`).Scan(&unread); err != nil {
		return nil, 0, err
	}
	rows, err := s.r.QueryContext(ctx, `SELECT id, site_id, alert_id, ts, title, body, url, read FROM notifications ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []model.Notification{}
	for rows.Next() {
		var n model.Notification
		if err := rows.Scan(&n.ID, &n.SiteID, &n.AlertID, &n.TS, &n.Title, &n.Body, &n.URL, &n.Read); err != nil {
			return nil, 0, err
		}
		out = append(out, n)
	}
	return out, unread, rows.Err()
}

func (s *SQLite) MarkNotificationsRead(ctx context.Context) error {
	_, err := s.w.ExecContext(ctx, `UPDATE notifications SET read = 1 WHERE read = 0`)
	return err
}

func (s *SQLite) SavePushSubscription(ctx context.Context, p model.PushSubscription) error {
	_, err := s.w.ExecContext(ctx, `INSERT INTO push_subscriptions (endpoint, p256dh, auth, user_agent, created_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (endpoint) DO UPDATE SET p256dh = excluded.p256dh, auth = excluded.auth, user_agent = excluded.user_agent`,
		p.Endpoint, p.P256dh, p.Auth, p.UserAgent, now())
	return err
}

func (s *SQLite) ListPushSubscriptions(ctx context.Context) ([]model.PushSubscription, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT endpoint, p256dh, auth, user_agent, created_at FROM push_subscriptions ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.PushSubscription{}
	for rows.Next() {
		var p model.PushSubscription
		if err := rows.Scan(&p.Endpoint, &p.P256dh, &p.Auth, &p.UserAgent, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *SQLite) DeletePushSubscription(ctx context.Context, endpoint string) error {
	_, err := s.w.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE endpoint = ?`, endpoint)
	return err
}

// ---- API tokens

func (s *SQLite) ListAPITokens(ctx context.Context) ([]model.APIToken, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT id, name, prefix, token_hash, created_at, last_used_at FROM api_tokens ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.APIToken{}
	for rows.Next() {
		var t model.APIToken
		if err := rows.Scan(&t.ID, &t.Name, &t.Prefix, &t.TokenHash, &t.CreatedAt, &t.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *SQLite) CreateAPIToken(ctx context.Context, t *model.APIToken) error {
	t.CreatedAt = now()
	res, err := s.w.ExecContext(ctx, `INSERT INTO api_tokens (name, prefix, token_hash, created_at) VALUES (?, ?, ?, ?)`,
		t.Name, t.Prefix, t.TokenHash, t.CreatedAt)
	if err != nil {
		return err
	}
	t.ID, err = res.LastInsertId()
	return err
}

func (s *SQLite) DeleteAPIToken(ctx context.Context, id int64) error {
	res, err := s.w.ExecContext(ctx, `DELETE FROM api_tokens WHERE id = ?`, id)
	return affected(res, err)
}

func (s *SQLite) APITokenByHash(ctx context.Context, hash string) (model.APIToken, error) {
	var t model.APIToken
	err := s.r.QueryRowContext(ctx, `SELECT id, name, prefix, token_hash, created_at, last_used_at FROM api_tokens WHERE token_hash = ?`, hash).
		Scan(&t.ID, &t.Name, &t.Prefix, &t.TokenHash, &t.CreatedAt, &t.LastUsedAt)
	return t, notFound(err)
}

func (s *SQLite) TouchAPIToken(ctx context.Context, id int64, at int64) error {
	_, err := s.w.ExecContext(ctx, `UPDATE api_tokens SET last_used_at = ? WHERE id = ?`, at, id)
	return err
}
