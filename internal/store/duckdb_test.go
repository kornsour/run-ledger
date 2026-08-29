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
	if !got.StartedAt.Equal(r.StartedAt) {
		t.Fatalf("StartedAt did not survive a close/reopen cycle: got %v, want %v", got.StartedAt, r.StartedAt)
	}
	if got.FingerprintVersion != lineage.CurrentFingerprintVersion {
		t.Fatalf("FingerprintVersion did not survive a close/reopen cycle: got %d, want %d", got.FingerprintVersion, lineage.CurrentFingerprintVersion)
	}
}

// TestDuckDBLegacyRowsDefaultToFingerprintVersion1 exercises the migration
// this change ships (ADR 0012): fingerprint_version is added by an ALTER
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
