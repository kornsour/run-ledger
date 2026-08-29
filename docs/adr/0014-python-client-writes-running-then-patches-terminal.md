# ADR 0014: The Python client writes a `running` record at start, and patches it to a terminal status at the end

Status: Accepted
Date: 2026-08-29

Supersedes [ADR 0005](0005-python-client-writes-once-at-the-end.md).

## Context

ADR 0005 chose to buffer a run's status and metrics locally and write the
ledger exactly once, in `__exit__`, once the outcome was known. Its own
"Revisited" section named the precondition for changing that: `spread`
(`internal/api/api.go`) had to consider only terminal runs, or a `running`
record's mid-training metric would be counted as a repeat measurement and
could rank a fingerprint top of "reproduces worst" -- a false positive on
the ledger's central claim.

#52 ("Exclude non-terminal runs from spread") landed that precondition:
`terminalRuns()` now filters both `spreadList` and `spreadOne`
(`internal/api/api.go`) before either ever reaches `spread.Compute`. Issue
#64 was filed believing this filter was still missing, and blocked on issue
#65 to add it. #65's first comment corrected that: the filter already
exists, landed by #52 before #64 was even filed, and #64 was never
actually blocked on it.

The reason ADR 0005 gave for writing once no longer holds. Meanwhile, that
design has carried a real cost the whole time it stood: `Run.start()`
wrote the ledger only from `__exit__`, and nothing runs `__exit__` for a
`SIGKILL`, an OOM kill, or a scheduler escalating past its grace period --
the single most common way a real training job actually dies. Those
produced *zero* trace: not a `failed` record, not a spooled line, nothing.
`SIGKILL` cannot be caught by anything running in the process that
receives it; the only way to survive it is to already have a record on the
server before it arrives. `/v1/fingerprints` no longer rewards writing a
`running` record early with a spread false positive, so writing early is
now the only cost-free way to survive an uncatchable kill, and the balance
that held ADR 0005 in place has flipped.

## Decision

`Run.start()` now writes the ledger twice:

1. At `_enter()`, immediately after git context is captured and validated
   (before any training happens), it `POST`s a `running` record. Success
   populates `run.run_id` and `run.fingerprint` right away, rather than
   only at the end of the run.
2. At `_finish()`, it `PATCH`es that run to its terminal status
   (`succeeded`/`failed`) with `ended_at` and the metrics logged before
   then. This is exactly the `rlctl start` / `rlctl finish` lifecycle
   (`cmd/rlctl/main.go`), and exactly the `created -> running ->
   {succeeded, failed, cancelled}` transition the server already enforces
   (`legalTransitions` in `internal/store/store.go`).

This closes the gap ADR 0005 named as its own remaining consequence: a
kill that bypasses `__exit__` entirely now still leaves the `running`
record behind, even though nothing ever updates it to a terminal status.
That is strictly better than the zero-trace status quo, and it is exactly
what a person debugging a pile of dead runs needs to reconstruct: which
configuration was running, on what commit, and when it stopped answering.

`SIGTERM` -- the signal a scheduler almost always sends before escalating
to `SIGKILL` -- is now also handled directly. A handler installed at
`_enter()` (main thread only; Python refuses to install a handler anywhere
else) records the active run(s) as `failed`, then chains to whatever
handler was previously installed, or restores the default disposition and
re-sends the signal to this process if there wasn't one worth calling --
so the signal still kills the process the way its sender intended, rather
than being silently absorbed. An `atexit` hook backstops whatever that
misses (a run started off the main thread; a chained handler further up
that calls `sys.exit()` instead of dying from the signal itself).

**Recording must still never fail the training run**, so the start-time
write follows the write side's existing degrade-to-warning rule
(`python/runledger/_run.py`'s "never let recording fail the training run"
design note), with one difference: it does not spool on failure. A
`running` record with nothing to ever follow it up is worse than no record
-- and spooling it would conflict with a spool contract (ADR 0008) that
already treats a spooled line as one *complete* run. Concretely:

- If the start-time `POST` fails, `_finish()` finds no `run_id` and falls
  back to a single full `POST` at the end -- exactly ADR 0005's original
  one-shot behaviour -- which spools on failure exactly as it always did.
- If the start-time `POST` succeeds but the closing `PATCH` fails, the
  full record is spooled in the patch's place. Replay (`replay.py`, ADR
  0008) only knows how to resend a complete `POST /runs` body, not a
  partial patch, so replaying that spool line creates a second, terminal
  row rather than resurrecting the original `run_id` -- leaving an
  orphaned `running` row behind that never reaches a terminal status. That
  row is invisible to `spread` either way (`terminalRuns`, #52); losing
  the run's final metrics would be the worse outcome of the two.

## Consequences

- A run killed by `SIGKILL`, the OOM killer, or a scheduler's hard timeout
  now leaves a `running` record behind: accurate identity and provenance,
  the `started_at` it began at, no terminal status and no final metrics --
  but it exists, and it's visible with `rlctl list --status running` (or
  `runledger.runs(status="running")`) to a person reconstructing what died.
- A run killed by `SIGTERM` -- which is most of them; `SIGKILL` is usually
  a scheduler's second resort after a grace period -- is now recorded
  `failed` with whatever metrics had been logged, the same as an
  in-process exception, provided the process is in its main thread (Python
  does not allow installing signal handlers anywhere else).
- The ledger now has a live "this is currently running" record while
  training runs, which ADR 0005 listed as a capability this client lacked.
  A dashboard can show in-flight runs from this client for the first time.
- Two HTTP calls per run instead of one, and up to two `RuntimeWarning`s
  instead of one in the fully-degraded (ledger unreachable for the whole
  run) case. Both still spool at most once, not twice.
- A `PATCH` failure after a successful start write leaves a permanently
  non-terminal `running` row behind once the spooled replacement is
  eventually replayed. Nothing here cleans that row up automatically --
  that is a known, accepted cost, not an oversight.
