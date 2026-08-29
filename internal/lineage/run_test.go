package lineage

import (
	"errors"
	"testing"
	"time"
)

func base() Run {
	return Run{Project: "demo", GitCommit: "abc123", ConfigHash: "cfg1", Seed: 7}
}

func TestFingerprintIsStableAcrossParamOrdering(t *testing.T) {
	// Go randomizes map iteration, so this is the test that would flake if
	// Compute stopped sorting. Repeat enough to make a regression certain.
	a := base()
	a.Params = map[string]string{"lr": "3e-4", "batch": "32", "warmup": "100", "clip": "0.2"}
	want := a.Compute()
	for i := 0; i < 200; i++ {
		if got := a.Compute(); got != want {
			t.Fatalf("fingerprint not stable across iterations: %s != %s", got, want)
		}
	}
}

func TestFingerprintIgnoresProvenance(t *testing.T) {
	a, b := base(), base()
	b.Host = "other-host"
	b.Device = "cuda"
	b.Status = StatusFailed
	b.StartedAt = time.Now()
	b.Metrics = map[string]float64{"loss": 0.4}
	if a.Compute() != b.Compute() {
		t.Fatal("provenance changed the fingerprint; the same experiment must fingerprint identically whatever its outcome")
	}
}

func TestFingerprintDistinguishesIdentityFields(t *testing.T) {
	for name, mutate := range map[string]func(*Run){
		"project":         func(r *Run) { r.Project = "other" },
		"git commit":      func(r *Run) { r.GitCommit = "def456" },
		"dirty flag":      func(r *Run) { r.GitDirty = true },
		"config hash":     func(r *Run) { r.ConfigHash = "cfg2" },
		"dataset version": func(r *Run) { r.DatasetVersion = "v2" },
		"model version":   func(r *Run) { r.ModelVersion = "m2" },
		"seed":            func(r *Run) { r.Seed = 8 },
		"param value":     func(r *Run) { r.Params = map[string]string{"lr": "1e-4"} },
	} {
		t.Run(name, func(t *testing.T) {
			a, b := base(), base()
			mutate(&b)
			if a.Compute() == b.Compute() {
				t.Fatalf("%s did not change the fingerprint", name)
			}
		})
	}
}

func TestFieldBoundariesAreNotAmbiguous(t *testing.T) {
	// Without length-prefixing, ("ab","c") and ("a","bc") hash identically.
	a, b := base(), base()
	a.DatasetVersion, a.ModelVersion = "ab", "c"
	b.DatasetVersion, b.ModelVersion = "a", "bc"
	if a.Compute() == b.Compute() {
		t.Fatal("adjacent fields are not delimited in the hash")
	}
}

func TestValidateRejectsUnreconstructibleRuns(t *testing.T) {
	r := base()
	r.GitDirty = true
	r.ConfigHash = ""
	if err := r.Validate(); !errors.Is(err, ErrDirtyTree) {
		t.Fatalf("want ErrDirtyTree, got %v", err)
	}
}

func TestValidateRejectsBadInput(t *testing.T) {
	for name, mutate := range map[string]func(*Run){
		"no project":     func(r *Run) { r.Project = "" },
		"no commit":      func(r *Run) { r.GitCommit = "" },
		"unknown status": func(r *Run) { r.Status = "elsewhere" },
		"backwards time": func(r *Run) {
			r.StartedAt = time.Now()
			endedAt := r.StartedAt.Add(-time.Hour)
			r.EndedAt = &endedAt
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := base()
			mutate(&r)
			if err := r.Validate(); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
}

func TestValidateAcceptsAGoodRun(t *testing.T) {
	r := base()
	r.Status = StatusSucceeded
	r.StartedAt = time.Now().Add(-time.Minute)
	endedAt := time.Now()
	r.EndedAt = &endedAt
	if err := r.Validate(); err != nil {
		t.Fatalf("valid run rejected: %v", err)
	}
}
