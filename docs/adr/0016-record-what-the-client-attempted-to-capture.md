# ADR 0016: Record what the client attempted to capture, not just what it captured

Status: Accepted

Date: 2026-08-29

> **`lineage.Run` gains `Capture *CaptureDeclaration`, kept alongside the run
> and never hashed.** `CaptureDeclaration` carries `Client` (which client
> library and version wrote this) and `Attempted` (which of the seven ADR
> 0011 fields that client's recording code actively tried to determine).
> `device: ""` with `"device"` in `Attempted` means the client looked and
> found nothing -- a fact about the environment. `device: ""` with `"device"`
> absent from `Attempted` means the client never looks -- a fact about the
> pipeline. Neither is expressible by widening `Device` itself, which is why
> this is a new field next to it rather than a change to it.

## Context

Issue #68 proposed exactly this shape and then argued, at equal length, that
it should not be built yet. The issue is worth quoting rather than
paraphrasing, because the caution it recorded is the reason this ADR exists
to state plainly rather than let quietly lapse:

> The trigger to build this is the first time something wants to **act** on
> the answer automatically, because a heuristic cannot carry that weight:
> a gate ... fleet scale ... attribution across client versions ... Overlaps
> with #67.

None of those three triggers has fired. There is no gate today that refuses
runs from clients missing a field, `examples/churn/completeness.py`'s
peer-comparison heuristic is still the only consumer of "was this
captured", and this repository has exactly two clients (`rlctl`, the Python
package), not a fleet on divergent versions. By the issue's own stated
test -- *is there something that wants to act on the answer* -- the honest
answer is still no.

This ADR exists anyway because the repository owner asked for it directly,
having read that reasoning. That is a legitimate way for a "not yet"
decision to become a "now": the issue's caution was about avoiding a
speculative field nobody asked for, not about the field being a bad idea in
itself, and "the person who owns this repository wants it" is precisely the
kind of concrete want the issue said to wait for. What follows is not a
retraction of the issue's reasoning -- the reasoning was right about what
makes this worth building, and stays right about what a good version has to
honor. It is a record that the trigger this time was a direct ask, not one
of the three the issue named, and that the caution was read, not skipped.

The underlying gap is the one ADR 0011's own "what would have to be true to
revisit this" section already named: "did the client try?" is a fact about
the recording process, not the experiment, and a nullable value field is
the wrong place for it -- `device: ""` can say *this run has no device*,
but it cannot say *this client does not know how to look for one*. Those
are different claims, and ADR 0011 declined pointers for exactly the reason
one field cannot hold both.

## Decision

### Shape

```json
{"device": "",
 "capture": {"client": "runledger-py/0.1.0",
             "attempted": ["host", "device", "framework_version"]}}
```

`lineage.CaptureDeclaration` (`internal/lineage/run.go`):

```go
type CaptureDeclaration struct {
	Client    string   `json:"client"`
	Attempted []string `json:"attempted,omitempty"`
}
```

`Attempted` may only name a field from `lineage.CaptureFields` -- the same
seven ADR 0011 gives a single "not recorded" meaning to (`config_hash`,
`dataset_version`, `model_version`, `host`, `device`, `framework_version`,
`checkpoint_uri`). `Run.Validate` rejects anything else, the same way it
rejects an unrecognized `Status`.

### Provenance, and never hashed -- `Capture` is the one pointer field on `Run`

`Capture` is added below the identity/provenance boundary; `Compute` never
reads it. `internal/lineage/run_property_test.go`'s
`TestProperty_MutatingAProvenanceFieldLeavesFingerprintUnchanged` and
`TestProperty_EveryRunFieldIsClassified` enforce this by construction, and
`Capture` is tagged `lineage:"provenance"` in `run.go` to be picked up by
them (see below).

Every other field ADR 0011 or ADR 0015 added stayed a plain `string`, and
that ADR rejected pointers specifically because a nullable *identity*
field would make the fingerprint depend on how a client serializes an
unset value. `Capture` is provenance, never hashed, so that objection does
not apply here -- but a different, real distinction does, and it is why
`Capture` is a pointer rather than a plain struct with `omitempty` (which
`encoding/json` does not honor for non-pointer structs regardless): `nil`
means no client ever sent a declaration for this run; non-nil means a
client asserted (possibly empty) knowledge of its own capture behaviour.
`examples/churn/completeness.py`'s peer-comparison fallback (below) depends
on telling those two apart -- collapsing them the way ADR 0011 collapses
`""` and absent would silently break that fallback for the runs it exists
to cover, which is most of them today.

### The identity/provenance split now comes from `run.go` itself

Issue #79's property-test work (`run_property_test.go`) had already asked
for the identity/provenance boundary to be derived from `lineage.Run`
rather than hand-duplicated in the test file, and considered a struct tag
for it -- rejecting the idea only because that task was scoped not to touch
`run.go`. This change is not so scoped, so every field on `Run` now carries
a `lineage:"identity"` or `lineage:"provenance"` struct tag, and
`identityFieldNames`/`provenanceFieldNames` in the test file are derived
from those tags via `reflect` instead of being written out twice.

This does not make the mutation tests redundant. A struct tag is an
assertion, not a proof: nothing stops a future field from being tagged
`provenance` while `Compute` quietly hashes it anyway. That is exactly what
`TestProperty_MutatingAProvenanceFieldLeavesFingerprintUnchanged` still
catches, by calling the real `Compute` and checking the real fingerprint --
the tag only removes the hand-duplication of *names*, not the need to
verify *behaviour*. `TestProperty_EveryRunFieldIsClassified` remains the
loud-failure backstop for the other mistake: a field with no tag at all,
which would otherwise fall through untested.

### Persisted by both store backends

`internal/store/conformance.go` gained subtests covering: a declaration
round-trips through `Record`/`Get` and through `List`; a run with no
declaration reads back with `Capture == nil`, not an empty one; filtering by
`capture_client`; and that `Update` never disturbs a run's declaration.
Both `Memory` and `DuckDB` pass the same suite.

`DuckDB` stores `Attempted` in a side table (`run_capture_attempted`,
`(run_id, field)`), the same shape `run_params`/`run_metrics` already use
for a variable-length, per-run collection, and two scalar columns on
`runs`: `capture_declared BOOLEAN` and `capture_client VARCHAR`.
`capture_declared` is what carries "no declaration at all" as a state
distinct from "declared, with an empty client name" -- `capture_client`
alone can't: both would read back as `''`. Both columns default (`DEFAULT
FALSE` / `DEFAULT ''`, not `NOT NULL` -- this DuckDB build rejects an
inline constraint on `ADD COLUMN`), so every pre-migration row backfills to
exactly "no declaration", which is the true state of a row written before
this feature existed. `TestDuckDBLegacyRowsHaveNoCaptureDeclaration` pins
this the same way the fingerprint-version and attribution migrations
already pin their own backfill.

`Attempted` is conceptually a set, but a `Store`'s idempotent re-record
check (and DuckDB's own side table, which has no row order of its own)
both compare it as a concrete slice. `lineage.Run.NormalizeCapture` sorts
it into a canonical order; every `Store.Record` calls it before comparing
or persisting, so a byte-identical retry that happened to build the list in
a different order is not refused as a spurious conflict.

### Not patchable

`PATCH /v1/runs/{id}` does not accept `capture`, unlike `host`, `device`,
`framework_version`, `submitter_claim`, and `job_id`. Those fields are
patchable because a run is often recorded before every fact about it is
knowable -- a device only knowable once a job schedules onto specific
hardware, a job id assigned by the scheduler after submission. A capture
declaration has no such case: it describes what the recording client's own
code tried, which the client already knows in full at the moment it makes
the very first request for a run (both `rlctl` and the Python client build
it before sending anything, start-time write included). Allowing a later
patch to set it would also raise a question the other patchable fields
never have to answer -- whose declaration is it, if a different call
supplies it after the run already exists -- for no case that needs
answering. `internal/store.Patch`'s doc comment states this explicitly, not
just by the field's absence.

### Filterable, not (yet) wired into spread

`GET /v1/runs?capture_client=` filters exact-match, the same shape
`device`/`submitter_claim`/`job_id` already have -- this is the "attribution
across client versions" trigger the issue named directly: "did capture
regress in client 2.3" is a query over runs by `capture_client`, not a
statistic. `rlctl list --capture-client` exposes it the same way
`--submitter-claim`/`--job-id` already do.

`internal/spread.provenanceFields` (the columns `GET /v1/fingerprints`
calls out as a likely explanation for a group's spread) does not gain
`capture.client`. `device`/`framework_version`/`submitter_claim`/`job_id`
are there because a disagreement on any of them is itself a plausible cause
of two runs measuring differently, or a strong pointer to one. Which client
recorded a run is not that kind of fact -- a capture declaration describes
the *recording*, not anything that could make training behave differently
-- so it does not belong on the same list, and is left off on purpose
rather than added reflexively because a new provenance field exists.
Auditing capture coverage is a different job than explaining a spread, and
`declared_blind_spots` (below) is where that job lives.

### `compare.Runs` reports two provenance fields

`capture.client` (optional string, "" means not recorded, same rule as
every other scalar provenance field) and `capture.attempted` (the set,
rendered sorted and comma-joined so two declarations naming the same
fields in different orders never manufacture a diff that isn't there). Both
are `KindProvenance`: a capture declaration never makes two runs different
experiments. A diff has no use for "no declaration" versus "declared, and
declared nothing" -- both render as absent, the same normalization ADR
0011 already applies to every other scalar field. That distinction is
real, but it belongs to `completeness.py`'s fallback logic, not to a
two-run diff.

### Both clients populate it

`rlctl` declares `Attempted: ["host"]` -- the only field it auto-detects
(`os.Hostname()`, called unconditionally in `cmdRecord`); `--device`,
`--dataset`, `--model`, `--config-hash` are plain pass-throughs of whatever
a caller typed, not something `rlctl`'s own code looks for, so they do not
belong in `Attempted`. The Python client declares
`Attempted: ["host", "device", "framework_version"]` -- all three
unconditionally attempted every time `_identity_and_provenance()` builds a
payload -- which is the issue's own worked example, not a coincidence:
`_provenance.device_name()` now correctly returns `""` when it cannot
determine a device (#66), and `attempted` is precisely what disambiguates
that `""` from "this client doesn't try". `internal/conformance`'s
cross-client fingerprint suite stays green unmodified, because `Capture`
is provenance -- it was never going to affect what the suite checks.

### `examples/churn/completeness.py` defers to a declaration when one exists

The peer-comparison heuristic (`odd_ones_out`, `blind_spots`) existed only
because "did the client try" was unavailable, and stated so in its own
module docstring. It still is unavailable for most runs recorded so far,
so it is not removed -- it is demoted to the fallback for exactly the runs
it always covered: ones with no capture declaration.

A run that carries a declaration is never a candidate for `odd_ones_out`
now, regardless of what it is missing: flagging it would report an
inferred, lower-confidence guess ("probably a bad launch") over a fact the
run already states outright ("this client never looks for X", or "it
looked and genuinely found nothing"). A new function, `declared_blind_spots`,
reports the ground-truth counterpart of the pipeline-level signal: a field
every capture-declaring run in the project agrees it never attempts, which
needs no peer-share threshold because it is not an inference. `report()`
surfaces it as "known blind spots", worded to read differently from the
inferred "blind spots (never recorded)" line beside it -- one is a
statistical prior, the other is what the clients themselves said.

## Consequences

- A capture declaration is exactly as self-asserted as `Host`/`Device`
  already are: a caller can claim `Attempted: ["device"]` and never
  actually look. Nothing about this field is attested. That is an accepted
  gap, not an oversight, the same posture ADR 0015 takes for
  `submitter_claim`.
- Every run recorded from here on by `rlctl` or the Python client carries a
  declaration; every run recorded before this change, or through raw HTTP
  without one, does not, and reads back with `Capture == nil` rather than
  an empty object. `completeness.py`'s fallback exists because of that
  imbalance and will keep mattering for as long as it lasts.
- `docs/openapi.yaml` documents `capture` as omitted-by-default on `Run`
  and `RunInput`, present only when a client sends one, and absent from
  `RunPatch` entirely -- `TestSchemasMatchGoTypes` pins the Go/spec field
  sets agreeing, and `TestRoutesMatchSpec` pins the routes.

## What would have to be true to revisit this

Nothing about the shape needs revisiting on its own terms. What could
change is scope: if a gate or an automated check ever wants to *act* on
`capture.attempted` (the first of the issue's three original triggers,
still not built), or if `capture.client` earns a place in
`spread.provenanceFields` because auditing coverage and explaining spread
turn out to be the same workflow after all, those are natural, additive
follow-ups -- not reversals of anything decided here.
