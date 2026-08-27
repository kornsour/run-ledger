package spread

import (
	"math"
	"testing"

	"github.com/kornsour/run-ledger/internal/lineage"
)

func run(id string, metrics map[string]float64) lineage.Run {
	r := lineage.Run{Project: "p", GitCommit: "abc", ConfigHash: "cfg", Seed: 1, RunID: id, Metrics: metrics}
	r.Fingerprint = r.Compute()
	return r
}

func TestGroupOfOneIsNoRepeats(t *testing.T) {
	a := run("a", map[string]float64{"loss": 0.42})
	g := One(a.Fingerprint, []lineage.Run{a})
	if !g.NoRepeats {
		t.Fatal("a single run must be reported as no repeats")
	}
	if g.Metrics != nil {
		t.Fatalf("a group with no repeats must not report metric stats, got %+v", g.Metrics)
	}
	if g.Count != 1 {
		t.Fatalf("want count 1, got %d", g.Count)
	}
}

func TestGroupStatsAcrossRepeats(t *testing.T) {
	fp := run("x", nil).Fingerprint
	runs := []lineage.Run{
		run("a", map[string]float64{"loss": 0.40}),
		run("b", map[string]float64{"loss": 0.50}),
	}
	runs[0].Fingerprint, runs[1].Fingerprint = fp, fp
	g := One(fp, runs)
	if g.NoRepeats {
		t.Fatal("two runs is a repeat, not a singleton")
	}
	m, ok := g.Metrics["loss"]
	if !ok {
		t.Fatal("want a loss entry")
	}
	if m.Count != 2 {
		t.Fatalf("want count 2, got %d", m.Count)
	}
	if m.Min != 0.40 || m.Max != 0.50 {
		t.Fatalf("want min 0.40 max 0.50, got min=%v max=%v", m.Min, m.Max)
	}
	if math.Abs(m.Mean-0.45) > 1e-9 {
		t.Fatalf("want mean 0.45, got %v", m.Mean)
	}
	wantStdDev := 0.05 // population stddev of {0.40, 0.50}
	if math.Abs(m.StdDev-wantStdDev) > 1e-9 {
		t.Fatalf("want stddev %v, got %v", wantStdDev, m.StdDev)
	}
}

func TestAbsentMetricIsNotCountedAsZero(t *testing.T) {
	runs := []lineage.Run{
		run("a", map[string]float64{"loss": 0.4, "acc": 0.9}),
		run("b", map[string]float64{"loss": 0.5}), // no acc reported
	}
	g := One("fp", runs)
	acc := g.Metrics["acc"]
	if acc.Count != 1 {
		t.Fatalf("acc was reported by one run, want count 1, got %d", acc.Count)
	}
	if acc.StdDev != 0 {
		t.Fatalf("a single sample has no spread, got stddev %v", acc.StdDev)
	}
	loss := g.Metrics["loss"]
	if loss.Count != 2 {
		t.Fatalf("loss was reported by both runs, want count 2, got %d", loss.Count)
	}
}

func TestProvenanceDiffsSurfaceDeviceAndFrameworkVersion(t *testing.T) {
	a := run("a", map[string]float64{"loss": 0.4})
	a.Device, a.FrameworkVersion = "cpu", "2.1"
	b := run("b", map[string]float64{"loss": 0.5})
	b.Device, b.FrameworkVersion = "cuda", "2.1"

	g := One("fp", []lineage.Run{a, b})
	if len(g.Provenance) != 1 {
		t.Fatalf("want exactly one provenance diff (device), got %+v", g.Provenance)
	}
	if g.Provenance[0].Field != "device" {
		t.Fatalf("want device to be flagged, got %q", g.Provenance[0].Field)
	}
	if len(g.Provenance[0].Values) != 2 {
		t.Fatalf("want both device values listed, got %v", g.Provenance[0].Values)
	}
}

func TestProvenanceDiffsEmptyWhenConstant(t *testing.T) {
	a := run("a", map[string]float64{"loss": 0.4})
	a.Device = "cpu"
	b := run("b", map[string]float64{"loss": 0.5})
	b.Device = "cpu"

	g := One("fp", []lineage.Run{a, b})
	if len(g.Provenance) != 0 {
		t.Fatalf("no provenance field differs, want none flagged, got %+v", g.Provenance)
	}
}

func TestComputeGroupsByFingerprint(t *testing.T) {
	a := run("a", map[string]float64{"loss": 0.4})
	b := run("b", map[string]float64{"loss": 0.5})
	b.Seed = 2
	b.Fingerprint = b.Compute() // a different experiment

	groups := Compute([]lineage.Run{a, b})
	if len(groups) != 2 {
		t.Fatalf("want two distinct fingerprint groups, got %d", len(groups))
	}
	for _, g := range groups {
		if g.Count != 1 || !g.NoRepeats {
			t.Fatalf("each fingerprint here has exactly one run: %+v", g)
		}
	}
}

func TestWidestRanksLargerCoefficientOfVariationFirst(t *testing.T) {
	tight := Group{Metrics: map[string]MetricStat{"loss": {Mean: 1.0, StdDev: 0.01}}}
	wide := Group{Metrics: map[string]MetricStat{"loss": {Mean: 1.0, StdDev: 0.5}}}
	if !(wide.Widest() > tight.Widest()) {
		t.Fatalf("want the wider relative spread to rank higher: wide=%v tight=%v", wide.Widest(), tight.Widest())
	}
}

func TestWidestIgnoresZeroMeanMetric(t *testing.T) {
	g := Group{Metrics: map[string]MetricStat{
		"delta": {Mean: 0, StdDev: 3}, // would divide by zero if not skipped
		"loss":  {Mean: 1.0, StdDev: 0.2},
	}}
	if got := g.Widest(); math.Abs(got-0.2) > 1e-9 {
		t.Fatalf("want the zero-mean metric skipped and loss's cv (0.2) used, got %v", got)
	}
}

func TestWidestOfNoRepeatsGroupRanksLast(t *testing.T) {
	g := Group{NoRepeats: true}
	if got := g.Widest(); got >= 0 {
		t.Fatalf("a group with nothing measurable should rank below any group that has a spread, got %v", got)
	}
}
