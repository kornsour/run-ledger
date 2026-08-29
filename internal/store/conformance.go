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
		// Fingerprint and FingerprintVersion are always stamped together --
		// Compute always implements CurrentFingerprintVersion, so a run
		// whose Fingerprint came from a fresh Compute() call must record
		// that version, the same pairing api.record does for a real request.
		r.Fingerprint = r.Compute()
		r.FingerprintVersion = lineage.CurrentFingerprintVersion
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

	t.Run("submitter_claim and job_id filter the listing", func(t *testing.T) {
		s := newStore(t)
		now := time.Now()
		alice := mk("a", "p", now)
		alice.SubmitterClaim, alice.JobID = "alice", "ci-1"
		alice.Fingerprint = alice.Compute()
		bob := mk("b", "p", now.Add(-time.Minute))
		bob.SubmitterClaim, bob.JobID = "bob", "ci-2"
		bob.Fingerprint = bob.Compute()
		if err := s.Record(ctx, alice); err != nil {
			t.Fatal(err)
		}
		if err := s.Record(ctx, bob); err != nil {
			t.Fatal(err)
		}

		page, err := s.List(ctx, Query{Project: "p", SubmitterClaim: "alice"})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Runs) != 1 || page.Runs[0].RunID != "a" {
			t.Fatalf("submitter_claim filter did not narrow: %+v", page.Runs)
		}

		page, err = s.List(ctx, Query{Project: "p", JobID: "ci-2"})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Runs) != 1 || page.Runs[0].RunID != "b" {
			t.Fatalf("job_id filter did not narrow: %+v", page.Runs)
		}
	})

	t.Run("a capture declaration round-trips through Record/Get, and a run with none stays nil", func(t *testing.T) {
		s := newStore(t)
		declared := mk("a", "p", time.Now())
		declared.Capture = &lineage.CaptureDeclaration{
			Client:    "runledger-py/0.1.0",
			Attempted: []string{"host", "device", "framework_version"},
		}
		declared.Fingerprint = declared.Compute()
		if err := s.Record(ctx, declared); err != nil {
			t.Fatal(err)
		}
		undeclared := mk("b", "p", time.Now())
		if err := s.Record(ctx, undeclared); err != nil {
			t.Fatal(err)
		}

		got, err := s.Get(ctx, "a")
		if err != nil {
			t.Fatal(err)
		}
		if got.Capture == nil || got.Capture.Client != "runledger-py/0.1.0" {
			t.Fatalf("capture declaration did not round-trip: %+v", got.Capture)
		}
		wantAttempted := []string{"device", "framework_version", "host"} // canonical order
		if len(got.Capture.Attempted) != len(wantAttempted) {
			t.Fatalf("want attempted %v, got %v", wantAttempted, got.Capture.Attempted)
		}
		for i := range wantAttempted {
			if got.Capture.Attempted[i] != wantAttempted[i] {
				t.Fatalf("want attempted %v, got %v", wantAttempted, got.Capture.Attempted)
			}
		}

		gotUndeclared, err := s.Get(ctx, "b")
		if err != nil {
			t.Fatal(err)
		}
		if gotUndeclared.Capture != nil {
			t.Fatalf("a run whose client sent no capture declaration must read back with Capture == nil, got %+v", gotUndeclared.Capture)
		}

		// The same distinction must survive List, not just Get -- List
		// hydrates capture.attempted through a separate batched query
		// (DuckDB), which is exactly the kind of thing that can silently
		// diverge from Get's own path if only one of the two is exercised.
		page, err := s.List(ctx, Query{Project: "p"})
		if err != nil {
			t.Fatal(err)
		}
		byID := map[string]lineage.Run{}
		for _, r := range page.Runs {
			byID[r.RunID] = r
		}
		if byID["a"].Capture == nil || len(byID["a"].Capture.Attempted) != 3 {
			t.Fatalf("List did not hydrate capture.attempted for %q: %+v", "a", byID["a"].Capture)
		}
		if byID["b"].Capture != nil {
			t.Fatalf("List must not manufacture a capture declaration for %q, got %+v", "b", byID["b"].Capture)
		}
	})

	t.Run("capture_client filters the listing", func(t *testing.T) {
		s := newStore(t)
		now := time.Now()
		a := mk("a", "p", now)
		a.Capture = &lineage.CaptureDeclaration{Client: "runledger-py/0.1.0", Attempted: []string{"host"}}
		a.Fingerprint = a.Compute()
		b := mk("b", "p", now.Add(-time.Minute))
		b.Capture = &lineage.CaptureDeclaration{Client: "rlctl/0.1.0", Attempted: []string{"host"}}
		b.Fingerprint = b.Compute()
		if err := s.Record(ctx, a); err != nil {
			t.Fatal(err)
		}
		if err := s.Record(ctx, b); err != nil {
			t.Fatal(err)
		}
		page, err := s.List(ctx, Query{Project: "p", CaptureClient: "runledger-py/0.1.0"})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Runs) != 1 || page.Runs[0].RunID != "a" {
			t.Fatalf("capture_client filter did not narrow: %+v", page.Runs)
		}
	})

	t.Run("re-recording a run with an equivalent, differently-ordered capture declaration is idempotent", func(t *testing.T) {
		// Attempted is a set; a client library retrying a request has no
		// reason to reorder it, but nothing about the type guarantees that,
		// and a backend's own storage (a side table, in DuckDB's case) has
		// no order of its own to begin with. This is the property
		// NormalizeCapture exists to guarantee.
		s := newStore(t)
		r := mk("a", "p", time.Now())
		r.Capture = &lineage.CaptureDeclaration{Attempted: []string{"host", "device", "framework_version"}}
		r.Fingerprint = r.Compute()
		if err := s.Record(ctx, r); err != nil {
			t.Fatal(err)
		}
		reordered := r
		reordered.Capture = &lineage.CaptureDeclaration{Attempted: []string{"framework_version", "device", "host"}}
		if err := s.Record(ctx, reordered); err != nil {
			t.Fatalf("re-recording with a reordered-but-equivalent capture declaration should be idempotent, got %v", err)
		}
	})

	t.Run("update does not disturb a run's capture declaration", func(t *testing.T) {
		s := newStore(t)
		r := mk("a", "p", time.Now())
		r.Status = lineage.StatusRunning
		r.Capture = &lineage.CaptureDeclaration{Client: "runledger-py/0.1.0", Attempted: []string{"host"}}
		r.Fingerprint = r.Compute()
		if err := s.Record(ctx, r); err != nil {
			t.Fatal(err)
		}
		succeeded := lineage.StatusSucceeded
		got, err := s.Update(ctx, "a", Patch{Status: &succeeded})
		if err != nil {
			t.Fatal(err)
		}
		if got.Capture == nil || got.Capture.Client != "runledger-py/0.1.0" {
			t.Fatalf("an unrelated patch must not disturb the run's capture declaration, got %+v", got.Capture)
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

	t.Run("update on an unknown id is ErrNotFound", func(t *testing.T) {
		s := newStore(t)
		status := lineage.StatusRunning
		if _, err := s.Update(ctx, "nope", Patch{Status: &status}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})

	t.Run("update walks the run through its lifecycle", func(t *testing.T) {
		s := newStore(t)
		r := mk("a", "p", time.Now())
		r.Status = lineage.StatusCreated
		if err := s.Record(ctx, r); err != nil {
			t.Fatal(err)
		}

		running := lineage.StatusRunning
		got, err := s.Update(ctx, "a", Patch{Status: &running})
		if err != nil {
			t.Fatalf("created -> running: %v", err)
		}
		if got.Status != lineage.StatusRunning {
			t.Fatalf("want running, got %s", got.Status)
		}

		succeeded := lineage.StatusSucceeded
		endedAt := time.Now()
		got, err = s.Update(ctx, "a", Patch{
			Status:  &succeeded,
			EndedAt: &endedAt,
			Metrics: map[string]float64{"loss": 0.1},
		})
		if err != nil {
			t.Fatalf("running -> succeeded: %v", err)
		}
		if got.Status != lineage.StatusSucceeded || got.Metrics["loss"] != 0.1 {
			t.Fatalf("want succeeded with loss=0.1, got %+v", got)
		}
		if got.EndedAt == nil || !got.EndedAt.Equal(endedAt) {
			t.Fatalf("ended_at not applied: got %v, want %v", got.EndedAt, endedAt)
		}

		// The updated run is durable, not just returned.
		reGot, err := s.Get(ctx, "a")
		if err != nil {
			t.Fatal(err)
		}
		if reGot.Status != lineage.StatusSucceeded || reGot.Metrics["loss"] != 0.1 {
			t.Fatalf("update did not persist: %+v", reGot)
		}
	})

	t.Run("submitter_claim and job_id are recorded and can be patched", func(t *testing.T) {
		s := newStore(t)
		r := mk("a", "p", time.Now())
		r.Status = lineage.StatusRunning
		r.SubmitterClaim = "alice"
		r.Fingerprint = r.Compute()
		if err := s.Record(ctx, r); err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(ctx, "a")
		if err != nil {
			t.Fatal(err)
		}
		if got.SubmitterClaim != "alice" || got.JobID != "" {
			t.Fatalf("attribution did not round-trip through Record/Get: %+v", got)
		}

		jobID := "ci-42"
		got, err = s.Update(ctx, "a", Patch{JobID: &jobID})
		if err != nil {
			t.Fatalf("patching job_id: %v", err)
		}
		if got.JobID != "ci-42" || got.SubmitterClaim != "alice" {
			t.Fatalf("want job_id patched and submitter_claim untouched, got %+v", got)
		}

		reGot, err := s.Get(ctx, "a")
		if err != nil {
			t.Fatal(err)
		}
		if reGot.JobID != "ci-42" {
			t.Fatalf("patched job_id did not persist: %+v", reGot)
		}
	})

	t.Run("a recorded run has no ended_at until it ends", func(t *testing.T) {
		s := newStore(t)
		r := mk("a", "p", time.Now())
		r.Status = lineage.StatusRunning
		r.Fingerprint = r.Compute()
		if err := s.Record(ctx, r); err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(ctx, "a")
		if err != nil {
			t.Fatal(err)
		}
		if got.EndedAt != nil {
			t.Fatalf("want no ended_at on an unfinished run, got %v", got.EndedAt)
		}
	})

	t.Run("a terminal update with no ended_at defaults it to receive time", func(t *testing.T) {
		s := newStore(t)
		r := mk("a", "p", time.Now())
		r.Status = lineage.StatusRunning
		r.Fingerprint = r.Compute()
		if err := s.Record(ctx, r); err != nil {
			t.Fatal(err)
		}

		before := time.Now()
		succeeded := lineage.StatusSucceeded
		got, err := s.Update(ctx, "a", Patch{Status: &succeeded})
		if err != nil {
			t.Fatal(err)
		}
		after := time.Now()

		if got.EndedAt == nil {
			t.Fatal("want ended_at defaulted on a terminal transition, got nil")
		}
		// Generous tolerance -- this is checking "did the store stamp receive
		// time," not measuring latency.
		if got.EndedAt.Before(before.Add(-5*time.Second)) || got.EndedAt.After(after.Add(5*time.Second)) {
			t.Fatalf("want ended_at near now (%v..%v), got %v", before, after, got.EndedAt)
		}

		reGot, err := s.Get(ctx, "a")
		if err != nil {
			t.Fatal(err)
		}
		if reGot.EndedAt == nil || !reGot.EndedAt.Equal(*got.EndedAt) {
			t.Fatalf("defaulted ended_at did not persist: got %v, want %v", reGot.EndedAt, got.EndedAt)
		}
	})

	t.Run("update merges metrics instead of replacing the map", func(t *testing.T) {
		s := newStore(t)
		r := mk("a", "p", time.Now())
		r.Status = lineage.StatusRunning
		r.Metrics = map[string]float64{"loss": 1.0}
		r.Fingerprint = r.Compute()
		if err := s.Record(ctx, r); err != nil {
			t.Fatal(err)
		}

		got, err := s.Update(ctx, "a", Patch{Metrics: map[string]float64{"acc": 0.5}})
		if err != nil {
			t.Fatal(err)
		}
		if got.Metrics["loss"] != 1.0 || got.Metrics["acc"] != 0.5 {
			t.Fatalf("want both metrics present, got %+v", got.Metrics)
		}

		// Re-reporting an existing key overwrites just that key.
		got, err = s.Update(ctx, "a", Patch{Metrics: map[string]float64{"loss": 0.2}})
		if err != nil {
			t.Fatal(err)
		}
		if got.Metrics["loss"] != 0.2 || got.Metrics["acc"] != 0.5 {
			t.Fatalf("want loss overwritten and acc untouched, got %+v", got.Metrics)
		}
	})

	t.Run("update refuses to change an identity field", func(t *testing.T) {
		s := newStore(t)
		r := mk("a", "p", time.Now())
		r.Status = lineage.StatusRunning
		if err := s.Record(ctx, r); err != nil {
			t.Fatal(err)
		}
		other := "different-commit"
		if _, err := s.Update(ctx, "a", Patch{GitCommit: &other}); !errors.Is(err, ErrConflict) {
			t.Fatalf("want ErrConflict for a changed identity field, got %v", err)
		}
		// The identity field matching the existing value is not a conflict.
		same := r.GitCommit
		if _, err := s.Update(ctx, "a", Patch{GitCommit: &same, Status: &r.Status}); err != nil {
			t.Fatalf("an unchanged identity field must not conflict, got %v", err)
		}
	})

	t.Run("update refuses an illegal status transition", func(t *testing.T) {
		s := newStore(t)
		r := mk("a", "p", time.Now())
		r.Status = lineage.StatusCreated
		if err := s.Record(ctx, r); err != nil {
			t.Fatal(err)
		}
		succeeded := lineage.StatusSucceeded
		if _, err := s.Update(ctx, "a", Patch{Status: &succeeded}); !errors.Is(err, ErrConflict) {
			t.Fatalf("created -> succeeded must skip illegally, want ErrConflict, got %v", err)
		}
	})

	t.Run("update refuses any change once a run is terminal", func(t *testing.T) {
		s := newStore(t)
		r := mk("a", "p", time.Now()) // mk sets StatusSucceeded
		if err := s.Record(ctx, r); err != nil {
			t.Fatal(err)
		}
		uri := "s3://bucket/ckpt"
		if _, err := s.Update(ctx, "a", Patch{CheckpointURI: &uri}); !errors.Is(err, ErrConflict) {
			t.Fatalf("a terminal run must refuse further updates, got %v", err)
		}
		cancelled := lineage.StatusCancelled
		if _, err := s.Update(ctx, "a", Patch{Status: &cancelled}); !errors.Is(err, ErrConflict) {
			t.Fatalf("a transition out of a terminal state must be ErrConflict, got %v", err)
		}
	})

	t.Run("an identical terminal patch retried is not a conflict", func(t *testing.T) {
		s := newStore(t)
		r := mk("a", "p", time.Now())
		r.Status = lineage.StatusRunning
		r.Metrics = map[string]float64{"loss": 0.4}
		r.Fingerprint = r.Compute()
		if err := s.Record(ctx, r); err != nil {
			t.Fatal(err)
		}
		succeeded := lineage.StatusSucceeded
		first, err := s.Update(ctx, "a", Patch{Status: &succeeded, Metrics: map[string]float64{"loss": 0.4}})
		if err != nil {
			t.Fatalf("first terminal patch: %v", err)
		}

		// A client with at-least-once delivery (e.g. a lost response) retries
		// the exact same request; the run is already terminal and the patch
		// asks for exactly what's already stored, so this must succeed rather
		// than 409 -- a 409 here is indistinguishable from an attempt to
		// rewrite the run's identity or outcome, which is not what happened.
		second, err := s.Update(ctx, "a", Patch{Status: &succeeded, Metrics: map[string]float64{"loss": 0.4}})
		if err != nil {
			t.Fatalf("identical retried terminal patch must not conflict, got %v", err)
		}
		if second.Status != lineage.StatusSucceeded || second.Metrics["loss"] != 0.4 {
			t.Fatalf("want the unchanged run back, got %+v", second)
		}
		if second.EndedAt == nil || !second.EndedAt.Equal(*first.EndedAt) {
			t.Fatalf("a no-op retry must not move ended_at, want %v, got %v", first.EndedAt, second.EndedAt)
		}
	})

	t.Run("a terminal patch with a genuinely different metric value still conflicts", func(t *testing.T) {
		s := newStore(t)
		r := mk("a", "p", time.Now())
		r.Status = lineage.StatusRunning
		r.Metrics = map[string]float64{"loss": 0.4}
		r.Fingerprint = r.Compute()
		if err := s.Record(ctx, r); err != nil {
			t.Fatal(err)
		}
		succeeded := lineage.StatusSucceeded
		if _, err := s.Update(ctx, "a", Patch{Status: &succeeded, Metrics: map[string]float64{"loss": 0.4}}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Update(ctx, "a", Patch{Metrics: map[string]float64{"loss": 0.9}}); !errors.Is(err, ErrConflict) {
			t.Fatalf("a different metric value on a terminal run must still conflict, got %v", err)
		}
	})

	t.Run("a terminal patch adding a brand-new metric key still conflicts", func(t *testing.T) {
		s := newStore(t)
		r := mk("a", "p", time.Now())
		r.Status = lineage.StatusRunning
		r.Metrics = map[string]float64{"loss": 0.4}
		r.Fingerprint = r.Compute()
		if err := s.Record(ctx, r); err != nil {
			t.Fatal(err)
		}
		succeeded := lineage.StatusSucceeded
		if _, err := s.Update(ctx, "a", Patch{Status: &succeeded, Metrics: map[string]float64{"loss": 0.4}}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Update(ctx, "a", Patch{Metrics: map[string]float64{"accuracy": 0.9}}); !errors.Is(err, ErrConflict) {
			t.Fatalf("a new metric key on a terminal run must still conflict even though existing keys match, got %v", err)
		}
	})

	t.Run("update rejects an unknown status", func(t *testing.T) {
		s := newStore(t)
		r := mk("a", "p", time.Now())
		r.Status = lineage.StatusCreated
		if err := s.Record(ctx, r); err != nil {
			t.Fatal(err)
		}
		bogus := lineage.Status("bogus")
		if _, err := s.Update(ctx, "a", Patch{Status: &bogus}); err == nil || errors.Is(err, ErrConflict) {
			t.Fatalf("an unrecognized status should be a plain error, not ErrConflict or nil: %v", err)
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

	t.Run("since and until narrow the listing to a half-open time range", func(t *testing.T) {
		s := newStore(t)
		base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
		_ = s.Record(ctx, mk("before", "p", base.Add(-time.Hour)))
		_ = s.Record(ctx, mk("at-since", "p", base))
		_ = s.Record(ctx, mk("mid", "p", base.Add(time.Hour)))
		_ = s.Record(ctx, mk("at-until", "p", base.Add(2*time.Hour)))
		_ = s.Record(ctx, mk("after", "p", base.Add(3*time.Hour)))

		page, err := s.List(ctx, Query{Project: "p", Since: base, Until: base.Add(2 * time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		// Newest first: at-until is excluded (Until is exclusive), at-since is
		// included (Since is inclusive), before/after fall outside the range
		// entirely.
		want := []string{"mid", "at-since"}
		var got []string
		for _, r := range page.Runs {
			got = append(got, r.RunID)
		}
		if len(got) != len(want) {
			t.Fatalf("want %v, got %v", want, got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("want %v, got %v", want, got)
			}
		}
	})

	t.Run("since/until compose with the other filters", func(t *testing.T) {
		s := newStore(t)
		base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
		_ = s.Record(ctx, mk("alpha-in", "alpha", base))
		_ = s.Record(ctx, mk("beta-in", "beta", base))
		_ = s.Record(ctx, mk("alpha-out", "alpha", base.Add(-time.Hour)))

		page, err := s.List(ctx, Query{Project: "alpha", Since: base, Until: base.Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Runs) != 1 || page.Runs[0].RunID != "alpha-in" {
			t.Fatalf("project and time-range filters together did not narrow correctly: %+v", page.Runs)
		}
	})

	t.Run("a since/until-narrowed listing still paginates without skipping or duplicating rows", func(t *testing.T) {
		s := newStore(t)
		base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
		const inWindow = 5
		var want []string
		for i := 0; i < inWindow; i++ {
			id := fmt.Sprintf("run-%d", i)
			_ = s.Record(ctx, mk(id, "p", base.Add(time.Duration(i)*time.Minute)))
			want = append(want, id)
		}
		// Recorded outside [since, until) -- pagination must never surface these.
		_ = s.Record(ctx, mk("too-early", "p", base.Add(-time.Hour)))
		_ = s.Record(ctx, mk("too-late", "p", base.Add(time.Hour)))

		since, until := base, base.Add(inWindow*time.Minute)
		var got []string
		var cursor *Cursor
		for {
			page, err := s.List(ctx, Query{Project: "p", Since: since, Until: until, Limit: 2, After: cursor})
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range page.Runs {
				got = append(got, r.RunID)
			}
			if page.Next == nil {
				break
			}
			cursor = page.Next
		}
		// Newest first.
		wantOrdered := []string{"run-4", "run-3", "run-2", "run-1", "run-0"}
		if len(got) != len(wantOrdered) {
			t.Fatalf("want %v, got %v", wantOrdered, got)
		}
		for i := range wantOrdered {
			if got[i] != wantOrdered[i] {
				t.Fatalf("want %v, got %v", wantOrdered, got)
			}
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
