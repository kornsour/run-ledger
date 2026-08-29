# ADR 0015: Run attribution is provenance, and is self-asserted until named tokens exist

Status: Accepted
Date: 2026-08-29

> **`lineage.Run` gains two provenance fields: `submitter_claim` (who says
> they recorded this run) and `job_id` (the CI/Slurm/scheduler job that
> launched it). Neither feeds `Compute`.** The same experiment run by two
> people, or relaunched under a different CI job, is still the same
> experiment; making either field identity-bearing would split every
> fingerprint group by author and destroy the reason fingerprinting exists.
> `submitter_claim` is named the way it is on purpose: `RUNLEDGER_TOKEN` is
> one shared secret today, so the server has no per-caller identity to read
> it from and check it against. Whatever a client sends is a claim, not an
> attested fact, and the field's name says so rather than leaving that
> caveat to a doc comment a reader might not open.

## Context

Issue #67: `lineage.Run` has no field naming the person, job, or system
that recorded a run, and auth is a single shared bearer token, so the
server cannot distinguish callers either. That is fine for the single-user
local ledger the README calls the default, and becomes a real gap the
moment a second person points at the same server -- which is also the
moment the token stops being optional.

The ledger's headline output is "these two runs are the same experiment and
measured differently -- go find out why." The first question anyone asks
next is *whose runs are these, and were they launched the same way?* The
record cannot currently answer it, and the peer-comparison heuristic in
`examples/churn/completeness.py` is a workaround for exactly this blind
spot: it infers "a different launch path" precisely because nothing records
what the launch path was.

The issue separates two facts that both get called "who":

- the human or service account
- the launching context: CI job id, Slurm job id, hostname, client name and
  version

Hostname is already captured (`lineage.Run.Host`, populated by `rlctl
record` from `os.Hostname()`); this ADR does not touch it. Client name and
version is explicitly out of scope here -- the issue itself notes it
overlaps with #68, and a client reporting its own name and version is a
different, self-describing kind of fact than who launched a given run. That
leaves two gaps to fill: a submitter identity, and a job/scheduler
identifier.

## Decision

### Two fields, both provenance

```go
SubmitterClaim string `json:"submitter_claim"`
JobID          string `json:"job_id"`
```

Both are added below the `Provenance` boundary in `lineage.Run`
(`internal/lineage/run.go`): `Compute` never reads either one. This is the
one property this ADR must not compromise. Two people running the identical
experiment, or one person rerunning it under a different CI job, are still
the identical experiment -- if attribution fed the fingerprint, the ledger's
core promise ("same fingerprint means same experiment") would stop holding
the moment a second person or a second pipeline touched the ledger.
`TestFingerprintIgnoresAttribution` (`internal/lineage/run_test.go`) pins
this directly: two runs identical except for `SubmitterClaim` and `JobID`
must fingerprint identically. `TestAttributionDoesNotAffectFingerprint`
(`internal/api/api_test.go`) pins the same property through the HTTP API,
not just the Go type.

Both fields follow ADR 0011's rule -- extended, not restated there, since
0011 is scoped to the seven fields it names -- `""` means "not recorded",
not a value competing with absence. `compare.Runs` (`internal/compare`)
normalizes an empty value to a null side of the diff for both fields, the
same as it already does for `host`/`device`/`framework_version`.

### `submitter_claim`: self-asserted, and named to say so

ADR 0001's reasoning is the reason this field is not simply called
`submitter` or `user`: a caller that can assert a field can lie about it,
and the fingerprint is protected from that by never letting the client set
it. Attribution is not hashed, so it cannot corrupt the fingerprint the
same way -- but it can still mislead a reader who takes "submitted_by:
alice" at face value if alice did not, in fact, submit the run.

`RUNLEDGER_TOKEN` (`internal/api.Auth`) is one shared secret today: every
caller with write access presents the same token, so the server has no
per-caller identity to read a submitter from and check the client's claim
against. A submitter read from the token would be *attested*; one sent in
the request body is a *claim*. Named tokens -- one credential per person or
service account, rather than one shared secret -- are the prerequisite for
attesting it, and are not implemented. Naming the field `submitter_claim`
rather than `submitter` makes that gap visible at every call site and in
every response body, not just in a doc comment a reader might not open.
`docs/openapi.yaml`'s description repeats the caveat for the same reason:
the field is part of the public contract, and the contract should not read
like a verified fact it is not.

**What was deliberately not built:** named, per-caller tokens. That is a
larger, separately-scoped change (issuing, storing, and revoking
credentials; deciding what "read" vs "write" scope means per caller rather
than per token) that this ADR's field addition does not need in order to be
useful today. `rlctl record --submitter-claim WHO` requires an explicit
flag with no default -- unlike `--job-id` below, nothing auto-populates it
from the environment. `$USER` (or similar) is a weak proxy for "who is
accountable for this run": a shared CI runner account or a shared research
VM login would make every run under it claim to be the same "submitter,"
which is a worse failure mode than an empty field, because an empty field
is honestly "not recorded" while a wrong one is confidently wrong.

### `job_id`: self-asserted too, but not the same kind of claim

`job_id` does not name a person, so it does not carry the same
misattribution risk `submitter_claim` does -- forging it gains a caller
nothing the way falsely claiming credit (or blame) for someone else's run
would. It gets no special naming treatment for that reason, the same way
`host` and `device` (also self-asserted, also unverified) do not.

One generic field, not `CIJobID`/`SlurmJobID`/etc.: the set of schedulers
this ledger might see is open-ended, and a caller can always namespace its
own value (e.g. `"gha:4821001233"`) without the server needing to know
every launcher by name. `rlctl record` defaults `--job-id` from the first
of `$SLURM_JOB_ID`, `$CI_JOB_ID`, or `$GITHUB_RUN_ID` that is set (in that
order) -- these are objective facts about the launching environment, the
same kind of fact `os.Hostname()` already supplies for `Host`, so
auto-populating is consistent with how `Host` already behaves and does not
raise the misattribution concern `submitter_claim` does.

### Filterable and patchable

`GET /v1/runs` gains `?submitter_claim=` and `?job_id=` filters, exact-match
only, consistent with the existing `device` filter -- this is the "filter
to your own runs" workflow #67 names directly. They compose with every
other filter the same way `device` already does.

`PATCH /v1/runs/{id}` accepts both as patchable fields, the same as `host`,
`device`, and `framework_version` already are. This follows directly from
classifying them as provenance rather than identity: `store.applyPatch`
(`internal/store/store.go`) only rejects identity-field changes as a
conflict, and provenance fields are applied when the patch sets them. A run
is often recorded before every provenance fact about it is known -- a CI
job id assigned after the record call fires, a submitter added once a
human operator picks the run up -- and being provenance means filling
either in later cannot rewrite what experiment the run was, the same
guarantee that already covers `host`/`device`/`framework_version`.

### Not done here: wiring attribution into spread

`internal/spread.provenanceFields` already lists `device` and
`framework_version` as the provenance columns most likely to explain a
same-fingerprint metric spread -- adding `submitter_claim` and `job_id` to
that list is the natural next step for the reason #67 gives directly:
"noticing that a fingerprint group's spread lines up with who submitted
each run rather than with anything technical" is exactly the workflow this
ADR exists to unblock. It is deliberately not done in this change: this
branch is stacked on, and scoped to avoid touching the same files as,
concurrent work elsewhere in the repository, and `internal/spread` was out
of that scope. `submitter_claim`/`job_id` are fully recorded, queryable,
and diffable (`compare.Runs`) as of this change regardless -- only the
`GET /v1/fingerprints` spread summary's own provenance-diff list does not
yet mention them.

## Consequences

- `submitter_claim` is a claim, and every consumer of it -- `rlctl`,
  dashboards, anything reading the API -- must treat it as one. Nothing in
  this change stops a malicious or careless caller from claiming to be
  someone else. That is an accepted, documented gap, not an oversight: the
  alternative was shipping a field that reads like a verified fact while
  being exactly as forgeable as any other client-supplied string, which is
  the worse failure mode.
- Filtering `GET /v1/runs?submitter_claim=alice` finds runs claimed as
  alice's, not runs cryptographically proven to be hers. The query
  parameter's description says so.
- Two new required-but-possibly-empty columns land on every `Run` response
  and every DuckDB row. `docs/openapi.yaml` marks both required in the
  `Run` schema (present, possibly `""`) and optional in `RunInput`/
  `RunPatch` (omittable, defaulting to `""`) -- the same shape as `host`,
  `device`, and `framework_version`.
- The DuckDB migration (`internal/store/duckdb.go`) adds both columns with
  `DEFAULT ''`, not `NOT NULL` -- this DuckDB build rejects `ADD COLUMN`
  with an inline constraint. Unlike the `fingerprint_version` migration
  (ADR 0013), there is no separate "legacy" sentinel to backfill existing
  rows to: `""` already means exactly what a pre-migration row's true state
  is ("not recorded"), so the plain default is correct, not a stopgap.

## What would have to be true to revisit this

Named, per-caller tokens replacing the single shared `RUNLEDGER_TOKEN`.
Once the server can distinguish callers, `submitter_claim` (or a new,
separately-named field alongside it) could be populated from the
authenticated caller's identity rather than the request body, and the
server could reject a client-supplied value that disagrees with it --
turning a claim into an attestation, the same shift ADR 0001 already made
for the fingerprint itself. Until then, the field's name is the honest
description of what it is.
