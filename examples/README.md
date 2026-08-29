# examples

A worked, dependency-free scenario that gives the ledger something true to
find. Unlike `python/examples/reproducibility.ipynb`, which demonstrates the
API against numbers chosen to illustrate it, this one trains an actual model
containing an actual reproducibility bug and lets the ledger discover it.

```bash
make example
```

That builds the binaries, starts an ephemeral in-memory ledger on `:8125`,
runs the scenario against it, and tears it down. Nothing is persisted and no
port you use for real is touched. To run it against a ledger you already
have up:

```bash
RUNLEDGER_ADDR=http://localhost:8080 python3 examples/churn/scenario.py
```

Standard library only — no `pip install` of any kind, including this repo's
own Python client. The project's pitch is "one binary, no external
dependencies to try it," and an example that needs a dependency install
before it demonstrates anything would undercut that.

## What it does

`churn/train.py` is a logistic-regression churn model in ~150 lines of pure
Python. It contains one real bug: `split_indices` draws the train/test
partition from an unseeded RNG. The run's recorded `seed` covers weight
initialization only, so the split is not in the record — not in `params`,
not in `config_hash`, not anywhere.

The scenario then walks the path a person actually walks:

| | | |
|---|---|---|
| **A** | Repeat one experiment four times | AUC scatters across ~0.75–0.81 with a byte-identical identity record. `unattributable`. |
| **B** | Go looking, find the split, put the seed in the config | `config_hash` changes, so the fingerprint changes. Spread collapses to **exactly** 0.00%. |
| **C** | Change the learning rate for real | Different fingerprint, metrics move, no alarm. |
| **D** | Record one run through a sloppier path | The capture report flags it without being told. |

**A is the finding and B is the lesson.** The fix is not merely to seed the
split — it is to make the seed part of the configuration, so it lands in
`config_hash` and therefore in the fingerprint. An unrecorded knob cannot
explain a difference; a recorded one can. That is the whole loop the ledger
is built around, and B is what closes it.

C matters as much as A: a tool that cried wolf on a legitimate hyperparameter
change would be worse than no tool. The ledger stays silent because the
change is *in* the record.

Because the working tree is dirty while you read this, every run here passes
an explicit `config_hash` — which is exactly what
[ADR 0003](../docs/adr/0003-dirty-tree-without-config-hash-is-refused.md)
requires, and not a workaround for it.

## `churn/completeness.py` — flagging a likely client fault

A separate question from "what changed": **is the record itself trustworthy?**

A lineage record cannot distinguish "this run genuinely had no
`dataset_version`" from "the client forgot to send one" — both arrive as the
empty string, and intent is unrecoverable from one record.

Across a *project*, it is recoverable statistically. The detector needs no
schema change and no new field; it reads what `GET /v1/runs` already returns
and compares a run against its peers:

- **Odd-one-out** — the field is well covered by peers and empty here.
  Points at a single bad launch: a hand-run notebook, a missing env var, a
  stale script. Guarded by both a share floor (60%) and a peer-count floor
  (3), so two runs agreeing is never treated as a convention.
- **Blind spot** — the field is empty for *every* run in the project. Points
  at the pipeline rather than the run. Reported quietly on its own, and
  escalated only when a fingerprint group also shows spread the record
  cannot explain — because an uncaptured field is then a candidate
  explanation by construction.

The two signals never double-report the same absence, which
`test_completeness.py` pins.

```
LIKELY CLIENT FAULT -- a run is missing what its peers record
  03b66aac0a75dfb9-dl1jzcqg41pc-d4a44ebd9f
    dataset_version    identity   empty here, recorded by 12/13 runs
    model_version      identity   empty here, recorded by 12/13 runs
    host               provenance empty here, recorded by 12/13 runs
    framework_version  provenance empty here, recorded by 12/13 runs
```

This is a heuristic and is labelled as one — "likely", never "is". It is a
prior supplied by the peer group, not a fact recovered from the record.
Making it a fact requires representing absence on the wire; see the note
below.

## Tests

```bash
python3 -m unittest discover examples/churn
```

Eleven tests, standard library only. They cover the cases the scenario
cannot stage without contorting itself — a genuine blind spot, the
evidence floors that keep the detector quiet, and the CV-over-raw-stddev
ranking. The negative cases matter more than the positive one: a detector
that fires on thin evidence would train people to ignore it.

## What this example is evidence for

Running it makes two gaps in the current data model concrete:

1. **`""` and "not recorded" are the same value on the wire**, for
   `config_hash`, `dataset_version`, `model_version`, `host`, `device`, and
   `framework_version` — a known gap the struct comment in
   [`internal/lineage/run.go`](../internal/lineage/run.go) already
   acknowledges. For the three identity fields it is worse than cosmetic: a
   run that genuinely had no dataset version and one whose client dropped it
   produce the *same fingerprint*, so they are grouped as the same
   experiment and their metric difference is reported as unattributable.
   That is a false positive on the single claim this ledger makes.

2. **The same conflation reaches the diff.** Two runs whose params are `{}`
   and `{"foo": ""}` fingerprint differently — `Compute` writes only the
   keys that are present — but `compare.Runs` renders both sides of
   `params.foo` as `""`, so no field is emitted. `GET /v1/comparisons`
   answers `same_experiment: false` with `fields: null`.

   `rlctl diff` used to key its verdict off `len(Fields)` and printed **"the
   two records are identical"** for that pair — the opposite of what the
   server had just said. That half is fixed: the verdict now comes from
   `SameExperiment`, and the no-rows case explains why a real difference has
   nothing to show. The underlying conflation in `compare.Runs` remains, and
   is the same gap as (1): a diff still cannot name which param it was.

The detector in this folder is the best available answer while both hold.
It is a workaround for (1), not a substitute for fixing it.
