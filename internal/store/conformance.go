package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
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

	t.Run("concurrent identical Record calls are idempotent", func(t *testing.T) {
		s := newStore(t)
		r := mk("a", "p", time.Now())
		const n = 64
		var wg sync.WaitGroup
		errs := make([]error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				// Every goroutine records the exact same value, so a correct
				// backend must accept all of them as the same content.
				errs[i] = s.Record(ctx, r)
			}(i)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("goroutine %d: re-recording identical content concurrently should succeed, got %v", i, err)
			}
		}
		got, err := s.List(ctx, Query{Project: "p"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("want exactly one row after %d concurrent identical writes, got %d: %+v", n, len(got), got)
		}
	})

	t.Run("concurrent conflicting Record calls yield exactly one winner", func(t *testing.T) {
		s := newStore(t)
		const n = 64
		var wg sync.WaitGroup
		errs := make([]error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				r := mk("a", "p", time.Now())
				// Every goroutine records different content under the same
				// run id, so at most one write may win.
				r.Seed = int64(i)
				r.Fingerprint = r.Compute()
				errs[i] = s.Record(ctx, r)
			}(i)
		}
		wg.Wait()

		var successes, conflicts int
		for i, err := range errs {
			switch {
			case err == nil:
				successes++
			case errors.Is(err, ErrConflict):
				conflicts++
			default:
				t.Fatalf("goroutine %d: want nil or ErrConflict, got %v", i, err)
			}
		}
		if successes != 1 {
			t.Fatalf("want exactly one goroutine to win, got %d successes and %d conflicts", successes, conflicts)
		}
		if conflicts != n-1 {
			t.Fatalf("want %d conflicts, got %d", n-1, conflicts)
		}
		if _, err := s.Get(ctx, "a"); err != nil {
			t.Fatalf("the winning write should be readable, got %v", err)
		}
		got, err := s.List(ctx, Query{Project: "p"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("want exactly one row after %d concurrent conflicting writes, got %d: %+v", n, len(got), got)
		}
	})

	t.Run("List never observes a partially-written run during concurrent Record", func(t *testing.T) {
		s := newStore(t)
		const n = 64
		stop := make(chan struct{})
		violations := make(chan error, n)

		var readers sync.WaitGroup
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				got, err := s.List(ctx, Query{Project: "p"})
				if err != nil {
					violations <- fmt.Errorf("List returned an error during concurrent writes: %w", err)
					return
				}
				for _, r := range got {
					// A torn write would show up here as a run missing the
					// fields Record is required to have set together, or as
					// a run whose provenance doesn't match its identity.
					if err := r.Validate(); err != nil {
						violations <- fmt.Errorf("List observed an incomplete run %q: %v", r.RunID, err)
						return
					}
					if r.GitCommit != "c"+r.RunID || r.ConfigHash != "cfg" || r.Fingerprint == "" {
						violations <- fmt.Errorf("List observed a run %q with mismatched fields: %+v", r.RunID, r)
						return
					}
				}
			}
		}()

		var writers sync.WaitGroup
		for i := 0; i < n; i++ {
			writers.Add(1)
			go func(i int) {
				defer writers.Done()
				id := fmt.Sprintf("run-%d", i)
				if err := s.Record(ctx, mk(id, "p", time.Now())); err != nil {
					violations <- fmt.Errorf("record %q: %w", id, err)
				}
			}(i)
		}
		writers.Wait()
		close(stop)
		readers.Wait()
		close(violations)

		for err := range violations {
			t.Fatal(err)
		}

		got, err := s.List(ctx, Query{Project: "p"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != n {
			t.Fatalf("want %d runs after concurrent writes, got %d", n, len(got))
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
