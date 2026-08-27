package store

import (
	"context"
	"reflect"
	"sort"
	"sync"

	"github.com/kornsour/run-ledger/internal/lineage"
)

// Memory is an in-process Store. It is the reference implementation and the
// default for `runledger` with no storage configured, so a clone runs with no
// external dependency.
type Memory struct {
	mu   sync.RWMutex
	runs map[string]lineage.Run
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{runs: make(map[string]lineage.Run)}
}

func (m *Memory) Record(_ context.Context, r lineage.Run) error {
	if err := r.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.runs[r.RunID]; ok {
		if !reflect.DeepEqual(existing, r) {
			return ErrConflict
		}
		return nil // idempotent re-record of identical content
	}
	m.runs[r.RunID] = r
	return nil
}

func (m *Memory) Get(_ context.Context, runID string) (lineage.Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.runs[runID]
	if !ok {
		return lineage.Run{}, ErrNotFound
	}
	return r, nil
}

func (m *Memory) List(_ context.Context, q Query) (Page, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]lineage.Run, 0, len(m.runs))
	for _, r := range m.runs {
		if q.Project != "" && r.Project != q.Project {
			continue
		}
		if q.GitCommit != "" && r.GitCommit != q.GitCommit {
			continue
		}
		if q.Fingerprint != "" && r.Fingerprint != q.Fingerprint {
			continue
		}
		if q.Status != "" && r.Status != q.Status {
			continue
		}
		if q.Device != "" && r.Device != q.Device {
			continue
		}
		out = append(out, r)
	}
	// Newest first, run id as the tiebreak so ordering is total and stable —
	// map iteration alone would reorder identical queries between calls.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].StartedAt.After(out[j].StartedAt)
		}
		return out[i].RunID < out[j].RunID
	})
	if q.After != nil {
		after := *q.After
		// out is already sorted in the traversal order, and "comes strictly
		// after the cursor" is monotonic over it (false*, then true*), so a
		// binary search finds the first surviving row in O(log n) instead of
		// a linear scan.
		i := sort.Search(len(out), func(i int) bool { return isAfterCursor(out[i], after) })
		out = out[i:]
	}
	return paginate(out, q.Limit), nil
}

// isAfterCursor reports whether r sorts strictly after c in List's total
// order (StartedAt descending, RunID ascending on a tie) — i.e. whether r
// belongs on the page that follows c.
func isAfterCursor(r lineage.Run, c Cursor) bool {
	if r.StartedAt.Equal(c.StartedAt) {
		return r.RunID > c.RunID
	}
	return r.StartedAt.Before(c.StartedAt)
}

// paginate truncates an already-ordered, already-after-cursor slice to
// Limit rows and reports the cursor for what follows, if anything does.
// Limit <= 0 means unbounded: the whole slice, no next page.
func paginate(runs []lineage.Run, limit int) Page {
	if limit <= 0 || len(runs) <= limit {
		return Page{Runs: runs}
	}
	page := runs[:limit]
	last := page[len(page)-1]
	return Page{Runs: page, Next: &Cursor{StartedAt: last.StartedAt, RunID: last.RunID}}
}

func (m *Memory) Close() error { return nil }
