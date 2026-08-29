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

// ErrIdentityConflict is returned when a patch would change one of a run's
// identity fields. It wraps ErrConflict so a caller that only checks
// errors.Is(err, ErrConflict) — the check that predates this distinction —
// still works unchanged; a caller that wants to report this case
// specifically (the HTTP API, to pick a machine-readable error code) checks
// errors.Is(err, ErrIdentityConflict) first.
var ErrIdentityConflict = fmt.Errorf("%w: identity fields cannot be changed by an update", ErrConflict)

// ErrIllegalTransition is returned when a patch would move a run's status
// somewhere its lifecycle does not allow, or would change anything about a
// run already in a terminal status. See ErrIdentityConflict for why this
// wraps ErrConflict instead of replacing it.
var ErrIllegalTransition = fmt.Errorf("%w: illegal status transition", ErrConflict)

// ErrUnknownStatus is returned when a patch names a status lineage does not
// recognize. It is a plain validation error, not a conflict — nothing
// stored disagrees with the request, the request itself is malformed.
var ErrUnknownStatus = errors.New("unknown status")

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
	// SubmitterClaim and JobID filter on the two attribution fields (ADR
	// 0015) the same way Device already filters on a provenance field --
	// this is the "filter GET /v1/runs to your own runs" capability #67
	// asks for.
	SubmitterClaim string
	JobID          string
	// CaptureClient filters to runs whose capture declaration names this
	// exact client string (e.g. "runledger-py/0.1.0") -- the query ADR 0016
	// names directly: "did capture regress in client X.Y" needs to isolate
	// every run that client recorded, the same exact-match shape Device
	// already has. A run with no capture declaration never matches a
	// non-empty CaptureClient, the same way an unset Device never matches a
	// non-empty filter.
	CaptureClient string
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
	// Since, when non-zero, restricts the listing to runs whose StartedAt is
	// at or after this instant -- an inclusive lower bound.
	Since time.Time
	// Until, when non-zero, restricts the listing to runs whose StartedAt is
	// strictly before this instant -- an exclusive upper bound. Pairing an
	// inclusive Since with an exclusive Until means a run starting exactly on
	// a boundary shared by two adjacent queries (e.g. paging by day) is
	// counted by exactly one of them, never both or neither.
	Until time.Time
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
	// SubmitterClaim and JobID are patchable for the same reason Host and
	// Device already are: a run is often recorded before every provenance
	// fact about it is known (device detection deferred until the job
	// actually schedules, a CI job id assigned after the record call
	// fires), and being provenance -- not identity -- means correcting or
	// filling one in later cannot rewrite what experiment the run was. See
	// ADR 0015.
	SubmitterClaim *string
	JobID          *string
	Metrics        map[string]float64

	// Capture (lineage.Run.Capture, ADR 0016) is deliberately not here, and
	// that is a decision, not an oversight. Host, Device, and JobID are
	// patchable because a run is often recorded before every fact about it
	// is known -- a device only knowable once a job schedules onto specific
	// hardware, a job id assigned by the scheduler after submission. A
	// capture declaration has no such deferred-availability case: it
	// describes what the recording client's own code tried to determine,
	// which the client already knows in full at the moment it makes the
	// very first request for a run (rlctl and the Python client both build
	// it before sending anything). There is no legitimate "fill this in
	// once known" scenario to support, and allowing one would raise a
	// question patchability for the other fields never has to answer: whose
	// declaration is it if a different call patches it in later? Leaving
	// Capture out keeps it what it is by construction -- a fact about the
	// request that created the run, fixed at that moment.
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
		return lineage.Run{}, fmt.Errorf("%w: %q", ErrUnknownStatus, *p.Status)
	}
	if lineage.Terminal(existing.Status) {
		// A terminal run is a finished outcome; nothing about it moves again.
		// But a patch that asks for exactly what's already stored is a
		// retry, not an attempt to move it -- a client with at-least-once
		// delivery (e.g. a "finish" call whose response was lost) must be
		// able to treat this the same way Record treats a re-recorded
		// identical run, or a lost 200 turns into a 409 it cannot safely
		// interpret as success.
		if isNoopPatch(existing, p) {
			return existing, nil
		}
		return lineage.Run{}, ErrIllegalTransition
	}

	updated := existing
	if p.Status != nil && *p.Status != existing.Status {
		if !legalTransitions[existing.Status][*p.Status] {
			return lineage.Run{}, ErrIllegalTransition
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
	if p.SubmitterClaim != nil {
		updated.SubmitterClaim = *p.SubmitterClaim
	}
	if p.JobID != nil {
		updated.JobID = *p.JobID
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

// isNoopPatch reports whether applying p to existing, an already-terminal
// run, would change nothing. Identity fields are not checked here --
// checkIdentityUnchanged has already run by the time this is called -- so
// this only has to compare the provenance fields a terminal patch could
// still legally repeat.
//
// A metrics-bearing patch is a no-op only if every key it sets already
// exists in existing.Metrics with the identical value: the merge semantics
// documented on Patch mean a new key would change the stored map even
// though every field on the run struct itself stays the same.
func isNoopPatch(existing lineage.Run, p Patch) bool {
	if p.Status != nil && *p.Status != existing.Status {
		return false
	}
	if p.EndedAt != nil && (existing.EndedAt == nil || !p.EndedAt.Equal(*existing.EndedAt)) {
		return false
	}
	if p.CheckpointURI != nil && *p.CheckpointURI != existing.CheckpointURI {
		return false
	}
	if p.Host != nil && *p.Host != existing.Host {
		return false
	}
	if p.Device != nil && *p.Device != existing.Device {
		return false
	}
	if p.FrameworkVersion != nil && *p.FrameworkVersion != existing.FrameworkVersion {
		return false
	}
	if p.SubmitterClaim != nil && *p.SubmitterClaim != existing.SubmitterClaim {
		return false
	}
	if p.JobID != nil && *p.JobID != existing.JobID {
		return false
	}
	for k, v := range p.Metrics {
		if stored, ok := existing.Metrics[k]; !ok || stored != v {
			return false
		}
	}
	return true
}

// checkIdentityUnchanged reports ErrIdentityConflict if p sets any identity
// field to a value that differs from existing's. A field p leaves nil is not
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
		return ErrIdentityConflict
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
