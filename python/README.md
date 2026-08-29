# runledger (Python)

Record a run's lineage from inside training code, instead of shelling out to
`rlctl` or hand-rolling an HTTP call. See the [repo root README](../README.md)
for what a run record is and why identity and provenance are recorded
separately.

## Install

```bash
pip install -e ./python          # from the repo root, for local development
```

No dependencies beyond the standard library — `urllib`, not `requests`.

## Use

```python
import runledger

with runledger.Run.start(project="demo", seed=1, params={"lr": 3e-4}) as run:
    for step in range(steps):
        loss = train_step()
        run.log_metric("loss", loss)  # overwrites -- only the final value is kept
```

**This is not a metric tracker.** `Run` stores what a run **was** — identity
plus a final outcome, for deciding whether two runs were the same
experiment — not how a run **went**. `log_metric` overwrites, so calling it
every step, as above, is fine to write but does not build a curve; the
record keeps whichever call happened last for each name. If you want the
curve, log it to a tracker (W&B, MLflow, TensorBoard) the same step you call
`log_metric` here — most training setups want both, and they answer
different questions: a metric belongs in the fingerprint's neighborhood only
if it is the final number you'd use to decide whether this run reproduced;
everything you want to watch move over time belongs on a dashboard instead.

> **Run this from inside a git checkout.** `Run.start()` captures the commit
> before the `with` body runs and raises `runledger.NoGitCommitError` if there
> isn't one — a run whose code can't be identified isn't lineage. That is the
> same rule the server enforces ([ADR 0003](../docs/adr/0003-dirty-tree-without-config-hash-is-refused.md)),
> and it means **hosted notebook environments without a checkout (Colab and
> friends) are not supported**. There is no flag to switch it off.

`Run.start()` captures git context (commit, dirty flag) the moment it is
called, the same way `rlctl record` does, and refuses to proceed — raising
`runledger.NoGitCommitError` or `runledger.DirtyTreeError` — if the run would
not be reconstructible. That check happens before any training runs, not
after.

### What `Run.start()` accepts

Every option is an explicit keyword argument, so your editor and type checker
can see them (the package ships a PEP 561 `py.typed` marker):

```python
Run.start(
    project,                  # required
    *,
    seed=0,
    params=None,              # dict; values are stringified on the wire
    dataset_version="",
    model_version="",
    config_hash="",           # required when the tree is dirty
    server=None,              # defaults to $RUNLEDGER_ADDR
    timeout=10.0,
    spool_path="~/.runledger/spool.jsonl",
)
```

The first six are **identity** — they are hashed into the run's fingerprint.
The last three configure the client and are not recorded in the ledger. The
names match the wire schema and `rlctl`'s flags exactly.

`run.log_metric(name, value)` records a measured metric as training
progresses. Nothing is sent to the ledger yet — see below.

On a normal exit, the run is recorded `succeeded`. On an exception, it is
recorded `failed`, with whatever metrics were logged before the exception,
and the exception is re-raised unchanged — `Run.start()` never swallows your
training script's own errors.

### The honest example

```python
import runledger

with runledger.Run.start(project="demo", seed=1) as run:
    for step in range(10):
        if step == 4:
            raise RuntimeError("GPU fell over")
        run.log_metric("loss", 1.0 / (step + 1))
```

```
Traceback (most recent call last):
  ...
RuntimeError: GPU fell over
```

The exception still propagates — this script still crashes, as it should.
But the run was recorded on the way out:

```bash
$ rlctl list --project demo --status failed
RUN                           PROJECT       STATUS      COMMIT      STARTED
a1b2c3d4e5f6a7b8-abc123        demo          failed      a1b2c3d     2026-08-27T...
$ rlctl show a1b2c3d4e5f6a7b8-abc123
{
  ...
  "status": "failed",
  "metrics": {
    "loss": 0.3333333333333333
  }
}
```

`loss` for steps 0–3 made it into the record; the run that would have
happened at step 4 did not. That is the point: the ledger records what
actually ran, including how far it got.

## The worked notebook

[`examples/reproducibility.ipynb`](examples/reproducibility.ipynb) records one
experiment three times, shows all three landing on a single fingerprint with
three different losses, and reads the verdict back — the ledger's central
claim, executed rather than asserted.

```bash
make build && ./bin/runledger &        # from the repo root
pip install -e './python[docs]'
jupyter notebook python/examples/reproducibility.ipynb
```

Or read it already executed, without installing anything:
**https://kornsour.github.io/run-ledger/reproducibility.html**

It is committed with outputs stripped and executed on every deploy, so the
published version always reflects a run that actually happened.

If you add a cell that draws a plot, put an `alt_text` entry in that cell's
metadata describing what the image *shows*. nbconvert emits no `alt`
attribute for output images and papers over the gap with a hardcoded "No
description has been provided for this image"; `make notebook` substitutes
what you declared, and fails the build if a plot has nothing to substitute. Same git
precondition as everything else here — it needs a checkout, so Colab will not
work.

## Reading the ledger back

Recording is half the client. The other half answers the question the ledger
exists for — *which of my experiments don't reproduce?* — without shelling out
to `rlctl` or hand-writing the pagination loop.

```python
import runledger

# Every run in a project, newest first. next_cursor is followed internally.
for r in runledger.runs(project="demo", status="succeeded"):
    print(r["run_id"], r["metrics"])

# One run by id.
r = runledger.run("a1b2c3d4e5f6a7b8-abc123")

# Fingerprints with more than one run, ranked worst-reproducing first.
for group in runledger.spread(project="demo"):
    print(group["fingerprint"], group["count"], group["metrics"])

# Or one experiment's own repeats.
group, = runledger.spread(fingerprint="a1b2c3...")

# What actually differs between two runs -- the ledger's headline question.
diff = runledger.compare(a1, a2)  # ids, or dicts with a run_id, either works
print(diff["same_experiment"], diff["unattributable"])
for field in diff["fields"]:
    print(field["kind"], field["name"], field["a"], "->", field["b"])
```

`runs()` walks every page by default; pass `limit=` to bound it. Both read
from `$RUNLEDGER_ADDR` and `$RUNLEDGER_TOKEN` the same way `Run.start()` does
— the token is never a keyword argument, for the same reason it is never an
`rlctl` flag.

`compare()` is the natural next step after `spread()` turns up a group with
a nonzero stddev: `group["run_ids"]` names the runs, and `compare()` says
which fields actually differ between two of them, each tagged `identity`,
`provenance`, or `metric`. `unattributable` is `true` when `same_experiment`
is `true` but the metrics still moved — same identity, different result,
nothing in the record explains why.

These return plain dicts, straight off the wire. The package still has no
dependencies; if you want a frame, you have lost nothing:

```python
import pandas as pd
df = pd.json_normalize(runledger.runs(project="demo"))
df[["run_id", "metrics.loss", "params.lr"]]
```

`params` and `metrics` are nested objects on the wire (`Dict[str, str]` and
`Dict[str, float]`), so plain `pd.DataFrame(...)` leaves them as columns of
dicts rather than usable columns. `json_normalize` flattens them to
`metrics.loss`, `params.lr`, and so on — the same dotted names the
`/v1/comparisons` endpoint already reports fields under.

### Reads raise; writes don't

The two halves fail differently, on purpose:

| | on an unreachable ledger |
|---|---|
| `Run.start()` | `RuntimeWarning` + spool to disk, never raises |
| `runs()` / `run()` / `spread()` / `compare()` | raises `LedgerUnreachableError` |

Recording must never fail an expensive training job, so the write path
degrades. A read has no such constraint, and the opposite default is the
safe one: silently returning `[]` when the server is down would answer "how
did my experiments do?" with "they didn't". `RunNotFoundError` (a subclass of
`LedgerError`, as is `LedgerUnreachableError`) is raised for an unknown run
id or fingerprint — `compare()` names which side, `a` or `b`, when it is the
one that doesn't exist.

## Why one HTTP call, not two

It might look natural for `Run.start()` to record the run as `running`
immediately, then update it to `succeeded`/`failed` at the end. The API
supports that — `PATCH /v1/runs/{id}` exists, and `rlctl start` / `rlctl finish`
use it. This client deliberately does not.

The obstacle is on the read side, not the write side. `/v1/fingerprints` groups
every run sharing a fingerprint without filtering on status, so a `running`
record would count as a *repeat measurement* of that experiment and its
mid-training metric would join the group's min/max/mean/stddev. A loss
sampled at step 50 of 10,000 sitting beside a finished run's final loss
would widen the spread and could rank that fingerprint worst-reproducing —
reporting "something affecting the result went unrecorded" when the real
explanation is "one of these has not finished." That is a false positive on
the ledger's central claim, so this client does not create the condition.

So it buffers the run's status and metrics locally for its whole lifetime,
and writes the ledger exactly once, in `Run.start()`'s `__exit__`, once the
outcome is known. `run.run_id` and `run.fingerprint` are only populated
after that one call completes. See
[ADR 0005](../docs/adr/0005-python-client-writes-once-at-the-end.md).

## When the ledger is unreachable

Recording must never fail the training run. If the final write can't reach
the server — connection refused, timed out, DNS failure, a bad response — the
client emits a `RuntimeWarning` and appends the run record as one JSON line
to a local spool file (`~/.runledger/spool.jsonl` by default, or
`spool_path=` on `Run.start()`) instead of raising. `run.spooled` is `True`
when that happened, and `run.resolved_spool_path()` is the file it actually
wrote — `spool_path` keeps whatever you passed, `~` and all.

### Replaying a spool

Getting spooled records back into the ledger is naturally idempotent:
`started_at` is client-supplied and part of the spooled payload, and the
server derives `run_id` from the fingerprint plus that timestamp, so
re-sending the same line twice lands on the same run both times. That is
what makes replay safe to interrupt and re-run.

```python
import runledger

result = runledger.replay_spool()                    # default spool path
result = runledger.replay_spool(path, dry_run=True)   # report without sending
print(result.sent, result.rejected, result.remaining)
```

Or from a shell, after the fact:

```bash
python -m runledger.replay                 # default spool path
python -m runledger.replay --dry-run        # report without sending
python -m runledger.replay ~/other-spool.jsonl --server https://ledger.example.com
```

`replay_spool()` reads from `$RUNLEDGER_ADDR` / `$RUNLEDGER_TOKEN`, the same
way `Run.start()` does. Records the ledger accepts are removed from the
spool; records it will *never* accept — a `400` or a `409` against a run id
that already exists with different content — are moved to
`<spool>.rejected.jsonl` instead of being retried forever. Unlike
`Run.start()`, this raises `runledger.LedgerUnreachableError` rather than
degrading, matching the read side: replay is something you invoke on
purpose to recover records, so silently reporting partial progress would be
the wrong default. Whatever was sent or rejected earlier in the call is not
undone; the record that failed, and everything after it — including
anything a live training run appended to the spool in the meantime — stays
in the spool for the next attempt.

## Configuration

| | env var | `Run.start()` kwarg | default |
|---|---|---|---|
| server address | `RUNLEDGER_ADDR` | `server=` | `http://localhost:8080` |
| bearer token | `RUNLEDGER_TOKEN` | — (never a kwarg — same reasoning as `rlctl`: a token in code or a flag lands somewhere it shouldn't) | none |
| request timeout (seconds) | — | `timeout=` | `10.0` |
| spool file path | — | `spool_path=` | `~/.runledger/spool.jsonl` |

## Framework and device provenance

If `torch` or `jax` are importable, their `__version__` and active device
are captured automatically — `framework_version` becomes e.g. `"torch
2.4.0"`, and `device` becomes the CUDA device name or JAX device string.
Neither is required; a run with neither installed records `device="cpu"`
and an empty `framework_version`.

## Deriving `dataset_version`

`dataset_version` is a free string — the server hashes it into the
fingerprint exactly as given and has no way to check that two runs labelled
the same string actually saw the same bytes (see the root README's note on
what that costs the `unattributable` verdict). `hash_dataset()` lets a
caller who wants the label to mean something derive it from the data
instead of typing it:

```python
import runledger

digest = runledger.hash_dataset("data/train")  # hex sha256, deterministic

with runledger.Run.start(project="demo", seed=1, dataset_version=digest) as run:
    ...
```

It hashes a manifest of `(relative path, size, content digest)` per file,
sorted by path — so the result does not depend on directory listing order,
the machine it runs on, or where the tree happens to live on disk.
Deliberately excluded: absolute paths, mtimes, and permissions, none of
which are the data and all of which change on a plain copy or checkout
without the bytes changing. An empty directory hashes to a fixed, defined
value rather than raising; a missing path raises `FileNotFoundError`; a
symlink anywhere in the tree raises `SymlinkNotSupportedError` rather than
guessing whether the manifest should describe the link or whatever it
currently resolves to. Files are hashed streaming, in chunks (`chunk_size=`,
default 1 MiB), so a multi-gigabyte file never has to fit in memory at once.

This is entirely client-side and opt-in: the server goes on storing
whatever string it is given as `dataset_version`, unchanged. Nothing about
the wire schema or the fingerprint contract
([ADR 0004](../docs/adr/0004-fingerprint-input-is-a-versioned-contract.md))
is affected — `hash_dataset()` just gives a caller a principled way to
produce that string instead of typing `"v1"` by hand.

## Reference

Generated from the source by `pdoc`:
**https://kornsour.github.io/run-ledger/python/**

## Test

```bash
python3 -m unittest discover -s python/tests -v
```

That includes structural checks on the notebook (no committed outputs, public
API only). Actually *executing* it is CI's `notebook` job, or locally:

```bash
make notebook
```
