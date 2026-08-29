# Architecture decision records

This folder records the decisions in run-ledger that a reader is most likely to
want to overturn, and that are most expensive to overturn silently. Each record
is short: context, decision, consequences, and what would have to be true to
revisit it.

The README's ["Decisions worth knowing"](../../README.md#decisions-worth-knowing)
section is a summary of these; this folder is the account.

## Records

| # | Title | Status |
|---|---|---|
| [0001](0001-server-computes-the-fingerprint.md) | The server computes the fingerprint, never the client | Accepted |
| [0002](0002-unknown-json-fields-are-rejected.md) | Unknown JSON fields are rejected | Accepted |
| [0003](0003-dirty-tree-without-config-hash-is-refused.md) | A dirty working tree without a config hash is refused | Accepted |
| [0004](0004-fingerprint-input-is-a-versioned-contract.md) | The fingerprint input is a versioned contract | Accepted |
| [0005](0005-python-client-writes-once-at-the-end.md) | The Python client writes the ledger exactly once, at the end of a run | Accepted |
| [0006](0006-duckdb-store-backend-and-the-cgo-cost.md) | `store.DuckDB` as the durable backend, and accepting cgo to get it | Accepted |
| [0007](0007-keyset-pagination-cursor-consistency.md) | `GET /runs` paginates by keyset cursor, and a page is consistent with the cursor's position, not a fixed snapshot | Accepted |
| [0008](0008-replay-raises-quarantines-and-tracks-a-read-offset.md) | `replay_spool()` raises on an unreachable ledger, quarantines permanent rejections, and rewrites the spool by tracked read offset | Accepted |
| [0009](0009-url-path-versioning-for-the-http-api.md) | URL path versioning (`/v1`) for the HTTP API, excluding health/readiness/metrics | Accepted |
| [0010](0010-comparisons-is-a-resource-not-a-verb.md) | `/comparisons` is a resource, not a verb | Accepted |
| [0011](0011-empty-string-means-not-recorded.md) | An empty string means "not recorded" | Accepted |

## Adding a record

Copy the format of an existing record. Number new records sequentially; never
reuse or renumber. A record that is later reversed gets a new record that
supersedes it (linked both ways) — existing records are not edited to pretend
the original reasoning never held.
