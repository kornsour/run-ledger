package store

import "testing"

func TestMemoryConformance(t *testing.T) {
	RunConformance(t, func(t *testing.T) Store {
		s := NewMemory()
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}
