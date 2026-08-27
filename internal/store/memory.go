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

func (m *Memory) Update(_ context.Context, runID string, p Patch) (lineage.Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.runs[runID]
	if !ok {
		return lineage.Run{}, ErrNotFound
	}
	updated, err := applyPatch(existing, p)
	if err != nil {
		return lineage.Run{}, err
	}
	m.runs[runID] = updated
	return updated, nil
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

func (m *Memory) List(_ context.Context, q Query) ([]lineage.Run, error) {
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
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func (m *Memory) Close() error { return nil }
