# ADR 0010: `/comparisons` is a resource, not a verb

Status: Accepted
Date: 2026-08-29

> **`GET /v1/comparisons?a=&b=` replaces `GET /compare?a=&b=`.** A
> comparison is identified by the unordered pair of run ids being compared,
> not owned by either run, so it sits next to `/v1/runs` and
> `/v1/fingerprints` as its own resource collection — addressed by query
> parameters, the same way `/v1/fingerprints` already is — rather than a verb
> hanging off the root.

## Context

`GET /compare?a=X&b=Y` (`internal/api/api.go`'s `compare` handler) is the
only route in the API named as a verb rather than a resource. Every other
route reads as a noun a client fetches, lists, or updates: `/runs`,
`/runs/{id}`, `/fingerprints`, `/fingerprints/{fingerprint}`. `/compare`
reads as a remote procedure call instead — closer to an RPC endpoint than
to a REST resource, and the odd one out stylistically once a reader has
seen the rest of the API.

`internal/compare.Runs(a, b)` is symmetric: it returns `Result{A: a.RunID,
B: b.RunID, ...}`, but nothing about the comparison privileges one run over
the other — swapping `a` and `b` swaps which side of each `Field` a value
lands on, not which comparison is being asked for. Two shapes were
considered for the replacement, both raised in
[issue #32](https://github.com/kornsour/run-ledger/issues/32):

- `GET /runs/{id}/comparisons?to={other-id}` nests the comparison under one
  of the two runs, treating it as *that run's* comparison to another.
- `GET /comparisons?a=&b=` addresses the comparison directly, by the pair of
  ids, owned by neither.

Nesting under `/runs/{id}` would manufacture an asymmetry the underlying
operation does not have — there is no principled way to choose which of two
equally-weighted run ids becomes the path segment and which becomes the
query parameter, and a client would have to make that arbitrary choice on
every call. It also does not generalize as cleanly: a future N-way
comparison (`a`, `b`, `c`, ...) has an obvious query-parameter shape
(`?a=&b=&c=`) but no obvious "which one owns the path" answer once there is
no longer a single "other" run to name with `?to=`.

## Decision

The route becomes `GET /v1/comparisons?a=&b=`, keeping the same `a`/`b`
query parameters `compare` already validated and read. The handler function
and `internal/compare` package are unchanged — only the route pattern in
`internal/api/api.go`'s `routes()` and the corresponding path in
`docs/openapi.yaml` move from `/compare` to (the now-versioned)
`/v1/comparisons`.

## Consequences

- `rlctl diff`, the only caller of this route today, now requests
  `/v1/comparisons?a=&b=` instead of `/compare?a=&b=`. No other client in
  this repository calls it directly.
- A future N-way comparison, if one is ever built, has a query-parameter
  shape to grow into (`?a=&b=&c=`) without restructuring the resource
  hierarchy — it stays a comparison identified by the set of run ids
  involved, the same pattern this decision establishes for two.
- The API surface is now uniformly noun-shaped: every route names a
  resource a client fetches or mutates, and `/v1/comparisons` reads the same
  way `/v1/fingerprints` already does.
- This ships as a breaking change to `/compare`'s path, landing in the same
  change as [ADR 0009](0009-url-path-versioning-for-the-http-api.md)'s `/v1`
  prefix — the last route rename this project can make for free, before a
  URL path version exists to make the next one for free instead.

## What would have to be true to revisit this

A real need to compare more than two runs at once, with a query-parameter
list that becomes awkward at that scale (long URLs, ambiguous ordering
across many ids). That would call for a request body on the comparison
resource — `POST /v1/comparisons` with a list of run ids in the body,
returning the same kind of structured result — rather than reworking the
two-run shape this ADR settles.
