# ADR 0004: The fingerprint input is a versioned contract

Status: Accepted
Date: 2026-08-25

> **The fingerprint input is a versioned contract.** Any change to which
> fields are hashed, their order, or the delimiting is a breaking change. It
> orphans every fingerprint already recorded — two records of the same real
> experiment, one hashed before the change and one after, will no longer
> compare equal, and the ledger's central claim ("same fingerprint means same
> experiment") silently stops holding for old data. Changing the hash input
> needs a version field on the record plus a documented migration. It is
> never a patch release.

## Context

`Run.Compute` (`internal/lineage/run.go`) hashes the identity fields in a
fixed order, each field length-prefixed:

```go
write := func(parts ...string) {
    for _, p := range parts {
        fmt.Fprintf(h, "%d:%s", len(p), p)
    }
}
write(r.Project, r.GitCommit, fmt.Sprint(r.GitDirty), r.ConfigHash,
    r.DatasetVersion, r.ModelVersion, fmt.Sprint(r.Seed))
```

Without length-prefixing, concatenating fields directly would let different
identities collide on the same bytes — `Project="ab", GitCommit="c"` and
`Project="a", GitCommit="bc"` would hash identically, because
`"ab"+"c" == "a"+"bc"`. Length-prefixing (`fmt.Fprintf(h, "%d:%s", len(p),
p)`) makes each field's boundary part of what's hashed, so this cannot
happen. `Params` is sorted by key before hashing for the same
reason `Compute`'s doc comment gives: Go's map iteration order is randomized,
so an unsorted hash would make the same experiment fingerprint differently
between two runs of the same binary.

## Decision

The exact byte sequence `Compute` feeds into SHA-256 — which fields, in what
order, with what delimiting — is a versioned contract on the wire, not an
implementation detail. See the boxed rule above.

Today there is no version field on `Run` and no second hash version, because
nothing has needed to change the input yet. This record exists so that the
first time something does, the person making the change finds the rule
before they make it, not after.

## Consequences

- Adding a new identity field, removing one, or reordering the `write(...)`
  call changes every fingerprint computed from that code, including for
  historical runs whose identity fields didn't change at all. It must ship
  with a version field on the record and a migration, not as a normal code
  change.
- Fixing a bug in `Compute` (e.g. a hypothetical delimiter collision) is
  still a hash-input change by this rule, even though it reads as a "fix." A
  correctness fix to the hash input is exactly the case this rule is for: it
  is tempting to ship as a patch because it's "just a fix," and doing so is
  what would orphan existing fingerprints silently.
- Every future storage backend (see the storage-engine ADR once #2 lands)
  must treat `Fingerprint` as opaque and versioned rather than deriving it
  itself, so the version lives with the record, not with whichever backend
  happens to compute it.

## What would have to be true to revisit this

This is a standing rule, not a decision that gets revisited — it is the
precondition for ever safely changing `Compute`. Revisit only the mechanism:
today "the hash input never changes" is enforced by nothing but this
document; if that stops being reliable enough, add an automated check (e.g. a
golden-fingerprint test that fails if `Compute`'s output changes for a fixed
input) so a hash-input change cannot land without the person making it
noticing.
