// Command rlctl is the researcher-facing client for the run ledger.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/kornsour/run-ledger/internal/compare"
	"github.com/kornsour/run-ledger/internal/lineage"
	"github.com/kornsour/run-ledger/internal/spread"
)

// apiVersion is the path segment every resource route on the server is
// versioned under -- see ADR 0009. Kept in one place so a future version
// bump is one edit here, not one per call site below.
const apiVersion = "/v1"

const usage = `rlctl — record and compare experiment runs

  rlctl record --project P [--seed N] [--dataset V] [--model V] [--param k=v ...] [--metric k=v ...]
               [--submitter-claim WHO] [--job-id ID]
                           Capture git context from the working tree and record a run.
                           --submitter-claim is self-asserted, not verified (see ADR 0015).
                           --job-id defaults from $SLURM_JOB_ID/$CI_JOB_ID/$GITHUB_RUN_ID if set.
  rlctl start  <run-id>    Mark a recorded run running.
  rlctl finish [--status succeeded|failed|cancelled] [--metric k=v ...] [--checkpoint URI] <run-id>
                           Move a running run to a terminal status. --status defaults to succeeded.
  rlctl fail   [--metric k=v ...] <run-id>
                           Shorthand for finish --status failed.
  rlctl list   [--project P] [--commit SHA] [--status S] [--submitter-claim WHO] [--job-id ID]
               [--limit N] [--cursor C]
                           Server caps --limit; pass the printed "next cursor"
                           back as --cursor to fetch the following page.
  rlctl show   <run-id>
  rlctl diff   <run-a> <run-b>
  rlctl spread [--project P]     Rank fingerprints with repeats by widest metric spread.
  rlctl spread <fingerprint>     Show per-metric count/min/max/mean/stddev for one group.

  --server defaults to $RUNLEDGER_ADDR or http://localhost:8080
  RUNLEDGER_TOKEN, if set, is sent as a bearer token on every request. It is
  never read from a flag: a token in a flag lands in shell history and in the
  process table.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "record":
		err = cmdRecord(os.Args[2:])
	case "start":
		err = cmdStart(os.Args[2:])
	case "finish":
		err = cmdFinish(os.Args[2:])
	case "fail":
		err = cmdFail(os.Args[2:])
	case "list":
		err = cmdList(os.Args[2:])
	case "show":
		err = cmdShow(os.Args[2:])
	case "diff":
		err = cmdDiff(os.Args[2:])
	case "spread":
		err = cmdSpread(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

type kvFlag map[string]string

func (k kvFlag) String() string { return "" }
func (k kvFlag) Set(s string) error {
	name, value, ok := strings.Cut(s, "=")
	if !ok {
		return fmt.Errorf("want key=value, got %q", s)
	}
	k[name] = value
	return nil
}

func cmdRecord(args []string) error {
	fs := flag.NewFlagSet("record", flag.ExitOnError)
	server := fs.String("server", defaultServer(), "ledger address")
	project := fs.String("project", "", "project name (required)")
	seed := fs.Int64("seed", 0, "random seed")
	dataset := fs.String("dataset", "", "dataset version")
	model := fs.String("model", "", "model version")
	device := fs.String("device", "", "device the run used")
	configHash := fs.String("config-hash", "", "hash of the run configuration")
	status := fs.String("status", string(lineage.StatusCreated), "run status")
	// submitterClaim has no environment-derived default, unlike host and
	// job-id below: a hostname or a scheduler's job id is an objective fact
	// about the environment, but $USER (or similar) is a weak proxy for
	// "who is accountable for this run" -- a shared CI runner account, a
	// shared research VM login -- and defaulting to it would manufacture
	// exactly the "reads like a fact" problem ADR 0015 exists to avoid.
	// Requiring an explicit --submitter-claim keeps the claim as deliberate
	// as it should be.
	submitterClaim := fs.String("submitter-claim", "", "who recorded this run (self-asserted; see ADR 0015)")
	jobID := fs.String("job-id", jobIDFromEnv(), "launching job id; defaults from $SLURM_JOB_ID/$CI_JOB_ID/$GITHUB_RUN_ID if set")
	params := kvFlag{}
	metrics := kvFlag{}
	fs.Var(params, "param", "identity parameter, key=value (repeatable)")
	fs.Var(metrics, "metric", "measured metric, key=value (repeatable)")
	_ = fs.Parse(args)

	if *project == "" {
		return fmt.Errorf("--project is required")
	}
	commit, dirty := gitContext()
	if commit == "" {
		return fmt.Errorf("no git commit found: run inside a repository, or the record would not be reconstructible")
	}

	run := lineage.Run{
		Project: *project, GitCommit: commit, GitDirty: dirty,
		ConfigHash: *configHash, DatasetVersion: *dataset, ModelVersion: *model,
		Seed: *seed, Device: *device, Status: lineage.Status(*status),
		SubmitterClaim: *submitterClaim, JobID: *jobID,
		StartedAt: time.Now().UTC(),
	}
	if host, err := os.Hostname(); err == nil {
		run.Host = host
	}
	if len(params) > 0 {
		run.Params = params
	}
	if len(metrics) > 0 {
		run.Metrics = map[string]float64{}
		for k, v := range metrics {
			var f float64
			if _, err := fmt.Sscanf(v, "%g", &f); err != nil {
				return fmt.Errorf("metric %s=%s is not a number", k, v)
			}
			run.Metrics[k] = f
		}
	}
	if err := run.Validate(); err != nil {
		return err
	}

	body, _ := json.Marshal(run)
	var out lineage.Run
	if err := call(http.MethodPost, *server, "/runs", body, &out); err != nil {
		return err
	}
	fmt.Println(out.RunID)
	return nil
}

func cmdStart(args []string) error {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	server := fs.String("server", defaultServer(), "ledger address")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("start takes exactly one run id")
	}
	out, err := patchRun(*server, fs.Arg(0), map[string]any{"status": string(lineage.StatusRunning)})
	if err != nil {
		return err
	}
	fmt.Println(out.RunID, out.Status)
	return nil
}

func cmdFinish(args []string) error {
	fs := flag.NewFlagSet("finish", flag.ExitOnError)
	server := fs.String("server", defaultServer(), "ledger address")
	status := fs.String("status", string(lineage.StatusSucceeded), "terminal status: succeeded, failed, or cancelled")
	checkpoint := fs.String("checkpoint", "", "checkpoint URI")
	metrics := kvFlag{}
	fs.Var(metrics, "metric", "measured metric, key=value (repeatable)")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("finish takes exactly one run id")
	}
	return finishRun(*server, fs.Arg(0), *status, *checkpoint, metrics)
}

func cmdFail(args []string) error {
	fs := flag.NewFlagSet("fail", flag.ExitOnError)
	server := fs.String("server", defaultServer(), "ledger address")
	metrics := kvFlag{}
	fs.Var(metrics, "metric", "measured metric, key=value (repeatable)")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("fail takes exactly one run id")
	}
	return finishRun(*server, fs.Arg(0), string(lineage.StatusFailed), "", metrics)
}

// finishRun moves a run to a terminal status, setting ended_at to now and
// optionally a checkpoint URI and metrics. finish and fail share it because
// fail is exactly finish --status failed with no checkpoint.
func finishRun(server, runID, status, checkpoint string, metricFlags kvFlag) error {
	patch := map[string]any{
		"status":   status,
		"ended_at": time.Now().UTC(),
	}
	if checkpoint != "" {
		patch["checkpoint_uri"] = checkpoint
	}
	if len(metricFlags) > 0 {
		m := map[string]float64{}
		for k, v := range metricFlags {
			var f float64
			if _, err := fmt.Sscanf(v, "%g", &f); err != nil {
				return fmt.Errorf("metric %s=%s is not a number", k, v)
			}
			m[k] = f
		}
		patch["metrics"] = m
	}
	out, err := patchRun(server, runID, patch)
	if err != nil {
		return err
	}
	fmt.Println(out.RunID, out.Status)
	return nil
}

func patchRun(server, runID string, fields map[string]any) (lineage.Run, error) {
	body, err := json.Marshal(fields)
	if err != nil {
		return lineage.Run{}, err
	}
	var out lineage.Run
	if err := call(http.MethodPatch, server, "/runs/"+url.PathEscape(runID), body, &out); err != nil {
		return lineage.Run{}, err
	}
	return out, nil
}

func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	server := fs.String("server", defaultServer(), "ledger address")
	project := fs.String("project", "", "filter by project")
	commit := fs.String("commit", "", "filter by git commit")
	status := fs.String("status", "", "filter by status")
	submitterClaim := fs.String("submitter-claim", "", "filter by submitter_claim (self-asserted; see ADR 0015)")
	jobID := fs.String("job-id", "", "filter by job_id")
	limit := fs.Int("limit", 20, "maximum rows (server-capped; see --help)")
	cursor := fs.String("cursor", "", "opaque page cursor from a previous list's \"next cursor\" line")
	_ = fs.Parse(args)

	q := url.Values{}
	set(q, "project", *project)
	set(q, "git_commit", *commit)
	set(q, "status", *status)
	set(q, "submitter_claim", *submitterClaim)
	set(q, "job_id", *jobID)
	set(q, "cursor", *cursor)
	q.Set("limit", fmt.Sprint(*limit))

	var out struct {
		Runs       []lineage.Run `json:"runs"`
		Count      int           `json:"count"`
		NextCursor string        `json:"next_cursor"`
	}
	if err := call(http.MethodGet, *server, "/runs?"+q.Encode(), nil, &out); err != nil {
		return err
	}
	renderList(os.Stdout, out.Runs, out.NextCursor)
	return nil
}

// renderList prints the `list` table. Split out from cmdList, which can only
// be exercised against a live server, so the table -- column widths, the
// empty-page message, the cursor hint -- is directly testable. Mirrors
// renderDiff's split for the same reason.
func renderList(w io.Writer, runs []lineage.Run, nextCursor string) {
	if len(runs) == 0 {
		fmt.Fprintln(w, "no runs recorded")
		return
	}
	fmt.Fprintf(w, "%-28s  %-12s  %-10s  %-10s  %s\n", "RUN", "PROJECT", "STATUS", "COMMIT", "STARTED")
	for _, r := range runs {
		fmt.Fprintf(w, "%-28s  %-12s  %-10s  %-10s  %s\n",
			r.RunID, trunc(r.Project, 12), r.Status, trunc(r.GitCommit, 10),
			r.StartedAt.Local().Format(time.RFC3339))
	}
	// A page reflects the ledger as of the cursor's position: a run recorded
	// after this list started, and sorting earlier than the traversal has
	// reached, will not appear when you follow this cursor.
	if nextCursor != "" {
		fmt.Fprintf(w, "\nmore runs available: rlctl list ... --cursor %s\n", nextCursor)
	}
}

func cmdShow(args []string) error {
	fs := flag.NewFlagSet("show", flag.ExitOnError)
	server := fs.String("server", defaultServer(), "ledger address")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("show takes exactly one run id")
	}
	var run lineage.Run
	if err := call(http.MethodGet, *server, "/runs/"+url.PathEscape(fs.Arg(0)), nil, &run); err != nil {
		return err
	}
	return renderShow(os.Stdout, run)
}

// renderShow prints one run as indented JSON. Split out from cmdShow, which
// can only be exercised against a live server, so the encoding is directly
// testable. Mirrors renderDiff's split for the same reason.
//
// Unlike every other renderer here, this one does NOT get an
// absent-vs-empty golden case: encoding/json has no omitempty on Host,
// Device, FrameworkVersion, ConfigHash, DatasetVersion, or ModelVersion, so
// a run that never recorded one of those and a run that recorded it as ""
// both encode as `""` -- see lineage.Run's own doc comment, which names this
// gap and the reason it is not closed here (widening those fields to
// pointers ripples into Compute and every JSON decode site, a bigger,
// separately-scoped change). Asserting the invariant against this renderer
// would just fail on a gap the codebase has already scoped out on purpose;
// the golden fixture below records today's output, gap included, rather
// than pretending the gap doesn't exist.
func renderShow(w io.Writer, run lineage.Run) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(run)
}

func cmdDiff(args []string) error {
	fs := flag.NewFlagSet("diff", flag.ExitOnError)
	server := fs.String("server", defaultServer(), "ledger address")
	_ = fs.Parse(args)
	if fs.NArg() != 2 {
		return fmt.Errorf("diff takes exactly two run ids")
	}
	q := url.Values{"a": {fs.Arg(0)}, "b": {fs.Arg(1)}}
	var out compare.Result
	if err := call(http.MethodGet, *server, "/comparisons?"+q.Encode(), nil, &out); err != nil {
		return err
	}
	renderDiff(os.Stdout, out)
	return nil
}

// renderDiff writes a comparison's verdict and field table.
//
// The verdict is read from SameExperiment, never inferred from whether any
// field rendered. The two can disagree, and did: params {} and
// {"foo": ""} fingerprint differently, because lineage.Run.Compute hashes
// only the keys that are present -- while compare.Runs reads both sides
// through a map index that yields "" for a missing key, so it emits no
// field at all. Keying the "identical" message off len(Fields) therefore
// printed the exact opposite of what the server had just reported.
//
// Split out from cmdDiff, which can only be exercised against a live
// server, so the verdict logic is directly testable.
func renderDiff(w io.Writer, res compare.Result) {
	if res.SameExperiment && len(res.Fields) == 0 {
		fmt.Fprintln(w, "the two records are identical")
		return
	}
	if res.SameExperiment {
		fmt.Fprintln(w, "same experiment (fingerprints match)")
	} else {
		fmt.Fprintln(w, "different experiments (fingerprints differ)")
	}
	if len(res.Fields) == 0 {
		// Fingerprints differ and nothing rendered. compare.Runs covers
		// every input lineage.Run.Compute hashes, so reaching this means
		// the two have drifted apart -- not something a reader can act on,
		// but far better said out loud than papered over with a verdict of
		// "identical". It was reachable until params were compared by
		// presence rather than through a map index.
		fmt.Fprintln(w, "\nNo field differs, which should not be possible for fingerprints")
		fmt.Fprintln(w, "that differ: the diff covers every field the fingerprint hashes.")
		fmt.Fprintln(w, "Please report this, with the output of `rlctl show` for each id.")
		return
	}
	fmt.Fprintf(w, "\n%-24s  %-12s  %-20s  %s\n", "FIELD", "KIND", "A", "B")
	for _, f := range res.Fields {
		fmt.Fprintf(w, "%-24s  %-12s  %-20s  %s\n", f.Name, f.Kind, cell(f.A), cell(f.B))
	}
	if res.Unattributable {
		fmt.Fprintln(w, "\nThese runs describe the same experiment but measured differently.")
		fmt.Fprintln(w, "Something that affected the result is not captured in the record.")
	}
}

func cmdSpread(args []string) error {
	fs := flag.NewFlagSet("spread", flag.ExitOnError)
	server := fs.String("server", defaultServer(), "ledger address")
	project := fs.String("project", "", "filter by project (spread listing only)")
	_ = fs.Parse(args)

	switch fs.NArg() {
	case 0:
		return spreadList(*server, *project)
	case 1:
		return spreadOne(*server, fs.Arg(0))
	default:
		return fmt.Errorf("spread takes either --project or exactly one fingerprint, not both")
	}
}

func spreadList(server, project string) error {
	q := url.Values{}
	set(q, "project", project)
	var out struct {
		Groups []spread.Group `json:"groups"`
		Count  int            `json:"count"`
	}
	if err := call(http.MethodGet, server, "/fingerprints?"+q.Encode(), nil, &out); err != nil {
		return err
	}
	renderSpreadList(os.Stdout, out.Groups)
	return nil
}

// renderSpreadList prints the `spread` (no fingerprint argument) table.
// Split out from spreadList, which can only be exercised against a live
// server, so the table is directly testable. Mirrors renderDiff's split for
// the same reason.
//
// Takes groups rather than the count the server also returns: the count is
// always len(groups) for this endpoint (there is no filtering between the
// two), so a second, independently-suppliable number here would only invite
// the two to drift apart with nothing to catch it.
func renderSpreadList(w io.Writer, groups []spread.Group) {
	if len(groups) == 0 {
		fmt.Fprintln(w, "no fingerprint has more than one recorded run")
		return
	}
	// The server already ranks widest-first; keep that order.
	fmt.Fprintf(w, "%-18s  %-5s  %-10s  %s\n", "FINGERPRINT", "RUNS", "WIDEST CV", "PROVENANCE DIFFERS")
	for _, g := range groups {
		fmt.Fprintf(w, "%-18s  %-5d  %-10.4f  %s\n",
			trunc(g.Fingerprint, 18), g.Count, g.Widest(), provenanceFields(g.Provenance))
	}
}

func spreadOne(server, fingerprint string) error {
	var g spread.Group
	if err := call(http.MethodGet, server, "/fingerprints/"+url.PathEscape(fingerprint), nil, &g); err != nil {
		return err
	}
	renderSpreadGroup(os.Stdout, g)
	return nil
}

// renderSpreadGroup prints one fingerprint group's spread summary. Split out
// from spreadOne, which can only be exercised against a live server, so the
// rendering -- in particular how a provenance field's values print -- is
// directly testable. Mirrors renderDiff's split for the same reason.
func renderSpreadGroup(w io.Writer, g spread.Group) {
	fmt.Fprintf(w, "fingerprint %s — %d run(s)\n", g.Fingerprint, g.Count)
	if g.NoRepeats {
		fmt.Fprintln(w, "no repeats: only one run has been recorded for this experiment")
		return
	}

	keys := make([]string, 0, len(g.Metrics))
	for k := range g.Metrics {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintf(w, "\n%-20s  %-4s  %-12s  %-12s  %-12s  %s\n", "METRIC", "N", "MIN", "MAX", "MEAN", "STDDEV")
	for _, k := range keys {
		m := g.Metrics[k]
		fmt.Fprintf(w, "%-20s  %-4d  %-12g  %-12g  %-12g  %g\n", k, m.Count, m.Min, m.Max, m.Mean, m.StdDev)
	}

	if len(g.Provenance) > 0 {
		fmt.Fprintln(w, "\nprovenance differs across these runs — the likeliest explanation for the spread:")
		for _, p := range g.Provenance {
			rendered := make([]string, len(p.Values))
			for i, v := range p.Values {
				rendered[i] = provenanceValue(v)
			}
			fmt.Fprintf(w, "  %-18s %s\n", p.Field, strings.Join(rendered, ", "))
		}
	}
}

// provenanceValue renders one value from a ProvenanceDiff.Values slice.
// Every field spread.provenanceFields tracks is a scalar provenance field
// (device, framework_version, submitter_claim, job_id), where ADR 0011
// gives "" exactly one meaning, "not recorded" -- unlike compare.Field,
// which also carries params and so needs cell's second case (a pointer to
// "" for a value genuinely recorded as empty), nothing a ProvenanceDiff
// carries can be that. So this does not reuse cell(*string) -- there is no
// second case to distinguish here, and wrapping v in a pointer just to fit
// cell's signature would invite a reader to look for one that does not
// exist. It renders "" the same way cell renders a nil field (an em dash),
// which matters because strings.Join on an untranslated "" produced a
// dangling comma in a values list ("job_id   , slurm-7") that both read as
// a formatting bug and silently dropped the fact that some run in the
// group never recorded the field at all -- precisely the interesting half
// of a submitter_claim/job_id disagreement, since those two are unrecorded
// far more often than device or framework_version are.
func provenanceValue(v string) string {
	if v == "" {
		return "—"
	}
	return v
}

func provenanceFields(diffs []spread.ProvenanceDiff) string {
	if len(diffs) == 0 {
		return "—"
	}
	names := make([]string, len(diffs))
	for i, d := range diffs {
		names[i] = d.Field
	}
	return strings.Join(names, ", ")
}

// call issues one request against path on server, splicing in apiVersion so
// every caller gets it for free instead of remembering to add it by hand.
func call(method, server, path string, body []byte, out any) error {
	req, err := http.NewRequest(method, server+apiVersion+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token := os.Getenv("RUNLEDGER_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(payload, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s: %s", resp.Status, e.Error)
		}
		return fmt.Errorf("%s", resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(payload, out)
}

func gitContext() (commit string, dirty bool) {
	run := func(args ...string) string {
		out, err := exec.Command("git", args...).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	commit = run("rev-parse", "HEAD")
	dirty = run("status", "--porcelain") != ""
	return commit, dirty
}

// jobIDFromEnv reads the first known scheduler/CI environment variable that
// is set, in the order a run is most likely to be launched by one: a Slurm
// allocation, then a generic CI job id (GitLab CI, CircleCI, and others all
// use CI_JOB_ID), then GitHub Actions' own run id. Unlike submitterClaim,
// no --job-id flag is required to get a value: these are objective facts
// about the launching environment, the same kind of fact os.Hostname()
// already supplies for Host, not a claim about who is accountable.
func jobIDFromEnv() string {
	for _, k := range []string{"SLURM_JOB_ID", "CI_JOB_ID", "GITHUB_RUN_ID"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func defaultServer() string {
	if v := os.Getenv("RUNLEDGER_ADDR"); v != "" {
		return v
	}
	return "http://localhost:8080"
}

func set(q url.Values, k, v string) {
	if v != "" {
		q.Set(k, v)
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// cell renders one side of a diff row. A nil value was never recorded; a
// pointer to "" is a value the run actually carried, which only a param
// can currently be (ADR 0011). The two used to print the same em dash.
func cell(v *string) string {
	switch {
	case v == nil:
		return "—"
	case *v == "":
		return `""`
	default:
		return *v
	}
}
