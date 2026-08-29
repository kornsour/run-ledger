package lineage

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// This file adds metamorphic / property-based tests on Run.Compute,
// alongside the example-based tests in run_test.go. See GitHub issue #79
// item 2: every existing test on Compute was "this input gives this hash";
// the defects that have escaped this codebase so far all lived in inputs
// nobody happened to write down. These tests instead assert relationships
// that must hold for *all* inputs, and generate the inputs rather than
// hand-picking them.
//
// Every generator here is seeded from propertySeed, a single fixed
// constant. A property test whose failure can't be reproduced is worse
// than no test at all -- per the issue, a flake that fails one run in fifty
// erodes trust in CI faster than it catches bugs. Fixing the seed makes
// every run of this file, on any machine, walk the exact same sequence of
// generated Runs, spellings, and field mutations. newPropertyRand logs the
// seed on every call so a failure's test output names the exact constant
// to hardcode if you ever needed to isolate one generated case by hand
// (t.Log output is only shown by `go test` for a failing test, or under
// -v, so this costs nothing when everything passes).

const propertySeed = 20260829 // arbitrary; fixed is the only requirement

func newPropertyRand(t *testing.T) *rand.Rand {
	t.Helper()
	t.Logf("property test seed: %d (fixed and deterministic; rerun with this exact seed to reproduce any failure below)", propertySeed)
	return rand.New(rand.NewPCG(propertySeed, propertySeed))
}

// --- identity/provenance classification, derived from the struct itself ---
//
// Item 5 asks for this split to come from lineage.Run's fields, not a
// hand-written assumption baked into each test -- so that a provenance
// field added later is automatically covered, and a field that starts
// accidentally feeding the hash is caught without anyone remembering to
// update a test.
//
// Three ways to express the boundary were considered:
//
//  1. A struct tag on each field (e.g. `lineage:"identity"`). This would be
//     the most machine-checkable option, but it requires editing run.go,
//     which this task is explicitly scoped not to touch -- and even if it
//     weren't, a tag is just as capable of drifting from what Compute
//     actually hashes as anything else here: nothing stops someone from
//     adding a field, tagging it "provenance", and also (by mistake) adding
//     it to Compute's write() call. A tag only moves the hand-maintained
//     source of truth from a test file to the struct; it doesn't remove the
//     need for one.
//  2. Parsing the "--- identity ---" / "--- provenance ---" boundary
//     comment out of run.go via go/ast. This sounds the most "automatic",
//     but a comment is not a language-level contract: nothing stops it from
//     drifting out of sync with the fields around it after a refactor, a
//     parser has to make an editorial judgment call about which comment
//     line is "the" boundary, and the payoff over option 3 is small since
//     both still require a human to keep two things in agreement.
//  3. Two explicit field-name whitelists in this test file, one mirroring
//     exactly what Compute's write() call and Params loop hash
//     (identityFieldNames) and one covering everything else
//     (provenanceFieldNames) -- with a separate test
//     (TestProperty_EveryRunFieldIsClassified) asserting every field on
//     Run appears in exactly one of the two, and failing loudly if a field
//     appears in neither.
//
// Option 3 is what's implemented below, because it's the only one of the
// three whose failure mode actually matches the goal. Consider the case
// this exists to catch: someone adds a new field to Run, hashes it inside
// Compute, but forgets to touch this test file. identityFieldNames doesn't
// contain the new field's name, so TestProperty_EveryRunFieldIsClassified
// fails immediately and loudly ("unclassified field") -- it can't be
// silently treated as provenance, because the classification test runs
// before either mutation test does. Now suppose instead they add it to
// provenanceFieldNames by mistake, believing it's not hashed, when in fact
// it is: TestProperty_MutatingAProvenanceFieldLeavesFingerprintUnchanged
// mutates it, calls the *real* Compute, and fails because the fingerprint
// actually changed. Either mistake is caught by exercising real behavior,
// not by the whitelist agreeing with itself -- a struct tag or a parsed
// comment would only have prevented the first mistake, not the second,
// since both of those approaches would have happily believed whatever the
// human wrote down.

var identityFieldNames = map[string]bool{
	"Project": true, "GitCommit": true, "GitDirty": true, "ConfigHash": true,
	"DatasetVersion": true, "ModelVersion": true, "Seed": true, "Params": true,
}

var provenanceFieldNames = map[string]bool{
	"RunID": true, "Fingerprint": true, "FingerprintVersion": true,
	"Host": true, "Device": true, "FrameworkVersion": true,
	"SubmitterClaim": true, "JobID": true, "Status": true,
	"StartedAt": true, "EndedAt": true, "CheckpointURI": true, "Metrics": true,
}

// TestProperty_EveryRunFieldIsClassified is the loud-failure backstop
// described above: it must run (and be seen) before either mutation test
// below can be trusted, because it is what turns "not classified" into a
// hard failure instead of a field silently falling through as provenance
// by default.
func TestProperty_EveryRunFieldIsClassified(t *testing.T) {
	typ := reflect.TypeOf(Run{})
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		inIdentity := identityFieldNames[name]
		inProvenance := provenanceFieldNames[name]
		switch {
		case inIdentity && inProvenance:
			t.Fatalf("field %q is listed in both identityFieldNames and provenanceFieldNames in run_property_test.go", name)
		case !inIdentity && !inProvenance:
			t.Fatalf("field %q on lineage.Run is not classified in run_property_test.go -- "+
				"decide, from run.go's identity/provenance boundary comment and Compute's write() call, "+
				"whether it is hashed into Fingerprint, and add it to identityFieldNames or "+
				"provenanceFieldNames; an unclassified field must never be silently treated as provenance", name)
		}
	}
}

// TestProperty_MutatingAnIdentityFieldChangesFingerprint generalizes
// TestFingerprintDistinguishesIdentityFields (a fixed example per field) to
// generated Runs: for any generated Run, changing any identity field to a
// different value must change the fingerprint.
func TestProperty_MutatingAnIdentityFieldChangesFingerprint(t *testing.T) {
	rng := newPropertyRand(t)
	typ := reflect.TypeOf(Run{})
	for trial := 0; trial < 20; trial++ {
		base := randRun(rng)
		for i := 0; i < typ.NumField(); i++ {
			name := typ.Field(i).Name
			if !identityFieldNames[name] {
				continue
			}
			t.Run(fmt.Sprintf("trial%d/%s", trial, name), func(t *testing.T) {
				mutant := cloneRun(base)
				field := reflect.ValueOf(&mutant).Elem().FieldByName(name)
				mutated := mutateField(t, name, field, rng)
				if reflect.DeepEqual(field.Interface(), mutated.Interface()) {
					t.Fatalf("test bug: mutateField did not actually change field %s", name)
				}
				field.Set(mutated)
				if base.Compute() == mutant.Compute() {
					t.Fatalf("changing identity field %s did not change the fingerprint", name)
				}
			})
		}
	}
}

// TestProperty_MutatingAProvenanceFieldLeavesFingerprintUnchanged
// generalizes TestFingerprintIgnoresProvenance and
// TestFingerprintIgnoresAttribution (which each pin a handful of fields by
// hand) to the whole provenance half of Run, by construction: for any
// generated Run, changing any provenance field must never change the
// fingerprint. This is the test PR #75's ADR asked for -- "it should hold
// for the whole provenance half by construction" -- and it is the one that
// would catch a future field added to the provenance section that
// accidentally gets read by Compute.
func TestProperty_MutatingAProvenanceFieldLeavesFingerprintUnchanged(t *testing.T) {
	rng := newPropertyRand(t)
	typ := reflect.TypeOf(Run{})
	for trial := 0; trial < 20; trial++ {
		base := randRun(rng)
		for i := 0; i < typ.NumField(); i++ {
			name := typ.Field(i).Name
			if !provenanceFieldNames[name] {
				continue
			}
			t.Run(fmt.Sprintf("trial%d/%s", trial, name), func(t *testing.T) {
				mutant := cloneRun(base)
				field := reflect.ValueOf(&mutant).Elem().FieldByName(name)
				mutated := mutateField(t, name, field, rng)
				if reflect.DeepEqual(field.Interface(), mutated.Interface()) {
					t.Fatalf("test bug: mutateField did not actually change field %s", name)
				}
				field.Set(mutated)
				if base.Compute() != mutant.Compute() {
					t.Fatalf("changing provenance field %s changed the fingerprint -- provenance must never leak into identity", name)
				}
			})
		}
	}
}

// mutateField returns a value of the same type as the field named name,
// guaranteed to differ from the value currently held at v, using generic
// per-Kind rules. A field whose Kind (or, for Struct/Ptr, whose exact
// type) this function doesn't recognize fails the test immediately rather
// than being skipped: a field this function cannot mutate is a field the
// two tests above cannot prove is correctly classified, which is exactly
// the "cannot classify" case item 5 asks to fail loudly rather than pass
// silently.
func mutateField(t *testing.T, name string, v reflect.Value, rng *rand.Rand) reflect.Value {
	t.Helper()
	switch v.Kind() {
	case reflect.String:
		nv := reflect.New(v.Type()).Elem()
		nv.SetString(v.String() + "-mut-" + randString(rng, 6))
		return nv
	case reflect.Bool:
		nv := reflect.New(v.Type()).Elem()
		nv.SetBool(!v.Bool())
		return nv
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		nv := reflect.New(v.Type()).Elem()
		nv.SetInt(v.Int() + 1)
		return nv
	case reflect.Map:
		return mutateMap(t, name, v, rng)
	case reflect.Struct:
		if v.Type() != reflect.TypeOf(time.Time{}) {
			t.Fatalf("mutateField: field %q is an unsupported struct type %v -- extend run_property_test.go's mutator before this field can be classified", name, v.Type())
		}
		cur := v.Interface().(time.Time)
		return reflect.ValueOf(cur.Add(time.Duration(1+rng.IntN(10000)) * time.Second))
	case reflect.Ptr:
		if v.Type() != reflect.TypeOf((*time.Time)(nil)) {
			t.Fatalf("mutateField: field %q is an unsupported pointer type %v -- extend run_property_test.go's mutator before this field can be classified", name, v.Type())
		}
		if v.IsNil() {
			nt := time.Now().Add(time.Duration(rng.IntN(10000)) * time.Second)
			return reflect.ValueOf(&nt)
		}
		cur := v.Interface().(*time.Time)
		nt := cur.Add(time.Duration(1+rng.IntN(10000)) * time.Second)
		return reflect.ValueOf(&nt)
	default:
		t.Fatalf("mutateField: field %q has kind %v, which run_property_test.go's mutator does not yet handle -- "+
			"a field this test cannot mutate is a field it cannot prove is correctly classified as identity or "+
			"provenance, so it must fail loudly here rather than being silently skipped", name, v.Kind())
	}
	panic("unreachable") // t.Fatalf calls runtime.Goexit; this only satisfies the compiler's return requirement
}

func mutateMap(t *testing.T, name string, v reflect.Value, rng *rand.Rand) reflect.Value {
	t.Helper()
	if v.Type().Key().Kind() != reflect.String {
		t.Fatalf("mutateField: field %q has a non-string map key type %v -- extend the mutator", name, v.Type().Key())
	}
	nv := reflect.MakeMap(v.Type())
	iter := v.MapRange()
	for iter.Next() {
		nv.SetMapIndex(iter.Key(), iter.Value())
	}
	for {
		k := randString(rng, 4+rng.IntN(6))
		kv := reflect.ValueOf(k)
		if v.MapIndex(kv).IsValid() {
			continue // collided with an existing key; try another
		}
		var newVal reflect.Value
		switch v.Type().Elem().Kind() {
		case reflect.String:
			newVal = reflect.ValueOf(randString(rng, 4+rng.IntN(6)))
		case reflect.Float64:
			newVal = reflect.ValueOf(rng.Float64() * 1000)
		default:
			t.Fatalf("mutateField: field %q has an unsupported map value type %v -- extend the mutator", name, v.Type().Elem())
		}
		nv.SetMapIndex(kv, newVal)
		return nv
	}
}

// --- generators ---

const randAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./"

func randString(rng *rand.Rand, n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = randAlphabet[rng.IntN(len(randAlphabet))]
	}
	return string(b)
}

func randHex(rng *rand.Rand, n int) string {
	const hex = "0123456789abcdef"
	b := make([]byte, n)
	for i := range b {
		b[i] = hex[rng.IntN(len(hex))]
	}
	return string(b)
}

func randParams(rng *rand.Rand) map[string]string {
	n := rng.IntN(6) // 0..5 params
	if n == 0 {
		return nil
	}
	m := make(map[string]string, n)
	for len(m) < n {
		k := randString(rng, 3+rng.IntN(6))
		var v string
		if rng.IntN(2) == 0 {
			v = decimalSpellings(rng)[0]
		} else {
			v = randString(rng, 1+rng.IntN(10))
		}
		m[k] = v
	}
	return m
}

// randRun generates a Run with pseudo-random identity fields, so property
// tests exercise more than the single hand-picked base() fixture every
// example-based test in run_test.go shares.
func randRun(rng *rand.Rand) Run {
	return Run{
		Project:        randString(rng, 6+rng.IntN(10)),
		GitCommit:      randHex(rng, 40), // sha1-shaped, like a real commit
		GitDirty:       rng.IntN(2) == 1,
		ConfigHash:     randHex(rng, 12),
		DatasetVersion: randString(rng, 4+rng.IntN(8)),
		ModelVersion:   randString(rng, 4+rng.IntN(8)),
		Seed:           int64(rng.Uint64()),
		Params:         randParams(rng),
	}
}

// cloneRun deep-copies the map and pointer fields so mutating a clone can
// never alias back into the original -- without this, mutateMap's
// SetMapIndex on a shared underlying map would silently corrupt the
// control Run every property test compares against.
func cloneRun(r Run) Run {
	c := r
	if r.Params != nil {
		c.Params = make(map[string]string, len(r.Params))
		for k, v := range r.Params {
			c.Params[k] = v
		}
	}
	if r.Metrics != nil {
		c.Metrics = make(map[string]float64, len(r.Metrics))
		for k, v := range r.Metrics {
			c.Metrics[k] = v
		}
	}
	if r.EndedAt != nil {
		et := *r.EndedAt
		c.EndedAt = &et
	}
	return c
}

func cloneParamMap(m map[string]string) map[string]string {
	c := make(map[string]string, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

// decimalSpellings returns several textual spellings of one randomly
// chosen finite, nonzero decimal value, all built from the same digit
// sequence so they are exactly -- not approximately -- the same rational
// number. They are never produced by formatting a float64 and hoping a
// second parse round-trips back to it: strconv.ParseFloat is correctly
// rounded, so any two decimal strings that denote the identical rational
// number are *guaranteed* to parse to the identical float64. That is what
// makes this generator sound for testing "these must normalize
// identically" without depending on floating-point luck for the harder
// (many-significant-digit) cases FormatFloat-based generation would risk.
func decimalSpellings(rng *rand.Rand) []string {
	digits := 1 + rng.IntN(6)
	m := make([]byte, digits)
	m[0] = byte('1' + rng.IntN(9)) // first digit is never a leading zero
	for i := 1; i < digits; i++ {
		m[i] = byte('0' + rng.IntN(10))
	}
	mantissa := string(m)

	sign := ""
	if rng.IntN(2) == 0 {
		sign = "-"
	}

	pointPos := rng.IntN(digits + 1) // count of mantissa digits before the point
	intPart, fracPart := mantissa[:pointPos], mantissa[pointPos:]
	if intPart == "" {
		intPart = "0"
	}
	plain := intPart
	if fracPart != "" {
		plain += "." + fracPart
	}
	plain = sign + plain

	spellings := []string{plain}
	if fracPart != "" {
		spellings = append(spellings, plain+"0", plain+"00") // redundant trailing zeros
	} else {
		spellings = append(spellings, plain+".0", plain+".00")
	}

	// An exponential spelling of the identical value: every mantissa digit,
	// with the point notionally after the first digit, compensated by an
	// exponent of exactly -(len(fracPart)) -- equal by digit-accounting,
	// not by reformatting a float and checking it comes out close enough.
	allDigits := strings.TrimLeft(intPart+fracPart, "0")
	if allDigits == "" {
		allDigits = "0" // unreachable given m[0] is always nonzero, kept defensive
	}
	exp := -len(fracPart)
	if exp != 0 {
		spellings = append(spellings, fmt.Sprintf("%s%se%d", sign, allDigits, exp))
		// A zero-padded exponent and an explicit '+' sign are both legal
		// under numericParamPattern's `[eE][+-]?\d+`, so both must still
		// normalize identically -- cover the exponent grammar, not just
		// the mantissa grammar.
		if exp < 0 {
			spellings = append(spellings, fmt.Sprintf("%s%se-0%d", sign, allDigits, -exp))
		} else {
			spellings = append(spellings, fmt.Sprintf("%s%se+0%d", sign, allDigits, exp))
		}
	} else {
		spellings = append(spellings, sign+allDigits+"e0")
	}
	return spellings
}

// randomZeroSpelling returns a random textual spelling of exact zero, in
// the same spirit as decimalSpellings but for the value normalizeParamValue
// treats specially (ADR 0013: "-0", "-0.0", and "0" collapse to one
// canonical spelling because FormatFloat would otherwise keep signed and
// unsigned zero apart for a distinction nothing measures).
func randomZeroSpelling(rng *rand.Rand) string {
	sign := ""
	if rng.IntN(2) == 0 {
		sign = "-"
	}
	s := sign + "0"
	if rng.IntN(2) == 0 {
		s += "." + strings.Repeat("0", 1+rng.IntN(5))
	}
	if rng.IntN(2) == 0 {
		expSign := ""
		switch rng.IntN(3) {
		case 0:
			expSign = "-"
		case 1:
			expSign = "+"
		}
		s += "e" + expSign + strconv.Itoa(rng.IntN(20))
	}
	return s
}

// --- properties ---

// TestProperty_DeterminismAcrossGeneratedRuns generalizes the fixed-example
// determinism test in run_test.go to arbitrarily generated Runs: whatever
// the run's content, hashing it repeatedly must always agree with itself.
// This is the property the sort in Compute exists for -- Go's map
// iteration order is randomized per call, so if the sort were ever removed,
// or short-circuited for some map shape the hand-picked example never
// exercised, this is the test that would catch it on generated inputs
// nobody hand-picked.
func TestProperty_DeterminismAcrossGeneratedRuns(t *testing.T) {
	rng := newPropertyRand(t)
	for trial := 0; trial < 25; trial++ {
		r := randRun(rng)
		t.Run(fmt.Sprintf("trial%d", trial), func(t *testing.T) {
			want := r.Compute()
			for i := 0; i < 50; i++ {
				if got := r.Compute(); got != want {
					t.Fatalf("fingerprint not stable across repeated calls: %s != %s (params=%v)", got, want, r.Params)
				}
			}
		})
	}
}

// TestProperty_ParamMapInsertionOrderIrrelevant builds the same set of
// param key/value pairs via three different insertion sequences (forward,
// reverse, and a random shuffle) and asserts all three fingerprint
// identically. Go's map iteration order is randomized independent of
// insertion order already, which is what
// TestProperty_DeterminismAcrossGeneratedRuns's 50-repeat loop exercises
// indirectly; this test targets the same property item 2 asks for more
// directly, at the level of "how the map was built" rather than "how it
// happens to be iterated this process".
func TestProperty_ParamMapInsertionOrderIrrelevant(t *testing.T) {
	rng := newPropertyRand(t)
	type kv struct{ k, v string }
	for trial := 0; trial < 20; trial++ {
		n := 3 + rng.IntN(8)
		pairs := make([]kv, 0, n)
		seen := map[string]bool{}
		for len(pairs) < n {
			k := randString(rng, 3+rng.IntN(5))
			if seen[k] {
				continue
			}
			seen[k] = true
			pairs = append(pairs, kv{k, randString(rng, 1+rng.IntN(8))})
		}
		t.Run(fmt.Sprintf("trial%d", trial), func(t *testing.T) {
			forward := map[string]string{}
			for _, p := range pairs {
				forward[p.k] = p.v
			}
			backward := map[string]string{}
			for i := len(pairs) - 1; i >= 0; i-- {
				backward[pairs[i].k] = pairs[i].v
			}
			shuffled := append([]kv(nil), pairs...)
			rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
			random := map[string]string{}
			for _, p := range shuffled {
				random[p.k] = p.v
			}

			a, b, c := base(), base(), base()
			a.Params, b.Params, c.Params = forward, backward, random
			fa, fb, fc := a.Compute(), b.Compute(), c.Compute()
			if fa != fb || fa != fc {
				t.Fatalf("fingerprint depends on param insertion order: forward=%s backward=%s shuffled=%s", fa, fb, fc)
			}
		})
	}
}

// TestProperty_EquivalentNumericSpellingsCollapse generalizes ADR 0013's
// worked example (3e-4 / 0.0003 / 0.00030) to arbitrarily generated numeric
// values: any set of textual spellings that denote the same rational
// number must normalize -- and therefore fingerprint -- identically,
// regardless of which spelling a particular client happened to send.
func TestProperty_EquivalentNumericSpellingsCollapse(t *testing.T) {
	rng := newPropertyRand(t)
	for trial := 0; trial < 40; trial++ {
		spellings := decimalSpellings(rng)
		t.Run(fmt.Sprintf("trial%d", trial), func(t *testing.T) {
			var canonical, wantFingerprint string
			for i, s := range spellings {
				if !numericParamPattern.MatchString(s) {
					t.Fatalf("generator bug: spelling %q does not match numericParamPattern", s)
				}
				norm := normalizeParamValue(s)
				r := base()
				r.Params = map[string]string{"x": s}
				got := r.Compute()
				if i == 0 {
					canonical, wantFingerprint = norm, got
					continue
				}
				if norm != canonical {
					t.Errorf("spelling %q normalized to %q, want %q (same as %q)", s, norm, canonical, spellings[0])
				}
				if got != wantFingerprint {
					t.Errorf("spelling %q fingerprinted differently from equivalent spelling %q", s, spellings[0])
				}
			}
		})
	}
}

// TestProperty_ZeroSpellingsCollapseToZero covers the special case
// normalizeParamValue documents separately from generic numeric collapsing:
// every spelling of exact zero, however padded or exponentiated, must
// normalize to the single canonical "0".
func TestProperty_ZeroSpellingsCollapseToZero(t *testing.T) {
	rng := newPropertyRand(t)
	for trial := 0; trial < 20; trial++ {
		s := randomZeroSpelling(rng)
		t.Run(fmt.Sprintf("trial%d/%s", trial, s), func(t *testing.T) {
			if !numericParamPattern.MatchString(s) {
				t.Fatalf("generator bug: spelling %q does not match numericParamPattern", s)
			}
			if got := normalizeParamValue(s); got != "0" {
				t.Fatalf("normalizeParamValue(%q) = %q, want \"0\"", s, got)
			}
		})
	}
}

// TestProperty_DifferentNumericValuesStayDistinct is the mirror image of
// the collapsing properties above: normalization must never over-collapse.
// Two independently generated numeric values that are not the same
// rational number must still fingerprint differently.
func TestProperty_DifferentNumericValuesStayDistinct(t *testing.T) {
	rng := newPropertyRand(t)
	for trial := 0; trial < 30; trial++ {
		a := decimalSpellings(rng)[0]
		b := decimalSpellings(rng)[0]
		na, nb := normalizeParamValue(a), normalizeParamValue(b)
		if na == nb {
			continue // the two draws happened to be the same value; nothing to assert
		}
		t.Run(fmt.Sprintf("trial%d", trial), func(t *testing.T) {
			ra, rb := base(), base()
			ra.Params = map[string]string{"x": a}
			rb.Params = map[string]string{"x": b}
			if ra.Compute() == rb.Compute() {
				t.Fatalf("distinct values %q (normalized %s) and %q (normalized %s) fingerprinted identically", a, na, b, nb)
			}
		})
	}
}

// TestProperty_NonNumericAndUnrepresentableValuesPassThroughAndStayDistinct
// covers the second half of item 3: values normalizeParamValue does not
// treat as numeric -- including the overflow/underflow/non-finite cases
// ADR 0013 calls out by name -- must pass through Compute completely
// unchanged, and must remain pairwise distinguishable from each other and
// from ordinary numbers.
func TestProperty_NonNumericAndUnrepresentableValuesPassThroughAndStayDistinct(t *testing.T) {
	values := []string{
		"1e400", "1e-400", // overflow / underflow (ADR 0013)
		"NaN", "Inf", "-Inf", "Infinity", // non-finite spellings ParseFloat accepts but this pattern rejects
		"007",                                       // zero-padded: excluded so it isn't misread as a shard id
		"1_000",                                     // Go numeric-literal separator this system doesn't recognize
		"",                                          // empty param value
		"run-42", "/mnt/data/v2", "a1b2c3d", "true", // ordinary non-numeric identifiers
	}
	for _, v := range values {
		if got := normalizeParamValue(v); got != v {
			t.Errorf("normalizeParamValue(%q) = %q, want it unchanged", v, got)
		}
	}
	for i := range values {
		for j := range values {
			if i == j {
				continue
			}
			a, b := base(), base()
			a.Params = map[string]string{"x": values[i]}
			b.Params = map[string]string{"x": values[j]}
			if a.Compute() == b.Compute() {
				t.Errorf("values %q and %q fingerprinted identically; they must stay distinct", values[i], values[j])
			}
		}
	}
}

// TestProperty_FieldBoundariesResistShifting generalizes
// TestFieldBoundariesAreNotAmbiguous (one fixed example) to arbitrary
// strings and arbitrary split points: for any string T and any two
// different ways of splitting it into (before, after), hashing (before,
// after) as two length-prefixed, adjacent fields must not collide with
// hashing a different split of the same T. Without length-prefixing this
// always collides, because concatenation erases the split point -- see
// ADR 0004's own worked example -- so this is a direct regression test for
// the length-prefixing `write` closure in Compute.
func TestProperty_FieldBoundariesResistShifting(t *testing.T) {
	rng := newPropertyRand(t)
	for trial := 0; trial < 30; trial++ {
		total := randString(rng, 2+rng.IntN(20))
		i := rng.IntN(len(total) + 1)
		j := rng.IntN(len(total) + 1)
		if i == j {
			continue // not a boundary shift; nothing to assert
		}
		t.Run(fmt.Sprintf("trial%d/split_%d_vs_%d_of_%d", trial, i, j, len(total)), func(t *testing.T) {
			// Two adjacent identity string fields.
			a, b := base(), base()
			a.DatasetVersion, a.ModelVersion = total[:i], total[i:]
			b.DatasetVersion, b.ModelVersion = total[:j], total[j:]
			if a.Compute() == b.Compute() {
				t.Errorf("splitting %q at %d and %d collided across DatasetVersion/ModelVersion", total, i, j)
			}

			// The same shift within one Params entry's key/value boundary
			// -- the exact shape of the {"ab":"c"} vs {"a":"bc"} example.
			pa, pb := base(), base()
			pa.Params = map[string]string{total[:i]: total[i:]}
			pb.Params = map[string]string{total[:j]: total[j:]}
			if pa.Compute() == pb.Compute() {
				t.Errorf("splitting %q at %d and %d collided across a Params key/value boundary", total, i, j)
			}
		})
	}
}

// TestProperty_AbsentParamDiffersFromEmptyParam generalizes item 7: Compute
// writes only the keys present in the map (ADR 0011 excludes Params from
// its "" == absent collapse for exactly this reason), so omitting a key and
// setting it to "" must never fingerprint the same way, for any base set of
// other params.
func TestProperty_AbsentParamDiffersFromEmptyParam(t *testing.T) {
	rng := newPropertyRand(t)
	for trial := 0; trial < 20; trial++ {
		params := randParams(rng)
		var key string
		for {
			key = randString(rng, 3+rng.IntN(6))
			if _, exists := params[key]; !exists {
				break
			}
		}
		t.Run(fmt.Sprintf("trial%d", trial), func(t *testing.T) {
			absent := base()
			absent.Params = cloneParamMap(params)

			withEmpty := base()
			withEmpty.Params = cloneParamMap(params)
			withEmpty.Params[key] = ""

			if absent.Compute() == withEmpty.Compute() {
				t.Fatalf("param %q absent and param %q=\"\" fingerprinted identically (other params: %v)", key, key, params)
			}
		})
	}
}
