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
        run.log_metric("loss", loss)
```

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

## Why one HTTP call, not two

It might look natural for `Run.start()` to record the run as `running`
immediately, then update it to `succeeded`/`failed` at the end. The API
supports that — `PATCH /runs/{id}` exists, and `rlctl start` / `rlctl finish`
use it. This client deliberately does not.

The obstacle is on the read side, not the write side. `/fingerprints` groups
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
when that happened. Re-record spooled lines later with, e.g.:

```bash
while IFS= read -r line; do
  curl -sS -X POST "$RUNLEDGER_ADDR/runs" -H 'Content-Type: application/json' -d "$line"
done < ~/.runledger/spool.jsonl
```

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

## Test

```bash
python3 -m unittest discover -s python/tests -v
```
