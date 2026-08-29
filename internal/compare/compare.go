// Package compare answers "what is different about these two runs?"
package compare

import (
	"fmt"
	"sort"

	"github.com/kornsour/run-ledger/internal/lineage"
)

// Kind says which half of the record a difference falls in.
type Kind string

const (
	// KindIdentity is a field that feeds the fingerprint: the two runs are
	// different experiments.
	KindIdentity Kind = "identity"
	// KindProvenance is a field that does not: same experiment, different
	// circumstances or outcome.
	KindProvenance Kind = "provenance"
	// KindMetric is a measured result.
	KindMetric Kind = "metric"
)

// Field is one difference between two runs.
//
// A and B are nil when the run did not record the field at all, and point
// at "" when it recorded an empty value. Only params can currently be the
// latter: for the seven scalar fields where an empty string is not a
// meaningful value, "" is normalized to nil here -- see ADR 0011.
//
// Carrying pointers makes Field unsafe to compare with ==, which would
// test pointer identity rather than the values. Use reflect.DeepEqual.
type Field struct {
	Name string  `json:"name"`
	Kind Kind    `json:"kind"`
	A    *string `json:"a"`
	B    *string `json:"b"`
}

// value marks a field the run recorded, whatever it recorded.
func value(s string) *string { return &s }

// optional marks a field where "" and "not recorded" are the same claim,
// so an empty value is reported as absent rather than as a value of "".
// ADR 0011 is what makes that a fact about the data rather than a guess.
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// lookup distinguishes a param the run did not set from one it set to "".
// The map already carries that difference and lineage.Run.Compute already
// hashes the two differently -- reading through a bare map index, which
// yields "" for a missing key, was what discarded it.
func lookup(m map[string]string, k string) *string {
	v, ok := m[k]
	if !ok {
		return nil
	}
	return &v
}

func equal(x, y *string) bool {
	if x == nil || y == nil {
		return x == nil && y == nil
	}
	return *x == *y
}

// Result is the structured diff of two runs.
type Result struct {
	A string `json:"a"`
	B string `json:"b"`
	// SameExperiment is true when the fingerprints match — no identity field
	// differs, so any metric difference is attributable to something the record
	// does not capture (nondeterminism, hardware, an unpinned dependency).
	SameExperiment bool    `json:"same_experiment"`
	Fields         []Field `json:"fields"`
	// Unattributable is true for a same-experiment pair whose measured
	// results still differ. That combination is the interesting one: the
	// lineage record claims the two runs were identical, and they were not,
	// so something real is going unrecorded.
	Unattributable bool `json:"unattributable"`
}

// SameExperiment reports whether two runs are the same experiment.
//
// The stored fingerprint is authoritative whenever both runs carry one. It
// is what the server computed at record time (ADR 0001) and what
// spread.Compute groups by, so reading it here keeps one answer to the
// question rather than two. Recomputing unconditionally -- which this used
// to do -- is indistinguishable from that today, and stops being so the
// moment lineage.Run.Compute changes: a record written under the old
// contract would keep its stored fingerprint while being rehashed under
// the new rules at read time, so spread would group two runs that compare
// called different experiments. ADR 0004 anticipates exactly that change.
//
// Compute remains the fallback for runs that have not been through a
// store, which is how callers construct them in tests.
func SameExperiment(a, b lineage.Run) bool {
	if a.Fingerprint != "" && b.Fingerprint != "" {
		return a.Fingerprint == b.Fingerprint
	}
	return a.Compute() == b.Compute()
}

// Runs diffs two run records.
func Runs(a, b lineage.Run) Result {
	res := Result{A: a.RunID, B: b.RunID, SameExperiment: SameExperiment(a, b)}
	add := func(name string, kind Kind, x, y *string) {
		if equal(x, y) {
			return
		}
		res.Fields = append(res.Fields, Field{Name: name, Kind: kind, A: x, B: y})
		if res.SameExperiment && kind == KindMetric {
			res.Unattributable = true
		}
	}

	// project and git_commit are required by Run.Validate, and git_dirty
	// and seed always have a value, so all four are always recorded.
	add("project", KindIdentity, value(a.Project), value(b.Project))
	add("git_commit", KindIdentity, value(a.GitCommit), value(b.GitCommit))
	add("git_dirty", KindIdentity, value(fmt.Sprint(a.GitDirty)), value(fmt.Sprint(b.GitDirty)))
	add("config_hash", KindIdentity, optional(a.ConfigHash), optional(b.ConfigHash))
	add("dataset_version", KindIdentity, optional(a.DatasetVersion), optional(b.DatasetVersion))
	add("model_version", KindIdentity, optional(a.ModelVersion), optional(b.ModelVersion))
	add("seed", KindIdentity, value(fmt.Sprint(a.Seed)), value(fmt.Sprint(b.Seed)))
	for _, k := range unionKeys(a.Params, b.Params) {
		add("params."+k, KindIdentity, lookup(a.Params, k), lookup(b.Params, k))
	}

	add("host", KindProvenance, optional(a.Host), optional(b.Host))
	add("device", KindProvenance, optional(a.Device), optional(b.Device))
	add("framework_version", KindProvenance, optional(a.FrameworkVersion), optional(b.FrameworkVersion))
	add("status", KindProvenance, optional(string(a.Status)), optional(string(b.Status)))
	add("checkpoint_uri", KindProvenance, optional(a.CheckpointURI), optional(b.CheckpointURI))

	for _, k := range unionKeysF(a.Metrics, b.Metrics) {
		add("metrics."+k, KindMetric, fmtMetric(a.Metrics, k), fmtMetric(b.Metrics, k))
	}
	return res
}

// fmtMetric returns nil for a metric the run did not report, which is
// distinct from a metric it reported as zero.
func fmtMetric(m map[string]float64, k string) *string {
	v, ok := m[k]
	if !ok {
		return nil
	}
	return value(fmt.Sprintf("%g", v))
}

func unionKeys(a, b map[string]string) []string {
	seen := map[string]bool{}
	for k := range a {
		seen[k] = true
	}
	for k := range b {
		seen[k] = true
	}
	return sorted(seen)
}

func unionKeysF(a, b map[string]float64) []string {
	seen := map[string]bool{}
	for k := range a {
		seen[k] = true
	}
	for k := range b {
		seen[k] = true
	}
	return sorted(seen)
}

func sorted(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
