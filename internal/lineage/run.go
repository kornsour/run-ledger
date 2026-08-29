// Package lineage defines the record an experiment run must emit to be
// reproducible, and the content-addressed identity derived from it.
package lineage

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Run is the lineage record for a single experiment run.
//
// The split matters: the fields above Provenance determine *what was run* and
// are hashed into the RunID; the fields below record *what happened* and are
// not. Two runs with the same Fingerprint were the same experiment, whatever
// their outcome — which is what makes "did this change anything?" answerable.
type Run struct {
	// --- identity: hashed into Fingerprint ---
	Project        string            `json:"project"`
	GitCommit      string            `json:"git_commit"`
	GitDirty       bool              `json:"git_dirty"`
	ConfigHash     string            `json:"config_hash"`
	DatasetVersion string            `json:"dataset_version"`
	ModelVersion   string            `json:"model_version"`
	Seed           int64             `json:"seed"`
	Params         map[string]string `json:"params,omitempty"`

	// --- provenance: recorded, not hashed ---
	//
	// For Host, Device, FrameworkVersion, ConfigHash, DatasetVersion, and
	// ModelVersion, an empty string means "not recorded" -- not a value of
	// its own. None of them has a meaningful empty value (an empty config
	// hash *is* no config hash; a host always has a name), so the two
	// spellings a client might send were deliberately collapsed into one
	// meaning rather than distinguished. See ADR 0011.
	//
	// This block previously described that as a known gap and proposed
	// widening these to pointers. ADR 0011 considered and rejected exactly
	// that: it would have changed the fingerprint contract for the three
	// identity fields, and made the fingerprint sensitive to whether a
	// client serializes an unset key or omits it -- how a client spells
	// things, rather than what the experimenter chose. Consumers may rely
	// on "" == absent for these fields; compare.Runs already does.
	RunID       string `json:"run_id"`
	Fingerprint string `json:"fingerprint"`
	// FingerprintVersion records which version of Compute's hashing contract
	// produced Fingerprint -- see ADR 0004 and ADR 0013. It is provenance
	// about the fingerprint, not an input to it: hashing the version itself
	// would be circular, the same reason Fingerprint is not hashed either.
	//
	// Never set this directly. The server stamps it alongside Fingerprint
	// (api.record) whenever it computes a fresh one, and every Store
	// implementation persists and returns whatever was stamped. A run
	// decoded from a source that predates this field -- a JSON payload
	// written before FingerprintVersion existed, or a store row from before
	// its migration -- must not be read as FingerprintVersion's Go zero
	// value (0), which names no real contract. DuckDB's migration backfills
	// every pre-existing row to FingerprintVersionLegacy explicitly, and any
	// other caller constructing a Run by hand for a legacy value should do
	// the same rather than leave the field at its zero value.
	FingerprintVersion int    `json:"fingerprint_version"`
	Host               string `json:"host"`
	Device             string `json:"device"`
	FrameworkVersion   string `json:"framework_version"`
	// SubmitterClaim names the human or service account the caller says
	// recorded this run. The field is named "claim", not "submitter" or
	// "user", on purpose: RUNLEDGER_TOKEN is one shared secret today (see
	// internal/api.Auth), so the server has no per-caller identity to read
	// this from and check it against. Whatever a client puts here is exactly
	// as trustworthy as GitCommit would be if nothing computed it -- a value
	// the caller typed, not one the server verified. See ADR 0015 for why
	// that is an accepted, documented gap rather than a silently-shipped
	// claim that reads like a fact, and what attesting it later would need
	// (named, per-caller tokens in place of the single shared one).
	//
	// It is provenance, deliberately: Compute never reads it. Two people
	// running the identical experiment are still the identical experiment --
	// making the submitter identity-bearing would fingerprint the same run
	// differently depending on who happened to launch it, which destroys the
	// one property fingerprinting exists to give (same fingerprint means
	// same experiment, full stop).
	SubmitterClaim string `json:"submitter_claim"`
	// JobID is the identifier of the job or scheduler invocation that
	// launched this run -- a CI job id, a Slurm job id, or whatever a
	// launcher calls its own unit of work. Deliberately one generic field
	// rather than CIJobID/SlurmJobID/etc.: the set of schedulers this ledger
	// might see is open-ended, and a caller can always namespace its own
	// value (e.g. "gha:4821001233") without the server having to know every
	// launcher by name. Self-asserted the same way Host and Device already
	// are -- unlike SubmitterClaim it does not name a person, so it does not
	// need the same "claim" framing in its name; forging it gains a caller
	// nothing the way misattributing a run to someone else would.
	JobID     string    `json:"job_id"`
	Status    Status    `json:"status"`
	StartedAt time.Time `json:"started_at"`
	// EndedAt is a pointer because a struct's zero value is never omitted by
	// encoding/json's "omitempty" -- without the pointer, every run that
	// hasn't ended would serialize ended_at as 0001-01-01T00:00:00Z instead
	// of leaving the field out.
	EndedAt       *time.Time         `json:"ended_at,omitempty"`
	CheckpointURI string             `json:"checkpoint_uri,omitempty"`
	Metrics       map[string]float64 `json:"metrics,omitempty"`
}

// Status is the lifecycle state of a run.
type Status string

const (
	StatusCreated   Status = "created"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

var validStatuses = map[Status]bool{
	StatusCreated: true, StatusRunning: true, StatusSucceeded: true,
	StatusFailed: true, StatusCancelled: true,
}

// ValidStatus reports whether s is one of the known lifecycle states.
func ValidStatus(s Status) bool { return validStatuses[s] }

// Terminal reports whether s is a state a run does not leave: succeeded,
// failed, and cancelled are outcomes, not waypoints.
func Terminal(s Status) bool {
	return s == StatusSucceeded || s == StatusFailed || s == StatusCancelled
}

// Fingerprint hashing contract versions. See ADR 0004 (the fingerprint input
// is a versioned contract) and ADR 0013 (param values are normalized before
// hashing).
const (
	// FingerprintVersionLegacy is the contract in effect before ADR 0013:
	// Params were hashed by the literal string spelling a client sent, so
	// "3e-4", "0.0003", and "0.00030" fingerprinted as three different
	// experiments. There is no way to recompute a legacy fingerprint under
	// today's Compute and expect it to still match what was stored --
	// Compute only ever implements the current contract -- so this constant
	// exists purely to label old records, never to select old behavior.
	// Every run recorded before FingerprintVersion existed is this version,
	// by definition of the migration that introduced the field (see
	// internal/store/duckdb.go's schema migration), not by anything
	// recomputed from its content.
	FingerprintVersionLegacy = 1
	// CurrentFingerprintVersion is the contract Compute implements: param
	// values that parse as a finite decimal number are rewritten to a
	// canonical spelling (normalizeParamValue) before hashing, so numeric
	// params with different spellings but the same value fingerprint
	// identically. Bump this, and add a new FingerprintVersion* constant
	// documenting what changed, the next time Compute's input changes --
	// per ADR 0004, that is never a change made without one.
	CurrentFingerprintVersion = 2
)

// ErrDirtyTree is returned by Validate when a run reports a dirty working tree
// without an explicit config hash. A dirty tree means the commit does not
// describe the code that ran, so the config hash is the only remaining handle
// on what was actually executed.
var ErrDirtyTree = errors.New("git_dirty is set but config_hash is empty: the run is not reconstructible")

// Validate reports why a run record cannot be trusted as lineage.
func (r *Run) Validate() error {
	if strings.TrimSpace(r.Project) == "" {
		return errors.New("project is required")
	}
	if strings.TrimSpace(r.GitCommit) == "" {
		return errors.New("git_commit is required")
	}
	if r.Status != "" && !validStatuses[r.Status] {
		return fmt.Errorf("unknown status %q", r.Status)
	}
	if r.GitDirty && strings.TrimSpace(r.ConfigHash) == "" {
		return ErrDirtyTree
	}
	if r.EndedAt != nil && r.EndedAt.Before(r.StartedAt) {
		return errors.New("ended_at precedes started_at")
	}
	return nil
}

// Compute returns the content-addressed fingerprint of a run's identity
// fields. It always implements CurrentFingerprintVersion -- Compute has no
// notion of "compute the old way," because a fingerprint already recorded
// under FingerprintVersionLegacy is never recomputed (see FingerprintVersion
// and ADR 0004): it stays exactly what was stored, and FingerprintVersion is
// what tells a reader which contract produced it.
//
// Map iteration order in Go is deliberately randomized, so Params is sorted
// before hashing: the same experiment described twice must produce the same
// fingerprint, or nothing downstream can group runs. Each param value is
// also normalized (normalizeParamValue) before hashing, so a param that
// merely looks like a different spelling of the same number -- "3e-4" versus
// "0.0003" versus "0.00030" -- hashes the same regardless of which spelling
// a particular client happened to send (ADR 0013).
func (r *Run) Compute() string {
	h := sha256.New()
	write := func(parts ...string) {
		for _, p := range parts {
			// Length-prefix each field so ("ab","c") and ("a","bc") differ.
			fmt.Fprintf(h, "%d:%s", len(p), p)
		}
	}
	write(r.Project, r.GitCommit, fmt.Sprint(r.GitDirty), r.ConfigHash,
		r.DatasetVersion, r.ModelVersion, fmt.Sprint(r.Seed))

	keys := make([]string, 0, len(r.Params))
	for k := range r.Params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		write(k, normalizeParamValue(r.Params[k]))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// numericParamPattern matches the shape Compute treats as "a number, however
// it's spelled": an optional sign, a JSON-number-shaped integer part (no
// leading zeros, matching RFC 8259) or a bare leading decimal point, an
// optional fractional part, and an optional exponent.
//
// It deliberately does not match everything strconv.ParseFloat accepts,
// because ParseFloat's grammar is Go's floating-point-literal syntax, not
// "numbers as ML tooling and JSON spell them," and two things it accepts
// would silently misfire if this function fed them straight to ParseFloat:
//
//   - "1_000": ParseFloat accepts underscores as a Go numeric-literal digit
//     separator. A param value is arbitrary user/CLI/JSON input, not Go
//     source -- nothing else in this system treats "1_000" as a spelling of
//     1000, so silently normalizing it here would be Compute inventing an
//     equivalence no client asked for.
//   - "007": ParseFloat parses leading zeros as an ordinary decimal (007 ==
//     7). A zero-padded value is at least as likely to be an opaque
//     identifier -- a shard id, a zero-padded run suffix -- as a number
//     whose leading zeros are insignificant, and this package has no basis
//     for guessing which. JSON's own number grammar excludes leading zeros
//     for the same reason; this pattern follows it.
//
// "NaN", "Inf", "-Inf", and "Infinity" are excluded structurally, not by
// special-casing the literal strings: they contain letters the pattern
// doesn't allow outside the "e"/"E" exponent marker, so they never reach
// strconv.ParseFloat and pass through Compute unchanged. That is deliberate
// even though ParseFloat itself accepts all four (case-insensitively) with
// no error -- unlike a finite number, there is no single canonical spelling
// two different "Inf" literals necessarily agree on being about the same
// underlying value, and NaN famously isn't equal to itself, which would make
// folding every NaN spelling into one canonical string actively misleading
// as an identity key rather than merely unnormalized.
var numericParamPattern = regexp.MustCompile(`^-?((0|[1-9]\d*)(\.\d+)?|\.\d+)([eE][+-]?\d+)?$`)

// normalizeParamValue rewrites v to a canonical spelling when it is
// unambiguously a finite decimal number, so the same hyperparameter value
// hashes identically no matter which of several equivalent spellings a
// client sent. A value this function does not recognize as numeric --
// including every case numericParamPattern's doc comment calls out --
// passes through byte-for-byte unchanged: normalizing is only ever a no-op
// or a like-for-like rewrite, never a way to lose information Compute has no
// basis for discarding.
func normalizeParamValue(v string) string {
	if !numericParamPattern.MatchString(v) {
		return v
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		// The pattern already guarantees valid float syntax, so the only
		// way ParseFloat still errors is ErrRange: v is a syntactically
		// well-formed number too large for float64 (e.g. "1e400", which
		// ParseFloat resolves to +Inf plus this error). Formatting that
		// would silently collapse every magnitude beyond float64's range
		// onto one canonical "+Inf" spelling -- a far bigger identity
		// change than reconciling spellings of the same in-range number --
		// so an unrepresentable magnitude is left exactly as written.
		return v
	}
	if f == 0 && !isZeroLiteral(v) {
		// The mirror image of the overflow case, and easier to miss:
		// ParseFloat silently underflows a nonzero magnitude too small for
		// float64 (e.g. "1e-400") to exactly 0, with no error at all.
		// Formatting that would conflate a tiny-but-nonzero literal with an
		// actual zero, so it gets the same treatment as overflow -- left
		// alone rather than misrepresented.
		return v
	}
	if f == 0 {
		// "-0", "-0.0", and "0" are the same hyperparameter value, but
		// FormatFloat renders a parsed negative zero back out as "-0" --
		// collapse the sign so they hash identically rather than keeping
		// the two apart for a distinction nothing measures.
		f = 0
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// isZeroLiteral reports whether s's significant digits (everything before
// an exponent marker) are all zero -- i.e. s spells an exact zero rather
// than a nonzero magnitude that merely underflows float64 to 0. Only
// meaningful once numericParamPattern has already confirmed s is
// number-shaped, which is the only way normalizeParamValue calls it.
func isZeroLiteral(s string) bool {
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		s = s[:i]
	}
	for _, r := range s {
		if r >= '1' && r <= '9' {
			return false
		}
	}
	return true
}
