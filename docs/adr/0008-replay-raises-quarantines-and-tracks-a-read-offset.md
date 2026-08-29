# ADR 0008: `replay_spool()` raises on an unreachable ledger, quarantines permanent rejections, and rewrites the spool by tracked read offset

Status: Accepted
Date: 2026-08-28

## Context

`Run` spools a record to `~/.runledger/spool.jsonl` when the ledger is
unreachable, because recording must never fail a training job (ADR 0005's
sibling concern on the write side). Getting those records back into the
ledger was, until now, a shell snippet in `python/README.md` — no auth, no
failure detection, and it never drained the spool.

`python/runledger/replay.py` replaces that snippet with
`runledger.replay_spool()`. Three things about it are decisions, not just
implementation, and each is cheap to get wrong silently:

1. What happens when the ledger cannot be reached at all.
2. What happens to a record the ledger will *never* accept, as opposed to
   one it currently can't.
3. How the spool file gets rewritten without racing a training run that is
   still appending to it.

## Decision

**1. Replay raises `LedgerUnreachableError`; it does not degrade.**
`Run` warns and spools because an expensive training job must not die over
a recording failure. Replay has no such job to protect — it is a command a
person runs on purpose, after training, specifically to recover records —
so it follows the read side's convention (`read.py`) instead: a ledger that
cannot be reached raises. Reporting "0 sent, 3 unreachable" and returning
normally would invite exactly the silent, easy-to-ignore failure mode ADR
0005's sibling reasoning warns against on the read side.

**2. A `400` or `409` is quarantined to `<spool>.rejected.jsonl`, not
retried.** Replay is safe to interrupt and re-run *because* the server's
identity rule makes `POST /runs` idempotent: `started_at` is client-supplied
and part of the payload, `run_id` is derived from the fingerprint plus that
timestamp, and re-recording identical content against an existing id
succeeds again rather than conflicting (`store.Record`). That guarantee
does not extend to a `400` (the payload itself is invalid) or a `409` (a run
with this id already exists with *different* content) — retrying the same
bytes against the same server will get the same answer every time. Leaving
a record like that in the spool would have every future replay attempt it
again, alongside records that only need the server to come back, and a
handful of permanently-bad records would eventually dominate every replay
attempt. Any other status — a `5xx`, a connection refused, a timeout — says
nothing about the payload and stops the replay per (1) instead.

**3. The spool is rewritten from a byte offset recorded at the start of the
call, not from what replay computed alone.** A training run can append to
the spool while a replay is in flight. `replay_spool()` records
`len(raw)` from its initial read, and `_rewrite()` re-opens the file at
rewrite time, seeks to that offset, and appends whatever is there now ahead
of the lines replay is keeping. This gets the same safety a lock would
without needing one — the standard library has no cross-platform advisory
lock — at the cost of two file opens on every call instead of one. Both the
spool and the quarantine file are written through a temp file plus
`os.replace()`, so a process killed mid-rewrite leaves one complete file,
never a half-written one.

## Consequences

- A user who runs `replay_spool()` (or `python -m runledger.replay`)
  against a down ledger sees an exception, not a quiet zero-progress
  return — consistent with every other explicitly-invoked ledger call in
  this client.
- A malformed or permanently-conflicting record cannot wedge future
  replays: it is moved out of the spool once, onto disk, where it stays
  until a person looks at it. Nothing here deletes a `.rejected.jsonl`
  entry automatically — recovering from it, if that is even possible, is a
  decision for whoever reads the quarantine file, not this client's to
  make.
- Replay's own failure mode is "stop at the first non-permanent error and
  keep everything from there on in the spool," not "attempt every record
  and report a mixed bag." A ledger that is down will make every subsequent
  request fail the same way, so stopping avoids `N` timeouts to report what
  the first one already said; the cost is that one already-rejected record
  earlier in the file does not prevent a good record after it from being
  attempted (rejections are checked per record, before the stop condition
  can apply), while a *non-rejection* failure partway through the file
  does — records after it are not attempted this call, even if some of
  them would have succeeded.
- `ReplayResult` has no `unreachable` count: a call either completes and
  reports `sent`/`rejected`/`remaining`, or it raises. There is no partial
  state to name in the successful-return case, because reaching the
  unreachable case always raises before returning.
