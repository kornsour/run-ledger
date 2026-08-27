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
	"strings"
	"time"

	"github.com/kornsour/run-ledger/internal/compare"
	"github.com/kornsour/run-ledger/internal/lineage"
)

const usage = `rlctl — record and compare experiment runs

  rlctl record --project P [--seed N] [--dataset V] [--model V] [--param k=v ...] [--metric k=v ...]
                           Capture git context from the working tree and record a run.
  rlctl start  <run-id>    Mark a recorded run running.
  rlctl finish [--status succeeded|failed|cancelled] [--metric k=v ...] [--checkpoint URI] <run-id>
                           Move a running run to a terminal status. --status defaults to succeeded.
  rlctl fail   [--metric k=v ...] <run-id>
                           Shorthand for finish --status failed.
  rlctl list   [--project P] [--commit SHA] [--status S] [--limit N]
  rlctl show   <run-id>
  rlctl diff   <run-a> <run-b>

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
	if err := call(http.MethodPost, *server+"/runs", body, &out); err != nil {
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
	if err := call(http.MethodPatch, server+"/runs/"+url.PathEscape(runID), body, &out); err != nil {
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
	limit := fs.Int("limit", 20, "maximum rows")
	_ = fs.Parse(args)

	q := url.Values{}
	set(q, "project", *project)
	set(q, "git_commit", *commit)
	set(q, "status", *status)
	q.Set("limit", fmt.Sprint(*limit))

	var out struct {
		Runs  []lineage.Run `json:"runs"`
		Count int           `json:"count"`
	}
	if err := call(http.MethodGet, *server+"/runs?"+q.Encode(), nil, &out); err != nil {
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
	if err := call(http.MethodGet, *server+"/runs/"+url.PathEscape(fs.Arg(0)), nil, &run); err != nil {
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
	var out struct {
		Result         compare.Result `json:"result"`
		Unattributable bool           `json:"unattributable"`
	}
	if err := call(http.MethodGet, *server+"/compare?"+q.Encode(), nil, &out); err != nil {
		return err
	}
	if len(out.Result.Fields) == 0 {
		fmt.Println("the two records are identical")
		return nil
	}
	if out.Result.SameExperiment {
		fmt.Println("same experiment (fingerprints match)")
	} else {
		fmt.Println("different experiments (fingerprints differ)")
	}
	fmt.Printf("\n%-24s  %-12s  %-20s  %s\n", "FIELD", "KIND", "A", "B")
	for _, f := range out.Result.Fields {
		fmt.Printf("%-24s  %-12s  %-20s  %s\n", f.Name, f.Kind, or(f.A, "—"), or(f.B, "—"))
	}
	if out.Unattributable {
		fmt.Println("\nThese runs describe the same experiment but measured differently.")
		fmt.Println("Something that affected the result is not captured in the record.")
	}
	return nil
}

func call(method, endpoint string, body []byte, out any) error {
	req, err := http.NewRequest(method, endpoint, bytes.NewReader(body))
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
