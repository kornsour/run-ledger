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
			page, err := s.List(ctx, Query{Project: "p"})
			if err != nil {
				t.Fatal(err)
			}
			got := page.Runs
			if len(got) != len(want) {
				t.Fatalf("want %d runs, got %d", len(want), len(got))
			}
			for j := range want {
				if got[j].RunID != want[j] {
					t.Fatalf("iteration %d: want %v, got %s at %d", i, want, got[j].RunID, j)
				}
			}
			if page.Next != nil {
				t.Fatalf("an unbounded query must not report a next page, got %+v", page.Next)
			}
		}
	})

	t.Run("filters narrow the listing", func(t *testing.T) {
		s := newStore(t)
		now := time.Now()
		_ = s.Record(ctx, mk("a", "alpha", now))
		_ = s.Record(ctx, mk("b", "beta", now.Add(-time.Minute)))
		page, err := s.List(ctx, Query{Project: "alpha"})
		if err != nil {
			t.Fatal(err)
		}
		got := page.Runs
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
		page, err := s.List(ctx, Query{Project: "p"})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Runs) != 1 {
			t.Fatalf("want exactly one row after %d concurrent identical writes, got %d: %+v", n, len(page.Runs), page.Runs)
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
		page, err := s.List(ctx, Query{Project: "p"})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Runs) != 1 {
			t.Fatalf("want exactly one row after %d concurrent conflicting writes, got %d: %+v", n, len(page.Runs), page.Runs)
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
				page, err := s.List(ctx, Query{Project: "p"})
				if err != nil {
					violations <- fmt.Errorf("List returned an error during concurrent writes: %w", err)
					return
				}
				for _, r := range page.Runs {
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

		page, err := s.List(ctx, Query{Project: "p"})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Runs) != n {
			t.Fatalf("want %d runs after concurrent writes, got %d", n, len(page.Runs))
		}
	})

	t.Run("limit truncates after ordering, and reports a next cursor", func(t *testing.T) {
		s := newStore(t)
		now := time.Now()
		_ = s.Record(ctx, mk("old", "p", now.Add(-time.Hour)))
		_ = s.Record(ctx, mk("new", "p", now))
		page, err := s.List(ctx, Query{Project: "p", Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Runs) != 1 || page.Runs[0].RunID != "new" {
			t.Fatalf("limit must keep the newest, got %+v", page.Runs)
		}
		if page.Next == nil {
			t.Fatal("a truncated page must report a next cursor")
		}
		next, err := s.List(ctx, Query{Project: "p", Limit: 1, After: page.Next})
		if err != nil {
			t.Fatal(err)
		}
		if len(next.Runs) != 1 || next.Runs[0].RunID != "old" {
			t.Fatalf("the next page must pick up where the cursor left off, got %+v", next.Runs)
		}
		if next.Next != nil {
			t.Fatalf("the last page must not report a further cursor, got %+v", next.Next)
		}
	})

	t.Run("a page exactly the size of the result set reports no next cursor", func(t *testing.T) {
		s := newStore(t)
		now := time.Now()
		_ = s.Record(ctx, mk("old", "p", now.Add(-time.Hour)))
		_ = s.Record(ctx, mk("new", "p", now))
		page, err := s.List(ctx, Query{Project: "p", Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Runs) != 2 {
			t.Fatalf("want both runs, got %+v", page.Runs)
		}
		if page.Next != nil {
			t.Fatalf("a page that exhausts the result set must not report a next cursor, got %+v", page.Next)
		}
	})

	t.Run("keyset pagination visits every pre-existing row exactly once under concurrent inserts", func(t *testing.T) {
		s := newStore(t)
		now := time.Now()
		const preExisting = 40
		const pageSize = 7
		want := map[string]bool{}
		for i := 0; i < preExisting; i++ {
			id := fmt.Sprintf("pre-%03d", i)
			// Spread StartedAt out so ties (same instant, different run id) get
			// exercised too, not just the common case of distinct timestamps.
			at := now.Add(-time.Duration(i/2) * time.Second)
			if err := s.Record(ctx, mk(id, "p", at)); err != nil {
				t.Fatal(err)
			}
			want[id] = true
		}

		// A concurrent writer keeps inserting new, newer-than-anything-so-far
		// rows for the whole traversal. A traversal that used LIMIT/OFFSET
		// would have every page after the first shift by however many of
		// these landed ahead of it, and would skip or repeat a pre-existing
		// row; keyset pagination must not.
		stopInserts := make(chan struct{})
		var inserters sync.WaitGroup
		inserters.Add(1)
		go func() {
			defer inserters.Done()
			i := 0
			for {
				select {
				case <-stopInserts:
					return
				default:
				}
				id := fmt.Sprintf("concurrent-%04d", i)
				_ = s.Record(ctx, mk(id, "p", time.Now().Add(time.Duration(i)*time.Millisecond)))
				i++
			}
		}()

		seen := map[string]int{}
		var cursor *Cursor
		for {
			page, err := s.List(ctx, Query{Project: "p", Limit: pageSize, After: cursor})
			if err != nil {
				close(stopInserts)
				inserters.Wait()
				t.Fatal(err)
			}
			for _, r := range page.Runs {
				seen[r.RunID]++
			}
			if page.Next == nil {
				break
			}
			cursor = page.Next
		}
		close(stopInserts)
		inserters.Wait()

		for id := range want {
			if seen[id] != 1 {
				t.Fatalf("pre-existing row %q was visited %d times, want exactly 1", id, seen[id])
			}
		}
		for id, n := range seen {
			if n > 1 {
				t.Fatalf("row %q was visited %d times, want at most 1", id, n)
			}
		}
	})
}
