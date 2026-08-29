# run-ledger

> A small service that records what an experiment run **was**, so two runs can be
> compared and a difference in results can be attributed — or shown to be
> unattributable. Go, one binary, no external dependencies to try it.

## The problem

An experiment produced a number. A week later it produces a different number.
Which of these changed: the code, the config, the data, the seed, the hardware,
a dependency — or nothing you recorded?

Most run tracking answers "what was the metric". The harder and more useful
question is **"were these the same experiment?"**, and answering it needs the run's
identity to be captured separately from its outcome.

## The idea: identity and provenance are different halves

A run record splits in two:

- **Identity** — project, git commit, dirty flag, config hash, dataset version,
  model version, seed, and hyperparameters. These are hashed into a
  **fingerprint**. They determine *what was run*.
- **Provenance** — host, device, framework version, status, timings, checkpoint
  URI, and measured metrics. These are recorded but **not** hashed. They record
  *what happened*.

Two runs with the same fingerprint were the same experiment, whatever their
outcome. That makes one specific situation detectable:

> **same fingerprint + different metrics = something real is going unrecorded.**

Nondeterminism, an unpinned dependency, different hardware, a data race. The
ledger cannot tell you which — but it can tell you that the explanation is not in
the record, which is the point at which you go looking.

That verdict is only as trustworthy as the identity fields feeding it.
`dataset_version`, `model_version`, and `config_hash` are free strings the
caller supplies — the server hashes them into the fingerprint exactly as
given, and has no way to check that a run labelled `"v1"` saw the same bytes
as another run labelled `"v1"`; it never sees the dataset, the weights, or
the config file, only the label. So before chasing nondeterminism, rule out
the cheaper explanation first: a relabeled or re-exported dataset under an
unchanged label produces the exact same signature as real nondeterminism —
same fingerprint, different metrics, `unattributable`. The explanation was
in the record; the record was just wrong. See
[`runledger.hash_dataset()`](python/README.md#deriving-dataset_version) if
you want `dataset_version` derived from the data's own bytes instead of
typed by hand.

## Try it

```bash
make build
./bin/runledger &                      # listens on :8080, in-memory store

export RUNLEDGER_ADDR=http://localhost:8080
A=$(./bin/rlctl record --project demo --seed 1 --config-hash cfg1 \
      --param lr=3e-4 --metric loss=0.42 --device cpu --status succeeded)
B=$(./bin/rlctl record --project demo --seed 1 --config-hash cfg1 \
      --param lr=3e-4 --metric loss=0.51 --device cuda --status succeeded)

./bin/rlctl diff $A $B
```

A run can also be recorded at submission time, before its outcome is known,
and walked through its lifecycle as it happens:

```bash
R=$(./bin/rlctl record --project demo --seed 1 --config-hash cfg1)  # status: created
./bin/rlctl start $R                                                # status: running
./bin/rlctl finish --metric loss=0.42 --checkpoint s3://bucket/ckpt $R
# status: succeeded, ended_at set, loss recorded

# or, if the run didn't make it:
./bin/rlctl fail $R                                                  # status: failed
```

`start`/`finish`/`fail` only move a run forward — `created → running →
{succeeded, failed, cancelled}` — and can never change what experiment the
run was; see [PATCH `/v1/runs/{id}`](#api) below.

```
same experiment (fingerprints match)

FIELD                     KIND          A                     B
device                    provenance    cpu                   cuda
metrics.loss              metric        0.42                  0.51

These runs describe the same experiment but measured differently.
Something that affected the result is not captured in the record.
```

Change the seed and the verdict changes with it:

```
different experiments (fingerprints differ)

FIELD                     KIND          A                     B
seed                      identity      1                     2
params.lr                 identity      3e-4                  1e-4
metrics.loss              metric        0.42                  0.3
```

Or bring it up in a container instead of building locally:

```bash
docker compose up
```

That builds the image from the `Dockerfile` and starts `runledger` on `:8080`
with the durable DuckDB backend (`internal/store.DuckDB`), the database file
on a persistent volume at `/data/runs.duckdb`. Published images land on
`ghcr.io/lurking-walrus/run-ledger` on tagged releases; `make image` builds
the same image locally for the host platform.

Running `./bin/runledger` directly still defaults to the in-memory store --
nothing survives a restart, and no DSN or cgo build is required to try it.
Pass `--store duckdb --dsn ./runs.duckdb` (or `RUNLEDGER_STORE=duckdb` /
`RUNLEDGER_DSN=./runs.duckdb`) to opt into the durable backend directly:

```bash
./bin/runledger --store duckdb --dsn ./runs.duckdb &
```

## Layout

```
cmd/runledger/     HTTP server
cmd/rlctl/         researcher CLI: record · start · finish · fail · list · show · diff · spread
internal/lineage/  the Run record, validation, content-addressed fingerprint
internal/store/    Store interface + in-memory reference and DuckDB backends
internal/compare/  structured diff, identity vs provenance vs metric
internal/spread/   per-fingerprint metric spread across repeated runs
internal/api/      HTTP handlers
python/runledger/  Python client: `Run.start()` to record, runs()/spread()/compare() to read back
python/examples/   the worked reproducibility notebook
dashboard/         local marimo app: browse spread(), drill into a group, compare() two runs
scripts/           docs build helpers used by `make docs`
```

Every directory here contains code. The store is an interface with a conformance
suite (`store.RunConformance`) that any future backend must pass, so a second
implementation cannot quietly disagree about ordering or idempotency.

## API

| Method | Path | |
|---|---|---|
| `POST` | `/v1/runs` | record a run; the server assigns the fingerprint and id |
| `PATCH` | `/v1/runs/{id}` | update a run's provenance: `status`, `ended_at`, `checkpoint_uri`, `metrics`, `host`, `device`, `framework_version` |
| `GET` | `/v1/runs` | list, filtered by `project`, `git_commit`, `fingerprint`, `status`, `device`; paginated, see below |
| `GET` | `/v1/runs/{id}` | one run |
| `GET` | `/v1/comparisons?a=X&b=Y` | structured diff of two runs, with `unattributable` |
| `GET` | `/v1/fingerprints?project=P` | fingerprints with more than one run, ranked by widest relative metric spread |
| `GET` | `/v1/fingerprints/{fingerprint}` | one group: per-metric count/min/max/mean/stddev, or `no_repeats` for a lone run |
| `GET` | `/healthz` | liveness — unversioned; see [ADR 0009](docs/adr/0009-url-path-versioning-for-the-http-api.md) |
| `GET` | `/readyz` | readiness — the store answers a call |
| `GET` | `/metrics` | self-metrics, Prometheus exposition format |

Resource routes are versioned (`/v1/...`); the three operational endpoints
above are not — see [ADR 0009](docs/adr/0009-url-path-versioning-for-the-http-api.md).
`/v1/comparisons` addresses a comparison by the pair of run ids being
compared rather than nesting it under one of the two runs; see
[ADR 0010](docs/adr/0010-comparisons-is-a-resource-not-a-verb.md).

The full contract, including request/response schemas, lives in
[`docs/openapi.yaml`](docs/openapi.yaml). It has to stay in sync with
`internal/api` by hand, but not silently: `TestRoutesMatchSpec` and
`TestSchemasMatchGoTypes` in
[`internal/api/spec_test.go`](internal/api/spec_test.go) check the spec's
routes and request/response field names against the actual Go types on
every `go test ./...`, so CI fails a PR that lets the two drift apart. The
spec is also a valid import for Postman, Insomnia, or any other client that
reads OpenAPI, if you want to exercise the API from one of those instead.

### The documentation site

**https://kornsour.github.io/run-ledger/** carries three things:

| | |
|---|---|
| [`/api/`](https://kornsour.github.io/run-ledger/api/) | Interactive API reference, rendered from `docs/openapi.yaml`. |
| [`/reproducibility.html`](https://kornsour.github.io/run-ledger/reproducibility.html) | A worked notebook: record one experiment three times, find the spread, read the verdict. |
| [`/python/`](https://kornsour.github.io/run-ledger/python/) | Python client reference, generated by `pdoc` from the source. |

All three are **generated at deploy time** by
[`.github/workflows/pages.yml`](.github/workflows/pages.yml), not committed —
`make docs` builds the same site locally. That matters for the notebook in
particular: it is executed against a real ephemeral ledger on every deploy,
so a notebook that has stopped working fails the deploy instead of publishing
a stale page. Its committed form carries no outputs at all
(`python/tests/test_notebook.py` enforces that), which keeps diffs readable
and stops a stored result from looking authoritative long after it stopped
being true.

### Pagination

`GET /v1/runs` returns a page, not the whole result set:

```json
{"runs": [ ... ], "count": 50, "limit": 50, "next_cursor": "v1:..."}
```

- `limit` requests a page size; the server accepts up to 500 and defaults to
  50 when omitted. The response's own `limit` field is the size actually
  applied — always check it rather than assuming the request was honored
  verbatim.
- `next_cursor` is present only when more rows may follow. Pass it back as
  `?cursor=...` to fetch the next page; its absence means the traversal is
  done. Treat the value as opaque — it encodes an internal sort position, not
  a row offset, and its format is not part of the API contract.
- Pages are ordered newest-first (`started_at`, then `run_id` as a tiebreak)
  and paginated by that same key, not by `LIMIT`/`OFFSET`. A run inserted
  between two `List` calls does not shift what a later page returns the way
  it would with offset pagination — no row already visited is ever skipped
  or repeated because of a concurrent insert.
- **What a page is consistent with:** each page reflects the ledger as of
  its cursor's position, not a single fixed snapshot of the whole listing.
  A row recorded after a traversal begins, and sorting behind whatever
  position the traversal has already reached, is simply not visited by that
  traversal — it is exactly the kind of row a fresh `GET /v1/runs` (no cursor)
  would show first. For an append-mostly ledger this is the cheap, honest
  contract: no page ever repeats or skips a row that existed before the
  traversal reached it, and nothing is promised about rows that did not.
  See [ADR 0007](docs/adr/0007-keyset-pagination-cursor-consistency.md).

## Python client

Training code is Python, and the moment a run's lineage is available is
inside the training script — so recording it there, in-process, beats
shelling out to `rlctl` or hand-rolling an HTTP call.

```python
import runledger

with runledger.Run.start(project="demo", seed=1, params={"lr": 3e-4}) as run:
    for step in range(steps):
        loss = train_step()
        run.log_metric("loss", loss)  # overwrites -- only the final value is kept
```

It captures the same git context `rlctl record` does — commit, dirty flag —
and refuses the same way when there is no commit, or when the tree is dirty
with no `config_hash`. Framework and device provenance (`torch.__version__`,
`jax.__version__`, the active device) are captured automatically when either
library is importable. A ledger that is down, slow, or unreachable degrades
to a warning and a local spool file — it never raises into the middle of a
training run. See [`python/README.md`](python/README.md) for the full
worked example (wrap a loop, raise partway through, see the run recorded as
`failed` with the metrics it got to) and for how it survives a kill that
never reaches `__exit__`: it `POST`s a `running` record the moment the run
starts and `PATCH`es it to a terminal status when the run ends, so a
`SIGKILL` or an OOM kill still leaves the start-time record behind, and a
`SIGTERM` is caught and recorded `failed`
([ADR 0014](docs/adr/0014-python-client-writes-running-then-patches-terminal.md),
superseding [ADR 0005](docs/adr/0005-python-client-writes-once-at-the-end.md)).

**This is not a metric tracker, and does not try to be one.** `log_metric`
overwrites — calling it every step, as above, keeps only the value from the
last call for a given name, not a curve. run-ledger records what a run
**was**: identity plus the final outcome, for deciding whether two runs
were the same experiment. A tracker (W&B, MLflow, TensorBoard) records how a
run **went**: the step-by-step curve, for watching training happen. Most
setups want both, pointed at the same run — log the curve to a tracker,
record the fingerprint and final metrics here. The boundary is what each
side needs to answer its own question: a fingerprint only has to include
what must match for two runs to count as the same experiment, and everything
you want to watch move belongs on a dashboard instead, precisely because the
fingerprint must not be sensitive to it.

```bash
pip install -e ./python
```

To browse instead of scripting — a ranked list of which experiments
reproduce worst, a group's runs and provenance disagreements, and a
`compare()` verdict between two of them — see [`dashboard/README.md`](dashboard/README.md).
It is a local-only marimo app built on this same client, not a hosted
service.

## Auth

By default the server accepts writes from anyone who can reach it — the right
default for a single-user local ledger, and it stays the default. Running it
anywhere reachable by more than one party needs a token:

```bash
export RUNLEDGER_TOKEN=<write-secret>          # grants reads and writes
export RUNLEDGER_READ_TOKEN=<read-secret>      # optional: grants reads only
./bin/runledger
```

`--token-file` and `--read-token-file` read the same tokens from a file
instead, for a secrets manager that mounts one. With no token configured
either way, the server logs once, at startup, that it is running
unauthenticated.

- Tokens are compared with `crypto/subtle.ConstantTimeCompare`.
- The read token cannot write; the write token can do both, so `rlctl` needs
  only one credential for a normal workflow.
- `/healthz` stays unauthenticated so a probe does not need a credential.
- `rlctl` reads its token from `RUNLEDGER_TOKEN` only — never from a flag,
  since a token in a flag lands in shell history and in the process table.

## Decisions worth knowing

This is a summary. The full reasoning — context, consequences, and what would
have to be true to revisit each one — lives in [`docs/adr/`](docs/adr/).

- **The server computes the fingerprint, never the client.** A caller that could
  assert its own fingerprint could claim two different experiments were the same
  run. ([ADR 0001](docs/adr/0001-server-computes-the-fingerprint.md))
- **Unknown JSON fields are rejected.** A typo'd field name in a lineage record
  would otherwise store a run that claims to describe an experiment it does not.
  ([ADR 0002](docs/adr/0002-unknown-json-fields-are-rejected.md))
- **A dirty working tree without a config hash is refused.** The commit no longer
  describes the code that ran, so the config hash is the only remaining handle on
  what actually executed.
  ([ADR 0003](docs/adr/0003-dirty-tree-without-config-hash-is-refused.md))
- **Re-recording identical content is idempotent; different content under the
  same id is a conflict.** Silently overwriting a lineage record would make
  history unreliable.
- **`PATCH /v1/runs/{id}` moves a run's provenance forward; it can never rewrite
  what experiment it was.** Identity fields (`project`, `git_commit`,
  `git_dirty`, `config_hash`, `dataset_version`, `model_version`, `seed`,
  `params`) are checked against the stored run and a mismatch is a `409`, not
  an update. Status only moves `created → running → {succeeded, failed,
  cancelled}`; a transition out of a terminal state — including a same-status
  or metrics-only patch — is also a `409`, because a terminal run is a
  finished outcome, not a waypoint. Metrics are merged into the existing map,
  not replaced, so a long run can report as it goes; re-reporting an existing
  key overwrites just that key.
- **An absent metric is not a zero.** They print differently and diff
  differently. The same rule now holds for an absent param: `{}` and
  `{"foo": ""}` fingerprint differently, because `Compute` hashes only the
  keys present, so the diff reports the two sides distinctly rather than
  reading both through a map index that yields `""` for a missing key.
- **For the scalar fields, an empty string *means* "not recorded".**
  `config_hash`, `dataset_version`, `model_version`, `host`, `device`,
  `framework_version`, and `checkpoint_uri` have no meaningful empty value,
  so `""` is one spelling of absence rather than a distinction the record
  failed to capture. That keeps the fields as plain strings, leaves the
  fingerprint contract untouched, and stops a client that switches from
  sending `""` to omitting a key from silently changing every fingerprint
  it produces. `params` is exempt — presence there is real and already
  hashed. ([ADR 0011](docs/adr/0011-empty-string-means-not-recorded.md))
- **`unattributable` assumes the identity strings are honest.** `dataset_version`,
  `model_version`, and `config_hash` are free strings the caller supplies; the
  server has no way to verify that two runs labelled the same value actually
  saw the same bytes, so a mislabelled or re-exported dataset produces the
  identical signature as real nondeterminism — same fingerprint, different
  metrics. Rule that out first. `runledger.hash_dataset()` derives
  `dataset_version` from a dataset's own content instead of a typed label,
  for callers who want that check to be possible — opt-in, client-side only,
  no fingerprint contract change.
- **A comparison reads the stored fingerprint, not a fresh one.** Recomputing
  it in `compare` gave a second, independent answer to the question
  `spread` already answers from the stored value. The two agree only while
  `Compute` never changes, and [ADR 0004](docs/adr/0004-fingerprint-input-is-a-versioned-contract.md)
  exists because one day it will.
- **Hashed fields are length-prefixed** so `("ab","c")` and `("a","bc")` cannot
  collide, and **params are sorted before hashing** because Go randomizes map
  iteration — without that, the same experiment fingerprints differently between
  runs of the same binary. **The fingerprint input — which fields, in what
  order, with what delimiting — is a versioned contract**: changing it orphans
  every fingerprint already recorded, and needs a version field plus a
  documented migration, not a patch release.
  ([ADR 0004](docs/adr/0004-fingerprint-input-is-a-versioned-contract.md))
- **`/metrics` is hand-rolled, not built on a client library.** The project ships
  as one dependency-free binary, and a few counters and a histogram don't
  justify breaking that. `runledger_runs` is read from the store at scrape
  time rather than tracked as a local counter, since recording is idempotent
  and a retried record must not inflate it.
- **The Python client writes a `running` record when a run starts, and
  `PATCH`es it to a terminal status when the run ends.** It first wrote the
  ledger once, at the end, because a `running` record's mid-training metric
  would be counted as a repeat measurement and could rank a fingerprint
  worst-reproducing before the run had even finished
  ([ADR 0005](docs/adr/0005-python-client-writes-once-at-the-end.md)). Once
  `spread` excluded non-terminal runs (#52), that reason no longer applied,
  and writing once had been paying a real cost the whole time: nothing runs
  `__exit__` for a `SIGKILL` or an OOM kill, so a run killed that way left no
  trace at all. Writing early means the start-time record survives a kill
  that bypasses Python entirely.
  ([ADR 0014](docs/adr/0014-python-client-writes-running-then-patches-terminal.md),
  superseding ADR 0005)
- **`store.DuckDB` links libduckdb via cgo, on purpose.** Analytical queries over
  a high-cardinality key space are what this ledger is for, and that is a
  column-oriented workload SQLite (row-oriented) answers slowly and ClickHouse
  (a server to operate) answers at the wrong deployment shape. The cost is real:
  `CGO_ENABLED=0` no longer builds this module, and the container image needs a
  C toolchain and a matching libc instead of `distroless/static`.
  ([ADR 0006](docs/adr/0006-duckdb-store-backend-and-the-cgo-cost.md))
- **`GET /v1/runs` paginates by keyset cursor, not `LIMIT`/`OFFSET`, and is capped
  server-side.** An offset shifts under concurrent inserts, silently skipping a
  row a client paging through history should have seen; a cursor encoding the
  last row's sort key does not. A page is consistent with the ledger as of the
  cursor's position — a row recorded after a traversal begins is simply not in
  it, the same as it wouldn't be in a fresh, uncursored request made at that
  same moment.
  ([ADR 0007](docs/adr/0007-keyset-pagination-cursor-consistency.md))
- **`runledger.replay_spool()` raises on an unreachable ledger, unlike
  `Run.start()`.** Replay is invoked on purpose, after training, specifically
  to recover records — the read side's convention applies, not the write
  side's. A `400`/`409` is quarantined to `<spool>.rejected.jsonl` instead of
  being retried forever, since retrying the same bytes against the same
  server gets the same answer every time; replay is safe to interrupt and
  re-run at all because `POST /v1/runs` is idempotent for identical content
  under the same id.
  ([ADR 0008](docs/adr/0008-replay-raises-quarantines-and-tracks-a-read-offset.md))
- **Resource routes are versioned with a `/v1` URL path prefix; health,
  readiness, and metrics are not.** A path segment can be routed on
  unmodified, so a future breaking change can add `/v2` and run it alongside
  `/v1` for as long as a transition needs, without touching the ops
  endpoints a probe or a scrape config already points at.
  ([ADR 0009](docs/adr/0009-url-path-versioning-for-the-http-api.md))
- **`/v1/comparisons?a=&b=` replaces `GET /compare`.** A comparison is
  identified by the pair of run ids being compared, not owned by either one,
  so it sits next to `/v1/runs` and `/v1/fingerprints` as its own resource
  collection instead of a verb hanging off the root — and the query-param
  shape has somewhere to grow if comparing more than two runs ever matters.
  ([ADR 0010](docs/adr/0010-comparisons-is-a-resource-not-a-verb.md))
- **Run attribution (`submitter_claim`, `job_id`) is provenance, not identity,
  and `submitter_claim` is self-asserted.** The same experiment run by two
  people is still the same experiment, so neither field feeds the
  fingerprint. `RUNLEDGER_TOKEN` is one shared secret today, so the server
  cannot attest who a caller is — `submitter_claim` is named to say plainly
  that it is a claim, not a verified fact, until named per-caller tokens
  exist.
  ([ADR 0015](docs/adr/0015-run-attribution-is-provenance-self-asserted.md))
- **A capture declaration (`capture.client`, `capture.attempted`) records what
  the client tried to capture, not just what it captured, and is never
  hashed.** `device: ""` can mean "this run has no device" or "this client
  never looks" -- a single value field can't say which, so `attempted`
  answers the question the value field can't: was `device` even named in it?
  `Capture` is the one pointer field on `Run`, on purpose -- unlike ADR
  0011's fields, "no declaration at all" and "declared, and declared
  nothing" are different, useful claims, and
  `examples/churn/completeness.py`'s peer-comparison fallback depends on
  telling them apart. Not patchable: a client already knows what it
  attempted in full at record time, so there is no "fill this in later"
  case the way there is for `host`/`device`/`job_id`.
  ([ADR 0016](docs/adr/0016-record-what-the-client-attempted-to-capture.md))

## Status

`Memory` (the default; `--store memory` or unset) keeps nothing on disk. `DuckDB`
(`--store duckdb --dsn <path>`, or `RUNLEDGER_STORE=duckdb` / `RUNLEDGER_DSN=...`)
is durable and answers the analytical queries this ledger is for — every run of
a project, grouped by fingerprint, with the spread of a metric across the
group — without the linear Go-side scan `Memory` does. `Query`
(`internal/store/store.go`) does not expose a `params.*` / `metrics.*` filter
yet; the schema (`internal/store/duckdb.go`) is shaped so that is a `WHERE`
clause away, not a migration, when it's needed.

## Archive

Historical and superseded documentation lives in [`archive/`](archive/). Its
contents are past decisions, plans, or state — not the current design.
Don't use anything under `archive/` to understand this project as it is
today or to guide new work; use this README and the code instead.

## License

MIT
