# ADR 0012: Param values are normalized before hashing, and the fingerprint is now versioned

Status: Accepted
Date: 2026-08-29

> **A param value that parses as a finite decimal number is rewritten to a
> canonical spelling before it is hashed into the fingerprint.** "3e-4",
> "0.0003", and "0.00030" are the same learning rate and must fingerprint
> identically; before this change they did not, and nothing detected it.
> This changes what `Compute` hashes, so per ADR 0004 it ships with a
> `fingerprint_version` field on every run and existing records are
> migrated to version 1 rather than silently reinterpreted.

## Context

`lineage.Run.Compute` hashed each param value as the exact string it
arrived as. Two clients disagree about how to spell the same number:
`rlctl --param lr=3e-4` sends the literal `"3e-4"` a user typed, while the
Python client's `Run.start(params={"lr": 3e-4})` sends `str(3e-4)`, which
is `"0.0003"` (see the dict comprehension in `python/runledger/_run.py`).
The same experiment, recorded from the CLI once and from training code
once, produced two different fingerprints:

```
lr="3e-4"    -> eb26d16282d557d91b3b
lr="0.0003"  -> 77e0335cc066b1a38273
lr="0.00030" -> 4771e6d266eb039f2641
```

This is a false *negative*, and that makes it worse than it looks. The
ledger's one claim is "same fingerprint means same experiment," made
positively every time `spread` groups two runs together and
`compare.Result.Unattributable` fires. A false negative here doesn't
produce a wrong answer that someone goes and checks -- it produces no
answer at all. `spread` never sees a repeat, `unattributable` never fires,
and nothing about the recorded data says a claim was missed. Silence is
the failure mode, and it is the one this ledger exists to prevent for
exactly this kind of case: a hyperparameter genuinely re-run, whose
repeat measurement never gets to inform anything.

## Decision

### Normalize server-side, in `Compute`

`normalizeParamValue` (`internal/lineage/run.go`) runs inside `Compute`,
not in either client. ADR 0001 already settled where identity decisions
belong: the server computes the fingerprint so a client cannot assert
identity that isn't real. A fix that instead relied on every client
formatting its numbers the same way would repeat exactly the mistake ADR
0001 exists to prevent -- it would just move "whose spelling wins" from an
accident of who wrote to the ledger first to an accident of which client
library got updated first. `rlctl` and the Python client need no change
for correctness; both already send some string, and the server now treats
equivalent strings equivalently regardless of source. (`python/runledger/_run.py`
gets one comment, not a behavior change -- see below.)

### What counts as "numeric," and how it canonicalizes

A value is normalized only if it matches `numericParamPattern`: an
optional sign, a JSON-number-shaped integer part or a bare leading decimal
point, an optional fractional part, an optional exponent. A match is then
parsed with `strconv.ParseFloat` and re-emitted with
`strconv.FormatFloat(f, 'g', -1, 64)` -- shortest decimal that round-trips
back to the same `float64`. Anything that does not match, or that
`ParseFloat` cannot represent, passes through **byte-for-byte unchanged**.
That last property matters as much as the normalization itself: this
function is either a no-op or a like-for-like rewrite, never a way to
discard information it has no basis to discard.

The pattern is deliberately narrower than what `strconv.ParseFloat` itself
accepts, because two things ParseFloat is lenient about would misfire if
fed to it unfiltered:

- **`1_000`.** `ParseFloat` accepts underscores as Go's numeric-literal
  digit separator and would parse this as `1000`. A param value is
  arbitrary CLI/JSON/Python input, not Go source -- nothing else on this
  system's surface treats `1_000` as a spelling of one thousand, so
  letting Go's leniency decide would be `Compute` inventing an equivalence
  no client asked for.
- **`007`.** `ParseFloat` parses leading zeros as an ordinary decimal
  (`007 == 7`). A zero-padded string is at least as likely to be an opaque
  identifier -- a shard id, a padded run suffix -- as a number whose
  leading zeros don't matter, and this package has no way to tell which.
  JSON's own number grammar excludes leading zeros for the same reason;
  the pattern follows it.

Two more cases needed a deliberate answer rather than an implicit one from
whatever `ParseFloat` happened to do:

- **`1e400` (overflow).** Syntactically a number, but too large for
  `float64`; `ParseFloat` returns `+Inf` *and* an error. Formatting the
  `+Inf` would collapse every magnitude beyond `float64`'s range onto one
  canonical spelling -- a far bigger identity change than reconciling
  spellings of an in-range number. On this error, `normalizeParamValue`
  returns `v` unchanged.
- **`1e-400` (underflow) -- found while testing the overflow case, not
  called out in the original bug report.** `ParseFloat` silently
  underflows this to exactly `0`, with **no error at all** -- there is no
  `ErrRange` to catch it by, unlike overflow. Left unhandled, this would
  quietly conflate a nonzero (if unrepresentable) magnitude with an actual
  zero. `normalizeParamValue` detects it by checking whether the original
  string's significant digits were already all zero (`isZeroLiteral`); if
  a supposedly-zero result came from a string with a nonzero digit in it,
  that's underflow, not zero, and the value is left unchanged too.

`"NaN"`, `"Inf"`, `"-Inf"`, and `"Infinity"` never reach `ParseFloat` at
all -- they contain letters the pattern doesn't allow outside the `e`/`E`
exponent marker, so they fail the shape check structurally. This is
deliberate, not incidental: `ParseFloat` itself accepts all four
case-insensitively with no error, so excluding them took an explicit
choice, not the pattern's default behavior. Unlike a finite number, there
is no single spelling that every "Inf" necessarily means the same thing
by, and NaN is not equal to itself, which would make folding every NaN
spelling into one canonical string actively misleading as an identity key
rather than merely unnormalized.

One case gets normalized beyond simply reconciling spellings: `"-0"`,
`"-0.0"`, and `"0"` all canonicalize to `"0"`. `FormatFloat` renders a
parsed negative zero back out as `"-0"`, which would otherwise keep signed
and unsigned zero apart from each other for a distinction nothing about a
hyperparameter measures.

### The fingerprint is now a versioned contract (`FingerprintVersion`)

This changes what `Compute` hashes for any param whose spelling was not
already canonical, which is exactly the case ADR 0004 exists for: *"Fixing
a bug in `Compute`... is still a hash-input change by this rule, even
though it reads as a 'fix.'"* `lineage.Run` gains a `FingerprintVersion`
field (`fingerprint_version` on the wire), stamped by `api.record`
alongside `Fingerprint` -- never independently, since a fingerprint's
meaning depends on which contract produced it. `FingerprintVersionLegacy`
(1) names the pre-normalization contract; `CurrentFingerprintVersion` (2)
names this one.

`FingerprintVersion` is provenance, not identity: it is recorded
alongside `Fingerprint` but is **not** itself hashed into it, for the same
reason `Fingerprint` doesn't hash itself -- that would be circular. It
also means a version-1 record and a version-2 record whose params were
*already* canonically spelled (no exponents, no trailing zeros, nothing
`normalizeParamValue` would touch) fingerprint identically, and correctly
so: `Compute`'s param-hashing loop runs the same code either way, so two
runs with the same real content produce the same hash regardless of which
contract's era recorded them. Versioning here doesn't force every old
record apart from every new one; it only distinguishes them where the
normalization rule actually changed what got hashed, and it makes the
distinction visible to a reader instead of silent.

`Store` persists whatever `FingerprintVersion` arrives with a run, the
same way it already persists `Fingerprint`, and never recomputes either.
DuckDB's schema gets one new migration, appended to the existing
`migrations` slice rather than editing the original `CREATE TABLE`
(matching that file's own stated convention): `ALTER TABLE runs ADD COLUMN
fingerprint_version INTEGER DEFAULT 1`. `DEFAULT 1` backfills every row
that already exists at migration time to `FingerprintVersionLegacy` in the
same statement that adds the column -- there is no window where a
pre-migration row reads back with an undefined version. (The column is
not declared `NOT NULL`: this repository's DuckDB version rejects `ADD
COLUMN` with an inline column constraint. `DEFAULT` alone still backfills
every existing row to a real value, and every future insert supplies an
explicit `FingerprintVersion`, so `NOT NULL` would only have caught a bug
that had already written bad data -- it would not have prevented one.)

`internal/compare.SameExperiment` needed no change for this to be
consistent. It already prefers the *stored* fingerprint over recomputing
whenever both runs have one (a decision this ADR's own reasoning is
called out in that function's doc comment as anticipating): reading the
stored value, rather than recomputing under whichever contract `Compute`
implements today, is exactly what keeps a version-1 fingerprint meaning
what it meant when it was recorded. `Compute` remains the fallback only
for runs that never went through a store -- e.g. two `lineage.Run` values
built directly in a test -- and it always implements
`CurrentFingerprintVersion`; there is no way to ask it for the legacy
behavior, because nothing should ever need to.

### Why this can't be solved the way ADR 0011 solved a similar-looking problem

ADR 0011 collapsed `""` and "not recorded" into one meaning for seven
scalar fields, and did it **without** triggering ADR 0004, because it
established that the two spellings were never in real conflict: nothing
needed `""` to mean anything other than absence, so declaring one
canonical spelling retroactively changed nothing about what any existing
record actually meant.

That move doesn't apply here, and the difference is worth being explicit
about because the two bugs look similar on the surface (a field with more
than one way to spell the same value): **existing param records already
carry whichever spelling their client happened to send, and both
spellings are values a real client actually sent on purpose.** A dataset
with `lr="3e-4"` recorded by `rlctl` and `lr="0.0003"` recorded by the
Python client are not "one meaning, two accidental spellings of a gap" the
way `""` and an omitted key were -- they are two clients disagreeing about
formatting, and every fingerprint already computed from either spelling
is a real, load-bearing value some other record might already match
against. There is no canonical spelling to declare after the fact that
doesn't change what at least one already-recorded fingerprint means.
That's precisely the situation ADR 0004 exists for, and precisely why this
change carries a version field and a migration instead of a one-line fix
to `Compute`.

## Consequences

- Two runs recorded with different spellings of the same numeric param now
  fingerprint identically going forward, closing the false negative this
  ADR opens with. `spread` will start grouping repeats it previously
  missed, and `unattributable` can now fire for pairs it previously
  couldn't see as related.
- Every fingerprint already recorded keeps its stored value and reads back
  tagged `FingerprintVersionLegacy`. None of them are silently reinterpreted,
  and none of them retroactively start (or stop) matching anything they
  didn't already match, because nothing recomputes them.
- A version-1 and a version-2 run of the *same* experiment, recorded with
  an unnormalized spelling on one side, will **not** fingerprint
  identically even though they're the same experiment -- the version-1
  fingerprint was permanently computed under the old contract before this
  change existed. This is the real, visible cost of "never reinterpret a
  stored fingerprint": some pre-existing false negatives are fixed only
  for runs recorded from now on, not retroactively for old data. Closing
  that gap for historical data would need an explicit backfill (recompute
  every legacy row's params under the new contract, store the result
  alongside the old value, bump its version) which is a larger, separately
  scoped migration project, not a byproduct of this fix.
- `docs/openapi.yaml`'s `Run` and `RunInput` schemas, and
  `internal/api/spec_test.go`'s coverage of them, gained
  `fingerprint_version` for free by construction, since both schemas are
  checked directly against `lineage.Run`'s JSON tags.
- `python/runledger/_run.py` needed no functional change: it already sends
  `str(v)` for each param, and the server now normalizes whatever it
  receives. The file has a comment recording why duplicating
  `normalizeParamValue`'s canonicalization logic client-side would be
  redundant, not why it would be wrong -- it would still work, just add a
  second place the two rules can drift apart. `cmd/rlctl/main.go` needed no
  change for the same reason: it already forwards whatever string the
  `--param` flag was given.

## What would have to be true to revisit this

**The normalization boundary.** If a real param value turns out to need
the leniency this pattern deliberately excludes -- a workflow that
genuinely wants `1_000` treated as `1000`, say -- widening
`numericParamPattern` is a `CurrentFingerprintVersion` bump like this one,
not a patch to the existing pattern; ADR 0004 applies to every future
change here exactly as it applied to this one.

**Backfilling historical fingerprints.** If false negatives in the
existing (pre-this-change) data turn out to matter enough to justify it,
a batch job that recomputes every `FingerprintVersionLegacy` run's
`Compute()` under the current contract, records the result as a *new*
fingerprint value alongside (never replacing) the original, and stamps it
`CurrentFingerprintVersion` would close the historical gap the last
Consequences bullet describes. That is new, deliberate work with its own
migration story, not something this record already does implicitly.
