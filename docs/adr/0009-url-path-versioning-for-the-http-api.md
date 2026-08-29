# ADR 0009: URL path versioning for the HTTP API, and which routes it excludes

Status: Accepted
Date: 2026-08-29

> **Every resource route is versioned with a `/v1` URL path prefix.**
> `/healthz`, `/readyz`, and `/metrics` are not: they are fixed operational
> conventions a probe or a scrape config points at once, not part of the
> ledger's resource model, so an API version bump must never also mean
> reconfiguring one of those. A future breaking change adds `/v2` alongside
> `/v1` and runs both for as long as a transition needs, rather than forcing
> every client to move on the same day the server does.

## Context

Every route the server registered was unversioned: `/runs`, `/runs/{id}`,
`/compare`, `/fingerprints`, `/fingerprints/{fingerprint}`, plus the three
operational endpoints. Nothing about the shape of a route, a query
parameter, or a response body carried a version a client could pin to or a
server could branch on.

[ADR 0004](0004-fingerprint-input-is-a-versioned-contract.md) already
establishes that the fingerprint hash input is a versioned contract, on the
grounds that the first time it needs to change, the person making the
change should find that rule before they make it, not after.
[Issue #32](https://github.com/kornsour/run-ledger/issues/32) raised the
same question for the HTTP surface, which had no equivalent rule, alongside
a concrete route that was already due for a breaking change: `/compare`
reads as a verb where every other route reads as a resource (see
[ADR 0010](0010-comparisons-is-a-resource-not-a-verb.md) for that reshape).
Making that change against unversioned routes would have meant either
breaking every existing client the moment the new server deployed, or
inventing a versioning scheme under pressure, mid-migration, with no time
to think about what it should look like for the *next* one.

There is no tagged release of this project and no external consumer
depending on the current route shapes — the ledger has never promised route
stability to anyone. That is what makes now the cheap moment to adopt a
scheme: this migration does not itself need to serve both an old and a new
shape, because nothing has shipped that depends on the old one. The value
of doing it now is that the *next* breaking change — the one that lands
after real clients exist — has somewhere to go.

### Why a URL path prefix over the alternatives

- **Media-type versioning** (`Accept:
  application/vnd.runledger.v1+json`) keeps the URL stable across versions,
  which is attractive in principle, but it pushes the version into a header
  a browser address bar, a `curl` one-liner, and `rlctl`'s own request
  logging can't show without extra work. This project's own tooling
  (`rlctl`, the Python client, the OpenAPI viewer) all work from a URL
  first; a header-only scheme would fight that instead of fitting it.
- **A query parameter** (`?version=1`) is easy to forget on a single
  request, which defeats the point of a version that is supposed to be
  unmissable, and reads as an optional knob rather than the top-level fact
  about what a route means that it actually is.
- **A URL path prefix** (`/v1/runs`) makes the version the first thing a
  reader of any request line sees, requires no new tooling to route on (Go's
  `net/http.ServeMux`, and any reverse proxy in front of it, already
  dispatches on path), and is what lets `/v1/...` and a future `/v2/...`
  coexist as two entries in the same route table rather than two branches
  inside every handler.

### Why the three operational endpoints are excluded

`/healthz` and `/readyz` are read by infrastructure that was not written for
this project — a Kubernetes liveness/readiness probe, a load balancer health
check — and follows its own naming convention regardless of what the
application underneath it calls its resources. `/metrics` is read by a
Prometheus scrape config that names the path once, in YAML, usually managed
by someone other than whoever deploys a new API version. Versioning any of
the three would mean every API version bump is also an infrastructure
change to a probe or a scrape target, for three endpoints whose contract
(their response is a liveness signal or an exposition-format dump, not a
resource shape) has no reason to break the way `/runs` or `/comparisons`'s
JSON schemas might.

## Decision

`internal/api/api.go` defines `apiVersion = "/v1"` and every resource
route's pattern is built as `apiVersion + "/the/path"`; its `call` helper
splices the same prefix into every request `cmd/rlctl/main.go` sends, so a
call site there names only a server and a bare path, never `/v1` by hand.
The `Location` header `POST /v1/runs` sets on success carries the same
prefix. `/healthz`, `/readyz`, and `/metrics` keep their bare paths.
Neither Go client goes through `internal/api`, so the constant is
duplicated once, deliberately, at the client/server boundary -- `internal/
api`'s version is an implementation detail of the server, and pulling
`cmd/rlctl` into that package to avoid one repeated string would trade a
one-line duplication for a real dependency on the server's internals. The
Python package (`python/runledger`) has no such boundary to justify
duplication *within* it: `read.py` defines `API_VERSION = "/v1"` once, and
`_run.py` and `replay.py` both import it rather than each hardcoding their
own copy.

`docs/openapi.yaml`'s `paths:` keys carry the same `/v1/...` prefix (except
the three operational endpoints), so `TestRoutesMatchSpec`
([internal/api/spec_test.go](../../internal/api/spec_test.go)) continues to
fail the build the moment code and spec disagree about what is versioned
and what is not.

## Consequences

- A future breaking change to `/runs`, `/comparisons`, or `/fingerprints`
  gets a `/v2` prefix and can run alongside `/v1` for however long a
  deprecation window needs to be, rather than forcing a coordinated
  simultaneous upgrade of every client. This migration itself does not
  exercise that path — see Context above for why that is fine right now —
  but the mechanism exists once it is needed.
- Every client of this API (`rlctl`, the Python package, any external
  script hitting the HTTP surface directly) needs `/v1` added to its request
  paths. That is the entire migration this ADR authorizes: a one-time,
  coordinated update, made now while it is free, instead of later under a
  real deprecation deadline.
- A reverse proxy or gateway placed in front of this server can route
  `/v1/*` to one backend version and `/v2/*` to another without inspecting
  headers or bodies — ordinary path-based routing, which every common proxy
  already supports.
- `/healthz`, `/readyz`, and `/metrics` staying unversioned means a
  Kubernetes manifest, a load balancer config, or a Prometheus scrape target
  written against this server today never needs to change on an API version
  bump. That is a permanent property of this decision, not a transitional
  one.

## What would have to be true to revisit this

A second, incompatible versioning axis becoming necessary at the same time
as URL path versioning — for example, needing to version the wire format of
a single route independently of the rest of the API. That would call for
combining this scheme with a narrower one (e.g. a `schema_version` field on
just that route's body) rather than replacing URL path versioning outright;
the two are not mutually exclusive. Short of that, this is the standing
scheme, the same way ADR 0004 is a standing rule rather than a decision
that gets periodically revisited.
