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
                           Capture git context from the working tree and record a run.
  rlctl start  <run-id>    Mark a recorded run running.
  rlctl finish [--status succeeded|failed|cancelled] [--metric k=v ...] [--checkpoint URI] <run-id>
                           Move a running run to a terminal status. --status defaults to succeeded.
  rlctl fail   [--metric k=v ...] <run-id>
                           Shorthand for finish --status failed.
  rlctl list   [--project P] [--commit SHA] [--status S] [--limit N] [--cursor C]
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
	limit := fs.Int("limit", 20, "maximum rows (server-capped; see --help)")
	cursor := fs.String("cursor", "", "opaque page cursor from a previous list's \"next cursor\" line")
	_ = fs.Parse(args)

	q := url.Values{}
	set(q, "project", *project)
	set(q, "git_commit", *commit)
	set(q, "status", *status)
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
	if out.Count == 0 {
		fmt.Println("no runs recorded")
		return nil
	}
	fmt.Printf("%-28s  %-12s  %-10s  %-10s  %s\n", "RUN", "PROJECT", "STATUS", "COMMIT", "STARTED")
	for _, r := range out.Runs {
		fmt.Printf("%-28s  %-12s  %-10s  %-10s  %s\n",
			r.RunID, trunc(r.Project, 12), r.Status, trunc(r.GitCommit, 10),
			r.StartedAt.Local().Format(time.RFC3339))
	}
	// A page reflects the ledger as of the cursor's position: a run recorded
	// after this list started, and sorting earlier than the traversal has
	// reached, will not appear when you follow this cursor.
	if out.NextCursor != "" {
		fmt.Printf("\nmore runs available: rlctl list ... --cursor %s\n", out.NextCursor)
	}
	return nil
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
	enc := json.NewEncoder(os.Stdout)
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
		// Differing fingerprints with nothing to show. Every other hashed
		// field is compared as a string and would have rendered, so an
		// absent-versus-empty param is what is left.
		fmt.Fprintln(w, "\nNo field differs in this view. For fingerprints that differ, that")
		fmt.Fprintln(w, "means a param is absent on one run and set to the empty string on")
		fmt.Fprintln(w, "the other: those hash differently but render the same here. Run")
		fmt.Fprintln(w, "`rlctl show` on each id to see which.")
		return
	}
	fmt.Fprintf(w, "\n%-24s  %-12s  %-20s  %s\n", "FIELD", "KIND", "A", "B")
	for _, f := range res.Fields {
		fmt.Fprintf(w, "%-24s  %-12s  %-20s  %s\n", f.Name, f.Kind, or(f.A, "—"), or(f.B, "—"))
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
	if out.Count == 0 {
		fmt.Println("no fingerprint has more than one recorded run")
		return nil
	}
	// The server already ranks widest-first; keep that order.
	fmt.Printf("%-18s  %-5s  %-10s  %s\n", "FINGERPRINT", "RUNS", "WIDEST CV", "PROVENANCE DIFFERS")
	for _, g := range out.Groups {
		fmt.Printf("%-18s  %-5d  %-10.4f  %s\n",
			trunc(g.Fingerprint, 18), g.Count, g.Widest(), provenanceFields(g.Provenance))
	}
	return nil
}

func spreadOne(server, fingerprint string) error {
	var g spread.Group
	if err := call(http.MethodGet, server, "/fingerprints/"+url.PathEscape(fingerprint), nil, &g); err != nil {
		return err
	}
	fmt.Printf("fingerprint %s — %d run(s)\n", g.Fingerprint, g.Count)
	if g.NoRepeats {
		fmt.Println("no repeats: only one run has been recorded for this experiment")
		return nil
	}

	keys := make([]string, 0, len(g.Metrics))
	for k := range g.Metrics {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Printf("\n%-20s  %-4s  %-12s  %-12s  %-12s  %s\n", "METRIC", "N", "MIN", "MAX", "MEAN", "STDDEV")
	for _, k := range keys {
		m := g.Metrics[k]
		fmt.Printf("%-20s  %-4d  %-12g  %-12g  %-12g  %g\n", k, m.Count, m.Min, m.Max, m.Mean, m.StdDev)
	}

	if len(g.Provenance) > 0 {
		fmt.Println("\nprovenance differs across these runs — the likeliest explanation for the spread:")
		for _, p := range g.Provenance {
			fmt.Printf("  %-18s %s\n", p.Field, strings.Join(p.Values, ", "))
		}
	}
	return nil
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

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
