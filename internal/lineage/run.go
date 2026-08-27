// Package lineage defines the record an experiment run must emit to be
// reproducible, and the content-addressed identity derived from it.
package lineage

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
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
	RunID            string             `json:"run_id"`
	Fingerprint      string             `json:"fingerprint"`
	Host             string             `json:"host"`
	Device           string             `json:"device"`
	FrameworkVersion string             `json:"framework_version"`
	Status           Status             `json:"status"`
	StartedAt        time.Time          `json:"started_at"`
	EndedAt          time.Time          `json:"ended_at,omitempty"`
	CheckpointURI    string             `json:"checkpoint_uri,omitempty"`
	Metrics          map[string]float64 `json:"metrics,omitempty"`
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
	if !r.EndedAt.IsZero() && r.EndedAt.Before(r.StartedAt) {
		return errors.New("ended_at precedes started_at")
	}
	return nil
}

// Compute returns the content-addressed fingerprint of a run's identity fields.
//
// Map iteration order in Go is deliberately randomized, so Params is sorted
// before hashing: the same experiment described twice must produce the same
// fingerprint, or nothing downstream can group runs.
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
		write(k, r.Params[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}
