package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kornsour/run-ledger/internal/compare"
)

// The verdict a diff prints must come from SameExperiment, not from whether
// any field happened to render. Those two disagree for a real input -- see
// TestRenderDiffDifferentExperimentWithNoRenderedFields -- and the earlier
// ordering reported the opposite of what the server said.
func TestRenderDiffVerdictFollowsSameExperiment(t *testing.T) {
	field := []compare.Field{{Name: "seed", Kind: compare.KindIdentity, A: "1", B: "2"}}

	for _, tc := range []struct {
		name   string
		res    compare.Result
		want   string
		reject string
	}{
		{
			name: "same experiment with differing fields",
			res:  compare.Result{SameExperiment: true, Fields: field},
			want: "same experiment (fingerprints match)",
		},
		{
			name: "different experiments with differing fields",
			res:  compare.Result{SameExperiment: false, Fields: field},
			want: "different experiments (fingerprints differ)",
		},
		{
			name: "same experiment, nothing differs",
			res:  compare.Result{SameExperiment: true},
			want: "the two records are identical",
		},
		{
			name:   "different experiments, nothing renders",
			res:    compare.Result{SameExperiment: false},
			want:   "different experiments (fingerprints differ)",
			reject: "identical",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			renderDiff(&buf, tc.res)
			if got := buf.String(); !strings.Contains(got, tc.want) {
				t.Errorf("want %q in output, got:\n%s", tc.want, got)
			}
			if tc.reject != "" && strings.Contains(buf.String(), tc.reject) {
				t.Errorf("output must not claim %q:\n%s", tc.reject, buf.String())
			}
		})
	}
}

// The regression, stated as the situation that produces it. Two runs whose
// params are {} and {"foo": ""} hash differently -- Run.Compute writes only
// the keys present -- while compare.Runs reads both sides as "" and emits
// no field. The server answers same_experiment:false with fields:null, and
// the CLI used to print "the two records are identical".
func TestRenderDiffDifferentExperimentWithNoRenderedFields(t *testing.T) {
	var buf bytes.Buffer
	renderDiff(&buf, compare.Result{A: "run-a", B: "run-b", SameExperiment: false})
	out := buf.String()

	if strings.Contains(out, "identical") {
		t.Fatalf("differing fingerprints reported as identical:\n%s", out)
	}
	if !strings.Contains(out, "different experiments (fingerprints differ)") {
		t.Errorf("want the differing-fingerprint verdict, got:\n%s", out)
	}
	// The empty table would be useless on its own; the reader needs to know
	// why a real difference has nothing to show.
	if !strings.Contains(out, "empty string") {
		t.Errorf("want an explanation of the absent-versus-empty param case, got:\n%s", out)
	}
	if strings.Contains(out, "FIELD") {
		t.Errorf("no rows to show, so no table header should print:\n%s", out)
	}
}

func TestRenderDiffUnattributableNoteOnlyWhenFlagged(t *testing.T) {
	metric := []compare.Field{{Name: "metrics.loss", Kind: compare.KindMetric, A: "0.42", B: "0.51"}}
	const note = "not captured in the record"

	var flagged bytes.Buffer
	renderDiff(&flagged, compare.Result{SameExperiment: true, Fields: metric, Unattributable: true})
	if !strings.Contains(flagged.String(), note) {
		t.Errorf("want the unattributable note, got:\n%s", flagged.String())
	}

	var plain bytes.Buffer
	renderDiff(&plain, compare.Result{SameExperiment: false, Fields: metric})
	if strings.Contains(plain.String(), note) {
		t.Errorf("a recorded difference must not be called unattributable:\n%s", plain.String())
	}
}

// An absent value renders as an em dash so it is not mistaken for a value
// the run actually had.
func TestRenderDiffMarksAbsentValues(t *testing.T) {
	var buf bytes.Buffer
	renderDiff(&buf, compare.Result{
		SameExperiment: true,
		Fields:         []compare.Field{{Name: "metrics.auc", Kind: compare.KindMetric, A: "0.94", B: ""}},
	})
	if !strings.Contains(buf.String(), "—") {
		t.Errorf("want an em dash for the absent metric, got:\n%s", buf.String())
	}
}
