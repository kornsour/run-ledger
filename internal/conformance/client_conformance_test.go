// Package conformance asserts that every supported way of describing an
// experiment run -- rlctl, the Python client, and a hand-built raw HTTP
// request -- produces the *same fingerprint* against a real server.
//
// This is the client-side twin of internal/store/conformance.go's
// RunConformance, which exists so two Store backends can never quietly
// disagree about ledger semantics. Two clients must not quietly disagree
// either, and it is a strictly worse failure: a backend disagreement
// corrupts storage, a client disagreement corrupts *identity* -- the same
// experiment, recorded through two paths, silently becomes two experiments.
// That is exactly what #63 was: `rlctl --param lr=3e-4` sent the literal
// string "3e-4"; runledger.Run.start(params={"lr": 3e-4}) sent str(3e-4) ==
// "0.0003". Two different fingerprints for one experiment, and nothing
// failed -- spread saw no repeat to measure, unattributable never fired.
// Each client was tested against itself; neither was ever tested against
// the other. This package is that missing test.
//
// Design note -- why a Go test, not a Python one
// ------------------------------------------------
// This suite needs three things at once: a running server, the built rlctl
// binary, and an importable Python client. .github/workflows/ci.yml's
// `build` job runs on ubuntu-latest with a Go toolchain installed and ships
// python3 preinstalled (no explicit Python setup step needed); its `python`
// job runs on ubuntu-latest too but never installs a Go toolchain, so it
// cannot build or run rlctl at all. A Go test that shells out to `go build
// ./cmd/rlctl` and `python3` therefore has exactly one CI home -- the build
// job -- and lands there for free: this package is part of the module, so
// `go test -race ./...` (already a build-job step) picks it up with no
// workflow edit required. A Python-side test driving the other two paths
// was the alternative considered and rejected: it would need its own Go
// toolchain step added to the `python` job (or a third job entirely) to
// reach rlctl, for no benefit over just writing the test in Go, which
// already has the standard library's exec.Command and net/http/httptest.
//
// A real server (net/http/httptest.Server wrapping api.New) is used rather
// than the actual `runledger` binary: rlctl and the Python client only ever
// talk to the server over HTTP, so an httptest.Server serving the exact same
// api.Server.Handler() is indistinguishable from the real thing to either
// client, and it is far cheaper to start/stop per test and needs no port
// coordination.
//
// Design note -- graceful skip, but not a silent one in CI
// ------------------------------------------------------------
// A developer running `go test ./...` on a machine with no python3, or in a
// checkout where the Python client can't import for some unrelated reason,
// must not see a spurious failure -- that is what t.Skip (via maybeSkip
// below) is for. But the same missing prerequisite in CI is not a skip, it
// is exactly the failure this suite exists to catch failing to run at all
// (see the issue's whole premise: silent absence is the failure mode). CI
// runs with CI=true (GitHub Actions sets it on every job), so maybeSkip
// checks that and escalates to t.Fatal there instead -- a genuinely missing
// prerequisite on the build job (python3 removed from the runner image, the
// module failing to build) becomes a loud, attributable CI failure instead
// of a quietly-skipped suite nobody notices stopped running.
//
// Design note -- determinism
// -----------------------------
// GitCommit and GitDirty are identity fields (hashed into Fingerprint) that
// rlctl and the Python client each capture live from the working tree --
// neither exposes a way to override them, so this suite cannot inject a
// fixed value into either invocation. Instead it captures the same two
// facts itself, once, from the same working tree (gitContext below), and
// uses that snapshot to build the raw-HTTP path's body. As long as nothing
// commits or dirties the tree mid-test -- true for both a developer's
// checkout and a CI run -- rlctl's and the Python client's live captures and
// this snapshot describe the same tree, so a fingerprint mismatch can only
// come from how each path encodes params/seed/dataset/model, never from an
// incidental difference in when git was asked.
//
// Every other identity field (project, seed, dataset_version, model_version,
// config_hash, params) is supplied explicitly and identically in spirit
// across all three paths per test case -- "identically in spirit" because
// the whole point of a case is to send the *same value* spelled differently
// per path (rlctl's --param string, the Python client's typed value, the
// raw JSON's string), the way #63's lr did.
package conformance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/kornsour/run-ledger/internal/api"
	"github.com/kornsour/run-ledger/internal/store"
)

// maybeSkip reports a missing prerequisite. Locally it is a skip (a
// developer's machine legitimately might not have python3); in CI -- where
// the build job is provisioned with everything this suite needs -- the same
// condition is promoted to a hard failure so the suite cannot quietly stop
// running without anyone noticing. See the package doc's "graceful skip"
// note.
func maybeSkip(t *testing.T, reason string) {
	t.Helper()
	if os.Getenv("CI") != "" {
		t.Fatalf("client conformance prerequisite missing while running in CI, where it must be present (this must not become a silent skip): %s", reason)
	}
	t.Skip(reason)
}

// repoRoot locates the module root from this file's own path rather than
// assuming a working directory: `go test` runs a package's tests with that
// package's directory as the working directory, not the repo root, and
// `go build ./cmd/rlctl` and the git snapshot below both need the real root.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine this test file's own path")
	}
	root, err := filepath.Abs(filepath.Join(filepath.Dir(file), "..", ".."))
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("computed repo root %q does not contain go.mod, this file may have moved: %v", root, err)
	}
	return root
}

// buildRlctl builds the CLI once per test run into a temp directory and
// returns its path. A build failure is treated as a missing prerequisite,
// not a conformance failure: a genuine compile error in cmd/rlctl is already
// caught by `go build ./...`, which every contributor and CI runs anyway
// (see the top-level task's required checks), so failing loudly a second
// time here would just be a worse-attributed duplicate of that error. This
// suite's job is to compare fingerprints, not to be the thing that first
// discovers rlctl doesn't compile.
func buildRlctl(t *testing.T, root string) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		maybeSkip(t, "no `go` toolchain on PATH: "+err.Error())
	}
	bin := filepath.Join(t.TempDir(), "rlctl")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/rlctl")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		maybeSkip(t, fmt.Sprintf("building rlctl failed: %v\n%s", err, out))
	}
	return bin
}

// checkPythonClient locates python3 and confirms the runledger package
// importable via PYTHONPATH (the client is stdlib-only, so no install step
// is needed -- see python/pyproject.toml's empty dependencies list). Returns
// the interpreter path and the directory to put on PYTHONPATH for every
// later invocation.
func checkPythonClient(t *testing.T, root string) (python3, pythonDir string) {
	t.Helper()
	py, err := exec.LookPath("python3")
	if err != nil {
		maybeSkip(t, "no python3 on PATH: "+err.Error())
	}
	pythonDir = filepath.Join(root, "python")
	cmd := exec.Command(py, "-c", "import runledger")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "PYTHONPATH="+pythonDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		maybeSkip(t, fmt.Sprintf("python3 cannot import runledger (PYTHONPATH=%s): %v\n%s", pythonDir, err, out))
	}
	return py, pythonDir
}

// gitContext snapshots the two git-derived identity fields once, from the
// same repo rlctl and the Python client will independently query moments
// later. See the package doc's determinism note for why one snapshot,
// reused for the raw-HTTP path, is the right amount of control here rather
// than trying to fix git_commit/git_dirty inside either client.
func gitContext(t *testing.T, root string) (commit string, dirty bool) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		maybeSkip(t, "no git on PATH: "+err.Error())
	}
	out, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	commit = strings.TrimSpace(string(out))
	statusOut, err := exec.Command("git", "-C", root, "status", "--porcelain").Output()
	if err != nil {
		t.Fatalf("git status --porcelain: %v", err)
	}
	dirty = strings.TrimSpace(string(statusOut)) != ""
	return commit, dirty
}

// requestRecorder captures the raw body of every POST /v1/runs the test
// server receives, so the "do both clients send the same identity fields"
// check (see identityKeySet) has something to inspect -- rlctl and the
// Python client only report back a run id and fingerprint, never the wire
// body they actually sent.
type requestRecorder struct {
	mu    sync.Mutex
	posts [][]byte
}

func (r *requestRecorder) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost && req.URL.Path == "/v1/runs" {
			body, _ := io.ReadAll(req.Body)
			_ = req.Body.Close()
			req.Body = io.NopCloser(bytes.NewReader(body))
			r.mu.Lock()
			r.posts = append(r.posts, body)
			r.mu.Unlock()
		}
		next.ServeHTTP(w, req)
	})
}

// takeOnlyPost returns the single POST /v1/runs body captured since the last
// call, and clears the log. Each helper below (recordViaRlctl,
// recordViaPython) makes exactly one such request per invocation, so more or
// fewer than one is itself a bug worth failing loudly on rather than
// silently comparing the wrong body.
func (r *requestRecorder) takeOnlyPost(t *testing.T) []byte {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.posts) != 1 {
		var projects []string
		for _, p := range r.posts {
			var m map[string]any
			_ = json.Unmarshal(p, &m)
			projects = append(projects, fmt.Sprintf("%v", m["project"]))
		}
		t.Fatalf("want exactly one POST /v1/runs captured for this invocation, got %d (projects: %v) -- every recording path (rlctl, the Python client, and the raw-HTTP path) must drain its own capture before the next one runs", len(r.posts), projects)
	}
	body := r.posts[0]
	r.posts = nil
	return body
}

// newTestServer starts a real server -- the same api.Server.Handler() the
// production binary serves -- backed by an in-memory store, wrapped so every
// POST /v1/runs is captured. Neither rlctl nor the Python client can tell
// this apart from cmd/runledger over the wire.
func newTestServer(t *testing.T) (addr string, rec *requestRecorder) {
	t.Helper()
	rec = &requestRecorder{}
	s := api.New(store.NewMemory(), nil)
	ts := httptest.NewServer(rec.wrap(s.Handler()))
	t.Cleanup(ts.Close)
	return ts.URL, rec
}

// identityFieldNames are the lineage.Run fields lineage.Run.Compute hashes
// into the fingerprint (internal/lineage/run.go) -- the set a client sending
// a run must all be present for, or it is silently describing a different,
// less-complete experiment than it thinks it is.
var identityFieldNames = map[string]bool{
	"project": true, "git_commit": true, "git_dirty": true,
	"config_hash": true, "dataset_version": true, "model_version": true,
	"seed": true, "params": true,
}

// identityKeySet returns which of identityFieldNames actually appear as
// top-level keys in a captured request body. Presence, not value, is what
// this checks -- the fingerprint-equality assertion already covers whether
// the values agree; this covers whether a client sent the field *at all*,
// which is its own failure class (the issue's "worth asserting if it falls
// out naturally" ask): a client that silently dropped, say, dataset_version
// would still produce *a* fingerprint, just one that stopped depending on a
// field it should have.
func identityKeySet(t *testing.T, body []byte) map[string]bool {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decoding captured request body: %v\n%s", err, body)
	}
	out := map[string]bool{}
	for k := range m {
		if identityFieldNames[k] {
			out[k] = true
		}
	}
	return out
}

// keysDiff renders the symmetric difference between two identity key sets,
// or "" if they match.
func keysDiff(a, b map[string]bool) string {
	var onlyA, onlyB []string
	for k := range a {
		if !b[k] {
			onlyA = append(onlyA, k)
		}
	}
	for k := range b {
		if !a[k] {
			onlyB = append(onlyB, k)
		}
	}
	if len(onlyA) == 0 && len(onlyB) == 0 {
		return ""
	}
	sort.Strings(onlyA)
	sort.Strings(onlyB)
	return fmt.Sprintf("rlctl sent %v (not sent by the Python client); the Python client sent %v (not sent by rlctl)", onlyA, onlyB)
}

// paramCase is one identity param, described three ways: the literal string
// rlctl's --param flag sends, how the Python client should construct its
// (typed) value, and the literal string the raw-HTTP path sends. pyKind and
// pyRaw -- rather than a single Go value handed to the Python side as JSON
// -- exist because JSON cannot carry the distinction these test cases are
// built to exercise: encoding/json marshals float64(100) as the bare number
// 100 with no decimal point, and Python's json.load then hands that back as
// an int, not a float -- silently collapsing exactly the
// int-vs-float-param case this suite means to cover. Passing the literal
// ("100.0") and the constructor to use (float(...)) instead makes the
// Python-side type explicit and JSON-round-trip-proof.
type paramCase struct {
	key    string
	rlctl  string
	pyKind string // "float", "int", or "str"
	pyRaw  string
	raw    string
}

// conformanceCase is one experiment, described through all three paths.
// Every case supplies its own project name so runs never collide across
// cases, and its own config_hash/dataset/model/seed so each case is a
// self-contained repro if it ever fails.
type conformanceCase struct {
	name       string
	seed       int64
	dataset    string
	model      string
	configHash string
	params     []paramCase
}

func conformanceCases() []conformanceCase {
	return []conformanceCase{
		{
			// This is #63 itself: the literal spelling rlctl's user typed
			// versus str() of the Python float for the same value.
			name: "numeric-spelling-exponential-notation", seed: 7,
			dataset: "ds-v1", model: "model-v3", configHash: "cfg-exp-notation",
			params: []paramCase{
				{key: "lr", rlctl: "3e-4", pyKind: "float", pyRaw: "0.0003", raw: "0.00030"},
			},
		},
		{
			name: "integer-vs-float-param", seed: 13,
			dataset: "ds-v2", model: "model-v9", configHash: "cfg-int-float",
			params: []paramCase{
				{key: "batch_size", rlctl: "32", pyKind: "int", pyRaw: "32", raw: "32.0"},
			},
		},
		{
			// Zero seed and empty dataset/model deliberately exercise Go's
			// and Python's zero values for these fields, not just their
			// "obviously present" cases.
			name: "non-numeric-param-and-zero-identity-fields", seed: 0,
			dataset: "", model: "", configHash: "cfg-non-numeric",
			params: []paramCase{
				{key: "optimizer", rlctl: "adamw", pyKind: "str", pyRaw: "adamw", raw: "adamw"},
			},
		},
		{
			// Several params at once, mixing kinds, plus the negative-zero
			// case lineage.Run.Compute's own doc comment calls out
			// explicitly ("-0", "-0.0", and "0" are the same hyperparameter
			// value) -- if that collapsing ever regressed, this is the case
			// that would catch it from the client side.
			name: "mixed-params-with-nonzero-seed-and-versions", seed: 999999937,
			dataset: "churn-2026-08", model: "resnet-50-v2", configHash: "cfg-mixed",
			params: []paramCase{
				{key: "lr", rlctl: "1e2", pyKind: "float", pyRaw: "100.0", raw: "100"},
				{key: "weight_decay", rlctl: "0.0", pyKind: "int", pyRaw: "0", raw: "-0.0"},
				{key: "optimizer", rlctl: "sgd-momentum", pyKind: "str", pyRaw: "sgd-momentum", raw: "sgd-momentum"},
			},
		},
	}
}

// recordResult is what each of the three paths reports back for comparison.
type recordResult struct {
	fingerprint string
	identity    map[string]bool // nil for the raw-HTTP path -- see its comment
}

// recordViaRlctl records c through `rlctl record`, then `rlctl show` to read
// back the fingerprint the server assigned -- `record` itself only prints
// the run id (cmd/rlctl/main.go's cmdRecord), by design: it is meant to be
// piped into a follow-up command, not parsed for other fields.
func recordViaRlctl(t *testing.T, bin, root, addr string, rec *requestRecorder, c conformanceCase) recordResult {
	t.Helper()
	args := []string{
		"record", "--server", addr,
		"--project", c.name,
		"--seed", strconv.FormatInt(c.seed, 10),
		"--dataset", c.dataset,
		"--model", c.model,
		"--config-hash", c.configHash,
	}
	for _, p := range c.params {
		args = append(args, "--param", p.key+"="+p.rlctl)
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rlctl record %q: %v\nstderr:\n%s", c.name, err, stderrOf(err))
	}
	runID := strings.TrimSpace(string(out))
	body := rec.takeOnlyPost(t)

	showCmd := exec.Command(bin, "show", "--server", addr, runID)
	showCmd.Dir = root
	showOut, err := showCmd.Output()
	if err != nil {
		t.Fatalf("rlctl show %q: %v\nstderr:\n%s", runID, err, stderrOf(err))
	}
	var shown struct {
		Fingerprint string `json:"fingerprint"`
	}
	if err := json.Unmarshal(showOut, &shown); err != nil {
		t.Fatalf("decoding `rlctl show` output: %v\n%s", err, showOut)
	}
	return recordResult{fingerprint: shown.Fingerprint, identity: identityKeySet(t, body)}
}

// pythonRecordScript is written once to a temp file and reused for every
// case and every subtest. It takes one argument: a path to a small JSON
// config file (see recordViaPython) describing the run to record. Params
// are described as {"kind": ..., "raw": ...} pairs, reconstructed with
// float()/int()/str() here rather than trusting JSON's own number type --
// see paramCase's doc comment for why.
const pythonRecordScript = `
import json
import sys

with open(sys.argv[1]) as fh:
    cfg = json.load(fh)


def _coerce(spec):
    kind, raw = spec["kind"], spec["raw"]
    if kind == "float":
        return float(raw)
    if kind == "int":
        return int(raw)
    return raw


import runledger

params = {k: _coerce(v) for k, v in (cfg.get("params") or {}).items()}

with runledger.Run.start(
    project=cfg["project"],
    seed=cfg["seed"],
    params=params,
    dataset_version=cfg.get("dataset_version", ""),
    model_version=cfg.get("model_version", ""),
    config_hash=cfg.get("config_hash", ""),
    server=cfg["server"],
) as run:
    pass

print(json.dumps({"run_id": run.run_id, "fingerprint": run.fingerprint}))
`

// recordViaPython records c through runledger.Run.start(), the way training
// code actually calls it, then reports back the fingerprint the server
// returned -- available directly as run.fingerprint (_run.py's _send()
// stores it off the POST response), so no separate read-back call is needed
// the way rlctl's `show` was.
func recordViaPython(t *testing.T, python3, pythonDir, root, scriptPath, addr string, rec *requestRecorder, c conformanceCase) recordResult {
	t.Helper()
	cfg := map[string]any{
		"project": c.name, "seed": c.seed,
		"dataset_version": c.dataset, "model_version": c.model,
		"config_hash": c.configHash, "server": addr,
	}
	if len(c.params) > 0 {
		params := map[string]any{}
		for _, p := range c.params {
			params[p.key] = map[string]string{"kind": p.pyKind, "raw": p.pyRaw}
		}
		cfg["params"] = params
	}
	cfgBytes, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshaling python config for %q: %v", c.name, err)
	}
	cfgPath := filepath.Join(t.TempDir(), "cfg.json")
	if err := os.WriteFile(cfgPath, cfgBytes, 0o600); err != nil {
		t.Fatalf("writing python config for %q: %v", c.name, err)
	}

	cmd := exec.Command(python3, scriptPath, cfgPath)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "PYTHONPATH="+pythonDir)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("python record %q: %v\nstderr:\n%s", c.name, err, stderrOf(err))
	}
	body := rec.takeOnlyPost(t)

	var result struct {
		RunID       string `json:"run_id"`
		Fingerprint string `json:"fingerprint"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(lastLine(out)), &result); err != nil {
		t.Fatalf("decoding python client output for %q: %v\n%s", c.name, err, out)
	}
	return recordResult{fingerprint: result.Fingerprint, identity: identityKeySet(t, body)}
}

// recordViaRawHTTP records c as a hand-built JSON body against POST
// /v1/runs directly -- the ground truth the other two paths are compared
// against, the same role a manually-constructed lineage.Run plays in
// store.RunConformance. Its identity is nil, not compared: this path is the
// test's own construction, not a client under test, so there is nothing
// about "does it send the right fields" to check -- only the fingerprint it
// gets back matters. It still goes through the same server as the other two
// paths, so it still lands in rec's capture log; takeOnlyPost drains that
// entry (unused) so it can't be mistaken for the next case's rlctl or
// Python post.
func recordViaRawHTTP(t *testing.T, addr, commit string, dirty bool, rec *requestRecorder, c conformanceCase) recordResult {
	t.Helper()
	body := map[string]any{
		"project":         c.name,
		"git_commit":      commit,
		"git_dirty":       dirty,
		"config_hash":     c.configHash,
		"dataset_version": c.dataset,
		"model_version":   c.model,
		"seed":            c.seed,
	}
	if len(c.params) > 0 {
		params := map[string]string{}
		for _, p := range c.params {
			params[p.key] = p.raw
		}
		body["params"] = params
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshaling raw HTTP body for %q: %v", c.name, err)
	}
	resp, err := http.Post(addr+"/v1/runs", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("raw POST /v1/runs for %q: %v", c.name, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("raw POST /v1/runs for %q: want 201, got %d: %s", c.name, resp.StatusCode, respBody)
	}
	var out struct {
		Fingerprint string `json:"fingerprint"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		t.Fatalf("decoding raw POST response for %q: %v\n%s", c.name, err, respBody)
	}
	rec.takeOnlyPost(t) // drain our own request from the capture log; see doc comment above
	return recordResult{fingerprint: out.Fingerprint}
}

// stderrOf pulls the child process's stderr out of an *exec.ExitError, the
// way exec.Cmd.Output populates it, so a failing rlctl/python invocation's
// actual error message reaches the test failure instead of just an exit
// code.
func stderrOf(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return string(ee.Stderr)
	}
	return err.Error()
}

// lastLine returns b's final non-empty line -- the python script prints
// exactly one JSON line as its result, but is not guaranteed to be the only
// output (e.g. a RuntimeWarning would go to stderr, not stdout, but this is
// cheap insurance against anything else writing to stdout ahead of it).
func lastLine(b []byte) []byte {
	lines := bytes.Split(bytes.TrimRight(b, "\n"), []byte("\n"))
	if len(lines) == 0 {
		return b
	}
	return lines[len(lines)-1]
}

// TestClientFingerprintConformance is the suite described in the package
// doc: for every case, rlctl, the Python client, and a hand-built raw HTTP
// request record the same experiment, and must get back the same
// fingerprint. It also checks that rlctl and the Python client agree on
// which identity fields they send at all, since a client silently omitting
// one is the same class of bug as sending it spelled differently.
func TestClientFingerprintConformance(t *testing.T) {
	root := repoRoot(t)
	bin := buildRlctl(t, root)
	python3, pythonDir := checkPythonClient(t, root)
	commit, dirty := gitContext(t, root)
	addr, rec := newTestServer(t)

	scriptPath := filepath.Join(t.TempDir(), "record.py")
	if err := os.WriteFile(scriptPath, []byte(pythonRecordScript), 0o600); err != nil {
		t.Fatalf("writing python helper script: %v", err)
	}

	for _, c := range conformanceCases() {
		t.Run(c.name, func(t *testing.T) {
			rlctlResult := recordViaRlctl(t, bin, root, addr, rec, c)
			pyResult := recordViaPython(t, python3, pythonDir, root, scriptPath, addr, rec, c)
			rawResult := recordViaRawHTTP(t, addr, commit, dirty, rec, c)

			if rlctlResult.fingerprint == "" || pyResult.fingerprint == "" || rawResult.fingerprint == "" {
				t.Fatalf("a path returned an empty fingerprint: rlctl=%q python=%q raw=%q",
					rlctlResult.fingerprint, pyResult.fingerprint, rawResult.fingerprint)
			}
			if rlctlResult.fingerprint != pyResult.fingerprint || rlctlResult.fingerprint != rawResult.fingerprint {
				t.Fatalf("the same experiment fingerprinted differently across clients -- this is the #63 failure mode (same experiment, disagreeing identity):\n  rlctl:  %s\n  python: %s\n  raw:    %s",
					rlctlResult.fingerprint, pyResult.fingerprint, rawResult.fingerprint)
			}
			if diff := keysDiff(rlctlResult.identity, pyResult.identity); diff != "" {
				t.Fatalf("rlctl and the Python client disagree about which identity fields they send: %s", diff)
			}
		})
	}
}
