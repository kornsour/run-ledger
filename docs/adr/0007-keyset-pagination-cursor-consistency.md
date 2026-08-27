# ADR 0007: `GET /runs` paginates by keyset cursor, and a page is consistent with the cursor's position, not a fixed snapshot

Status: Accepted
Date: 2026-08-27

> **A page is consistent with the data as of its cursor's position, not with
> a single fixed snapshot of the whole listing.** A row recorded after a
> traversal begins, sorting behind wherever the traversal has already
> reached, is not visited by that traversal. This is the cheap, honest
> consistency model for an append-mostly ledger: no page ever skips or
> repeats a row that existed before the traversal reached it, and nothing
> stronger is promised about rows written during it.

## Context

Before this record, `GET /runs` had one knob: an optional `limit` that
truncated an otherwise-unbounded, newest-first result set. Two problems
followed directly from that:

- **Unbounded by default.** With no `limit`, a caller got the whole filtered
  listing. That was fine while every ledger fit in a Go map; it stops being
  fine the moment `store.DuckDB` (#2) makes it plausible for a ledger to hold
  a real number of runs.
- **No way to page.** A client that wanted "the next 50 after these" had
  nothing to ask for beyond a larger `limit` and re-fetching everything already
  seen. `LIMIT`/`OFFSET` was the obvious next step, and the wrong one: the
  listing is sorted newest-first, so a run recorded between two page fetches
  shifts every offset after it. A client paging through history with
  `?limit=50&offset=50`, `?limit=50&offset=100`, ... silently skips whatever
  row landed at the boundary, or sees one twice, depending on which direction
  the shift goes. Nothing about that failure is visible to the client — the
  request still succeeds, the page still has 50 rows, and one of them is
  simply not the row it should have been.

## Decision

`GET /runs` pages by an opaque cursor instead of an offset. The cursor
encodes the sort key of the last row on the page — `(started_at, run_id)`,
the same total order `store.RunConformance` already required every backend
to produce (`internal/store/{memory,duckdb}.go`) — not a row count. Passing
it back as `?cursor=...` asks for "everything that sorts after this row,"
which is a question concurrent inserts cannot invalidate the way "everything
at position 100" can: a new row lands somewhere in the total order and either
sorts before the cursor (already behind the traversal, never seen) or after
it (still ahead, seen on some future page the same way it would have been
if it existed when the traversal started).

That "future page" case is the trade this ADR names on purpose. A row
inserted after the traversal started, sorting behind the traversal's current
position, is not retroactively inserted into a page already served, and the
traversal is not guaranteed to double back for it. The alternative —
snapshotting the entire result set at the first page so every subsequent page
reads that snapshot — would need either an actual database snapshot/transaction
held open for the lifetime of a client's traversal (unbounded, client-paced,
exactly what `store.DuckDB`'s single-writer model does not want to hold open)
or copying the whole matching set up front (defeating the reason pagination
exists). For a ledger whose workload is "record a run, occasionally page
through history," the honest and cheap answer is: a page reflects the ledger
as of the cursor's position, full stop, and that is what the API docs
(README's "Pagination" section) say rather than leaving a client to discover
it empirically.

`limit` gets a server-side ceiling for the same reason the endpoint needed a
cursor at all: an unbounded page is a caller (or the size of the ledger
itself) deciding how large a response the server hands back. `GET /runs` now
defaults to 50 and refuses to return more than 500 regardless of what a
request asks for, and echoes the limit it actually applied (`limit` in the
response body) so a client doesn't have to assume its request was honored
verbatim.

## Consequences

- `store.Query` gained `After *store.Cursor`; `store.Store.List` returns
  `store.Page{Runs, Next}` instead of a bare slice. Both `Memory` and
  `DuckDB` implement the same keyset predicate — "strictly after this
  `(started_at, run_id)` in the listing's total order" — so pagination
  behavior does not depend on which backend answered, the same guarantee
  `store.RunConformance` already enforced for ordering and idempotency.
- A client that stops paging once `next_cursor` is absent has seen every row
  that existed at or before the moment its traversal reached the end — not
  every row that exists now. A dashboard that wants "what's new since I last
  looked" needs a different query shape than "page through everything";
  `GET /runs` answers the second one.
- The cursor is deliberately opaque and version-tagged (`v1:` in
  `internal/api/api.go`) rather than a bare `started_at,run_id` pair a client
  might be tempted to construct itself. Changing what a cursor encodes later
  is then a matter of bumping the tag and rejecting the old one, not a
  breaking change to a format clients depend on.
- `limit=0` is no longer how a client asks for "everything" — it is now a
  rejected request (`limit must be a positive integer`). There is no
  unbounded `GET /runs` anymore; a caller that genuinely needs the whole
  listing pages through it.

## What would have to be true to revisit this

If a client needs a page that is consistent with a single fixed snapshot —
provably every row that existed at the instant the traversal began, including
rows that would otherwise land behind the traversal's later cursors — that is
a different, stronger guarantee than this ADR provides, and would need either
a store-level snapshot/transaction primitive held for the traversal's
lifetime, or a materialized copy of the matching set taken up front. Nothing
about the current cursor format prevents adding that as a separate mode later
(e.g. a snapshot id embedded alongside the sort key); it just is not what
`GET /runs` promises today, and should not be assumed until it is added and
documented as such.
