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

`Run.start()` captures git context (commit, dirty flag) the moment it is
called, the same way `rlctl record` does, and refuses to proceed — raising
`runledger.NoGitCommitError` or `runledger.DirtyTreeError` — if the run would
not be reconstructible. That check happens before any training runs, not
after.

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
immediately, then update it to `succeeded`/`failed` at the end. The ledger's
API does not support that yet: `POST /runs` is idempotent only for a
byte-identical re-record of the same run id, and returns a conflict for the
same id with different content — there is no `PATCH`
([issue #1](https://github.com/Lurking-Walrus/run-ledger/issues/1)).

So this client buffers the run's status and metrics locally for its whole
lifetime, and writes the ledger exactly once, in `Run.start()`'s `__exit__`,
once the outcome is known. That is what makes one accurate record per run
possible against the API as it exists today. `run.run_id` and
`run.fingerprint` are only populated after that one call completes.

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
