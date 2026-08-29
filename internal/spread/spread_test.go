package spread

import (
	"math"
	"testing"

	"github.com/kornsour/run-ledger/internal/lineage"
)

// run builds a terminal (succeeded) run: the common case these tests want,
// now that One excludes anything else. Tests of the in-flight/terminal
// split itself override Status explicitly.
func run(id string, metrics map[string]float64) lineage.Run {
	r := lineage.Run{Project: "p", GitCommit: "abc", ConfigHash: "cfg", Seed: 1, RunID: id, Status: lineage.StatusSucceeded, Metrics: metrics}
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

// TestInFlightRunExcludedFromStats pins ADR 0012: a non-terminal run's
// metric must not join the group's min/max/mean/stddev, even though the
// finished runs beside it still report a repeat.
func TestInFlightRunExcludedFromStats(t *testing.T) {
	a := run("a", map[string]float64{"loss": 0.40})
	b := run("b", map[string]float64{"loss": 0.50})
	c := run("c", map[string]float64{"loss": 99.0})
	c.Status = lineage.StatusRunning
	a.Fingerprint, b.Fingerprint, c.Fingerprint = "fp", "fp", "fp"

	g := One("fp", []lineage.Run{a, b, c})
	m, ok := g.Metrics["loss"]
	if !ok {
		t.Fatal("want a loss entry")
	}
	if m.Count != 2 {
		t.Fatalf("the running run's metric must not be counted, want count 2, got %d", m.Count)
	}
	if m.Max != 0.50 {
		t.Fatalf("the running run's 99.0 must not enter the stats, got max %v", m.Max)
	}
}

// TestInFlightCountReported checks the other half of ADR 0012: the
// in-flight runs excluded from stats are not dropped silently, they are
// reported as a count alongside the terminal group.
func TestInFlightCountReported(t *testing.T) {
	a := run("a", map[string]float64{"loss": 0.40})
	b := run("b", map[string]float64{"loss": 0.50})
	c := run("c", map[string]float64{"loss": 0.60})
	c.Status = lineage.StatusCreated
	d := run("d", map[string]float64{"loss": 0.70})
	d.Status = lineage.StatusRunning
	a.Fingerprint, b.Fingerprint, c.Fingerprint, d.Fingerprint = "fp", "fp", "fp", "fp"

	g := One("fp", []lineage.Run{a, b, c, d})
	if g.Count != 2 {
		t.Fatalf("want 2 terminal runs counted, got %d", g.Count)
	}
	if g.InFlight != 2 {
		t.Fatalf("want 2 in-flight runs reported, got %d", g.InFlight)
	}
}

// TestGroupOfOnlyInFlightRuns checks a fingerprint that has not produced a
// single finished measurement yet: it must not error or panic, it reports
// no_repeats (0 terminal runs is fewer than 2) with the in-flight runs
// still visible via InFlight rather than vanishing entirely.
func TestGroupOfOnlyInFlightRuns(t *testing.T) {
	a := run("a", map[string]float64{"loss": 0.40})
	a.Status = lineage.StatusRunning
	b := run("b", map[string]float64{"loss": 0.50})
	b.Status = lineage.StatusCreated
	a.Fingerprint, b.Fingerprint = "fp", "fp"

	g := One("fp", []lineage.Run{a, b})
	if g.Count != 0 {
		t.Fatalf("want 0 terminal runs, got %d", g.Count)
	}
	if g.InFlight != 2 {
		t.Fatalf("want both in-flight runs reported, got %d", g.InFlight)
	}
	if !g.NoRepeats {
		t.Fatal("a group with no finished runs has nothing measurable, want no_repeats")
	}
	if g.Metrics != nil {
		t.Fatalf("no terminal runs means no stats to report, got %+v", g.Metrics)
	}
	if len(g.RunIDs) != 0 {
		t.Fatalf("run_ids tracks only terminal runs, got %v", g.RunIDs)
	}
}

// TestInFlightRunReducesGroupBelowTwoStillNoRepeats checks that filtering
// by status, not just raw run count, decides no_repeats: two runs recorded
// under one fingerprint is not a repeat once only one of them is terminal.
func TestInFlightRunReducesGroupBelowTwoStillNoRepeats(t *testing.T) {
	a := run("a", map[string]float64{"loss": 0.40})
	b := run("b", map[string]float64{"loss": 0.50})
	b.Status = lineage.StatusRunning
	a.Fingerprint, b.Fingerprint = "fp", "fp"

	g := One("fp", []lineage.Run{a, b})
	if !g.NoRepeats {
		t.Fatal("2 raw runs but only 1 terminal is not a repeat, want no_repeats")
	}
	if g.Count != 1 {
		t.Fatalf("want 1 terminal run counted, got %d", g.Count)
	}
	if g.InFlight != 1 {
		t.Fatalf("want the running run reported as in-flight, got %d", g.InFlight)
	}
	if g.Metrics != nil {
		t.Fatalf("a no-repeats group must not report metric stats, got %+v", g.Metrics)
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
