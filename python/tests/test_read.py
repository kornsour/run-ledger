"""Tests for the read half of the client: runs(), run(), spread().

Like test_run.py, these run against a tiny in-process stand-in for the
server rather than needing Go, a build, or a live process. The fake
reproduces the parts of the read contract the client actually depends on:
keyset pagination via next_cursor, next_cursor's *absence* meaning the
traversal is done, /fingerprints returning null rather than [] when nothing
qualifies, and the {"error": ...} body shape.
"""

from __future__ import annotations

import json
import os
import sys
import threading
import unittest
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest import mock

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import runledger  # noqa: E402


def _run_row(n):
    return {"run_id": f"run-{n}", "project": "demo", "fingerprint": "fp1"}


class _Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):  # silence the default request logging
        pass

    def _json(self, code, body):
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps(body).encode("utf-8"))

    def do_GET(self):
        parsed = urllib.parse.urlparse(self.path)
        query = dict(urllib.parse.parse_qsl(parsed.query))
        self.server.requests.append(
            {
                "path": parsed.path,
                "query": query,
                "authorization": self.headers.get("Authorization"),
            }
        )

        if parsed.path == "/v1/runs":
            # Three rows total, served two at a time, so a caller that does
            # not follow next_cursor visibly comes up short.
            cursor = query.get("cursor", "")
            if not cursor:
                self._json(200, {"runs": [_run_row(0), _run_row(1)], "count": 2,
                                 "limit": 2, "next_cursor": "page2"})
            else:
                # No next_cursor: the traversal is done.
                self._json(200, {"runs": [_run_row(2)], "count": 1, "limit": 2})
            return

        if parsed.path.startswith("/v1/runs/"):
            run_id = urllib.parse.unquote(parsed.path[len("/v1/runs/"):])
            if run_id == "missing":
                self._json(404, {"error": "run not found"})
            else:
                self._json(200, {"run_id": run_id, "project": "demo"})
            return

        if parsed.path == "/v1/fingerprints":
            if query.get("project") == "empty":
                # The server sends null, not [], when nothing qualifies.
                self._json(200, {"groups": None, "count": 0})
            else:
                self._json(200, {"groups": [{"fingerprint": "fp1", "count": 3}],
                                 "count": 1})
            return

        if parsed.path.startswith("/v1/fingerprints/"):
            fp = parsed.path[len("/v1/fingerprints/"):]
            if fp == "missing":
                self._json(404, {"error": 'no run recorded with fingerprint "missing"'})
            else:
                self._json(200, {"fingerprint": fp, "count": 1, "no_repeats": True})
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


class RunsTests(unittest.TestCase):
    def test_follows_next_cursor_until_it_is_absent(self):
        with _FakeLedger() as led:
            got = runledger.runs(project="demo", server=led.addr)
        self.assertEqual([r["run_id"] for r in got], ["run-0", "run-1", "run-2"])
        self.assertEqual(len(led.requests), 2, "should have walked exactly two pages")
        self.assertEqual(led.requests[1]["query"]["cursor"], "page2")

    def test_limit_bounds_the_walk(self):
        with _FakeLedger() as led:
            got = runledger.runs(server=led.addr, limit=2)
        self.assertEqual(len(got), 2)
        self.assertEqual(len(led.requests), 1, "limit was satisfied by the first page")

    def test_empty_filters_are_not_sent(self):
        # The server treats a zero-valued filter as "do not filter", so
        # sending ?project= is at best noise.
        with _FakeLedger() as led:
            runledger.runs(project="demo", server=led.addr, limit=1)
        query = led.requests[0]["query"]
        self.assertIn("project", query)
        for absent in ("git_commit", "fingerprint", "status", "device", "cursor"):
            self.assertNotIn(absent, query)

    def test_token_comes_from_the_environment_only(self):
        with _FakeLedger() as led:
            with mock.patch.dict(os.environ, {"RUNLEDGER_TOKEN": "sekrit"}):
                runledger.runs(server=led.addr, limit=1)
        self.assertEqual(led.requests[0]["authorization"], "Bearer sekrit")

    def test_no_token_means_no_authorization_header(self):
        with _FakeLedger() as led:
            with mock.patch.dict(os.environ, {}, clear=True):
                runledger.runs(server=led.addr, limit=1)
        self.assertIsNone(led.requests[0]["authorization"])

    def test_server_defaults_to_the_env_var(self):
        with _FakeLedger() as led:
            with mock.patch.dict(os.environ, {"RUNLEDGER_ADDR": led.addr}):
                got = runledger.runs(limit=1)
        self.assertEqual(len(got), 1)


class RunTests(unittest.TestCase):
    def test_returns_the_run(self):
        with _FakeLedger() as led:
            got = runledger.run("run-7", server=led.addr)
        self.assertEqual(got["run_id"], "run-7")

    def test_missing_run_raises_run_not_found(self):
        with _FakeLedger() as led:
            with self.assertRaises(runledger.RunNotFoundError) as ctx:
                runledger.run("missing", server=led.addr)
        # The server's own message survives, rather than being flattened.
        self.assertIn("run not found", str(ctx.exception))

    def test_run_id_is_url_escaped(self):
        with _FakeLedger() as led:
            runledger.run("a/b?c", server=led.addr)
        self.assertEqual(led.requests[0]["path"], "/v1/runs/a%2Fb%3Fc")


class SpreadTests(unittest.TestCase):
    def test_lists_groups(self):
        with _FakeLedger() as led:
            got = runledger.spread(project="demo", server=led.addr)
        self.assertEqual(got[0]["fingerprint"], "fp1")

    def test_null_groups_becomes_an_empty_list(self):
        with _FakeLedger() as led:
            got = runledger.spread(project="empty", server=led.addr)
        self.assertEqual(got, [], "the server's null must not reach the caller")

    def test_one_fingerprint_returns_a_single_group(self):
        with _FakeLedger() as led:
            got = runledger.spread(fingerprint="fp9", server=led.addr)
        self.assertEqual(len(got), 1)
        self.assertTrue(got[0]["no_repeats"])
        self.assertEqual(led.requests[0]["path"], "/v1/fingerprints/fp9")

    def test_unknown_fingerprint_raises(self):
        with _FakeLedger() as led:
            with self.assertRaises(runledger.RunNotFoundError):
                runledger.spread(fingerprint="missing", server=led.addr)


class ReadsRaiseRatherThanDegradeTests(unittest.TestCase):
    """The write path spools and warns; the read path must not.

    Silently returning [] when the ledger is down answers "how did my
    experiments do?" with "they didn't", which is worse than an exception.
    """

    def test_unreachable_server_raises(self):
        # Port 1 on loopback: nothing is listening.
        with self.assertRaises(runledger.LedgerUnreachableError):
            runledger.runs(server="http://127.0.0.1:1", timeout=2.0)

    def test_unreachable_error_is_a_ledger_error(self):
        with self.assertRaises(runledger.LedgerError):
            runledger.runs(server="http://127.0.0.1:1", timeout=2.0)

    def test_reads_do_not_warn(self):
        import warnings

        with warnings.catch_warnings(record=True) as caught:
            warnings.simplefilter("always")
            with self.assertRaises(runledger.LedgerUnreachableError):
                runledger.runs(server="http://127.0.0.1:1", timeout=2.0)
        self.assertEqual([w for w in caught], [], "a read must raise, not warn")


if __name__ == "__main__":
    unittest.main()
