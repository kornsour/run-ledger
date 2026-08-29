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

// provenanceFields are the provenance columns spread checks for a group
// disagreement -- a deliberately narrower list than every field compare.go
// classifies as KindProvenance (which also has host, status, and
// checkpoint_uri): these are the columns worth a reader's first look, not
// the exhaustive set.
//
// The four fall into two kinds, not one:
//
//   - device and framework_version are technical: a group that disagrees on
//     one of these can have the difference itself as the cause of a metric
//     moving (different hardware, different numerics).
//   - submitter_claim and job_id cannot cause a metric to move the same
//     way -- who submitted a run, or what job launched it, is not a measured
//     input. What a disagreement here points at is a difference in *how* the
//     run was launched, which is often the real explanation behind a spread
//     that otherwise looks unattributable. Issue #67 names this workflow
//     directly: "noticing that a fingerprint group's spread lines up with
//     who submitted each run rather than with anything technical."
//
// Both kinds earn a place on this list for the same reason -- their
// disagreement is a good first place to look -- even though only the first
// kind is itself a plausible cause rather than a pointer toward one.
var provenanceFields = []struct {
	name string
	get  func(lineage.Run) string
}{
	{"device", func(r lineage.Run) string { return r.Device }},
	{"framework_version", func(r lineage.Run) string { return r.FrameworkVersion }},
	{"submitter_claim", func(r lineage.Run) string { return r.SubmitterClaim }},
	{"job_id", func(r lineage.Run) string { return r.JobID }},
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

// provenanceDiffs does not special-case "": by ADR 0011 an empty value
// already means "not recorded" for every field in provenanceFields, and
// treating it as an ordinary value here happens to get both cases the ADR
// requires right for free. A group where every run left a field empty
// collapses to one distinct value ("") and is not reported -- nobody
// recording job_id is not a disagreement. A group where only some runs
// recorded it collapses to two or more distinct values, one of them "",
// and is reported -- that split is itself the disagreement, the same as if
// the recorded values disagreed with each other. Singling out "" for
// different treatment (dropping it from Values, or requiring at least one
// non-empty value before reporting) would have to special-case it either
// way, and would obscure the recorded/not-recorded split this list exists
// to surface for submitter_claim and job_id in particular.
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
