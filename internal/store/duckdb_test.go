package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/kornsour/run-ledger/internal/lineage"
)

func TestDuckDBConformance(t *testing.T) {
	RunConformance(t, func(t *testing.T) Store {
		// Each subtest gets its own on-disk database, not the default
		// in-memory DSN: an empty DSN is a private instance per *sql.DB
		// connection, which would defeat the "one shared instance" premise
		// Record's mutex relies on the moment the pool opens more than one
		// physical connection.
		dsn := filepath.Join(t.TempDir(), "runs.duckdb")
		s, err := NewDuckDB(dsn)
		if err != nil {
			t.Fatalf("NewDuckDB: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}

func TestDuckDBPersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "runs.duckdb")

	s1, err := NewDuckDB(dsn)
	if err != nil {
		t.Fatalf("NewDuckDB: %v", err)
	}
	r := lineage.Run{
		Project: "p", GitCommit: "c1", ConfigHash: "cfg",
		RunID: "a", Status: lineage.StatusSucceeded, StartedAt: time.Now(), Device: "cpu",
		SubmitterClaim: "alice", JobID: "ci-1",
		Params: map[string]string{"lr": "3e-4"}, Metrics: map[string]float64{"loss": 0.1},
	}
	r.Fingerprint = r.Compute()
	r.FingerprintVersion = lineage.CurrentFingerprintVersion
	if err := s1.Record(ctx, r); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := NewDuckDB(dsn)
	if err != nil {
		t.Fatalf("reopening NewDuckDB: %v", err)
	}
	defer func() { _ = s2.Close() }()

	got, err := s2.Get(ctx, "a")
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if got.Project != "p" || got.Params["lr"] != "3e-4" || got.Metrics["loss"] != 0.1 {
		t.Fatalf("run did not survive a close/reopen cycle: %+v", got)
	}
	if got.SubmitterClaim != "alice" || got.JobID != "ci-1" {
		t.Fatalf("attribution did not survive a close/reopen cycle: %+v", got)
	}
	if !got.StartedAt.Equal(r.StartedAt) {
		t.Fatalf("StartedAt did not survive a close/reopen cycle: got %v, want %v", got.StartedAt, r.StartedAt)
	}
	if got.FingerprintVersion != lineage.CurrentFingerprintVersion {
		t.Fatalf("FingerprintVersion did not survive a close/reopen cycle: got %d, want %d", got.FingerprintVersion, lineage.CurrentFingerprintVersion)
	}
}

// TestDuckDBLegacyRowsDefaultToFingerprintVersion1 exercises the migration
// this change ships (ADR 0013): fingerprint_version is added by an ALTER
// TABLE ... DEFAULT, not baked into the original CREATE TABLE, precisely so
// that a row written before this migration existed reads back tagged
// FingerprintVersionLegacy -- never Compute run fresh against it, and never
// left at an undefined zero value.
//
// It simulates a pre-migration database the way the real one gets there: by
// creating the runs table from migrations[0] alone (the original schema,
// unedited -- see the comment on the migrations slice for why it is never
// edited after release) and inserting a row directly, bypassing NewDuckDB
// and its full migration chain entirely. Opening that database through
// NewDuckDB afterward is what a real upgrade does.
func TestDuckDBLegacyRowsDefaultToFingerprintVersion1(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "runs.duckdb")

	preMigration, err := sql.Open("duckdb", dsn)
	if err != nil {
		t.Fatalf("opening pre-migration duckdb: %v", err)
	}
	// migrations[0] is the original runs table, with no fingerprint_version
	// column at all -- exactly the shape a database predating this change
	// would have.
	if _, err := preMigration.ExecContext(ctx, migrations[0]); err != nil {
		t.Fatalf("creating pre-migration schema: %v", err)
	}
	const legacyFingerprint = "hashed-under-the-unnormalized-contract"
	_, err = preMigration.ExecContext(ctx, `
		INSERT INTO runs (
			run_id, project, git_commit, git_dirty, config_hash, dataset_version,
			model_version, seed, fingerprint, host, device, framework_version,
			status, started_at_ns, ended_at_ns, checkpoint_uri
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"legacy-run", "p", "c1", false, "cfg", "", "", 0,
		legacyFingerprint, "", "", "", string(lineage.StatusSucceeded), time.Now().UnixNano(), nil, "",
	)
	if err != nil {
		t.Fatalf("inserting pre-migration row: %v", err)
	}
	if err := preMigration.Close(); err != nil {
		t.Fatalf("closing pre-migration handle: %v", err)
	}

	// A real upgrade opens the existing database through NewDuckDB, which
	// runs every migration not yet applied -- here, only the new one, since
	// the table this DSN already has satisfies "IF NOT EXISTS" for the rest.
	s, err := NewDuckDB(dsn)
	if err != nil {
		t.Fatalf("NewDuckDB against a pre-migration database: %v", err)
	}
	defer func() { _ = s.Close() }()

	got, err := s.Get(ctx, "legacy-run")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.FingerprintVersion != lineage.FingerprintVersionLegacy {
		t.Fatalf("want a pre-migration row backfilled to FingerprintVersionLegacy (%d), got %d",
			lineage.FingerprintVersionLegacy, got.FingerprintVersion)
	}
	// The point of versioning the contract instead of just fixing Compute:
	// the stored fingerprint is never touched by the migration or by
	// reading it back, whatever Compute would produce for this content today.
	if got.Fingerprint != legacyFingerprint {
		t.Fatalf("a pre-existing fingerprint must not be reinterpreted by the migration: got %q, want %q",
			got.Fingerprint, legacyFingerprint)
	}
}

// TestDuckDBLegacyRowsDefaultAttributionToEmptyString exercises the ADR
// 0015 migration the way TestDuckDBLegacyRowsDefaultToFingerprintVersion1
// exercises ADR 0013's: simulate a database that predates submitter_claim
// and job_id (everything through the fingerprint_version migration, but no
// further), insert a row directly, then open it through NewDuckDB and
// confirm the new columns read back as "" -- which, per ADR 0011's rule as
// extended by ADR 0015, is exactly the correct claim ("not recorded") for a
// row that was written before this ledger could capture attribution at
// all. Unlike fingerprint_version, there is no separate legacy sentinel to
// backfill to: "" already means what it needs to mean.
func TestDuckDBLegacyRowsDefaultAttributionToEmptyString(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "runs.duckdb")

	preMigration, err := sql.Open("duckdb", dsn)
	if err != nil {
		t.Fatalf("opening pre-migration duckdb: %v", err)
	}
	// migrations[0] creates the runs table; migrations[5] is the
	// fingerprint_version column. Together they are the full schema that
	// existed just before submitter_claim/job_id were added. Unlike
	// TestDuckDBLegacyRowsDefaultToFingerprintVersion1 (which only needs
	// migrations[0] and lets NewDuckDB apply everything else fresh), this
	// simulation must also record 0-5 in schema_migrations itself: without
	// that bookkeeping, NewDuckDB's own migrate() would try to reapply
	// migrations[5]'s ALTER TABLE ADD COLUMN and fail on the column already
	// existing -- exactly what a real already-upgraded database would never
	// hit, since its schema_migrations already marks that migration done.
	if _, err := preMigration.ExecContext(ctx, `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("creating schema_migrations: %v", err)
	}
	for _, i := range []int{0, 5} {
		if _, err := preMigration.ExecContext(ctx, migrations[i]); err != nil {
			t.Fatalf("applying migrations[%d]: %v", i, err)
		}
		if _, err := preMigration.ExecContext(ctx, `INSERT INTO schema_migrations (version) VALUES (?)`, i); err != nil {
			t.Fatalf("recording migrations[%d] as applied: %v", i, err)
		}
	}
	_, err = preMigration.ExecContext(ctx, `
		INSERT INTO runs (
			run_id, project, git_commit, git_dirty, config_hash, dataset_version,
			model_version, seed, fingerprint, fingerprint_version, host, device,
			framework_version, status, started_at_ns, ended_at_ns, checkpoint_uri
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"legacy-run", "p", "c1", false, "cfg", "", "", 0,
		"fp", lineage.CurrentFingerprintVersion, "", "", "",
		string(lineage.StatusSucceeded), time.Now().UnixNano(), nil, "",
	)
	if err != nil {
		t.Fatalf("inserting pre-migration row: %v", err)
	}
	if err := preMigration.Close(); err != nil {
		t.Fatalf("closing pre-migration handle: %v", err)
	}

	s, err := NewDuckDB(dsn)
	if err != nil {
		t.Fatalf("NewDuckDB against a pre-migration database: %v", err)
	}
	defer func() { _ = s.Close() }()

	got, err := s.Get(ctx, "legacy-run")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.SubmitterClaim != "" || got.JobID != "" {
		t.Fatalf("want a pre-migration row backfilled to \"\" (not recorded) for both fields, got %+v", got)
	}
}

// TestDuckDBLegacyRowsHaveNoCaptureDeclaration exercises the ADR 0016
// migration the same way the two tests above exercise ADR 0013's and ADR
// 0015's: simulate a database that predates capture_declared/capture_client
// (everything through the job_id migration, migrations[7], but no
// further), insert a row directly, then open it through NewDuckDB and
// confirm the row reads back with Capture == nil -- not a declaration that
// merely happens to be empty. That distinction is the entire reason
// capture_declared is a separate column rather than inferring "no
// declaration" from capture_client == "": a pre-migration row and a
// modern client that declares an empty capture must not become
// indistinguishable, because examples/churn/completeness.py's
// peer-comparison fallback depends on telling them apart (see
// lineage.Run.Capture's doc comment and ADR 0016).
func TestDuckDBLegacyRowsHaveNoCaptureDeclaration(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "runs.duckdb")

	preMigration, err := sql.Open("duckdb", dsn)
	if err != nil {
		t.Fatalf("opening pre-migration duckdb: %v", err)
	}
	if _, err := preMigration.ExecContext(ctx, `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("creating schema_migrations: %v", err)
	}
	for _, i := range []int{0, 5, 6, 7} {
		if _, err := preMigration.ExecContext(ctx, migrations[i]); err != nil {
			t.Fatalf("applying migrations[%d]: %v", i, err)
		}
		if _, err := preMigration.ExecContext(ctx, `INSERT INTO schema_migrations (version) VALUES (?)`, i); err != nil {
			t.Fatalf("recording migrations[%d] as applied: %v", i, err)
		}
	}
	_, err = preMigration.ExecContext(ctx, `
		INSERT INTO runs (
			run_id, project, git_commit, git_dirty, config_hash, dataset_version,
			model_version, seed, fingerprint, fingerprint_version, host, device,
			framework_version, submitter_claim, job_id, status, started_at_ns,
			ended_at_ns, checkpoint_uri
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"legacy-run", "p", "c1", false, "cfg", "", "", 0,
		"fp", lineage.CurrentFingerprintVersion, "", "", "", "", "",
		string(lineage.StatusSucceeded), time.Now().UnixNano(), nil, "",
	)
	if err != nil {
		t.Fatalf("inserting pre-migration row: %v", err)
	}
	if err := preMigration.Close(); err != nil {
		t.Fatalf("closing pre-migration handle: %v", err)
	}

	s, err := NewDuckDB(dsn)
	if err != nil {
		t.Fatalf("NewDuckDB against a pre-migration database: %v", err)
	}
	defer func() { _ = s.Close() }()

	got, err := s.Get(ctx, "legacy-run")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Capture != nil {
		t.Fatalf("want a pre-migration row to read back with Capture == nil (no declaration, not an empty one), got %+v", got.Capture)
	}
}

func TestDuckDBMigrateIsIdempotent(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "runs.duckdb")
	s, err := NewDuckDB(dsn)
	if err != nil {
		t.Fatalf("NewDuckDB: %v", err)
	}
	defer func() { _ = s.Close() }()

	// Re-running migrate against an already-migrated database must not
	// error or duplicate schema objects.
	if err := s.migrate(context.Background()); err != nil {
		t.Fatalf("re-running migrate: %v", err)
	}
}
