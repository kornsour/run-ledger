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

That builds the image from the `Dockerfile` and starts `runledger` on
`:8080` with a persistent volume mounted for the store (currently unused --
see [Status](#status)). Published images land on `ghcr.io/lurking-walrus/run-ledger`
on tagged releases; `make image` builds the same image locally for the host
platform.

## Layout

```
cmd/runledger/     HTTP server
cmd/rlctl/         researcher CLI: record · list · show · diff
internal/lineage/  the Run record, validation, content-addressed fingerprint
internal/store/    Store interface + in-memory reference implementation
internal/compare/  structured diff, identity vs provenance vs metric
internal/api/      HTTP handlers
python/runledger/  in-process Python client: `Run.start()` as a context manager
```

Every directory here contains code. The store is an interface with a conformance
suite (`store.RunConformance`) that any future backend must pass, so a second
implementation cannot quietly disagree about ordering or idempotency.

## API

| Method | Path | |
|---|---|---|
| `POST` | `/runs` | record a run; the server assigns the fingerprint and id |
| `GET` | `/runs` | list, filtered by `project`, `git_commit`, `fingerprint`, `status`, `device`, `limit` |
| `GET` | `/runs/{id}` | one run |
| `GET` | `/compare?a=X&b=Y` | structured diff, with `unattributable` |
| `GET` | `/healthz` | liveness |
| `GET` | `/readyz` | readiness — the store answers a call |
| `GET` | `/metrics` | self-metrics, Prometheus exposition format |

## Python client

Training code is Python, and the moment a run's lineage is available is
inside the training script — so recording it there, in-process, beats
shelling out to `rlctl` or hand-rolling an HTTP call.

```python
import runledger

with runledger.Run.start(project="demo", seed=1, params={"lr": 3e-4}) as run:
    for step in range(steps):
        loss = train_step()
        run.log_metric("loss", loss)
```

It captures the same git context `rlctl record` does — commit, dirty flag —
and refuses the same way when there is no commit, or when the tree is dirty
with no `config_hash`. Framework and device provenance (`torch.__version__`,
`jax.__version__`, the active device) are captured automatically when either
library is importable. A ledger that is down, slow, or unreachable degrades
to a warning and a local spool file — it never raises into the middle of a
training run. See [`python/README.md`](python/README.md) for the full
worked example (wrap a loop, raise partway through, see the run recorded as
`failed` with the metrics it got to) and why the client makes exactly one
HTTP call, at the end, rather than one at each end of the run
([ADR 0005](docs/adr/0005-python-client-writes-once-at-the-end.md)).

```bash
pip install -e ./python
```

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
- **An absent metric is not a zero.** They print differently and diff
  differently.
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
- **The Python client writes the ledger once, at the end of a run, not once at
  each end.** `POST /runs` has no update path yet, so `runledger.Run.start()`
  buffers status and metrics locally and sends one record when the outcome is
  known, rather than a `running` record it cannot later revise.
  ([ADR 0005](docs/adr/0005-python-client-writes-once-at-the-end.md))

## Status

The in-memory store is the only backend, so nothing survives a restart. A durable,
queryable backend is the next piece of work — see the issue tracker.

## Archive

Historical and superseded documentation lives in [`archive/`](archive/). Its
contents are past decisions, plans, or state — not the current design.
Don't use anything under `archive/` to understand this project as it is
today or to guide new work; use this README and the code instead.

## License

MIT
