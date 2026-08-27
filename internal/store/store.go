// Package store persists run records and answers queries over them.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/kornsour/run-ledger/internal/lineage"
)

// ErrNotFound is returned when a run id matches nothing.
var ErrNotFound = errors.New("run not found")

// ErrConflict is returned when a run id already exists with different content.
// Recording is idempotent for identical content and an error otherwise —
// silently overwriting a lineage record would make history unreliable.
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

// Store is the persistence boundary.
//
// The in-memory implementation is the reference; it defines the semantics every
// other backend must reproduce, and store_test.go runs the same suite against
// any implementation via RunConformance.
type Store interface {
	Record(ctx context.Context, r lineage.Run) error
	Get(ctx context.Context, runID string) (lineage.Run, error)
	List(ctx context.Context, q Query) (Page, error)
	Close() error
}
