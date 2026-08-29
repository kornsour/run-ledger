"""Tests for the capture detector.

The interesting cases are the ones the scenario cannot stage without
contorting itself: a project with a genuine blind spot, and the guards that
stop the detector firing on too little evidence. A detector that cries wolf
would be worse than none, so the negative cases matter more than the
positive one.

    python3 -m unittest discover examples/churn
"""

from __future__ import annotations

import unittest

import completeness


def run(run_id: str, **fields) -> dict:
    """A run record with everything captured, minus whatever is overridden."""
    base = {
        "run_id": run_id,
        "fingerprint": "fp0",
        "config_hash": "cfg",
        "dataset_version": "ds-1",
        "model_version": "m-1",
        "host": "h1",
        "device": "cpu",
        "framework_version": "python 3.14",
        "metrics": {"auc": 0.8},
    }
    base.update(fields)
    return base


class OddOneOut(unittest.TestCase):
    def test_flags_a_run_missing_what_peers_record(self):
        runs = [run(f"r{i}") for i in range(5)] + [run("odd", dataset_version="")]
        [finding] = completeness.odd_ones_out(runs)
        self.assertEqual(finding["run_id"], "odd")
        self.assertEqual([m["field"] for m in finding["missing"]], ["dataset_version"])
        self.assertEqual(finding["missing"][0]["kind"], "identity")

    def test_silent_when_too_few_peers_record_it(self):
        # 2 of 3 clears the 60% share but not the 3-peer floor: two runs
        # agreeing is a coincidence, not a convention.
        runs = [run("a"), run("b"), run("c", dataset_version="")]
        self.assertEqual(completeness.odd_ones_out(runs), [])

    def test_silent_when_the_field_is_rare_among_peers(self):
        # Only 1 of 5 records it, so its absence is the norm, not the outlier.
        runs = [run(f"r{i}", dataset_version="") for i in range(4)] + [run("has")]
        self.assertEqual(completeness.odd_ones_out(runs), [])

    def test_a_fully_captured_project_is_quiet(self):
        self.assertEqual(completeness.odd_ones_out([run(f"r{i}") for i in range(6)]), [])


class BlindSpots(unittest.TestCase):
    def test_field_no_run_ever_records(self):
        runs = [run(f"r{i}", framework_version="") for i in range(5)]
        self.assertEqual(
            [s["field"] for s in completeness.blind_spots(runs)], ["framework_version"]
        )

    def test_a_blind_spot_is_not_an_odd_one_out(self):
        # Nobody records it, so no single run is the outlier. The two
        # signals must not double-report the same absence.
        runs = [run(f"r{i}", host="") for i in range(5)]
        self.assertTrue(completeness.blind_spots(runs))
        self.assertEqual(completeness.odd_ones_out(runs), [])

    def test_report_escalates_a_blind_spot_only_alongside_spread(self):
        quiet = [run(f"r{i}", host="", metrics={"auc": 0.8}) for i in range(4)]
        self.assertIn("nothing to chase", completeness.report(quiet))

        noisy = [
            run(f"r{i}", host="", metrics={"auc": auc})
            for i, auc in enumerate((0.70, 0.80, 0.90, 0.75))
        ]
        self.assertIn("BLIND SPOTS, AND THEY MATTER HERE", completeness.report(noisy))


class UnattributableGroups(unittest.TestCase):
    def test_identical_metrics_are_not_spread(self):
        runs = [run(f"r{i}", metrics={"auc": 0.8}) for i in range(4)]
        self.assertEqual(completeness.unattributable_groups(runs), [])

    def test_a_lone_run_is_not_a_group(self):
        self.assertEqual(completeness.unattributable_groups([run("solo")]), [])

    def test_ranks_by_coefficient_of_variation_not_raw_stddev(self):
        # loss moves less in absolute terms than auc, but far more
        # relative to its own mean -- that is the one to surface.
        runs = [
            run(f"r{i}", metrics={"auc": auc, "loss": loss})
            for i, (auc, loss) in enumerate(
                ((0.90, 0.010), (0.88, 0.050), (0.92, 0.005), (0.89, 0.040))
            )
        ]
        [group] = completeness.unattributable_groups(runs)
        self.assertEqual(group["metric"], "loss")

    def test_skips_a_metric_whose_mean_is_zero(self):
        runs = [run(f"r{i}", metrics={"centered": v}) for i, v in enumerate((-1.0, 1.0))]
        self.assertEqual(completeness.unattributable_groups(runs), [])


if __name__ == "__main__":
    unittest.main()
