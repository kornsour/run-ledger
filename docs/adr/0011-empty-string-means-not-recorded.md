# ADR 0011: An empty string means "not recorded"

Status: Accepted
Date: 2026-08-29

> **For the scalar fields where an empty string is not a meaningful value —
> `config_hash`, `dataset_version`, `model_version`, `host`, `device`,
> `framework_version`, and `checkpoint_uri` — `""` and "not recorded" are
> the same claim.** The ledger stops treating them as a distinction it
> failed to capture and starts treating them as one spelling of one
> meaning. `params` is excluded: a param key can be genuinely present with
> an empty value, the map already carries that difference, and
> `lineage.Run.Compute` already hashes the two differently.

## Context

`lineage.Run` types those seven fields as `string`, so a run that arrives
with `"dataset_version": ""` and one that omits the key entirely are
indistinguishable once decoded. The struct comment has acknowledged this as
a known gap since the type was written, describing it as something that
"can misrepresent a run that genuinely ran with none of these set," and
proposing pointers as the eventual fix.

Pointers would work mechanically. `encoding/json` already distinguishes an
absent key from an empty one when decoding into `*string`, and the
`schema_migrations` table in `internal/store/duckdb.go` is there to make
the column change. The cost is the part worth stating: roughly fifty call
sites across nine files, nullable columns in the DuckDB schema, a matching
change in the Python client and the OpenAPI spec, and — because three of
the seven are identity fields — a new version of the fingerprint contract
under ADR 0004, with every fingerprint recorded to date orphaned or
grandfathered.

That is a large price, and it was only ever worth paying if the
distinction buys something. It does not, for these seven fields, and the
question that settles it is simple: **is there an experiment whose
`dataset_version` is genuinely the empty string?**

There is not. An empty config hash *is* no config hash. A host always has a
name; if the record has no name for it, the client did not capture one. The
same holds for every field in the list. `""` is not a value competing with
absence — it is a second way of writing absence, produced by clients that
serialize every key rather than omitting the unset ones. This repository's
own Python client is one of them.

That reframes the problem. It is not two meanings the type cannot separate.
It is one meaning with two spellings, and nothing requires keeping both.

A second consideration argues the same way, and against pointers
specifically. If absence became identity-bearing, a client that switched
from sending `""` to omitting the key would silently change every
fingerprint it produces — a client version bump orphaning records, with no
server change involved. That makes the fingerprint a function of how a
client serializes rather than of what the experimenter chose, which is a
bad property for the ledger's most load-bearing promise.

`params` is genuinely different and is excluded for that reason. Param keys
are caller-defined, `--param foo=` is expressible and can mean something,
and `Compute` writes only the keys that are present — so an absent param
and an empty one already produce different fingerprints. There the
distinction is real and already identity-bearing; the bug was that
`compare.Runs` read both sides through a map index, which yields `""` for a
missing key, and so reported no difference at all. That is fixed by reading
presence from the map, not by changing any type.

## Decision

For the seven fields listed above, `""` means "not recorded". This is now a
stated invariant rather than an accident of the type.

- The fields stay `string`. No pointers, no nullable columns, no migration.
- The fingerprint input is unchanged, so ADR 0004 is not triggered and no
  recorded fingerprint changes meaning.
- Consumers may rely on `"" == absent` for these fields. `compare.Runs`
  does: it normalizes an empty value to a null side in the diff, so the API
  and `rlctl` report "not recorded" rather than a value of `""`.
- `params` is exempt. Presence there is read from the map and reported
  distinctly, matching what `Compute` already hashes.

## Consequences

The ambiguity is gone by construction rather than by capture: there is now
one spelling of "no value," and every layer can rely on it. `rlctl`'s
existing behaviour of rendering an empty scalar as an em dash becomes
correct rather than a conflation.

What is genuinely given up is the ability to record "this run had an
explicitly empty dataset version." Nothing needs that, which is the
premise of the decision — but it is a real loss and it is why this record
exists rather than the change being made silently.

The question pointers were reached for does not disappear; it turns out to
be a different question. "Did this client fail to capture `device`, or did
this run have none?" is a fact about the recording process, not about the
experiment, and a nullable value field is the wrong place to put it —
that is a category error, not a representation gap. Recording it properly
means recording what the client attempted: a capture declaration alongside
the run, kept out of the fingerprint. Nothing in the ledger asks for that
today. `examples/churn/completeness.py` approximates the same signal by
comparing a run against its peers within a project, and that is a
reasonable place to stop until something needs better.

Revisiting this record would take a field where an empty string is a
meaningful value distinct from absence. Such a field should not be added
to the list above; it should be typed so that its own representation says
so, and this decision left to cover the seven where the collapse is
correct.
