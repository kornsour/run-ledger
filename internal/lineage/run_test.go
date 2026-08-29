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

// TestComputeCollapsesEquivalentNumericSpellings pins the bug in issue #63:
// rlctl sends whatever literal a caller typed on the command line, and the
// Python client sends str() of an actual float, so the exact same
// hyperparameter arrives as three different strings depending on which
// client recorded it and how the number was typed. Before normalization,
// each spelling below produced a different fingerprint, so the same
// experiment recorded twice never grouped and spread/unattributable never
// fired -- a false negative, and a silent one.
func TestComputeCollapsesEquivalentNumericSpellings(t *testing.T) {
	spellings := []string{"3e-4", "0.0003", "0.00030", "3.0e-4", "30e-5"}
	want := ""
	for _, s := range spellings {
		r := base()
		r.Params = map[string]string{"lr": s}
		got := r.Compute()
		if want == "" {
			want = got
			continue
		}
		if got != want {
			t.Errorf("lr=%q fingerprinted as %s, want %s (same fingerprint as the other spellings)", s, got, want)
		}
	}
}

// TestComputeStillDistinguishesDifferentNumbers guards against a
// normalization bug in the other direction: collapsing spellings of the
// same number must not accidentally collapse genuinely different numbers
// too.
func TestComputeStillDistinguishesDifferentNumbers(t *testing.T) {
	a, b := base(), base()
	a.Params = map[string]string{"lr": "3e-4"}
	b.Params = map[string]string{"lr": "3e-5"}
	if a.Compute() == b.Compute() {
		t.Fatal("3e-4 and 3e-5 are different learning rates and must not fingerprint identically")
	}
}

// TestComputeLeavesNonNumericParamsUntouched is the negative case Compute
// must get right for normalization to be safe: a param value that isn't a
// number is identity-bearing exactly as written, and normalizeParamValue
// must never rewrite it.
func TestComputeLeavesNonNumericParamsUntouched(t *testing.T) {
	values := map[string]string{
		"git-sha-shaped":   "a1b2c3d",
		"path":             "/mnt/data/v2",
		"empty":            "",
		"boolean spelling": "true",
	}
	for name, v := range values {
		t.Run(name, func(t *testing.T) {
			if got := normalizeParamValue(v); got != v {
				t.Fatalf("normalizeParamValue(%q) = %q, want it unchanged", v, got)
			}
			// Compute must actually route through normalizeParamValue for
			// these, not just leave them untouched by accident of never
			// being called -- distinguish it from base() (no params).
			a := base()
			b := base()
			b.Params = map[string]string{"p": v}
			if a.Compute() == b.Compute() {
				t.Fatalf("adding param %q=%q did not change the fingerprint", "p", v)
			}
		})
	}
}

// TestNormalizeParamValueHandlesEdgeCasesDeliberately covers the cases the
// issue specifically called out: overflow, the two non-finite spellings,
// zero-padding, and Go's numeric-literal underscore separator. Each is
// either normalized to a well-defined canonical form or passed through
// completely unchanged -- never silently reinterpreted.
func TestNormalizeParamValueHandlesEdgeCasesDeliberately(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// The three spellings from the issue, plus a couple more, all
		// collapse to one canonical form.
		{"exponent form", "3e-4", "0.0003"},
		{"decimal form", "0.0003", "0.0003"},
		{"trailing zero", "0.00030", "0.0003"},
		{"plain integer unchanged in value", "100", "100"},
		{"trailing .0 dropped", "100.0", "100"},

		// Overflow: syntactically a number, but strconv.ParseFloat cannot
		// represent it -- left exactly as written rather than collapsed to
		// "+Inf".
		{"overflow", "1e400", "1e400"},
		// Underflow: the mirror case ParseFloat gives no error for at all
		// -- also left exactly as written rather than collapsed to "0".
		{"underflow", "1e-400", "1e-400"},

		// Not numbers as far as this function is concerned, even though
		// ParseFloat itself would happily accept every one of them.
		{"NaN passes through", "NaN", "NaN"},
		{"Inf passes through", "Inf", "Inf"},
		{"signed Inf passes through", "-Inf", "-Inf"},
		{"Infinity passes through", "Infinity", "Infinity"},

		// Go-source numeric-literal leniency that a param value must not
		// inherit: an underscore digit separator, and a leading zero.
		{"underscore separator untouched", "1_000", "1_000"},
		{"zero-padded untouched", "007", "007"},

		// Signed zero collapses to one spelling -- "-0" and "0" are the
		// same hyperparameter value.
		{"negative zero canonicalized", "-0", "0"},
		{"negative zero with fraction canonicalized", "-0.0", "0"},

		// Not numeric at all.
		{"identifier untouched", "run-42", "run-42"},
		{"empty string untouched", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := normalizeParamValue(c.in); got != c.want {
				t.Fatalf("normalizeParamValue(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestComputeNormalizationRoundTripsThroughIdenticalSpellings is a smaller,
// direct check that Compute (not just normalizeParamValue in isolation)
// applies normalization to every param, not just one hardcoded key.
func TestComputeNormalizationRoundTripsThroughIdenticalSpellings(t *testing.T) {
	a, b := base(), base()
	a.Params = map[string]string{"lr": "3e-4", "warmup_frac": "007", "batch": "32.0"}
	b.Params = map[string]string{"lr": "0.0003", "warmup_frac": "007", "batch": "32"}
	if a.Compute() != b.Compute() {
		t.Fatalf("expected normalized params to fingerprint identically: a=%s b=%s", a.Compute(), b.Compute())
	}
}

// TestFingerprintVersionIsStampedTogetherWithFingerprint documents the
// invariant callers must keep: Fingerprint and FingerprintVersion always
// describe the same computation. Compute doesn't set FingerprintVersion
// itself (it has no side effect on r, and returns only the hash), so this
// pins that CurrentFingerprintVersion is the version a fresh Compute call
// corresponds to -- the constant api.record and every Store test fixture
// must pair with a freshly computed Fingerprint.
func TestFingerprintVersionIsStampedTogetherWithFingerprint(t *testing.T) {
	if CurrentFingerprintVersion == FingerprintVersionLegacy {
		t.Fatal("CurrentFingerprintVersion must differ from FingerprintVersionLegacy, or a legacy record and a freshly computed one become indistinguishable")
	}
	// A legacy run whose stored Fingerprint was never recomputed keeps
	// whatever value it already had -- Compute must not be consulted to
	// "verify" or "upgrade" it. This is a documentation test: there is no
	// API that recomputes a stored fingerprint, which is the point.
	legacy := base()
	legacy.Params = map[string]string{"lr": "3e-4"}
	legacy.Fingerprint = "some-fingerprint-hashed-under-the-old-contract"
	legacy.FingerprintVersion = FingerprintVersionLegacy
	if legacy.Fingerprint == legacy.Compute() {
		t.Fatal("test setup: the legacy fixture's stored fingerprint should not equal a fresh Compute() by coincidence")
	}
	if legacy.Fingerprint != "some-fingerprint-hashed-under-the-old-contract" {
		t.Fatal("a version-1 record's stored fingerprint must not be reinterpreted or recomputed")
	}
}
