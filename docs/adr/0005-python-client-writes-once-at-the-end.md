# ADR 0005: The Python client writes the ledger exactly once, at the end of a run

Status: Accepted
Date: 2026-08-27

## Context

`runledger.Run.start()` (`python/runledger/run.py`) is a context manager
wrapping a training loop. The obvious design mirrors what a live dashboard
wants: record the run as `running` the moment it starts, so it is visible
immediately, then update it to `succeeded` or `failed` when it ends.

`POST /runs` does not support that. Recording is idempotent only for a
byte-identical re-record of the same run id (`store.Record`,
`internal/store/memory.go`); the same id recorded again with different
content — a different `status`, new `metrics` — returns `ErrConflict`. There
is no `PATCH`. [Issue #1](https://github.com/Lurking-Walrus/run-ledger/issues/1)
tracks adding one; until it lands, a run's record cannot be updated after
it is written.

## Decision

`Run` buffers status and metrics locally for the run's entire lifetime —
`log_metric()` writes to an in-memory dict, nothing more — and makes exactly
one `POST /runs` call, from `__exit__`, once the outcome (`succeeded` or
`failed`, from whether the `with` body raised) is known. `run.run_id` and
`run.fingerprint` are unset until that call completes.

## Consequences

- One run produces exactly one ledger record, with a final, accurate status
  and the full set of metrics logged before the run ended — never a
  `running` record left behind if the write-running/update-finished pair
  had been used instead and the process died before the update.
- There is no live "this run is currently in progress" record visible in
  the ledger while training runs. A dashboard cannot show in-flight runs
  from this client today.
- A crash that prevents `__exit__` from running at all (`os._exit`, `SIGKILL`,
  the interpreter itself segfaulting) means nothing is recorded, including
  no `failed` record — there is no partial write to find later. An `except`
  block, `KeyboardInterrupt`, or any exception that unwinds the Python call
  stack normally *does* trigger `__exit__` and produces a `failed` record;
  only a kill that bypasses Python's own exception handling does not.

## What would have to be true to revisit this

Once issue #1 lands a `PATCH` (or equivalent) endpoint, `Run.start()` could
record a `running` run at entry and `PATCH` it at exit instead — trading
"exactly one record, always accurate" for "a live record, occasionally
never updated if the process is killed outright." That is a genuine
trade-off, not a strict improvement, and should be a deliberate choice made
against the finished `PATCH` semantics (e.g., what a stale `running` record
that a crash left behind renders as), not a byproduct of adding the
endpoint.
