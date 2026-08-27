# ADR 0006: `store.DuckDB` as the durable backend, and accepting cgo to get it

Status: Accepted
Date: 2026-08-27

## Context

`store.Memory` (`internal/store/memory.go`) is the only backend: everything
is lost on restart, and every `List` is a linear scan over a Go map with
filters applied in application code. That is fine as the reference
implementation `store.RunConformance` checks every backend against, and it is
not a place to keep run history.

The queries this ledger is actually for are analytical: "every run of this
project on this dataset version, grouped by fingerprint, with the spread of
`loss` across each group." That is a column-oriented scan over a
high-cardinality key space — fingerprints, commits, arbitrary `params` and
`metrics` — not a transactional workload. Three options were on the table:

- **SQLite via `modernc.org/sqlite`.** Pure Go, no cgo, trivial to embed in
  the existing `CGO_ENABLED=0` cross-compiling Dockerfile. Wrong tool for the
  job, though: it is row-oriented, and the aggregate-over-many-runs queries
  this store exists for would still be Go-side scans over rows SQLite handed
  back one at a time.
- **ClickHouse.** Right shape for the workload — genuinely columnar,
  genuinely fast at this kind of aggregation. Wrong deployment shape for a
  tool whose whole pitch is `make build && ./bin/runledger` needing nothing:
  it is a server you run and operate, not a library you link.
- **DuckDB**, embedded via `github.com/marcboeker/go-duckdb/v2`. Columnar,
  answers the actual query shape well, and runs in-process like SQLite does —
  but the Go driver wraps the C++ engine via cgo, not a pure-Go port.

DuckDB is the right shape for the workload and the right deployment model
for this tool, so it is the one implemented here as `store.DuckDB`
(`internal/store/duckdb.go`). It passes `store.RunConformance` unchanged
(`internal/store/duckdb_test.go`).

## The cgo cost, specifically

`internal/store` now unconditionally imports `github.com/marcboeker/go-duckdb/v2`,
so anything that imports `internal/store` — which is everything, `cmd/runledger`
included — requires cgo to build. Concretely:

- **`CGO_ENABLED=0` no longer builds this module.** The Dockerfile's build
  stage set that flag specifically so one `golang:1.26-alpine` builder could
  cross-compile both `linux/amd64` and `linux/arm64` from whichever
  architecture it happened to run on, and so the runtime stage could be
  `distroless/static` with no libc at all. Both of those depended on the
  binary being pure Go; neither holds once cgo is required.
- **The runtime image needs a C toolchain to build, and a libc to run
  against.** `go-duckdb` vendors prebuilt static DuckDB libraries per
  platform (`duckdb-go-bindings/{darwin,linux,windows}-{amd64,arm64}`) so
  there is no DuckDB source compile at build time, but the cgo bridge code
  itself still needs `gcc`/`musl-dev` present in the build stage, and the
  resulting binary is no longer the fully static, libc-free artifact
  `distroless/static` assumed.
- **Local development needs a C toolchain too.** `make build`, `make test`,
  and `go vet ./...` all now require `CGO_ENABLED=1` and a working `cc` — true
  today on the runners this project's CI already uses, and on any machine
  with Xcode Command Line Tools or `build-essential` installed, but not on a
  from-scratch minimal container.

Both are accepted, not avoided, for this PR: see `Dockerfile` for the actual
per-architecture build change (Alpine `build-base` plus a musl static link,
building each target platform natively under `docker buildx`'s emulation
instead of cross-compiling one `GOARCH` from another), and `.github/workflows/ci.yml`
/ `image.yml`, which already run on hosts with a C toolchain available.

**If cgo later proves too expensive** — a build environment that genuinely
cannot carry a C toolchain, or the per-arch native build under QEMU emulation
in `image.yml` becoming too slow to be worth it — the documented fallback is
to drop the embedded engine and write Parquet files instead, using DuckDB
only as an external query layer (`duckdb -c "SELECT ... FROM 'runs/*.parquet'"`)
rather than a library this binary links. That is a larger change (no more
`Store` implementation with transactional `Record` semantics; the write path
and the query path stop being the same process) and is not what this PR
does — it is named here so the trade-off is on the record rather than
rediscovered.

## Schema

`runs` holds the scalar columns. `run_params` and `run_metrics` are narrow
`(run_id, key, value)` tables rather than a JSON column for `Params` /
`Metrics`: a JSON blob makes `params.lr = '3e-4'` a function-call predicate
DuckDB cannot use an index for, where a real column can be filtered and
indexed directly. `Query` (`internal/store/store.go`) does not expose
per-param filtering yet — this PR implements the backend behind the existing
`Store` interface, not a new one — but the schema is shaped so that adding it
later is a `WHERE` clause against `run_params`, not a migration.

`runs` is indexed on `(project, started_at DESC)` and on `fingerprint`: the
two access paths the API actually has today (`GET /runs?project=...`,
grouping by fingerprint for "did this change anything?").

## Concurrency

DuckDB's embedded engine is single-writer. `go-duckdb` maps one DSN to one
shared in-process database instance regardless of how many `*sql.DB`
connections open it in this process, so two concurrent write transactions
are expected to conflict rather than interleave cleanly. Rather than lean on
DuckDB's own conflict/retry behavior to reproduce `store.RunConformance`'s
"concurrent conflicting Record calls yield exactly one winner" semantics
exactly, `DuckDB.Record` serializes writes behind a `sync.Mutex` — the same
strategy `Memory` uses. `Get` and `List` are unguarded reads: DuckDB's MVCC
snapshot isolation is what the "List never observes a partially-written run"
conformance case actually depends on, and a plain transaction around
`Record`'s multi-table insert is enough to satisfy it.

## Migration path

`internal/store/duckdb.go` carries an ordered list of SQL statements
(`migrations`), applied once each and tracked in a `schema_migrations` table.
`NewDuckDB` runs any pending migrations before returning, so opening a
database — fresh or existing — always leaves it at the current schema.
Statements are never edited after release; schema changes are new entries
appended to the list, the way any SQL migration chain works.

## What this does not change

`Memory` stays the default (`--store memory` / `RUNLEDGER_STORE` unset), so
`make build && ./bin/runledger` still needs nothing — no DSN, no cgo build
requirement forced onto a caller who never asked for durability. `--store
duckdb --dsn /path/to/runs.duckdb` (or `RUNLEDGER_STORE=duckdb` /
`RUNLEDGER_DSN=...`) opts in.
