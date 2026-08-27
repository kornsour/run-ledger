// Package spread answers "how much do runs of this same experiment vary?"
//
// compare.Runs diffs exactly two runs. Spread groups every run that shares a
// fingerprint -- the same experiment, repeated -- and summarizes, per metric,
// how much the measured result actually moved. That number is a project's
// reproducibility floor: it is what tells you whether a later improvement is
// real or inside the noise these repeats already show.
package spread

import (
	"math"
	"sort"

	"github.com/kornsour/run-ledger/internal/lineage"
)

// MetricStat summarizes one metric across a fingerprint group's runs.
//
// Count is how many of the group's runs reported this metric, not the
// group's size -- a metric only some runs logged is not zero on the rest,
// the same rule compare.Result already follows for a missing metric.
type MetricStat struct {
	Count  int     `json:"count"`
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
	Mean   float64 `json:"mean"`
	StdDev float64 `json:"stddev"`
}

// ProvenanceDiff records a provenance field that is not constant across a
// group's runs. Provenance does not feed the fingerprint, so a group is free
// to disagree on it -- and when it does, that disagreement is the most
// common explanation for whatever metric spread the group shows.
type ProvenanceDiff struct {
	Field  string   `json:"field"`
	Values []string `json:"values"`
}

// Group is the spread summary for every recorded run of one fingerprint.
type Group struct {
	Fingerprint string   `json:"fingerprint"`
	RunIDs      []string `json:"run_ids"`
	Count       int      `json:"count"`
	// NoRepeats is true when exactly one run has this fingerprint. Metrics is
	// left empty rather than reporting a standard deviation of zero, which
	// would claim a reproducibility this group was never asked to show.
	NoRepeats  bool                  `json:"no_repeats"`
	Metrics    map[string]MetricStat `json:"metrics,omitempty"`
	Provenance []ProvenanceDiff      `json:"provenance,omitempty"`
}

// Widest is a group's ranking key: the largest coefficient of variation
// (|stddev / mean|) across its metrics. A metric with a zero mean, or a
// metric only one run in the group reported, contributes nothing measurable
// and is skipped rather than dividing by zero or resting on a single sample.
// A group with nothing measurable -- no repeats, or no metric with a
// nonzero mean -- ranks last: there is no spread here for this ranking to
// surface.
func (g Group) Widest() float64 {
	widest := -1.0
	for _, m := range g.Metrics {
		if m.Mean == 0 {
			continue
		}
		if cv := math.Abs(m.StdDev / m.Mean); cv > widest {
			widest = cv
		}
	}
	return widest
}

// provenanceFields are the provenance columns most likely to explain a
// same-fingerprint metric difference -- compare.go classifies these two the
// same way, as KindProvenance.
var provenanceFields = []struct {
	name string
	get  func(lineage.Run) string
}{
	{"device", func(r lineage.Run) string { return r.Device }},
	{"framework_version", func(r lineage.Run) string { return r.FrameworkVersion }},
}

// Compute groups runs by fingerprint and summarizes each group. runs need
// not already be grouped, sorted, or limited to one fingerprint or project.
func Compute(runs []lineage.Run) []Group {
	byFP := map[string][]lineage.Run{}
	var order []string
	for _, r := range runs {
		if _, ok := byFP[r.Fingerprint]; !ok {
			order = append(order, r.Fingerprint)
		}
		byFP[r.Fingerprint] = append(byFP[r.Fingerprint], r)
	}
	groups := make([]Group, 0, len(order))
	for _, fp := range order {
		groups = append(groups, One(fp, byFP[fp]))
	}
	return groups
}

// One summarizes a single fingerprint's runs. It is exported separately from
// Compute so GET /fingerprints/{fingerprint} -- which already has exactly the
// runs of one group, via Store.List(Query{Fingerprint: fp}) -- does not need
// to re-derive the grouping Compute exists to do over a wider listing.
func One(fingerprint string, runs []lineage.Run) Group {
	ids := make([]string, len(runs))
	for i, r := range runs {
		ids[i] = r.RunID
	}
	sort.Strings(ids)

	g := Group{Fingerprint: fingerprint, RunIDs: ids, Count: len(runs)}
	if len(runs) < 2 {
		g.NoRepeats = true
		return g
	}
	g.Metrics = metricStats(runs)
	g.Provenance = provenanceDiffs(runs)
	return g
}

func metricStats(runs []lineage.Run) map[string]MetricStat {
	values := map[string][]float64{}
	for _, r := range runs {
		for k, v := range r.Metrics {
			values[k] = append(values[k], v)
		}
	}
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]MetricStat, len(values))
	for k, vs := range values {
		out[k] = stat(vs)
	}
	return out
}

// stat computes count/min/max/mean/stddev over a non-empty slice.
//
// StdDev is the population standard deviation, not a sample estimate: a
// fingerprint group is every run of that experiment recorded so far, not a
// sample drawn from some larger population of runs.
func stat(vs []float64) MetricStat {
	n := len(vs)
	min, max, sum := vs[0], vs[0], 0.0
	for _, v := range vs {
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
		sum += v
	}
	mean := sum / float64(n)
	var sq float64
	for _, v := range vs {
		d := v - mean
		sq += d * d
	}
	return MetricStat{Count: n, Min: min, Max: max, Mean: mean, StdDev: math.Sqrt(sq / float64(n))}
}

func provenanceDiffs(runs []lineage.Run) []ProvenanceDiff {
	var diffs []ProvenanceDiff
	for _, f := range provenanceFields {
		seen := map[string]bool{}
		var values []string
		for _, r := range runs {
			v := f.get(r)
			if !seen[v] {
				seen[v] = true
				values = append(values, v)
			}
		}
		if len(values) > 1 {
			sort.Strings(values)
			diffs = append(diffs, ProvenanceDiff{Field: f.name, Values: values})
		}
	}
	return diffs
}
