// Package store persists run records and answers queries over them.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kornsour/run-ledger/internal/lineage"
)

// ErrNotFound is returned when a run id matches nothing.
var ErrNotFound = errors.New("run not found")

// ErrConflict is returned when a write would change what Record and Update
// are not allowed to change: Record refuses a second write under an id with
// different content, and Update refuses to alter a run's identity fields or
// to move it through an illegal status transition. Recording and updating
// are idempotent for a value that matches what is already stored, and an
// error otherwise — silently overwriting a lineage record would make
// history unreliable.
var ErrConflict = errors.New("run already recorded with different content")

// Cursor names a position in a run listing's total order — newest first by
// StartedAt, RunID ascending as the tiebreak (the same order every Store
// implementation sorts List's result in). It is how List paginates by
// keyset instead of offset: "give me what comes after this row" stays
// correct when rows are inserted concurrently, where "give me rows 20-40"
// does not, because an insert ahead of the traversal shifts every offset
// after it and a page silently skips or repeats a row.
type Cursor struct {
	StartedAt time.Time
	RunID     string
}

// Query filters and paginates a run listing. A zero-valued field does not
// filter.
type Query struct {
	Project     string
	GitCommit   string
	Fingerprint string
	Status      lineage.Status
	Device      string
	// Limit caps the number of rows a single List call returns. Zero means
	// unbounded — callers that page (the HTTP API's GET /runs) are expected
	// to supply their own default and maximum; Store itself imposes none, so
	// a Go-level caller that genuinely wants everything (the /readyz probe,
	// the ledger-size gauge) still can.
	Limit int
	// After, when non-nil, restricts the listing to rows strictly following
	// this cursor in the total order — the keyset for the page that comes
	// next. Nil means "start from the top."
	After *Cursor
}

// Page is one page of a Store.List result.
type Page struct {
	Runs []lineage.Run
	// Next is the cursor for the following page. It is non-nil only when
	// List's Limit truncated the result — i.e. more rows may exist beyond
	// this page. A caller that keeps passing Next back as Query.After until
	// it comes back nil has visited every row that existed at or before the
	// position each successive call reached; a row recorded elsewhere after
	// the traversal began, sorting behind where the traversal already is, is
	// never visited by it. That is a deliberate, documented trade for an
	// append-mostly ledger — see README.md's "Pagination" section — not an
	// oversight.
	Next *Cursor
}

// Patch is a partial update to a run, as accepted by Store.Update.
//
// The identity fields (everything above Provenance in lineage.Run) are
// listed here too, but only to be checked, never applied: if one is set and
// differs from the run's existing value, Update returns ErrConflict rather
// than silently rewriting what experiment the run's id refers to. A nil
// pointer field means "leave this alone"; a set pointer that matches the
// existing value is a no-op, the same idempotence Record gives identical
// content.
//
// Metrics is merged into the existing map rather than replacing it, so a
// long run can report as it goes — re-reporting an existing key overwrites
// just that key's value, not the whole map.
type Patch struct {
	// Identity — checked against the existing run, never written.
	Project        *string
	GitCommit      *string
	GitDirty       *bool
	ConfigHash     *string
	DatasetVersion *string
	ModelVersion   *string
	Seed           *int64
	Params         map[string]string

	// Provenance — applied when set.
	Status           *lineage.Status
	EndedAt          *time.Time
	CheckpointURI    *string
	Host             *string
	Device           *string
	FrameworkVersion *string
	Metrics          map[string]float64
}

// Store is the persistence boundary.
//
// The in-memory implementation is the reference; it defines the semantics every
// other backend must reproduce, and store_test.go runs the same suite against
// any implementation via RunConformance.
type Store interface {
	Record(ctx context.Context, r lineage.Run) error
	// Update applies a partial, provenance-only change to an already-recorded
	// run and returns the result. It returns ErrNotFound if runID matches no
	// run, and ErrConflict if the patch would change an identity field, move
	// the run through an illegal status transition, or apply at all to a run
	// already in a terminal status (succeeded, failed, cancelled) — a
	// terminal run is a finished outcome, not a waypoint.
	Update(ctx context.Context, runID string, p Patch) (lineage.Run, error)
	Get(ctx context.Context, runID string) (lineage.Run, error)
	List(ctx context.Context, q Query) (Page, error)
	Close() error
}

// legalTransitions is the run status lifecycle every backend enforces:
// created -> running -> {succeeded, failed, cancelled}. A status not listed
// as a key (including every terminal one) has no legal outgoing transition.
var legalTransitions = map[lineage.Status]map[lineage.Status]bool{
	lineage.StatusCreated: {lineage.StatusRunning: true},
	lineage.StatusRunning: {
		lineage.StatusSucceeded: true,
		lineage.StatusFailed:    true,
		lineage.StatusCancelled: true,
	},
}

// applyPatch computes the result of applying p to existing, without
// persisting it. Every backend's Update calls this after loading the
// current row, so the identity/transition/merge rules live in exactly one
// place instead of being reimplemented per backend.
func applyPatch(existing lineage.Run, p Patch) (lineage.Run, error) {
	if err := checkIdentityUnchanged(existing, p); err != nil {
		return lineage.Run{}, err
	}
	if p.Status != nil && !lineage.ValidStatus(*p.Status) {
		return lineage.Run{}, fmt.Errorf("unknown status %q", *p.Status)
	}
	if lineage.Terminal(existing.Status) {
		// A terminal run is a finished outcome. Nothing about it — status
		// included — moves again, even a same-value or metrics-only patch:
		// there is no "in progress" left for it to report.
		return lineage.Run{}, ErrConflict
	}

	updated := existing
	if p.Status != nil && *p.Status != existing.Status {
		if !legalTransitions[existing.Status][*p.Status] {
			return lineage.Run{}, ErrConflict
		}
		updated.Status = *p.Status
	}
	if p.EndedAt != nil {
		updated.EndedAt = p.EndedAt
	}
	// existing.Status can't already be terminal (checked above), so reaching
	// a terminal updated.Status means this patch is the transition that just
	// caused it -- the same moment a client would otherwise have to supply
	// EndedAt for itself. Default it here, mirroring the courtesy the record
	// handler gives StartedAt, so a terminal run never lacks an end time.
	if lineage.Terminal(updated.Status) && updated.EndedAt == nil {
		endedAt := time.Now().UTC()
		updated.EndedAt = &endedAt
	}
	if p.CheckpointURI != nil {
		updated.CheckpointURI = *p.CheckpointURI
	}
	if p.Host != nil {
		updated.Host = *p.Host
	}
	if p.Device != nil {
		updated.Device = *p.Device
	}
	if p.FrameworkVersion != nil {
		updated.FrameworkVersion = *p.FrameworkVersion
	}
	if p.Metrics != nil {
		merged := make(map[string]float64, len(updated.Metrics)+len(p.Metrics))
		for k, v := range updated.Metrics {
			merged[k] = v
		}
		for k, v := range p.Metrics {
			merged[k] = v
		}
		updated.Metrics = merged
	}

	if err := updated.Validate(); err != nil {
		return lineage.Run{}, err
	}
	return updated, nil
}

// checkIdentityUnchanged reports ErrConflict if p sets any identity field to
// a value that differs from existing's. A field p leaves nil is not
// checked, and a set field equal to the current value is a no-op, not a
// conflict — the same idempotence Record gives identical content.
func checkIdentityUnchanged(existing lineage.Run, p Patch) error {
	switch {
	case p.Project != nil && *p.Project != existing.Project,
		p.GitCommit != nil && *p.GitCommit != existing.GitCommit,
		p.GitDirty != nil && *p.GitDirty != existing.GitDirty,
		p.ConfigHash != nil && *p.ConfigHash != existing.ConfigHash,
		p.DatasetVersion != nil && *p.DatasetVersion != existing.DatasetVersion,
		p.ModelVersion != nil && *p.ModelVersion != existing.ModelVersion,
		p.Seed != nil && *p.Seed != existing.Seed,
		p.Params != nil && !paramsEqual(p.Params, existing.Params):
		return ErrConflict
	}
	return nil
}

// paramsEqual compares two params maps as equivalent regardless of whether
// "no params" is represented as nil or an empty map — the two mean the same
// thing and neither should look like a change from the other.
func paramsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
