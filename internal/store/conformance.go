package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kornsour/run-ledger/internal/lineage"
)

// RunConformance exercises the semantics every Store implementation must share.
//
// It lives in the non-test build so a future backend in another package can
// import and run it. A second implementation that quietly differs on ordering
// or idempotency is the failure this exists to prevent.
func RunConformance(t *testing.T, newStore func(t *testing.T) Store) {
	t.Helper()
	ctx := context.Background()
	mk := func(id, project string, at time.Time) lineage.Run {
		r := lineage.Run{
			Project: project, GitCommit: "c" + id, ConfigHash: "cfg",
			RunID: id, Status: lineage.StatusSucceeded, StartedAt: at, Device: "cpu",
		}
		r.Fingerprint = r.Compute()
		return r
	}

	t.Run("get on an unknown id is ErrNotFound", func(t *testing.T) {
		s := newStore(t)
		if _, err := s.Get(ctx, "nope"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})

	t.Run("recording identical content twice is idempotent", func(t *testing.T) {
		s := newStore(t)
		r := mk("a", "p", time.Now())
		if err := s.Record(ctx, r); err != nil {
			t.Fatal(err)
		}
		if err := s.Record(ctx, r); err != nil {
			t.Fatalf("re-recording identical content should succeed, got %v", err)
		}
	})

	t.Run("recording different content under one id is a conflict", func(t *testing.T) {
		s := newStore(t)
		r := mk("a", "p", time.Now())
		if err := s.Record(ctx, r); err != nil {
			t.Fatal(err)
		}
		r.Status = lineage.StatusFailed
		if err := s.Record(ctx, r); !errors.Is(err, ErrConflict) {
			t.Fatalf("want ErrConflict, got %v", err)
		}
	})

	t.Run("an invalid run is refused", func(t *testing.T) {
		s := newStore(t)
		if err := s.Record(ctx, lineage.Run{RunID: "x"}); err == nil {
			t.Fatal("a run with no project or commit was accepted")
		}
	})

	t.Run("listing is newest first and stable", func(t *testing.T) {
		s := newStore(t)
		now := time.Now()
		for _, r := range []lineage.Run{
			mk("old", "p", now.Add(-2*time.Hour)),
			mk("new", "p", now),
			mk("mid", "p", now.Add(-time.Hour)),
		} {
			if err := s.Record(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
		want := []string{"new", "mid", "old"}
		for i := 0; i < 20; i++ {
			got, err := s.List(ctx, Query{Project: "p"})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(want) {
				t.Fatalf("want %d runs, got %d", len(want), len(got))
			}
			for j := range want {
				if got[j].RunID != want[j] {
					t.Fatalf("iteration %d: want %v, got %s at %d", i, want, got[j].RunID, j)
				}
			}
		}
	})

	t.Run("filters narrow the listing", func(t *testing.T) {
		s := newStore(t)
		now := time.Now()
		_ = s.Record(ctx, mk("a", "alpha", now))
		_ = s.Record(ctx, mk("b", "beta", now.Add(-time.Minute)))
		got, err := s.List(ctx, Query{Project: "alpha"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].RunID != "a" {
			t.Fatalf("project filter did not narrow: %+v", got)
		}
	})

	t.Run("limit truncates after ordering", func(t *testing.T) {
		s := newStore(t)
		now := time.Now()
		_ = s.Record(ctx, mk("old", "p", now.Add(-time.Hour)))
		_ = s.Record(ctx, mk("new", "p", now))
		got, err := s.List(ctx, Query{Project: "p", Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].RunID != "new" {
			t.Fatalf("limit must keep the newest, got %+v", got)
		}
	})
}
