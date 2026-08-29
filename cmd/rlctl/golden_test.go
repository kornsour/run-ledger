package main

// This file is the golden-file harness for every rendered surface in
// main.go (renderDiff, renderSpreadGroup, renderSpreadList, renderList,
// renderShow). See issue #79 item 3: strings.Contains, which the rest of
// this package's tests use, cannot see a dangling comma, a misaligned
// column, or a value that silently vanished -- exactly the shape of the
// `job_id   , slurm-7` defect that motivated this file. Golden tests compare
// the literal bytes a renderer writes, so any of those failure modes shows
// up as a diff instead of passing silently.
//
// To add or change a case: write the *_test.go code, then run
//
//	go test ./cmd/rlctl -run TestGolden -update
//
// which writes cmd/rlctl/testdata/<name>.golden from the renderer's current
// output, and re-run `go test ./cmd/rlctl` (without -update) to confirm it
// now passes. -update trusts the renderer, not the reviewer: it will happily
// commit a regression's output as the new "correct" answer, so a diff to a
// .golden file is a deliberate change to a user-facing surface and must be
// read line by line in review, not rubber-stamped because "tests pass" --
// the tests passing is exactly what -update just made true by construction.

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kornsour/run-ledger/internal/compare"
	"github.com/kornsour/run-ledger/internal/lineage"
	"github.com/kornsour/run-ledger/internal/spread"
)

var update = flag.Bool("update", false, "update golden files")

// checkGolden compares got against testdata/<name>.golden, or writes it when
// -update is passed. name has no extension and no directory: every case
// picks one so a reviewer can find the file a failing test names without
// guessing a path.
func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("update golden %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run `go test ./cmd/rlctl -run TestGolden -update` to create it, then review the new file before committing)", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("golden mismatch for %s\n--- got ---\n%s\n--- want (%s) ---\n%s", path, got, path, want)
	}
}

// withUTCLocal points time.Local at UTC for the duration of a test and
// restores it on cleanup. renderList formats timestamps with .Local(), which
// reads the process-wide time.Local -- left alone, a golden fixture would
// pass on the machine that generated it and fail on any other in a
// different timezone (a developer's laptop vs. CI). time.Local is an
// ordinary package variable, not fixed at process start, so reassigning it
// here is enough; nothing about renderList's own code changes.
func withUTCLocal(t *testing.T) {
	t.Helper()
	orig := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = orig })
}

func ptrGolden(s string) *string { return &s }

// TestGoldenRenderDiff covers `rlctl diff`'s field table across the awkward
// cases: a value neither run recorded, a value recorded as "", a metric of
// exactly 0, a value long enough to overflow its column, and the two
// no-rendered-fields verdicts (identical vs. drifted-apart). Each subtest
// name is also the golden file's basename.
func TestGoldenRenderDiff(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  compare.Result
	}{
		{
			// Nothing differs at all: the short-circuit branch in renderDiff,
			// distinct from "different experiments, nothing rendered" below.
			name: "diff_identical",
			res:  compare.Result{A: "run-a", B: "run-a", SameExperiment: true},
		},
		{
			// The regression this package's own comment on renderDiff
			// documents: fingerprints differ but no field rendered. Kept here
			// too, as a golden, so the exact wording is pinned byte-for-byte
			// and not just checked with strings.Contains.
			name: "diff_different_no_fields",
			res:  compare.Result{A: "run-a", B: "run-b", SameExperiment: false},
		},
		{
			// One side never recorded the field at all (nil); the other
			// recorded it as the empty string. cell() must print these two
			// differently -- see TestAbsentNeverRendersLikeEmpty for the
			// assertion, this golden for the exact bytes.
			name: "diff_absent_vs_empty",
			res: compare.Result{
				A: "run-a", B: "run-b", SameExperiment: true,
				Fields: []compare.Field{
					{Name: "metrics.auc", Kind: compare.KindMetric, A: ptrGolden("0.94"), B: nil},
					{Name: "params.tag", Kind: compare.KindIdentity, A: nil, B: ptrGolden("")},
				},
			},
		},
		{
			// A metric of exactly 0 is a real measurement, not a missing
			// one, and must print as "0", not as an em dash or a blank cell.
			name: "diff_metric_zero",
			res: compare.Result{
				A: "run-a", B: "run-b", SameExperiment: true,
				Fields: []compare.Field{
					{Name: "metrics.loss", Kind: compare.KindMetric, A: ptrGolden("0"), B: ptrGolden("0.5")},
				},
			},
		},
		{
			// A value long enough to overflow its %-20s column. Printf does
			// not truncate a value wider than its field width, so this row's
			// own columns run long -- captured here so a future change that
			// starts truncating (or that fixes the misalignment some other
			// way) shows up as an intentional golden diff, not a surprise.
			name: "diff_long_value",
			res: compare.Result{
				A: "run-a", B: "run-b", SameExperiment: false,
				Fields: []compare.Field{
					{Name: "config_hash", Kind: compare.KindIdentity,
						A: ptrGolden("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
						B: ptrGolden("sha256:bbbb")},
					{Name: "seed", Kind: compare.KindIdentity, A: ptrGolden("1"), B: ptrGolden("2")},
				},
			},
		},
		{
			// Same experiment, but a metric still differs: the "something
			// unrecorded explains this" note must appear.
			name: "diff_unattributable",
			res: compare.Result{
				A: "run-a", B: "run-b", SameExperiment: true, Unattributable: true,
				Fields: []compare.Field{
					{Name: "metrics.loss", Kind: compare.KindMetric, A: ptrGolden("0.42"), B: ptrGolden("0.51")},
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			renderDiff(&buf, tc.res)
			checkGolden(t, tc.name, buf.Bytes())
		})
	}
}

// TestGoldenRenderSpreadGroup covers `rlctl spread <fingerprint>`: the
// no-repeats short-circuit, a group with no provenance disagreement, a group
// with several (mixing an unrecorded value into the list -- the exact shape
// of the shipped defect), and a metric/field name long enough to overflow
// its column.
func TestGoldenRenderSpreadGroup(t *testing.T) {
	for _, tc := range []struct {
		name string
		g    spread.Group
	}{
		{
			name: "spread_no_repeats",
			g:    spread.Group{Fingerprint: "fp-solo", Count: 1, NoRepeats: true},
		},
		{
			// Metrics vary but every run agrees on provenance: no
			// "provenance differs" section at all.
			name: "spread_no_provenance",
			g: spread.Group{
				Fingerprint: "fp-agree", Count: 3,
				Metrics: map[string]spread.MetricStat{
					"loss": {Count: 3, Min: 0.40, Max: 0.55, Mean: 0.48, StdDev: 0.06},
				},
			},
		},
		{
			// Several provenance fields disagree at once, one of them
			// (job_id) with an unrecorded value mixed into the recorded
			// ones -- this is the shape of the shipped defect: before
			// provenanceValue existed, the unrecorded "" rendered as
			// nothing and left a dangling comma.
			name: "spread_provenance_several",
			g: spread.Group{
				Fingerprint: "fp-disagree", Count: 3,
				Metrics: map[string]spread.MetricStat{
					"loss":     {Count: 3, Min: 0.40, Max: 0.55, Mean: 0.48, StdDev: 0.06},
					"accuracy": {Count: 3, Min: 0, Max: 0, Mean: 0, StdDev: 0},
				},
				Provenance: []spread.ProvenanceDiff{
					{Field: "device", Values: []string{"cpu", "cuda"}},
					{Field: "job_id", Values: []string{"", "slurm-7"}},
					{Field: "submitter_claim", Values: []string{"", "alice", "bob"}},
				},
			},
		},
		{
			// A metric key and a provenance value both long enough to
			// overflow their columns, exercised together the way
			// diff_long_value does for renderDiff.
			name: "spread_long_value_alignment",
			g: spread.Group{
				Fingerprint: "fp-long", Count: 2,
				Metrics: map[string]spread.MetricStat{
					"validation_accuracy_at_end_of_epoch_ten": {Count: 2, Min: 0.9, Max: 0.91, Mean: 0.905, StdDev: 0.005},
				},
				Provenance: []spread.ProvenanceDiff{
					{Field: "framework_version", Values: []string{
						"torch==2.4.1+cu121-nightly-build-2024-08-15-abcdefabcdef",
						"torch==2.5.0",
					}},
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			renderSpreadGroup(&buf, tc.g)
			checkGolden(t, tc.name, buf.Bytes())
		})
	}
}

// TestGoldenRenderSpreadList covers `rlctl spread` with no fingerprint (the
// ranked-listing form): no groups at all, a fingerprint long enough to be
// truncated by trunc(), and a mix of groups with and without a provenance
// disagreement (the "—" placeholder column).
func TestGoldenRenderSpreadList(t *testing.T) {
	for _, tc := range []struct {
		name   string
		groups []spread.Group
	}{
		{name: "spreadlist_empty", groups: nil},
		{
			name: "spreadlist_basic",
			groups: []spread.Group{
				{
					Fingerprint: "fp-widest", Count: 4,
					Metrics:    map[string]spread.MetricStat{"loss": {Count: 4, Min: 0.1, Max: 0.9, Mean: 0.5, StdDev: 0.3}},
					Provenance: []spread.ProvenanceDiff{{Field: "device", Values: []string{"cpu", "cuda"}}},
				},
				{
					// No disagreement: provenanceFields must print "—", not
					// an empty column that would misalign the row after it.
					Fingerprint: "fp-narrow", Count: 2,
					Metrics: map[string]spread.MetricStat{"loss": {Count: 2, Min: 0.49, Max: 0.51, Mean: 0.5, StdDev: 0.01}},
				},
			},
		},
		{
			// A fingerprint longer than trunc's 18-character cutoff.
			name: "spreadlist_long_fingerprint",
			groups: []spread.Group{
				{
					Fingerprint: "abcdef0123456789abcdef0123456789",
					Count:       2,
					Metrics:     map[string]spread.MetricStat{"loss": {Count: 2, Min: 0.4, Max: 0.5, Mean: 0.45, StdDev: 0.05}},
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			renderSpreadList(&buf, tc.groups)
			checkGolden(t, tc.name, buf.Bytes())
		})
	}
}

// TestGoldenRenderList covers `rlctl list`'s table: no runs, several runs,
// a next-page cursor hint, and project/commit values long enough for
// trunc() to cut.
func TestGoldenRenderList(t *testing.T) {
	withUTCLocal(t)
	started := time.Date(2026, 3, 4, 15, 30, 0, 0, time.UTC)

	for _, tc := range []struct {
		name       string
		runs       []lineage.Run
		nextCursor string
	}{
		{name: "list_empty"},
		{
			name: "list_basic",
			runs: []lineage.Run{
				{RunID: "run-aaaaaaaaaaaaaaaaaaaaaaaaaa", Project: "churn", GitCommit: "0123456789abcdef", Status: lineage.StatusSucceeded, StartedAt: started},
				{RunID: "run-bbbbbbbbbbbbbbbbbbbbbbbbbb", Project: "churn", GitCommit: "fedcba9876543210", Status: lineage.StatusFailed, StartedAt: started.Add(time.Hour)},
			},
		},
		{
			name:       "list_with_cursor",
			nextCursor: "opaque-cursor-value",
			runs: []lineage.Run{
				{RunID: "run-cccccccccccccccccccccccccc", Project: "churn", GitCommit: "0123456789abcdef", Status: lineage.StatusRunning, StartedAt: started},
			},
		},
		{
			// project and git_commit longer than trunc's 12/10-character
			// cutoffs, so the table stays aligned instead of one row pushing
			// every column after it to the right.
			name: "list_long_values_trunc",
			runs: []lineage.Run{
				{RunID: "run-dddddddddddddddddddddddddd", Project: "a-project-name-much-longer-than-twelve-chars", GitCommit: "0123456789abcdef0123456789abcdef", Status: lineage.StatusSucceeded, StartedAt: started},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			renderList(&buf, tc.runs, tc.nextCursor)
			checkGolden(t, tc.name, buf.Bytes())
		})
	}
}

// TestGoldenRenderShow covers `rlctl show`'s indented-JSON output.
//
// Deliberately one case, not an absent-vs-empty pair -- and not because
// renderShow falls short of an invariant it should meet. ADR 0011 decided
// that for config_hash, dataset_version, model_version, host, device,
// framework_version, and checkpoint_uri (and submitter_claim/job_id under
// the same rule per compare.go), "" and "not recorded" are one meaning with
// two spellings, not two states worth telling apart -- the ADR's own words
// are that widening these to pointers to preserve a distinction was
// considered and rejected, because no experiment has a genuinely empty
// dataset_version. So there is only one state for this surface to render,
// and this fixture is that one state, not an accepted gap.
func TestGoldenRenderShow(t *testing.T) {
	ended := time.Date(2026, 3, 4, 16, 0, 0, 0, time.UTC)
	run := lineage.Run{
		RunID: "run-eeeeeeeeeeeeeeeeeeeeeeeeeeee", Fingerprint: "fp-show", FingerprintVersion: lineage.CurrentFingerprintVersion,
		Project: "churn", GitCommit: "0123456789abcdef", GitDirty: false, ConfigHash: "cfg-1",
		DatasetVersion: "v3", ModelVersion: "v7", Seed: 42,
		Params: map[string]string{"lr": "0.0003"},
		Host:   "gpu-node-01", Device: "cuda:0", FrameworkVersion: "torch==2.5.0",
		// Empty on purpose, and correctly rendered as such: per ADR 0011,
		// "" here means "not recorded", the same claim a client that
		// omitted submitter_claim entirely would be making. This is not the
		// same "empty on purpose" as params.tag elsewhere in this file --
		// params are exempt from ADR 0011 and keep their absent/empty
		// distinction.
		SubmitterClaim: "",
		JobID:          "slurm-7",
		Status:         lineage.StatusSucceeded,
		StartedAt:      time.Date(2026, 3, 4, 15, 30, 0, 0, time.UTC),
		EndedAt:        &ended,
		Metrics:        map[string]float64{"loss": 0.42, "accuracy": 0},
	}
	var buf bytes.Buffer
	if err := renderShow(&buf, run); err != nil {
		t.Fatalf("renderShow: %v", err)
	}
	checkGolden(t, "show_basic", buf.Bytes())
}

// TestAbsentNeverRendersLikeEmpty is the cross-cutting invariant issue #79
// names directly: a value a run never recorded must not render identically
// to a value it recorded as empty. This was one bug class shipped four
// times under four different shapes (compare.Runs reading params through a
// bare map index, rlctl's old or(s, "—") helper, dashboard/app.py passing
// None into a table, and renderSpreadGroup joining an untranslated "" into
// a comma-separated list). Written once here, over every renderer that can
// make the distinction, instead of re-derived per golden case -- a future
// renderer that violates it fails this test even before anyone thinks to
// write a golden fixture for the specific case that exposes it.
func TestAbsentNeverRendersLikeEmpty(t *testing.T) {
	t.Run("cell (renderDiff's field cells)", func(t *testing.T) {
		absent, empty := cell(nil), cell(ptrGolden(""))
		if absent == empty {
			t.Fatalf("an absent field and a field recorded as \"\" rendered identically: %q", absent)
		}
	})

	t.Run("renderDiff end to end", func(t *testing.T) {
		diffResult := func(a, b *string) compare.Result {
			return compare.Result{
				A: "run-a", B: "run-b", SameExperiment: true,
				Fields: []compare.Field{{Name: "params.tag", Kind: compare.KindIdentity, A: a, B: b}},
			}
		}
		var absentBuf, emptyBuf bytes.Buffer
		renderDiff(&absentBuf, diffResult(nil, ptrGolden("x")))
		renderDiff(&emptyBuf, diffResult(ptrGolden(""), ptrGolden("x")))
		if absentBuf.String() == emptyBuf.String() {
			t.Fatalf("a never-recorded params.tag and one recorded as \"\" produced the same rlctl diff output:\n%s", absentBuf.String())
		}
	})

	// provenanceValue (and so renderSpreadGroup and its Values-list join)
	// has a narrower version of this invariant than cell does. ADR 0011
	// defines "" as meaning "not recorded" for every field
	// spread.provenanceFields tracks, by design -- there is no third,
	// separate "recorded as empty" state for provenanceValue to
	// distinguish from "absent", unlike a param in renderDiff. What the
	// shipped defect actually violated at this layer was narrower still:
	// the unrecorded marker must not itself be the empty string, because
	// strings.Join renders an empty element as nothing, producing a
	// dangling separator ("job_id   , slurm-7") that erases the fact that
	// a value was missing at all rather than saying so.
	t.Run("provenanceValue never renders the unrecorded marker as empty", func(t *testing.T) {
		if got := provenanceValue(""); got == "" {
			t.Fatalf("an unrecorded provenance value rendered as the empty string, which strings.Join would drop silently (the job_id defect this test file exists to prevent)")
		}
	})

	t.Run("provenanceValue distinguishes unrecorded from any recorded value", func(t *testing.T) {
		for _, recorded := range []string{"cpu", "0", "slurm-7"} {
			if got := provenanceValue(""); got == provenanceValue(recorded) {
				t.Fatalf("unrecorded and recorded(%q) rendered identically: %q", recorded, got)
			}
		}
	})
}
