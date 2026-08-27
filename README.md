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

## Layout

```
cmd/runledger/     HTTP server
cmd/rlctl/         researcher CLI: record · list · show · diff
internal/lineage/  the Run record, validation, content-addressed fingerprint
internal/store/    Store interface + in-memory reference implementation
internal/compare/  structured diff, identity vs provenance vs metric
internal/api/      HTTP handlers
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

- **The server computes the fingerprint, never the client.** A caller that could
  assert its own fingerprint could claim two different experiments were the same
  run.
- **Unknown JSON fields are rejected.** A typo'd field name in a lineage record
  would otherwise store a run that claims to describe an experiment it does not.
- **A dirty working tree without a config hash is refused.** The commit no longer
  describes the code that ran, so the config hash is the only remaining handle on
  what actually executed.
- **Re-recording identical content is idempotent; different content under the
  same id is a conflict.** Silently overwriting a lineage record would make
  history unreliable.
- **An absent metric is not a zero.** They print differently and diff
  differently.
- **Hashed fields are length-prefixed** so `("ab","c")` and `("a","bc")` cannot
  collide, and **params are sorted before hashing** because Go randomizes map
  iteration — without that, the same experiment fingerprints differently between
  runs of the same binary.

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
