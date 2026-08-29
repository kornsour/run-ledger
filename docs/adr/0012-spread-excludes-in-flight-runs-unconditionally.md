# ADR 0012: Spread excludes in-flight runs unconditionally, and reports their count instead

Status: Accepted
Date: 2026-08-29

> **`spread.One` and `spread.Compute` summarize only terminal
> (`succeeded`/`failed`/`cancelled`) runs, with no query parameter or flag
> to see it any other way.** A fingerprint's in-flight (`created`/`running`)
> runs are not silently dropped, though: `spread.Group` gains `InFlight
> int`, the count of them, reported beside the terminal-only `Count`. `GET
> /v1/fingerprints/{fingerprint}` stops 404ing for a fingerprint whose only
> runs are in-flight, since that fingerprint does exist — it just has
> nothing measured yet — and now says so with `count: 0, no_repeats: true,
> in_flight: N` instead of pretending not to have heard of it.

## Context

ADR 0005's "Revisited" section named this precisely: `spreadList` and
`spreadOne` (`internal/api/api.go`) handed every run for a project or
fingerprint to `spread.Compute`/`spread.One` without regard to `Status`, so
a `running` record's mid-training metric was counted as a *repeat
measurement*. Its half-finished loss widened the group's spread and could
rank that fingerprint top of "which experiments reproduce worst" — a false
positive on the one claim this ledger exists to make (same fingerprint,
different metrics, therefore something affecting the result went
unrecorded), produced by a run that had simply not finished yet.

Issue #23 fixed the first half of this already (`31f5199`): both handlers
filter to `lineage.Terminal` runs before computing spread, and a
fingerprint with zero terminal runs 404s. That much is correct and this
record does not reopen it. What #23 left undone, and issue #65 asks for, is
the second half: an in-flight run is excluded from the *numbers*, but it
still exists, and a caller staring at `count: 3` with no way to tell "and a
4th run is still going" is missing something real. `rlctl` polling a
fingerprint mid-sweep, or a dashboard rendering it, cannot distinguish "3
runs, done" from "3 finished, more coming" without this.

Two design questions had to be settled before writing the code:

1. Should the filter be unconditional, or should a caller be able to opt
   into (or out of) seeing non-terminal runs in the numbers?
2. Where does the terminal/in-flight split happen, given `spread.One` is
   called with runs already fetched for one fingerprint
   (`Store.List(Query{Fingerprint: fp})`), and `spread.Compute` groups a
   wider listing (`Store.List(Query{Project: p})`) and calls `One` per
   group internally? Both have to agree, and "both have to agree" is
   exactly the kind of requirement that drifts if it is satisfied by two
   independent call sites remembering the same rule.

## Decision

### No opt-out, no query parameter

`spread.One` and `spread.Compute` always exclude non-terminal runs from
`Count`, `Metrics`, `Provenance`, and `RunIDs`. There is no
`include_in_flight=true` or similar to ask for the old, unsafe reading.

The alternative — a query parameter defaulting to the safe (terminal-only)
behavior, with an explicit opt-in to include everything — was rejected.
Issue #65 was explicit that whichever way this went, the default had to be
the safe one, since the unsafe reading is what produces the false positive.
Once the default is safe, the only thing an opt-in parameter would add is a
way to *deliberately* reproduce the false positive. Nothing in this ledger
needs that: a running run's metric is not a competing valid interpretation
of "the group's spread" that some caller might legitimately prefer, the way
`min_runs` genuinely does need to be adjustable (deciding what counts as
"a repeat" is a real, disputable threshold). It is a category of data —
unfinished measurements — that never belongs in a finished-measurement
statistic, so there is no request that opt-in would serve honestly. Adding
one anyway would mean a hazard this ADR exists to close is still one query
parameter away, waiting for a future caller who does not know why it is
dangerous.

`InFlight` covers the actual reason someone would have reached for a
"show me the in-flight ones too" flag: visibility into what is still
running. It gives that without also reopening the numbers.

### The split lives in `spread.One`, once

Both handlers now pass unfiltered runs (of any status) straight to
`spread.Compute`/`spread.One`. The terminal/in-flight split happens inside
`One`, and `Compute` calls `One` once per fingerprint group internally
(unchanged from before this record). That means `spreadList` and
`spreadOne` cannot disagree on the filter by construction — there is
exactly one place status is inspected, not two call sites each expected to
remember to pre-filter before calling in. Before this change,
`internal/api/api.go` had its own `terminalRuns` helper that both handlers
called before handing runs to `spread`; that duplicated the same status
check the `spread` package now owns, immediately outside the boundary of
what `spread`'s own tests can verify. Moving it inside `spread` means the
tests in `internal/spread/spread_test.go` — not just
`internal/api/api_test.go` — pin the exclusion and the count.

### `GET /fingerprints/{fingerprint}` 404s only when no run at all carries the fingerprint

`spreadOne` previously 404d whenever the terminal-filtered run list was
empty, which included the case of a fingerprint that exists only as
in-flight runs (issue #23's second case, `TestFingerprintOneAllNonTerminalIs404`
prior to this change). That reading conflicts with `GET /v1/fingerprints`
itself: a `min_runs=0` (or `1`) request already lists a fingerprint the
moment `spread.Compute` produces a group for it, and a group now exists
for any fingerprint with at least one run of any status. Leaving the item
endpoint 404ing for that same fingerprint would be exactly the "two
disagreeing notions of the fingerprints" the collection endpoint's own
`min_runs` doc comment already treats as a bug to avoid — it would just be
reintroducing it at a different boundary (terminal-only existence) instead
of the one already fixed (repeat-only existence).

`spreadOne` now 404s only when `Store.List(Query{Fingerprint: fp})` returns
no runs at all. A fingerprint with only in-flight runs returns 200 with
`count: 0, no_repeats: true, in_flight: N` — genuinely nothing measured
yet, stated as such, rather than reported as though the fingerprint were
unknown to the store.

## Consequences

- `spread.Group` gains `in_flight`, a required field in the OpenAPI
  `SpreadGroup` schema (`docs/openapi.yaml`) and checked by
  `TestSchemasMatchGoTypes`.
- A group's `run_ids` and `count` describe terminal runs only; an in-flight
  run is visible solely via `in_flight`, never by run ID. Nothing today
  needs to know *which* run is still going from this endpoint — `GET
  /v1/runs?fingerprint=...` already answers that with full records,
  status included.
- `GET /v1/fingerprints/{fingerprint}` for a fingerprint with only in-flight
  runs changes from 404 to 200 (`TestFingerprintOneAllInFlightIsNoRepeatsNotNotFound`
  supersedes the old `TestFingerprintOneAllNonTerminalIs404`). 404 is now
  reserved for a fingerprint no stored run carries at all.
- `min_runs` at `GET /v1/fingerprints` continues to compare against the
  terminal-only `Count`, unchanged: a fingerprint with one finished run and
  three still running does not pass `min_runs=2` on runs that have not
  produced a measurement.
- Nothing changes for a project where every run finishes before the next
  one starts — the default behavior these endpoints already had for that
  case is exactly what this record keeps.

Revisiting the "no opt-out" half of this decision would need a caller with
a legitimate reason to see an in-flight run's metric folded into a
finished-run statistic. Debugging is not that reason: `GET
/v1/runs?fingerprint=...` already returns every run, in-flight included,
with its own status and metrics intact — nothing is hidden, only kept out
of one specific aggregate that exists to answer a narrower question than
"what happened."
