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
	// ADR 0013: Compute now normalizes numeric param spellings before
	// hashing, which changes what Fingerprint means for any run recorded
	// from here on. Every row that already exists at the moment this
	// migration runs was fingerprinted under the old, unnormalized contract
	// -- ADR 0004 is explicit that such a change must never silently
	// reinterpret what's already stored, so this only adds a column, never
	// touches the fingerprint column itself. DEFAULT 1 backfills every
	// pre-existing row to lineage.FingerprintVersionLegacy in the same
	// statement that adds the column, so there is no window where a
	// pre-migration row reads back with an undefined version. Every row
	// inserted after this migration gets an explicit value from the caller
	// (api.record stamps lineage.CurrentFingerprintVersion), so the default
	// only ever fires for rows that predate the column.
	//
	// Not declared NOT NULL: this DuckDB version rejects ADD COLUMN with an
	// inline column constraint ("Adding columns with constraints not yet
	// supported"). DEFAULT alone still backfills every existing row to a
	// real value, and Record always supplies an explicit
	// FingerprintVersion for every future insert, so a NULL in this column
	// should never occur in practice -- the constraint would only catch a
	// bug that got this far already writing bad data, not prevent one.
	`ALTER TABLE runs ADD COLUMN fingerprint_version INTEGER DEFAULT 1`,
	// ADR 0015: submitter_claim and job_id record who (self-asserted) and
	// what launching job recorded a run. Both are provenance -- neither
	// feeds Fingerprint -- so, unlike fingerprint_version above, a
	// pre-existing row needs no reinterpretation: it simply never had this
	// information captured, and DEFAULT '' backfills it to exactly that
	// claim ("" means "not recorded", per ADR 0011's rule, extended to
	// these two fields by ADR 0015). Two separate ALTER TABLE statements,
	// not one, because each migration entry in this slice is one statement
	// applied atomically -- see migrate's per-entry transaction below.
	`ALTER TABLE runs ADD COLUMN submitter_claim VARCHAR DEFAULT ''`,
	`ALTER TABLE runs ADD COLUMN job_id VARCHAR DEFAULT ''`,
	// ADR 0016: a capture declaration records what a client attempted to
	// capture, not what it captured -- a fact about the recording process,
	// never hashed, kept alongside the run the same way submitter_claim and
	// job_id are. Two columns, not one: capture_declared is what lets a
	// pre-migration row (or any run whose client never sends the field)
	// read back with lineage.Run.Capture == nil rather than a declaration
	// that merely happens to be empty -- those are different claims (see
	// lineage.Run.Capture's doc comment), and completeness.py's
	// peer-comparison fallback depends on telling them apart. DEFAULT
	// FALSE / DEFAULT '' backfills every pre-existing row to exactly that
	// "no declaration" state; NOT NULL is omitted for the same reason it is
	// on every other ALTER TABLE ADD COLUMN in this slice -- this DuckDB
	// build rejects an inline constraint on a new column.
	`ALTER TABLE runs ADD COLUMN capture_declared BOOLEAN DEFAULT FALSE`,
	`ALTER TABLE runs ADD COLUMN capture_client VARCHAR DEFAULT ''`,
	// Attempted is a variable-length list of field names, not a scalar, so
	// it gets a side table the same way Params and Metrics do -- a row per
	// (run_id, field) rather than a JSON blob in a column, so a future
	// "which runs attempted device" query is a real predicate. No value
	// column: membership is the only fact this table records, the same
	// shape a set takes when SQL has no native set type.
	`CREATE TABLE IF NOT EXISTS run_capture_attempted (
		run_id VARCHAR NOT NULL REFERENCES runs(run_id),
		field  VARCHAR NOT NULL,
		PRIMARY KEY (run_id, field)
	)`,
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
	// See lineage.Run.NormalizeCapture's doc comment: this backend in
	// particular reads Capture.Attempted back from a side table with no
	// row order of its own (see get(), below), so the incoming value must
	// agree with that canonical order before the idempotency comparison a
	// few lines down, or a byte-identical retry could be refused as a
	// conflict purely because of ordering.
	r.NormalizeCapture()

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
	if r.EndedAt != nil {
		endedAt = r.EndedAt.UnixNano()
	}
	captureDeclared, captureClient := captureColumns(r.Capture)
	_, err = tx.ExecContext(ctx, `
		INSERT INTO runs (
			run_id, project, git_commit, git_dirty, config_hash, dataset_version,
			model_version, seed, fingerprint, fingerprint_version, host, device,
			framework_version, submitter_claim, job_id, capture_declared,
			capture_client, status, started_at_ns, ended_at_ns, checkpoint_uri
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.RunID, r.Project, r.GitCommit, r.GitDirty, r.ConfigHash, r.DatasetVersion,
		r.ModelVersion, r.Seed, r.Fingerprint, r.FingerprintVersion, r.Host, r.Device,
		r.FrameworkVersion, r.SubmitterClaim, r.JobID, captureDeclared, captureClient,
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
	if r.Capture != nil {
		for _, field := range r.Capture.Attempted {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO run_capture_attempted (run_id, field) VALUES (?, ?)`, r.RunID, field); err != nil {
				return fmt.Errorf("inserting capture.attempted %q: %w", field, err)
			}
		}
	}

	return tx.Commit()
}

// captureColumns maps a (possibly nil) *lineage.CaptureDeclaration to the
// two scalar columns runs stores it in. capture_declared is what carries
// "no declaration at all" (nil) as a state distinct from "declared, with an
// empty client name" (non-nil, Client == "") -- collapsing the two the way
// ADR 0011 collapses "" and absent for the scalar fields would erase the
// exact distinction ADR 0016 exists to make available.
func captureColumns(c *lineage.CaptureDeclaration) (declared bool, client string) {
	if c == nil {
		return false, ""
	}
	return true, c.Client
}

// Update applies a partial, provenance-only change to an already-recorded
// run. Like Record, it serializes behind d.mu and reads-then-writes inside
// one critical section, since a read-modify-write across two unguarded
// statements is exactly the race the mutex exists to rule out.
func (d *DuckDB) Update(ctx context.Context, runID string, p Patch) (lineage.Run, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	existing, err := d.get(ctx, d.db, runID)
	if err != nil {
		return lineage.Run{}, err
	}
	updated, err := applyPatch(existing, p)
	if err != nil {
		return lineage.Run{}, err
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return lineage.Run{}, err
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed

	var endedAt any
	if updated.EndedAt != nil {
		endedAt = updated.EndedAt.UnixNano()
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE runs SET status = ?, ended_at_ns = ?, checkpoint_uri = ?,
			host = ?, device = ?, framework_version = ?, submitter_claim = ?, job_id = ?
		WHERE run_id = ?`,
		string(updated.Status), endedAt, updated.CheckpointURI,
		updated.Host, updated.Device, updated.FrameworkVersion,
		updated.SubmitterClaim, updated.JobID, runID,
	); err != nil {
		return lineage.Run{}, fmt.Errorf("updating run: %w", err)
	}

	// Only the keys the patch actually touched -- merging happened in
	// applyPatch against the Go value, so this upserts p.Metrics (the
	// delta), not updated.Metrics (the full merged map).
	for k, v := range p.Metrics {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO run_metrics (run_id, key, value) VALUES (?, ?, ?)
			ON CONFLICT (run_id, key) DO UPDATE SET value = excluded.value`,
			runID, k, v,
		); err != nil {
			return lineage.Run{}, fmt.Errorf("upserting metric %q: %w", k, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return lineage.Run{}, err
	}
	return updated, nil
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
	r, captureDeclared, captureClient, err := scanRun(q.QueryRowContext(ctx, `
		SELECT run_id, project, git_commit, git_dirty, config_hash, dataset_version,
			model_version, seed, fingerprint, fingerprint_version, host, device,
			framework_version, submitter_claim, job_id, capture_declared,
			capture_client, status, started_at_ns, ended_at_ns, checkpoint_uri
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
	if captureDeclared {
		attempted, err := loadCaptureAttempted(ctx, q, runID)
		if err != nil {
			return lineage.Run{}, err
		}
		r.Capture = &lineage.CaptureDeclaration{Client: captureClient, Attempted: attempted}
	}
	return r, nil
}

func scanRun(row *sql.Row) (r lineage.Run, captureDeclared bool, captureClient string, err error) {
	var status string
	var startedAtNS int64
	var endedAtNS sql.NullInt64
	err = row.Scan(
		&r.RunID, &r.Project, &r.GitCommit, &r.GitDirty, &r.ConfigHash, &r.DatasetVersion,
		&r.ModelVersion, &r.Seed, &r.Fingerprint, &r.FingerprintVersion, &r.Host, &r.Device,
		&r.FrameworkVersion, &r.SubmitterClaim, &r.JobID, &captureDeclared, &captureClient,
		&status, &startedAtNS, &endedAtNS, &r.CheckpointURI,
	)
	if err == sql.ErrNoRows {
		return lineage.Run{}, false, "", ErrNotFound
	}
	if err != nil {
		return lineage.Run{}, false, "", err
	}
	r.Status = lineage.Status(status)
	r.StartedAt = nsToTime(startedAtNS)
	if endedAtNS.Valid {
		endedAt := nsToTime(endedAtNS.Int64)
		r.EndedAt = &endedAt
	}
	return r, captureDeclared, captureClient, nil
}

// loadCaptureAttempted returns runID's capture.attempted list in a
// canonical (lexical) order -- ORDER BY, not left to whatever order the
// side table happens to scan in, for exactly the reason NormalizeCapture's
// doc comment gives: this value is compared against a freshly-decoded,
// NormalizeCapture-sorted Run during Record's idempotency check, and the
// two orders must agree or a legitimate retry could misread as a conflict.
func loadCaptureAttempted(ctx context.Context, q queryer, runID string) ([]string, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT field FROM run_capture_attempted WHERE run_id = ? ORDER BY field`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var field string
		if err := rows.Scan(&field); err != nil {
			return nil, err
		}
		out = append(out, field)
	}
	return out, rows.Err()
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

func (d *DuckDB) List(ctx context.Context, query Query) (Page, error) {
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
	add("submitter_claim", query.SubmitterClaim)
	add("job_id", query.JobID)
	if query.CaptureClient != "" {
		// Not add(): a run with no declaration at all stores
		// capture_client = '' (the same default a legacy row backfills to),
		// so a bare "capture_client = ?" predicate already excludes it
		// correctly without needing a separate capture_declared check --
		// an undeclared run can never equal a non-empty filter value.
		where = append(where, "capture_client = ?")
		args = append(args, query.CaptureClient)
	}
	if !query.Since.IsZero() {
		where = append(where, "started_at_ns >= ?")
		args = append(args, query.Since.UnixNano())
	}
	if !query.Until.IsZero() {
		where = append(where, "started_at_ns < ?")
		args = append(args, query.Until.UnixNano())
	}
	if query.After != nil {
		// Keyset predicate for "strictly after this row in (started_at_ns
		// DESC, run_id ASC) order": either an earlier started_at, or the same
		// started_at with a run_id that sorts later.
		where = append(where, "(started_at_ns < ? OR (started_at_ns = ? AND run_id > ?))")
		args = append(args, query.After.StartedAt.UnixNano(), query.After.StartedAt.UnixNano(), query.After.RunID)
	}

	sqlStr := `
		SELECT run_id, project, git_commit, git_dirty, config_hash, dataset_version,
			model_version, seed, fingerprint, fingerprint_version, host, device,
			framework_version, submitter_claim, job_id, capture_declared,
			capture_client, status, started_at_ns, ended_at_ns, checkpoint_uri
		FROM runs`
	if len(where) > 0 {
		sqlStr += " WHERE " + strings.Join(where, " AND ")
	}
	// Newest first, run id as the tiebreak: the same total order Memory
	// uses, so ordering does not depend on which backend answered.
	sqlStr += " ORDER BY started_at_ns DESC, run_id ASC"
	if query.Limit > 0 {
		// Ask for one extra row so we can tell "exactly Limit rows exist"
		// apart from "more rows follow" without a second round trip -- that
		// extra row, if present, is trimmed below and becomes the cursor for
		// the next page instead of being returned.
		sqlStr += fmt.Sprintf(" LIMIT %d", query.Limit+1)
	}

	rows, err := d.db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return Page{}, err
	}
	var runs []lineage.Run
	runIDs := []string{}
	for rows.Next() {
		var r lineage.Run
		var status string
		var startedAtNS int64
		var endedAtNS sql.NullInt64
		var captureDeclared bool
		var captureClient string
		if err := rows.Scan(
			&r.RunID, &r.Project, &r.GitCommit, &r.GitDirty, &r.ConfigHash, &r.DatasetVersion,
			&r.ModelVersion, &r.Seed, &r.Fingerprint, &r.FingerprintVersion, &r.Host, &r.Device,
			&r.FrameworkVersion, &r.SubmitterClaim, &r.JobID, &captureDeclared, &captureClient,
			&status, &startedAtNS, &endedAtNS, &r.CheckpointURI,
		); err != nil {
			rows.Close()
			return Page{}, err
		}
		r.Status = lineage.Status(status)
		r.StartedAt = nsToTime(startedAtNS)
		if endedAtNS.Valid {
			endedAt := nsToTime(endedAtNS.Int64)
			r.EndedAt = &endedAt
		}
		if captureDeclared {
			// Attempted is filled in below by hydrateCaptureAttempted, in one
			// batched query for the whole page rather than one per run --
			// the same reasoning hydrate (params/metrics) already follows.
			r.Capture = &lineage.CaptureDeclaration{Client: captureClient}
		}
		runs = append(runs, r)
		runIDs = append(runIDs, r.RunID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Page{}, err
	}
	rows.Close()

	var next *Cursor
	if query.Limit > 0 && len(runs) > query.Limit {
		runs = runs[:query.Limit]
		runIDs = runIDs[:query.Limit]
		last := runs[len(runs)-1]
		next = &Cursor{StartedAt: last.StartedAt, RunID: last.RunID}
	}

	// Hydrate params/metrics/capture.attempted for the page in batched
	// queries rather than one round trip per run -- List answers "every run
	// of this project", which is exactly the case with the most rows to
	// hydrate.
	if err := hydrate(ctx, d.db, runs, runIDs); err != nil {
		return Page{}, err
	}
	if err := hydrateCaptureAttempted(ctx, d.db, runs, runIDs); err != nil {
		return Page{}, err
	}
	return Page{Runs: runs, Next: next}, nil
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

// hydrateCaptureAttempted fills in Capture.Attempted for every run in the
// page that has a capture declaration (Capture != nil, stamped by List's
// row scan from capture_declared), the same batched-query shape hydrate
// uses for params and metrics. ORDER BY field, for the reason
// loadCaptureAttempted's doc comment gives: this must agree with
// NormalizeCapture's canonical order.
func hydrateCaptureAttempted(ctx context.Context, q queryer, runs []lineage.Run, runIDs []string) error {
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

	rows, err := q.QueryContext(ctx, fmt.Sprintf(
		`SELECT run_id, field FROM run_capture_attempted WHERE run_id IN (%s) ORDER BY field`, placeholders), args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, field string
		if err := rows.Scan(&id, &field); err != nil {
			return err
		}
		i := byID[id]
		if runs[i].Capture == nil {
			// Defensive only: Record never writes a run_capture_attempted
			// row without also setting capture_declared, so this should be
			// unreachable against data this backend itself wrote.
			continue
		}
		runs[i].Capture.Attempted = append(runs[i].Capture.Attempted, field)
	}
	return rows.Err()
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
	if !a.StartedAt.Equal(b.StartedAt) || !endedAtEqual(a.EndedAt, b.EndedAt) {
		return false
	}
	a.StartedAt, b.StartedAt = time.Time{}, time.Time{}
	a.EndedAt, b.EndedAt = nil, nil
	return reflect.DeepEqual(a, b)
}

// endedAtEqual compares two *time.Time the way runsEqual needs: both nil is
// equal, one nil and the other not is never equal, and two non-nil pointers
// compare by instant (time.Equal), not by address.
func endedAtEqual(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// nsToTime is the inverse of r.StartedAt.UnixNano() / r.EndedAt.UnixNano():
// it reconstructs the same instant Record stored, in UTC. time.Equal ignores
// location and any monotonic reading, so UTC vs. the original's local zone
// never affects runsEqual or any caller that compares instants correctly.
func nsToTime(ns int64) time.Time {
	return time.Unix(0, ns).UTC()
}
