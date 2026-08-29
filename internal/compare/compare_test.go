package compare

import (
	"encoding/json"
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
	res := Runs(a, b)
	if !res.SameExperiment {
		t.Fatal("provenance must not affect the fingerprint")
	}
	for _, f := range res.Fields {
		if f.Kind != KindProvenance {
			t.Fatalf("unexpected %s difference: %+v", f.Kind, res.Fields)
		}
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
	if res.Fields[0].A != "0" || res.Fields[0].B != "" {
		t.Fatalf("a recorded zero must not read the same as an absent metric: %+v", res.Fields[0])
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
			if got[j] != first[j] {
				t.Fatal("diff field order is not deterministic")
			}
		}
	}
}
