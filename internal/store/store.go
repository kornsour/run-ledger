// Package store persists run records and answers queries over them.
package store

import (
	"context"
	"errors"

	"github.com/kornsour/run-ledger/internal/lineage"
)

// ErrNotFound is returned when a run id matches nothing.
var ErrNotFound = errors.New("run not found")

// ErrConflict is returned when a run id already exists with different content.
// Recording is idempotent for identical content and an error otherwise —
// silently overwriting a lineage record would make history unreliable.
var ErrConflict = errors.New("run already recorded with different content")

// Query filters a run listing. A zero-valued field does not filter.
type Query struct {
	Project     string
	GitCommit   string
	Fingerprint string
	Status      lineage.Status
	Device      string
	Limit       int
}

// Store is the persistence boundary.
//
// The in-memory implementation is the reference; it defines the semantics every
// other backend must reproduce, and store_test.go runs the same suite against
// any implementation via RunConformance.
type Store interface {
	Record(ctx context.Context, r lineage.Run) error
	Get(ctx context.Context, runID string) (lineage.Run, error)
	List(ctx context.Context, q Query) ([]lineage.Run, error)
	Close() error
}
