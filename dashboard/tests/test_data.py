"""Tests for the dashboard's data layer -- no marimo runtime involved.

Same fake-HTTP-server approach as python/tests/test_read.py: a tiny
in-process stand-in for the ledger, not a live process or a mock of
runledger itself, so these exercise the real request/response path.
"""

from __future__ import annotations

import json
import os
import sys
import threading
import unittest
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

_HERE = os.path.dirname(__file__)
# Neither runledger nor runledger_dashboard is installed in every dev
# environment that runs these tests -- mirror python/tests' own raw
# sys.path insertion rather than requiring `pip install -e` first.
sys.path.insert(0, os.path.join(_HERE, "..", "..", "python"))
sys.path.insert(0, os.path.join(_HERE, ".."))

import runledger  # noqa: E402
from runledger_dashboard import (  # noqa: E402
    diff_cell,
    group_runs,
    list_projects,
    pair_diff,
    ranked_groups,
    widest_spread,
)


def _run_row(run_id, project="demo", **extra):
    row = {"run_id": run_id, "project": project}
    row.update(extra)
    return row


class _Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def _json(self, code, body):
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps(body).encode("utf-8"))

    def do_GET(self):
        parsed = urllib.parse.urlparse(self.path)
        query = dict(urllib.parse.parse_qsl(parsed.query))
        self.server.requests.append({"path": parsed.path, "query": query})

        if parsed.path == "/v1/runs":
            self._json(
                200,
                {
                    "runs": [
                        _run_row("run-a", project="demo", fingerprint="fp1"),
                        _run_row("run-b", project="demo", fingerprint="fp1"),
                        _run_row("run-c", project="other", fingerprint="fp2"),
                    ],
                    "count": 3,
                    "limit": 5000,
                },
            )
            return

        if parsed.path.startswith("/v1/runs/"):
            run_id = urllib.parse.unquote(parsed.path[len("/v1/runs/") :])
            if run_id == "missing":
                self._json(404, {"error": "run not found"})
            else:
                self._json(200, _run_row(run_id, metrics={"loss": 0.5}))
            return

        if parsed.path == "/v1/fingerprints":
            self._json(
                200,
                {
                    "groups": [
                        {
                            "fingerprint": "fp1",
                            "run_ids": ["run-a", "run-b"],
                            "count": 2,
                            "no_repeats": False,
                            "metrics": {
                                "loss": {
                                    "count": 2,
                                    "min": 0.4,
                                    "max": 0.6,
                                    "mean": 0.5,
                                    "stddev": 0.1,
                                },
                                # count == 1: not a candidate for widest_spread.
                                "lonely": {
                                    "count": 1,
                                    "min": 1.0,
                                    "max": 1.0,
                                    "mean": 1.0,
                                    "stddev": 0.0,
                                },
                                # mean == 0: would divide by zero, must be skipped.
                                "zeroed": {
                                    "count": 2,
                                    "min": -1.0,
                                    "max": 1.0,
                                    "mean": 0.0,
                                    "stddev": 1.0,
                                },
                            },
                        }
                    ],
                    "count": 1,
                },
            )
            return

        if parsed.path == "/v1/comparisons":
            a, b = query.get("a"), query.get("b")
            self._json(
                200,
                {
                    "a": a,
                    "b": b,
                    "same_experiment": True,
                    "fields": [{"name": "metrics.loss", "kind": "metric", "a": "0.4", "b": "0.6"}],
                    "unattributable": True,
                },
            )
            return

        self._json(500, {"error": "unexpected path"})


class _FakeLedger:
    def __enter__(self):
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), _Handler)
        self.server.requests = []
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.addr = f"http://127.0.0.1:{self.server.server_port}"
        return self

    def __exit__(self, *exc_info):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)

    @property
    def requests(self):
        return self.server.requests


class ListProjectsTests(unittest.TestCase):
    def test_returns_sorted_distinct_projects(self):
        with _FakeLedger() as led:
            got = list_projects(server=led.addr)
        self.assertEqual(got, ["demo", "other"])

    def test_scan_limit_is_forwarded(self):
        with _FakeLedger() as led:
            list_projects(server=led.addr, scan_limit=3)
        self.assertEqual(led.requests[0]["query"]["limit"], "3")


class RankedGroupsTests(unittest.TestCase):
    def test_passes_through_the_spread_response(self):
        with _FakeLedger() as led:
            got = ranked_groups(server=led.addr)
        self.assertEqual(got[0]["fingerprint"], "fp1")


class WidestSpreadTests(unittest.TestCase):
    def test_picks_the_highest_coefficient_of_variation(self):
        group = {
            "metrics": {
                "loss": {"count": 2, "mean": 0.5, "stddev": 0.1},
                "acc": {"count": 2, "mean": 10.0, "stddev": 5.0},
            }
        }
        got = widest_spread(group)
        self.assertEqual(got["metric"], "acc")

    def test_skips_a_zero_mean_metric(self):
        group = {"metrics": {"z": {"count": 2, "mean": 0.0, "stddev": 1.0}}}
        self.assertIsNone(widest_spread(group))

    def test_skips_a_metric_only_one_run_reported(self):
        group = {"metrics": {"lonely": {"count": 1, "mean": 1.0, "stddev": 0.0}}}
        self.assertIsNone(widest_spread(group))

    def test_no_repeats_group_has_no_metrics_to_rank(self):
        self.assertIsNone(widest_spread({"no_repeats": True}))

    def test_real_group_from_the_fake_ledger_skips_lonely_and_zeroed(self):
        with _FakeLedger() as led:
            group = ranked_groups(server=led.addr)[0]
        got = widest_spread(group)
        self.assertEqual(got["metric"], "loss")


class GroupRunsTests(unittest.TestCase):
    def test_fetches_every_run_id_in_the_group(self):
        with _FakeLedger() as led:
            got = group_runs({"run_ids": ["run-a", "run-b"]}, server=led.addr)
        self.assertEqual([r["run_id"] for r in got], ["run-a", "run-b"])

    def test_empty_group_fetches_nothing(self):
        with _FakeLedger() as led:
            got = group_runs({"run_ids": []}, server=led.addr)
        self.assertEqual(got, [])
        self.assertEqual(led.requests, [])


class PairDiffTests(unittest.TestCase):
    def test_returns_the_comparison(self):
        with _FakeLedger() as led:
            got = pair_diff("run-a", "run-b", server=led.addr)
        self.assertTrue(got["unattributable"])
        self.assertEqual(got["fields"][0]["name"], "metrics.loss")

    def test_accepts_run_dicts(self):
        with _FakeLedger() as led:
            pair_diff({"run_id": "run-a"}, {"run_id": "run-b"}, server=led.addr)
        self.assertEqual(led.requests[0]["query"], {"a": "run-a", "b": "run-b"})


class ErrorsPropagateTests(unittest.TestCase):
    """Every function here must raise, matching runledger.read's own
    convention -- the data layer is not where "the ledger did not answer"
    gets papered over.
    """

    def test_unreachable_ledger_raises_from_every_function(self):
        addr = "http://127.0.0.1:1"
        with self.assertRaises(runledger.LedgerUnreachableError):
            list_projects(server=addr, timeout=2.0)
        with self.assertRaises(runledger.LedgerUnreachableError):
            ranked_groups(server=addr, timeout=2.0)
        with self.assertRaises(runledger.LedgerUnreachableError):
            group_runs({"run_ids": ["x"]}, server=addr, timeout=2.0)
        with self.assertRaises(runledger.LedgerUnreachableError):
            pair_diff("a", "b", server=addr, timeout=2.0)

    def test_unknown_run_raises_not_found(self):
        with _FakeLedger() as led:
            with self.assertRaises(runledger.RunNotFoundError):
                group_runs({"run_ids": ["missing"]}, server=led.addr)


if __name__ == "__main__":
    unittest.main()


class DiffCell(unittest.TestCase):
    """A field the run never recorded and one it recorded as empty are
    different claims, and must not render alike. See ADR 0011."""

    def test_absent_renders_as_an_em_dash(self):
        self.assertEqual(diff_cell(None), "—")

    def test_empty_renders_as_visible_quotes(self):
        self.assertEqual(diff_cell(""), '""')

    def test_absent_and_empty_do_not_collide(self):
        self.assertNotEqual(diff_cell(None), diff_cell(""))

    def test_a_value_passes_through(self):
        self.assertEqual(diff_cell("cuda"), "cuda")
