package store

import (
	"context"
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
