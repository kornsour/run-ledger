# ADR 0005: The Python client writes the ledger exactly once, at the end of a run

**Superseded by [ADR 0014](0014-python-client-writes-running-then-patches-terminal.md).**

Status: Accepted
Date: 2026-08-27

## Context

`runledger.Run.start()` (`python/runledger/_run.py`) is a context manager
wrapping a training loop. The obvious design mirrors what a live dashboard
wants: record the run as `running` the moment it starts, so it is visible
immediately, then update it to `succeeded` or `failed` when it ends.

When this was decided, `POST /runs` could not support that: recording is
idempotent only for a byte-identical re-record of the same run id
(`store.Record`), and the same id recorded again with different content —
a different `status`, new `metrics` — returns `ErrConflict`. There was no
`PATCH`.

**That is no longer true**, and the decision below now rests on a different
reason. See "Revisited" at the end.

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

## Revisited 2026-08-28: the decision stands, for a different reason

`PATCH /runs/{id}` landed in #22, closing issue #1 the same day this ADR was
written. The precondition above was met within hours, so the stated context
was stale almost immediately. The decision does not change, but its
justification does, and the old one must not be quoted: it is false.

This ADR asked to be revisited "against the finished `PATCH` semantics
(e.g., what a stale `running` record that a crash left behind renders as)."
That question now has a measured answer, and it is worse than expected.

**A non-terminal run is invisible to the read side as a special case.**
`spreadList` and `spreadOne` (`internal/api/api.go`) call `store.List` with
`Query{Project: …}` and `Query{Fingerprint: …}` — neither sets `Status` —
and hand every returned run to `spread.Compute`. `spread` itself never
inspects `Status`. So a `running` record is counted as a *repeat
measurement* of its experiment:

- it satisfies the `Count > 1` test, so a fingerprint with one finished run
  and one in-flight run stops reporting `no_repeats` and starts reporting a
  spread;
- its mid-training metric joins the group's min/max/mean/stddev;
- `Group.Widest()` ranks by coefficient of variation, so a half-finished
  loss sitting beside a final one can rank that fingerprint **top** of
  "which experiments reproduce worst."

That is a false positive on the one claim this ledger exists to make — same
fingerprint, different metrics, therefore something affecting the result
went unrecorded — produced by a run that had simply not finished. The client
does not create that condition, so it keeps writing once, at the end.

Two things follow, and neither is hypothetical:

1. **The hazard already exists without this client.** `rlctl record` +
   `rlctl start` writes exactly the `running` record described above, and
   `spread` already ingests it. This is a defect in the read side today, not
   a consequence of changing the client.
2. **The precondition for revisiting is now specific.** `spread` must
   consider only terminal runs (`lineage.Terminal`), or report non-terminal
   ones as a separate, clearly-labelled count. Once it does, two-phase
   writing becomes attractive rather than merely possible — it would also
   close the gap named in Consequences above, where a `SIGKILL` leaves no
   record at all. Until then, "exactly one record, always accurate" is worth
   more than live visibility.
