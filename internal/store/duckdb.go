package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	_ "github.com/marcboeker/go-duckdb/v2" // registers the "duckdb" database/sql driver

	"github.com/kornsour/run-ledger/internal/lineage"
)

// DuckDB is a durable, column-oriented Store backed by an embedded DuckDB
// database.
//
// The workload this backend exists for is analytical -- "every run of this
// project on this dataset version, grouped by fingerprint, with the spread of
// loss across each group" -- not transactional. That is a column-oriented
// scan over a high-cardinality key space (fingerprints, commits, arbitrary
// params and metrics), which is what DuckDB is for and what Memory's linear
// scan-with-Go-filters is not.
//
// Schema: run scalars live in a single "runs" table; Params and Metrics live
// in narrow "run_params" / "run_metrics" key/value tables rather than as JSON
// blobs, so a future filter on params.lr is a real column predicate instead
// of a JSON-reach-in. See docs/adr for the cgo trade-off this backend costs.
//
// Concurrency: DuckDB's embedded engine is single-writer -- concurrent write
// transactions from separate connections are expected to conflict rather
// than interleave. Record therefore serializes writes behind a mutex, the
// same way Memory does; Get and List are unguarded reads and rely on
// DuckDB's MVCC snapshot isolation to never observe a write's partial
// effect. A go-duckdb DSN maps to one shared in-process database instance
// however many *sql.DB connections open it, so this mutex is what actually
// gives Record its all-or-nothing, single-winner semantics -- it does not
// depend on DuckDB's own conflict/retry behavior at all.
type DuckDB struct {
	db *sql.DB
	mu sync.Mutex
}

// NewDuckDB opens (creating if necessary) a DuckDB database at dsn and
// applies any pending schema migrations.
//
// dsn is whatever go-duckdb accepts as a data source name: a file path for a
// persistent database, or "" for a private in-memory instance that is gone
// once Close is called (useful for tests, not for a durable ledger).
func NewDuckDB(dsn string) (*DuckDB, error) {
	db, err := sql.Open("duckdb", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening duckdb %q: %w", dsn, err)
	}
	// DuckDB's embedded engine does not benefit from a connection pool the
	// way a network database does -- every connection opened against the
	// same DSN shares the one underlying database instance in this process,
	// and Record already serializes writes itself. Capping the pool avoids
	// DuckDB's own "unique connection required" limits on some operations
	// and keeps the mental model to one writer.
	db.SetMaxOpenConns(4)

	d := &DuckDB{db: db}
	if err := d.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrating duckdb %q: %w", dsn, err)
	}
	return d, nil
}

// migrations are applied once each, in order, tracked in schema_migrations.
// Ordering matters and entries are never edited after release -- add a new
// one instead of changing an old statement, the way any SQL migration chain
// works.
var migrations = []string{
	`CREATE TABLE IF NOT EXISTS runs (
		run_id            VARCHAR PRIMARY KEY,
		project           VARCHAR NOT NULL,
		git_commit        VARCHAR NOT NULL,
		git_dirty         BOOLEAN NOT NULL,
		config_hash       VARCHAR NOT NULL,
		dataset_version   VARCHAR NOT NULL,
		model_version     VARCHAR NOT NULL,
		seed              BIGINT NOT NULL,
		fingerprint       VARCHAR NOT NULL,
		host              VARCHAR NOT NULL,
		device            VARCHAR NOT NULL,
		framework_version VARCHAR NOT NULL,
		status            VARCHAR NOT NULL,
		started_at_ns     BIGINT NOT NULL,
		ended_at_ns       BIGINT,
		checkpoint_uri    VARCHAR NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS run_params (
		run_id VARCHAR NOT NULL REFERENCES runs(run_id),
		key    VARCHAR NOT NULL,
		value  VARCHAR NOT NULL,
		PRIMARY KEY (run_id, key)
	)`,
	`CREATE TABLE IF NOT EXISTS run_metrics (
		run_id VARCHAR NOT NULL REFERENCES runs(run_id),
		key    VARCHAR NOT NULL,
		value  DOUBLE NOT NULL,
		PRIMARY KEY (run_id, key)
	)`,
	// The two access paths the API actually has: newest-first within a
	// project, and an exact fingerprint lookup for "did this change
	// anything?" grouping.
	`CREATE INDEX IF NOT EXISTS idx_runs_project_started ON runs (project, started_at_ns DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_runs_fingerprint ON runs (fingerprint)`,
}

func (d *DuckDB) migrate(ctx context.Context) error {
	if _, err := d.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY)`); err != nil {
		return fmt.Errorf("creating schema_migrations: %w", err)
	}
	applied := map[int]bool{}
	rows, err := d.db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("reading schema_migrations: %w", err)
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()

	for v, stmt := range migrations {
		if applied[v] {
			continue
		}
		tx, err := d.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %d: %w", v, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version) VALUES (?)`, v); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %d: recording version: %w", v, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migration %d: %w", v, err)
		}
	}
	return nil
}

func (d *DuckDB) Record(ctx context.Context, r lineage.Run) error {
	if err := r.Validate(); err != nil {
		return err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	existing, err := d.get(ctx, d.db, r.RunID)
	if err == nil {
		if runsEqual(existing, r) {
			return nil // idempotent re-record of identical content
		}
		return ErrConflict
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed

	var endedAt any
	if !r.EndedAt.IsZero() {
		endedAt = r.EndedAt.UnixNano()
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO runs (
			run_id, project, git_commit, git_dirty, config_hash, dataset_version,
			model_version, seed, fingerprint, host, device, framework_version,
			status, started_at_ns, ended_at_ns, checkpoint_uri
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.RunID, r.Project, r.GitCommit, r.GitDirty, r.ConfigHash, r.DatasetVersion,
		r.ModelVersion, r.Seed, r.Fingerprint, r.Host, r.Device, r.FrameworkVersion,
		string(r.Status), r.StartedAt.UnixNano(), endedAt, r.CheckpointURI,
	)
	if err != nil {
		return fmt.Errorf("inserting run: %w", err)
	}

	for k, v := range r.Params {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO run_params (run_id, key, value) VALUES (?, ?, ?)`, r.RunID, k, v); err != nil {
			return fmt.Errorf("inserting param %q: %w", k, err)
		}
	}
	for k, v := range r.Metrics {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO run_metrics (run_id, key, value) VALUES (?, ?, ?)`, r.RunID, k, v); err != nil {
			return fmt.Errorf("inserting metric %q: %w", k, err)
		}
	}

	return tx.Commit()
}

// queryer is the subset of *sql.DB and *sql.Tx that reads need.
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func (d *DuckDB) Get(ctx context.Context, runID string) (lineage.Run, error) {
	return d.get(ctx, d.db, runID)
}

func (d *DuckDB) get(ctx context.Context, q queryer, runID string) (lineage.Run, error) {
	r, err := scanRun(q.QueryRowContext(ctx, `
		SELECT run_id, project, git_commit, git_dirty, config_hash, dataset_version,
			model_version, seed, fingerprint, host, device, framework_version,
			status, started_at_ns, ended_at_ns, checkpoint_uri
		FROM runs WHERE run_id = ?`, runID))
	if err != nil {
		return lineage.Run{}, err
	}
	if r.Params, err = loadKV(ctx, q, "run_params", runID); err != nil {
		return lineage.Run{}, err
	}
	metricsRaw, err := loadKVFloat(ctx, q, runID)
	if err != nil {
		return lineage.Run{}, err
	}
	r.Metrics = metricsRaw
	return r, nil
}

func scanRun(row *sql.Row) (lineage.Run, error) {
	var r lineage.Run
	var status string
	var startedAtNS int64
	var endedAtNS sql.NullInt64
	err := row.Scan(
		&r.RunID, &r.Project, &r.GitCommit, &r.GitDirty, &r.ConfigHash, &r.DatasetVersion,
		&r.ModelVersion, &r.Seed, &r.Fingerprint, &r.Host, &r.Device, &r.FrameworkVersion,
		&status, &startedAtNS, &endedAtNS, &r.CheckpointURI,
	)
	if err == sql.ErrNoRows {
		return lineage.Run{}, ErrNotFound
	}
	if err != nil {
		return lineage.Run{}, err
	}
	r.Status = lineage.Status(status)
	r.StartedAt = nsToTime(startedAtNS)
	if endedAtNS.Valid {
		r.EndedAt = nsToTime(endedAtNS.Int64)
	}
	return r, nil
}

func loadKV(ctx context.Context, q queryer, table, runID string) (map[string]string, error) {
	rows, err := q.QueryContext(ctx, fmt.Sprintf(`SELECT key, value FROM %s WHERE run_id = ?`, table), runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out map[string]string
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		if out == nil {
			out = map[string]string{}
		}
		out[k] = v
	}
	return out, rows.Err()
}

func loadKVFloat(ctx context.Context, q queryer, runID string) (map[string]float64, error) {
	rows, err := q.QueryContext(ctx, `SELECT key, value FROM run_metrics WHERE run_id = ?`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out map[string]float64
	for rows.Next() {
		var k string
		var v float64
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		if out == nil {
			out = map[string]float64{}
		}
		out[k] = v
	}
	return out, rows.Err()
}

func (d *DuckDB) List(ctx context.Context, query Query) ([]lineage.Run, error) {
	where := []string{}
	args := []any{}
	add := func(col, val string) {
		if val != "" {
			where = append(where, col+" = ?")
			args = append(args, val)
		}
	}
	add("project", query.Project)
	add("git_commit", query.GitCommit)
	add("fingerprint", query.Fingerprint)
	add("status", string(query.Status))
	add("device", query.Device)

	sqlStr := `
		SELECT run_id, project, git_commit, git_dirty, config_hash, dataset_version,
			model_version, seed, fingerprint, host, device, framework_version,
			status, started_at_ns, ended_at_ns, checkpoint_uri
		FROM runs`
	if len(where) > 0 {
		sqlStr += " WHERE " + strings.Join(where, " AND ")
	}
	// Newest first, run id as the tiebreak: the same total order Memory
	// uses, so ordering does not depend on which backend answered.
	sqlStr += " ORDER BY started_at_ns DESC, run_id ASC"
	if query.Limit > 0 {
		sqlStr += fmt.Sprintf(" LIMIT %d", query.Limit)
	}

	rows, err := d.db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, err
	}
	var runs []lineage.Run
	runIDs := []string{}
	for rows.Next() {
		var r lineage.Run
		var status string
		var startedAtNS int64
		var endedAtNS sql.NullInt64
		if err := rows.Scan(
			&r.RunID, &r.Project, &r.GitCommit, &r.GitDirty, &r.ConfigHash, &r.DatasetVersion,
			&r.ModelVersion, &r.Seed, &r.Fingerprint, &r.Host, &r.Device, &r.FrameworkVersion,
			&status, &startedAtNS, &endedAtNS, &r.CheckpointURI,
		); err != nil {
			rows.Close()
			return nil, err
		}
		r.Status = lineage.Status(status)
		r.StartedAt = nsToTime(startedAtNS)
		if endedAtNS.Valid {
			r.EndedAt = nsToTime(endedAtNS.Int64)
		}
		runs = append(runs, r)
		runIDs = append(runIDs, r.RunID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	// Hydrate params/metrics for the page in two batched queries rather than
	// one round trip per run -- List answers "every run of this project",
	// which is exactly the case with the most rows to hydrate.
	if err := hydrate(ctx, d.db, runs, runIDs); err != nil {
		return nil, err
	}
	return runs, nil
}

func hydrate(ctx context.Context, q queryer, runs []lineage.Run, runIDs []string) error {
	if len(runIDs) == 0 {
		return nil
	}
	byID := make(map[string]int, len(runs))
	for i, r := range runs {
		byID[r.RunID] = i
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(runIDs)), ",")
	args := make([]any, len(runIDs))
	for i, id := range runIDs {
		args[i] = id
	}

	prows, err := q.QueryContext(ctx, fmt.Sprintf(
		`SELECT run_id, key, value FROM run_params WHERE run_id IN (%s)`, placeholders), args...)
	if err != nil {
		return err
	}
	for prows.Next() {
		var id, k, v string
		if err := prows.Scan(&id, &k, &v); err != nil {
			prows.Close()
			return err
		}
		i := byID[id]
		if runs[i].Params == nil {
			runs[i].Params = map[string]string{}
		}
		runs[i].Params[k] = v
	}
	if err := prows.Err(); err != nil {
		prows.Close()
		return err
	}
	prows.Close()

	mrows, err := q.QueryContext(ctx, fmt.Sprintf(
		`SELECT run_id, key, value FROM run_metrics WHERE run_id IN (%s)`, placeholders), args...)
	if err != nil {
		return err
	}
	for mrows.Next() {
		var id, k string
		var v float64
		if err := mrows.Scan(&id, &k, &v); err != nil {
			mrows.Close()
			return err
		}
		i := byID[id]
		if runs[i].Metrics == nil {
			runs[i].Metrics = map[string]float64{}
		}
		runs[i].Metrics[k] = v
	}
	if err := mrows.Err(); err != nil {
		mrows.Close()
		return err
	}
	mrows.Close()
	return nil
}

func (d *DuckDB) Close() error {
	return d.db.Close()
}

// runsEqual reports whether two runs have identical content: the same
// notion of "identical" Memory expresses with reflect.DeepEqual on the whole
// struct, but with time.Equal for the timestamps instead of ==. A value read
// back out of SQL never carries the monotonic reading time.Now() attaches to
// its in-memory counterpart, so comparing time.Time fields with == or
// reflect.DeepEqual would report every re-record of identical content as a
// conflict.
func runsEqual(a, b lineage.Run) bool {
	if !a.StartedAt.Equal(b.StartedAt) || !a.EndedAt.Equal(b.EndedAt) {
		return false
	}
	a.StartedAt, b.StartedAt = time.Time{}, time.Time{}
	a.EndedAt, b.EndedAt = time.Time{}, time.Time{}
	return reflect.DeepEqual(a, b)
}

// nsToTime is the inverse of r.StartedAt.UnixNano() / r.EndedAt.UnixNano():
// it reconstructs the same instant Record stored, in UTC. time.Equal ignores
// location and any monotonic reading, so UTC vs. the original's local zone
// never affects runsEqual or any caller that compares instants correctly.
func nsToTime(ns int64) time.Time {
	return time.Unix(0, ns).UTC()
}
