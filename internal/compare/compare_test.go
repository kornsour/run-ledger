package compare

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/kornsour/run-ledger/internal/lineage"
)

func run(id string) lineage.Run {
	return lineage.Run{Project: "p", GitCommit: "abc", ConfigHash: "cfg", Seed: 1, RunID: id}
}

func TestSameExperimentDifferentOutcomeIsUnattributable(t *testing.T) {
	a, b := run("a"), run("b")
	a.Metrics = map[string]float64{"loss": 0.10}
	b.Metrics = map[string]float64{"loss": 0.14}
	res := Runs(a, b)
	if !res.SameExperiment {
		t.Fatal("identical identity fields must fingerprint the same")
	}
	if !res.Unattributable {
		t.Fatal("a metric difference between identical experiments must be flagged")
	}
}

func TestDifferentExperimentIsNotUnattributable(t *testing.T) {
	a, b := run("a"), run("b")
	b.Seed = 2
	a.Metrics = map[string]float64{"loss": 0.10}
	b.Metrics = map[string]float64{"loss": 0.14}
	res := Runs(a, b)
	if res.SameExperiment {
		t.Fatal("a differing seed must change the fingerprint")
	}
	if res.Unattributable {
		t.Fatal("a metric difference is attributable when the experiment differed")
	}
}

func TestProvenanceDifferenceDoesNotSplitTheExperiment(t *testing.T) {
	a, b := run("a"), run("b")
	b.Device = "cuda"
	b.Host = "gpu-01"
	b.SubmitterClaim = "bob"
	b.JobID = "ci-42"
	res := Runs(a, b)
	if !res.SameExperiment {
		t.Fatal("provenance must not affect the fingerprint")
	}
	for _, f := range res.Fields {
		if f.Kind != KindProvenance {
			t.Fatalf("unexpected %s difference: %+v", f.Kind, res.Fields)
		}
	}
	var sawSubmitter, sawJob bool
	for _, f := range res.Fields {
		switch f.Name {
		case "submitter_claim":
			sawSubmitter = true
		case "job_id":
			sawJob = true
		}
	}
	if !sawSubmitter || !sawJob {
		t.Fatalf("want submitter_claim and job_id reported as provenance differences, got %+v", res.Fields)
	}
}

func TestAbsentMetricIsNotZero(t *testing.T) {
	a, b := run("a"), run("b")
	a.Metrics = map[string]float64{"loss": 0}
	b.Metrics = map[string]float64{}
	res := Runs(a, b)
	if len(res.Fields) != 1 {
		t.Fatalf("want one difference, got %+v", res.Fields)
	}
	if got := res.Fields[0].A; got == nil || *got != "0" {
		t.Fatalf("a recorded zero must render as a value: %+v", res.Fields[0])
	}
	if res.Fields[0].B != nil {
		t.Fatalf("an absent metric must be nil, not a value: %+v", res.Fields[0])
	}
}

func TestUnattributableMarshalsAsASiblingField(t *testing.T) {
	a, b := run("a"), run("b")
	a.Metrics = map[string]float64{"loss": 0.10}
	b.Metrics = map[string]float64{"loss": 0.14}
	res := Runs(a, b)

	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	// unattributable belongs on Result itself, alongside a/b/same_experiment/
	// fields -- not nested under a separate wrapper the way GET /compare used
	// to shape the HTTP response.
	if _, ok := got["a"]; !ok {
		t.Fatalf("want a top-level, got %v", got)
	}
	unattributable, ok := got["unattributable"].(bool)
	if !ok || !unattributable {
		t.Fatalf("want unattributable: true as a top-level field, got %v", got)
	}
}

func TestFieldOrderIsDeterministic(t *testing.T) {
	a, b := run("a"), run("b")
	a.Params = map[string]string{"lr": "1", "batch": "2", "clip": "3", "warmup": "4"}
	b.Params = map[string]string{"lr": "9", "batch": "8", "clip": "7", "warmup": "6"}
	first := Runs(a, b).Fields
	for i := 0; i < 100; i++ {
		got := Runs(a, b).Fields
		for j := range first {
			// reflect.DeepEqual, not ==: Field carries *string, so ==
			// would compare pointer identity and fail on every call.
			if !reflect.DeepEqual(got[j], first[j]) {
				t.Fatal("diff field order is not deterministic")
			}
		}
	}
}

// The bug this presence handling exists for. Two runs whose params are {}
// and {"foo": ""} fingerprint differently, because Run.Compute writes only
// the keys that are present. Reading both sides through a bare map index
// yielded "" for each and emitted no field at all, so the API answered
// same_experiment:false with fields:null and rlctl called them identical.
func TestParamAbsentIsNotParamEmpty(t *testing.T) {
	a, b := run("a"), run("b")
	a.Params = nil
	b.Params = map[string]string{"foo": ""}

	res := Runs(a, b)
	if res.SameExperiment {
		t.Fatal("an absent param and an empty one hash differently, so these are different experiments")
	}
	var got *Field
	for i := range res.Fields {
		if res.Fields[i].Name == "params.foo" {
			got = &res.Fields[i]
		}
	}
	if got == nil {
		t.Fatalf("want params.foo reported as a difference, got %+v", res.Fields)
	}
	if got.A != nil {
		t.Errorf("a run that never set the param must report nil, got %q", *got.A)
	}
	if got.B == nil || *got.B != "" {
		t.Errorf("a run that set the param to empty must report a value, got %v", got.B)
	}
}

// "" and "not recorded" are the same claim for the scalar fields, so the
// diff reports the empty one as absent rather than as a value of "".
// ADR 0011 is what licenses that normalization.
func TestEmptyScalarFieldsReportAsAbsent(t *testing.T) {
	a, b := run("a"), run("b")
	a.Device = "cuda"
	b.Device = ""

	for _, f := range Runs(a, b).Fields {
		if f.Name != "device" {
			continue
		}
		if f.B != nil {
			t.Fatalf("an unrecorded device must report nil, got %q", *f.B)
		}
		return
	}
	t.Fatal("want device reported as a difference")
}

// submitter_claim and job_id follow the same rule as device -- ADR 0015
// extends ADR 0011's "empty means not recorded" policy to them.
func TestEmptyAttributionFieldsReportAsAbsent(t *testing.T) {
	a, b := run("a"), run("b")
	a.SubmitterClaim, a.JobID = "alice", "ci-1"
	b.SubmitterClaim, b.JobID = "", ""

	fields := Runs(a, b).Fields
	for _, name := range []string{"submitter_claim", "job_id"} {
		var found *Field
		for i := range fields {
			if fields[i].Name == name {
				found = &fields[i]
			}
		}
		if found == nil {
			t.Fatalf("want %s reported as a difference, got %+v", name, fields)
		}
		if found.B != nil {
			t.Fatalf("an unrecorded %s must report nil, got %q", name, *found.B)
		}
	}
}

// compare must not answer "same experiment" independently of the stored
// fingerprint the rest of the ledger groups by. Recomputing here gave a
// second answer to the same question, which stays invisible only while
// Run.Compute never changes -- and ADR 0004 exists because it will.
func TestSameExperimentPrefersTheStoredFingerprint(t *testing.T) {
	a, b := run("a"), run("b")
	a.Fingerprint, b.Fingerprint = "stored-fp", "stored-fp"
	a.Seed, b.Seed = 1, 2 // Compute would disagree; the store is authoritative.

	if !SameExperiment(a, b) {
		t.Error("want the stored fingerprint to decide when both runs carry one")
	}
	if !Runs(a, b).SameExperiment {
		t.Error("Runs must use the same source of truth as SameExperiment")
	}

	// A run that has not been through a store has no fingerprint to trust.
	c, d := run("c"), run("d")
	d.Seed = 99
	if SameExperiment(c, d) {
		t.Error("want Compute as the fallback when a fingerprint is missing")
	}
}
