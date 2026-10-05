// Package store keeps the price history in SQLite: every search with its
// full response, the offers it found, and the alerts that were sent. The
// database is one file in the data folder; it is the asset worth protecting.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure Go, no CGO

	"github.com/uros678/flightprice/internal/serpapi"
)

//go:embed schema.sql
var schema string

// schemaVersion is the version schema.sql creates, kept in PRAGMA
// user_version. A later change adds a migration step in migrate.
const schemaVersion = 1

// timeLayout is how times are stored: UTC, sortable as text.
const timeLayout = time.RFC3339

// Limits decide which offers count for the price history: the tracked price
// of a run is its cheapest offer within both.
type Limits struct {
	MaxStops       int
	MaxDurationMin int // 0 = no limit
}

// Store is an open database.
type Store struct {
	db   *sql.DB
	path string
}

// Open opens (or creates) the database at path, brings its schema up to date
// and creates the observations view for limits.
func Open(ctx context.Context, path string, limits Limits) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection: the app writes a few rows a day, and a single
	// connection keeps the pragmas and avoids SQLITE_BUSY between them.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: path}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := s.SetLimits(ctx, limits); err != nil {
		db.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	switch {
	case version == schemaVersion:
		return nil
	case version > schemaVersion:
		return fmt.Errorf("database schema %d is newer than this program (%d)", version, schemaVersion)
	}
	// An existing database is about to change for a newer program: keep a
	// copy of it as it was, so going back to the older version stays
	// possible (the older program refuses the newer schema).
	if version > 0 {
		if _, err := s.copyBeforeUpgrade(ctx, version); err != nil {
			return err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if version == 0 {
		if _, err := tx.ExecContext(ctx, schema); err != nil {
			return fmt.Errorf("creating the schema: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

// SetLimits recreates the observations view: one row per successful run,
// its cheapest offer within limits. Runs without such an offer have no row.
// SQLite returns the other columns of the row that holds the MIN().
func (s *Store) SetLimits(ctx context.Context, l Limits) error {
	if l.MaxStops < 0 || l.MaxDurationMin < 0 {
		return errors.New("limits must not be negative")
	}
	where := fmt.Sprintf("o.stops <= %d", l.MaxStops)
	if l.MaxDurationMin > 0 {
		where += fmt.Sprintf(" AND o.duration_min <= %d", l.MaxDurationMin)
	}
	view := `CREATE VIEW observations AS
SELECT r.id AS run_id, r.option, r.origin, r.started_at, r.price_level,
       MIN(o.price_cents) AS price_cents, o.currency, o.id AS offer_id,
       o.stops, o.duration_min, o.airlines, o.route
FROM runs r JOIN offers o ON o.run_id = r.id
WHERE r.status = 'SUCCESS' AND ` + where + `
GROUP BY r.id`
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DROP VIEW IF EXISTS observations"); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, view); err != nil {
		return err
	}
	return tx.Commit()
}

// Run is one search to save: what was asked, what came back and why it ran.
type Run struct {
	Option    string
	Origin    string
	StartedAt time.Time
	Source    string // scheduled, manual, retry or probe
	Request   serpapi.Request
	Result    serpapi.Result
	Err       error // the search error, if any
}

// SaveRun stores a run with its offers in one transaction and returns its id.
// A failed search is saved too: it never touches earlier data, and it may
// have counted against the quota.
func (s *Store) SaveRun(ctx context.Context, r Run) (int64, error) {
	if r.Option == "" || r.Origin == "" || r.Result.Status == "" {
		return 0, errors.New("run without option, origin or status")
	}
	req, err := json.Marshal(r.Request)
	if err != nil {
		return 0, err
	}
	var errText, raw sql.NullString
	if r.Err != nil {
		errText = sql.NullString{String: r.Err.Error(), Valid: true}
	}
	if r.Result.Raw != nil {
		raw = sql.NullString{String: string(r.Result.Raw), Valid: true}
	}
	var level sql.NullString
	var lowest, typLow, typHigh sql.NullInt64
	if in := r.Result.Insights; in != nil {
		level = sql.NullString{String: in.Level, Valid: in.Level != ""}
		lowest = sql.NullInt64{Int64: in.LowestCents, Valid: in.LowestCents > 0}
		typLow = sql.NullInt64{Int64: in.TypicalLowCents, Valid: in.TypicalLowCents > 0}
		typHigh = sql.NullInt64{Int64: in.TypicalHighCents, Valid: in.TypicalHighCents > 0}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO runs
		(option, origin, started_at, source, status, error, billed, request_json, raw_json, price_level, lowest, typical_low, typical_high)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Option, r.Origin, r.StartedAt.UTC().Format(timeLayout), r.Source, string(r.Result.Status), errText,
		r.Result.Raw != nil, string(req), raw, level, lowest, typLow, typHigh)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	for _, o := range r.Result.Offers {
		_, err := tx.ExecContext(ctx, `INSERT INTO offers
			(run_id, price_cents, currency, stops, duration_min, airlines, route, flight_numbers, depart_time, arrive_time, departure_token)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, o.PriceCents, r.Request.Currency, o.Stops, o.DurationMin, join(o.Airlines), join(o.Route),
			join(o.FlightNumbers), o.DepartTime, o.ArriveTime, o.DepartureToken)
		if err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}

// copyBeforeUpgrade writes backup/flightprice-schema-<from>.db next to the
// database. These copies are not pruned like the weekly backups.
func (s *Store) copyBeforeUpgrade(ctx context.Context, from int) (string, error) {
	dir := filepath.Join(filepath.Dir(s.path), "backup")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("flightprice-schema-%d.db", from))
	if _, err := os.Stat(path); err == nil {
		return path, nil // an earlier, interrupted upgrade already kept it
	}
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("copy before the schema upgrade: %w", err)
	}
	return path, nil
}

// CallsSince is the number of searches since t that SerpApi answered, i.e.
// that count against the quota.
func (s *Store) CallsSince(ctx context.Context, t time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM runs WHERE billed = 1 AND started_at >= ?",
		t.UTC().Format(timeLayout)).Scan(&n)
	return n, err
}

// LastChecked is, per origin, the time of the latest search that worked
// (with or without flights). Origins never checked are missing.
func (s *Store) LastChecked(ctx context.Context) (map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT origin, MAX(started_at) FROM runs
		WHERE status IN ('SUCCESS', 'NO_RESULTS') GROUP BY origin`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	last := map[string]time.Time{}
	for rows.Next() {
		var origin, at string
		if err := rows.Scan(&origin, &at); err != nil {
			return nil, err
		}
		t, err := time.Parse(timeLayout, at)
		if err != nil {
			return nil, err
		}
		last[origin] = t
	}
	return last, rows.Err()
}

// HasOffers reports whether an option has ever had a successful search,
// i.e. whether its fares are on sale.
func (s *Store) HasOffers(ctx context.Context, option string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM runs WHERE option = ? AND status = 'SUCCESS'", option).Scan(&n)
	return n > 0, err
}

// Observation is one point of the price history: the tracked price of one
// run.
type Observation struct {
	RunID       int64
	Option      string
	Origin      string
	At          time.Time
	PriceCents  int64
	Currency    string
	Level       string // Google's price level, "" when none
	OfferID     int64
	Stops       int
	DurationMin int
	Airlines    []string
	Route       []string
}

// Observations returns the price history since t, oldest first. An empty
// option or origin means all of them.
func (s *Store) Observations(ctx context.Context, option, origin string, since time.Time) ([]Observation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT run_id, option, origin, started_at, COALESCE(price_level, ''),
		price_cents, currency, offer_id, stops, duration_min, airlines, route
		FROM observations
		WHERE (? = '' OR option = ?) AND (? = '' OR origin = ?) AND started_at >= ?
		ORDER BY started_at, run_id`,
		option, option, origin, origin, since.UTC().Format(timeLayout))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Observation
	for rows.Next() {
		var o Observation
		var at, airlines, route string
		if err := rows.Scan(&o.RunID, &o.Option, &o.Origin, &at, &o.Level, &o.PriceCents, &o.Currency,
			&o.OfferID, &o.Stops, &o.DurationMin, &airlines, &route); err != nil {
			return nil, err
		}
		if o.At, err = time.Parse(timeLayout, at); err != nil {
			return nil, err
		}
		o.Airlines, o.Route = split(airlines), split(route)
		out = append(out, o)
	}
	return out, rows.Err()
}

// CountRuns is the number of runs since t whose source is one of sources.
func (s *Store) CountRuns(ctx context.Context, since time.Time, sources ...string) (int, error) {
	if len(sources) == 0 {
		return 0, errors.New("no source given")
	}
	args := []any{since.UTC().Format(timeLayout)}
	for _, src := range sources {
		args = append(args, src)
	}
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM runs WHERE started_at >= ? AND source IN ("+
		placeholders(len(sources))+")", args...).Scan(&n)
	return n, err
}

// RunInfo is one run for the log on the page.
type RunInfo struct {
	ID         int64
	Option     string
	Origin     string
	At         time.Time
	Source     string
	Status     string
	Error      string
	PriceCents int64 // tracked price, 0 when the run has none
}

// RecentRuns returns the newest runs first.
func (s *Store) RecentRuns(ctx context.Context, limit int) ([]RunInfo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.id, r.option, r.origin, r.started_at, r.source, r.status,
		COALESCE(r.error, ''), COALESCE(o.price_cents, 0)
		FROM runs r LEFT JOIN observations o ON o.run_id = r.id
		ORDER BY r.started_at DESC, r.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunInfo
	for rows.Next() {
		var r RunInfo
		var at string
		if err := rows.Scan(&r.ID, &r.Option, &r.Origin, &at, &r.Source, &r.Status, &r.Error, &r.PriceCents); err != nil {
			return nil, err
		}
		if r.At, err = time.Parse(timeLayout, at); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Offer is a stored offer with the run it belongs to.
type Offer struct {
	RunID         int64
	Option        string
	Origin        string
	PriceCents    int64
	Currency      string
	Stops         int
	DurationMin   int
	Airlines      []string
	Route         []string
	FlightNumbers []string
	DepartTime    string
	ArriveTime    string
}

// Offers returns all offers of the given runs, cheapest first.
func (s *Store) Offers(ctx context.Context, runIDs []int64) ([]Offer, error) {
	if len(runIDs) == 0 {
		return nil, nil
	}
	args := make([]any, len(runIDs))
	for i, id := range runIDs {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, `SELECT o.run_id, r.option, r.origin, o.price_cents, o.currency, o.stops,
		o.duration_min, o.airlines, o.route, o.flight_numbers, o.depart_time, o.arrive_time
		FROM offers o JOIN runs r ON r.id = o.run_id
		WHERE o.run_id IN (`+placeholders(len(runIDs))+`)
		ORDER BY o.price_cents, o.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Offer
	for rows.Next() {
		var o Offer
		var airlines, route, numbers string
		if err := rows.Scan(&o.RunID, &o.Option, &o.Origin, &o.PriceCents, &o.Currency, &o.Stops, &o.DurationMin,
			&airlines, &route, &numbers, &o.DepartTime, &o.ArriveTime); err != nil {
			return nil, err
		}
		o.Airlines, o.Route, o.FlightNumbers = split(airlines), split(route), split(numbers)
		out = append(out, o)
	}
	return out, rows.Err()
}

func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

// backupPrefix and backupLayout name the backup files:
// flightprice-2027-05-10.db.
const (
	backupPrefix = "flightprice-"
	backupLayout = "2006-01-02"
)

// LastBackup is the date of the newest backup in dir (zero if none).
func LastBackup(dir string) (time.Time, error) {
	files, err := backups(dir)
	if err != nil || len(files) == 0 {
		return time.Time{}, err
	}
	return backupDate(files[len(files)-1]), nil
}

// Backup writes a consistent copy of the database to dir as
// flightprice-YYYY-MM-DD.db (VACUUM INTO works while the database is in use)
// and deletes all but the newest keep copies. A copy for that day that
// already exists is kept as it is.
func (s *Store) Backup(ctx context.Context, dir string, day time.Time, keep int) (string, error) {
	if keep < 1 {
		return "", errors.New("keep must be at least 1")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, backupPrefix+day.Format(backupLayout)+".db")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
			os.Remove(path)
			return "", fmt.Errorf("backup to %s: %w", path, err)
		}
	} else if err != nil {
		return "", err
	}
	files, err := backups(dir)
	if err != nil {
		return path, err
	}
	for len(files) > keep {
		if err := os.Remove(files[0]); err != nil {
			return path, err
		}
		files = files[1:]
	}
	return path, nil
}

// backups lists the backup files in dir, oldest first.
func backups(dir string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, backupPrefix+"*.db"))
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range matches {
		if !backupDate(f).IsZero() {
			files = append(files, f)
		}
	}
	slices.Sort(files) // the dates sort as text
	return files, nil
}

func backupDate(path string) time.Time {
	name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), backupPrefix), ".db")
	t, err := time.Parse(backupLayout, name)
	if err != nil {
		return time.Time{}
	}
	return t
}

func join(list []string) string { return strings.Join(list, ",") }

func split(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}
