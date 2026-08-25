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
type Field struct {
	Name string `json:"name"`
	Kind Kind   `json:"kind"`
	A    string `json:"a"`
	B    string `json:"b"`
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
}

// Runs diffs two run records.
func Runs(a, b lineage.Run) Result {
	res := Result{A: a.RunID, B: b.RunID, SameExperiment: a.Compute() == b.Compute()}
	add := func(name string, kind Kind, x, y string) {
		if x != y {
			res.Fields = append(res.Fields, Field{Name: name, Kind: kind, A: x, B: y})
		}
	}

	add("project", KindIdentity, a.Project, b.Project)
	add("git_commit", KindIdentity, a.GitCommit, b.GitCommit)
	add("git_dirty", KindIdentity, fmt.Sprint(a.GitDirty), fmt.Sprint(b.GitDirty))
	add("config_hash", KindIdentity, a.ConfigHash, b.ConfigHash)
	add("dataset_version", KindIdentity, a.DatasetVersion, b.DatasetVersion)
	add("model_version", KindIdentity, a.ModelVersion, b.ModelVersion)
	add("seed", KindIdentity, fmt.Sprint(a.Seed), fmt.Sprint(b.Seed))
	for _, k := range unionKeys(a.Params, b.Params) {
		add("params."+k, KindIdentity, a.Params[k], b.Params[k])
	}

	add("host", KindProvenance, a.Host, b.Host)
	add("device", KindProvenance, a.Device, b.Device)
	add("framework_version", KindProvenance, a.FrameworkVersion, b.FrameworkVersion)
	add("status", KindProvenance, string(a.Status), string(b.Status))
	add("checkpoint_uri", KindProvenance, a.CheckpointURI, b.CheckpointURI)

	for _, k := range unionKeysF(a.Metrics, b.Metrics) {
		add("metrics."+k, KindMetric, fmtMetric(a.Metrics, k), fmtMetric(b.Metrics, k))
	}
	return res
}

// Unattributable reports a same-experiment pair whose measured results differ.
// That combination is the interesting one: the lineage record claims the two
// runs were identical, and they were not, so something real is going
// unrecorded.
func (r Result) Unattributable() bool {
	if !r.SameExperiment {
		return false
	}
	for _, f := range r.Fields {
		if f.Kind == KindMetric {
			return true
		}
	}
	return false
}

func fmtMetric(m map[string]float64, k string) string {
	v, ok := m[k]
	if !ok {
		return "" // absent, which is distinct from zero
	}
	return fmt.Sprintf("%g", v)
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
