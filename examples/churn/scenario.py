"""The worked example: a real bug, found from the ledger record alone.

Four scenarios, in the order a person would actually hit them.

  A. Repeat one experiment four times. The ledger says the results are
     unattributable -- same experiment, different numbers.
  B. Go looking, as the ledger just told you to. Find the unseeded split.
     Put it in the config, which puts it in the fingerprint. Repeat four
     more times. The ledger goes quiet.
  C. Change a hyperparameter for real. Different fingerprint, no alarm --
     the tool stays silent when silence is correct.
  D. Record one run through a sloppier path. The capture report flags it
     without anyone having said it was different.

Run with `make example`, or against a ledger you already have up:

    RUNLEDGER_ADDR=http://localhost:8080 python3 examples/churn/scenario.py
"""

from __future__ import annotations

import sys

import completeness
import ledger
import train

PROJECT = "churn-model"
DATASET = "synthetic-churn-2026-08-01"
MODEL = "logreg-gd-1.0"
REPEATS = 4
BASE_SEED = 7
EPOCHS = 150


def rule(title: str) -> None:
    print(f"\n{'=' * 72}\n{title}\n{'=' * 72}")


def record_repeats(*, cfg, params, split_seed, device, label) -> list:
    """Run the same experiment REPEATS times and record each one."""
    cfg_hash = ledger.config_hash(cfg)
    ids = []
    for i in range(REPEATS):
        metrics = train.run_experiment(
            seed=BASE_SEED,
            lr=cfg["lr"],
            epochs=cfg["epochs"],
            # None here is the bug; an int is the fix.
            split_seed=split_seed,
        )
        run_id = ledger.record(
            project=PROJECT,
            seed=BASE_SEED,
            params=params,
            cfg_hash=cfg_hash,
            metrics=metrics,
            dataset_version=DATASET,
            model_version=MODEL,
            device=device,
            framework_version=f"python {sys.version_info.major}.{sys.version_info.minor}",
            host="workstation-1",
        )
        ids.append(run_id)
        print(f"  {label} run {i + 1}/{REPEATS}  auc={metrics['auc']:.4f}  "
              f"log_loss={metrics['log_loss']:.4f}  {run_id}")
    return ids


def show_group(fingerprint: str) -> None:
    g = ledger.get(f"/v1/fingerprints/{fingerprint}")
    if g.get("no_repeats"):
        print("  no repeats -- nothing to compare")
        return
    print(f"  {'METRIC':<12}{'COUNT':>6}{'MIN':>11}{'MAX':>11}{'MEAN':>11}{'STDDEV':>11}{'CV':>10}")
    for name, s in sorted(g["metrics"].items()):
        cv = abs(s["stddev"] / s["mean"]) if s["mean"] else 0.0
        print(f"  {name:<12}{s['count']:>6}{s['min']:>11.4f}{s['max']:>11.4f}"
              f"{s['mean']:>11.4f}{s['stddev']:>11.4f}{cv:>9.2%}")


def _short(v: str) -> str:
    """Metric values arrive as full float repr; 6 significant figures is
    plenty to see a difference and keeps the columns aligned."""
    try:
        return f"{float(v):g}"
    except (TypeError, ValueError):
        return v if len(v) <= 18 else v[:17] + "\u2026"


def verdict(a: str, b: str) -> None:
    res = ledger.compare(a, b)
    if res["same_experiment"]:
        print("  fingerprints MATCH -- the ledger says these are the same experiment")
    else:
        print("  fingerprints DIFFER -- the ledger says these are different experiments")
    for f in res["fields"] or []:
        print(f"    {f['name']:<20}{f['kind']:<12}"
              f"{_short(f['a']):<20}{_short(f['b'])}")
    if res["unattributable"]:
        print("  UNATTRIBUTABLE: same experiment, different results.")
        print("  Something that affected the outcome is not in the record.")
    else:
        print("  not unattributable -- nothing here the record cannot explain")


def main() -> None:
    ledger.wait_until_up()

    rule("A. The same experiment, four times")
    print("""
Identical identity every time: same project, commit, config hash, dataset,
model, seed, and params. If the record is complete, the numbers should be
identical too.
""")
    buggy_cfg = {"lr": 0.5, "epochs": EPOCHS, "standardize": True}
    params = {"lr": "0.5", "epochs": str(EPOCHS)}
    buggy = record_repeats(
        cfg=buggy_cfg, params=params, split_seed=None, device="cpu", label="A"
    )
    print("\nWhat the ledger makes of the first two:")
    verdict(buggy[0], buggy[1])
    print("\nAnd across all four repeats:")
    buggy_fp = ledger.get(f"/v1/runs/{buggy[0]}")["fingerprint"]
    show_group(buggy_fp)
    print("""
That AUC range is the whole finding. Nothing in the record moved, and the
model got measurably better or worse anyway. The ledger cannot say why --
it can only say the explanation is not in what you recorded.
""")

    rule("B. Find the cause, record it, repeat")
    print("""
Going looking, as the verdict above says to: train.split_indices draws the
train/test partition from an unseeded RNG. The recorded `seed` covers weight
initialization only, so every run above trained and evaluated on a different
partition of the same data.

The fix is not just to seed it -- it is to make the seed part of the
configuration, so it lands in config_hash and therefore in the fingerprint.
An unrecorded knob cannot explain anything; a recorded one can.
""")
    fixed_cfg = {**buggy_cfg, "split_seed": 4242}
    fixed = record_repeats(
        cfg=fixed_cfg, params=params, split_seed=4242, device="cpu", label="B"
    )
    print("\nThe same two-run comparison, after the fix:")
    verdict(fixed[0], fixed[1])
    print("\nAnd across all four:")
    fixed_fp = ledger.get(f"/v1/runs/{fixed[0]}")["fingerprint"]
    show_group(fixed_fp)

    rule("C. A change that is supposed to change the answer")
    print("""
Dropping the learning rate from 0.5 to 0.02. This *should* move the
metrics, and the ledger should stay quiet about it -- a tool that cries wolf on legitimate work is
worse than no tool.
""")
    lr_cfg = {**fixed_cfg, "lr": 0.02}
    lr_params = {"lr": "0.02", "epochs": str(EPOCHS)}
    changed = record_repeats(
        cfg=lr_cfg, params=lr_params, split_seed=4242, device="cpu", label="C"
    )
    print("\nFixed baseline vs the new learning rate:")
    verdict(fixed[0], changed[0])

    rule("D. One run recorded through a sloppier path")
    print("""
The same experiment as B, but submitted by something that did not capture
dataset_version, framework_version, or host -- a notebook run by hand, a
script missing an env var. The ledger accepts it: every one of those fields
is allowed to be empty, and an empty string is indistinguishable from "not
recorded" on the wire.

Nobody tells the report that this run is different. It infers it.
""")
    sloppy = ledger.record(
        project=PROJECT,
        seed=BASE_SEED,
        params=params,
        cfg_hash=ledger.config_hash(fixed_cfg),
        metrics=train.run_experiment(
            seed=BASE_SEED, lr=0.5, epochs=EPOCHS, split_seed=4242
        ),
        device="cpu",
        # dataset_version, model_version, framework_version, host all omitted.
    )
    print(f"  recorded {sloppy}")

    rule("The capture report")
    all_runs = ledger.runs(PROJECT)["runs"]
    print()
    print(completeness.report(all_runs))

    rule("Summary")
    print(f"""
  A  {REPEATS} runs, identical identity, metrics scattered  -> UNATTRIBUTABLE
  B  the split seed moved into the config           -> quiet, CV 0.00%
  C  a real hyperparameter change                   -> different experiment, quiet
  D  one run missing what its peers record          -> flagged as likely client fault

The ledger never knew about the unseeded split. It only knew the record
claimed two runs were the same and the numbers disagreed -- which was
enough to send someone to look in the right place.
""")


if __name__ == "__main__":
    main()
