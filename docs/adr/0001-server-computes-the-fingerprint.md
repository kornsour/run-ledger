# ADR 0001: The server computes the fingerprint, never the client

Status: Accepted
Date: 2026-08-25

## Context

Every run record is compared by its fingerprint, a content-addressed hash of
its identity fields (`internal/lineage.Run.Compute`). `POST /runs` accepts a
`Run` as JSON. The `Run` struct is the same shape on the wire as it is
internally, so the fingerprint field could in principle be supplied by the
caller instead of computed.

## Decision

The fingerprint is always recomputed server-side in `Server.record`
(`internal/api/api.go`), overwriting anything the client sent:

```go
run.Fingerprint = run.Compute()
```

A client can send a `fingerprint` field — it is simply discarded.

## Consequences

- A caller cannot assert that two runs with different identity fields were
  the same experiment, or that two runs with the same identity fields were
  different ones. The one property the whole system depends on — "same
  fingerprint means same experiment" — cannot be forged by a client.
- The client cannot pre-compute and cache a fingerprint locally before
  recording; it must always round-trip to the server to learn it.
- Every store implementation can trust `Fingerprint` on a `lineage.Run` it
  receives from the API layer without re-validating it. A store used outside
  the API (e.g. in tests, via `store.RunConformance`) is not protected by this
  and must not treat a hand-built `Run.Fingerprint` as trustworthy.

## What would have to be true to revisit this

A use case where a client needs to assert identity across systems that don't
share a fingerprint algorithm — e.g. federating fingerprints computed by a
different, trusted service. That would need a signed or otherwise
authenticated fingerprint, not a plain client-supplied field, to avoid
reopening the forgery this decision closes.
