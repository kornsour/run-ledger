# ADR 0003: A dirty working tree without a config hash is refused

Status: Accepted
Date: 2026-08-25

## Context

A run's identity includes `GitCommit`, `GitDirty`, and `ConfigHash`. When
`GitDirty` is false, `GitCommit` fully describes the code that ran — checking
out that commit reproduces it. When `GitDirty` is true, uncommitted changes
were present at run time, and `GitCommit` alone no longer describes what
actually executed. `ConfigHash` is the run's own hash of the config it read,
independent of git state, so a caller can still supply a stable handle on
"what ran" even with a dirty tree.

## Decision

`Run.Validate` (`internal/lineage/run.go`) refuses a record where `GitDirty`
is true and `ConfigHash` is empty:

```go
var ErrDirtyTree = errors.New("git_dirty is set but config_hash is empty: the run is not reconstructible")
```

A dirty run with a non-empty `ConfigHash` is accepted; a clean run needs no
`ConfigHash` at all.

## Consequences

- A run recorded from an uncommitted working tree with no config hash is
  rejected outright rather than stored with a `GitCommit` that quietly
  understates what ran. There is no "best effort" partial record for this
  case.
- Callers that iterate against a dirty tree (common during early
  experimentation) must compute and pass a config hash, which pushes that
  responsibility onto whatever harness calls `POST /runs` — not onto the
  ledger to guess or accept an unreconstructible record.
- `ConfigHash` alone does not make a dirty run fully reproducible — it is a
  content handle on the config, not on the full diff of the working tree.
  This decision treats "we know it wasn't the plain commit, and we have
  something stable to distinguish one dirty run from another" as the bar, not
  "this run can be reconstructed byte-for-byte."

## What would have to be true to revisit this

A mechanism that captures the actual diff (e.g. a patch hash or an
auto-committed shadow ref) would make a dirty tree fully reconstructible
without a separately-supplied config hash, and could replace this check with
a stronger one rather than removing it — the goal (never record an
unreconstructible dirty run silently) should survive any change here.
