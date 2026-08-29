"""Tests for runledger.replay_spool().

Like test_run.py, these run against a tiny in-process stand-in for
POST /runs rather than needing Go, a build, or a live process. The fake
server's response for each spooled line is driven by a ``_reply`` field the
tests embed in the payload -- opaque to the real server, but enough for the
fake to answer "ok" / "400" / "409" / "500" on request without needing a
real ledger's validation rules.
"""

from __future__ import annotations

import io
import json
import os
import shutil
import sys
import tempfile
import threading
import unittest
import warnings
from contextlib import redirect_stdout, redirect_stderr
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest import mock

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import runledger  # noqa: E402
from runledger import replay as replay_module  # noqa: E402


def _line(project="demo", reply="ok"):
    return json.dumps({"project": project, "status": "succeeded", "_reply": reply})


class _Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):  # silence the default request logging
        pass

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(length)
        payload = json.loads(body.decode("utf-8"))
        self.server.received.append(
            {"payload": payload, "authorization": self.headers.get("Authorization")}
        )
        reply = payload.get("_reply", "ok")
        if reply == "ok":
            self.send_response(201)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps({"run_id": "r", "fingerprint": "fp"}).encode())
        elif reply == "400":
            self.send_response(400)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b'{"error":"bad request"}')
        elif reply == "409":
            self.send_response(409)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b'{"error":"already recorded with different content"}')
        elif reply == "500":
            self.send_response(500)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b'{"error":"boom"}')
        else:
            raise AssertionError(f"unknown _reply: {reply}")


class _FakeLedger:
    def __enter__(self):
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), _Handler)
        self.server.received = []
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.addr = f"http://127.0.0.1:{self.server.server_port}"
        return self

    def __exit__(self, *exc_info):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)

    @property
    def received(self):
        return self.server.received


class ReplaySpoolTests(unittest.TestCase):
    def setUp(self):
        self.tmpdir = tempfile.mkdtemp()
        self.spool_path = os.path.join(self.tmpdir, "spool.jsonl")

    def tearDown(self):
        shutil.rmtree(self.tmpdir, ignore_errors=True)

    def _write_spool(self, *lines):
        with open(self.spool_path, "w", encoding="utf-8") as fh:
            for line in lines:
                fh.write(line + "\n")

    def _read_spool(self):
        if not os.path.exists(self.spool_path):
            return []
        with open(self.spool_path, encoding="utf-8") as fh:
            return [line for line in fh.read().splitlines() if line.strip()]

    def _rejected_path(self):
        return self.spool_path[: -len(".jsonl")] + ".rejected.jsonl"

    # -- no spool / dry run --------------------------------------------

    def test_missing_spool_returns_zero_result_without_a_request(self):
        with _FakeLedger() as led:
            result = runledger.replay_spool(self.spool_path, server=led.addr)
        self.assertEqual(result, runledger.ReplayResult(0, 0, 0))
        self.assertEqual(led.received, [])

    def test_empty_spool_returns_zero_result(self):
        self._write_spool()
        with _FakeLedger() as led:
            result = runledger.replay_spool(self.spool_path, server=led.addr)
        self.assertEqual(result, runledger.ReplayResult(0, 0, 0))

    def test_dry_run_reports_without_sending(self):
        self._write_spool(_line(), _line(), _line())
        with _FakeLedger() as led:
            result = runledger.replay_spool(self.spool_path, server=led.addr, dry_run=True)
        self.assertEqual(result, runledger.ReplayResult(sent=0, rejected=0, remaining=3))
        self.assertEqual(led.received, [], "dry_run must not contact the ledger")
        self.assertEqual(len(self._read_spool()), 3, "dry_run must not modify the spool")

    # -- successful replay ------------------------------------------------

    def test_successful_replay_sends_each_line_and_empties_the_spool(self):
        self._write_spool(_line(project="a"), _line(project="b"))
        with _FakeLedger() as led:
            result = runledger.replay_spool(self.spool_path, server=led.addr)
        self.assertEqual(result, runledger.ReplayResult(sent=2, rejected=0, remaining=0))
        self.assertEqual([r["payload"]["project"] for r in led.received], ["a", "b"])
        self.assertEqual(self._read_spool(), [])

    def test_bearer_token_sent_when_set(self):
        self._write_spool(_line())
        with _FakeLedger() as led:
            with mock.patch.dict(os.environ, {"RUNLEDGER_TOKEN": "sekrit"}):
                runledger.replay_spool(self.spool_path, server=led.addr)
        self.assertEqual(led.received[0]["authorization"], "Bearer sekrit")

    def test_no_token_means_no_authorization_header(self):
        self._write_spool(_line())
        with _FakeLedger() as led:
            with mock.patch.dict(os.environ, {}, clear=True):
                runledger.replay_spool(self.spool_path, server=led.addr)
        self.assertIsNone(led.received[0]["authorization"])

    def test_server_defaults_to_the_env_var(self):
        self._write_spool(_line())
        with _FakeLedger() as led:
            with mock.patch.dict(os.environ, {"RUNLEDGER_ADDR": led.addr}):
                result = runledger.replay_spool(self.spool_path)
        self.assertEqual(result.sent, 1)

    # -- permanent rejection: quarantined, not retried --------------------

    def test_400_is_quarantined_and_removed_from_the_spool(self):
        self._write_spool(_line(reply="400"), _line(reply="ok"))
        with _FakeLedger() as led:
            with warnings.catch_warnings(record=True) as caught:
                warnings.simplefilter("always")
                result = runledger.replay_spool(self.spool_path, server=led.addr)
        self.assertEqual(result, runledger.ReplayResult(sent=1, rejected=1, remaining=0))
        self.assertEqual(self._read_spool(), [])
        with open(self._rejected_path(), encoding="utf-8") as fh:
            quarantined = [json.loads(line) for line in fh if line.strip()]
        self.assertEqual(len(quarantined), 1)
        self.assertEqual(quarantined[0]["_reply"], "400")
        self.assertTrue(
            any(issubclass(w.category, RuntimeWarning) for w in caught),
            "expected a RuntimeWarning about the rejected record",
        )

    def test_409_is_also_quarantined(self):
        self._write_spool(_line(reply="409"))
        with _FakeLedger() as led:
            with warnings.catch_warnings(record=True):
                warnings.simplefilter("always")
                result = runledger.replay_spool(self.spool_path, server=led.addr)
        self.assertEqual(result, runledger.ReplayResult(sent=0, rejected=1, remaining=0))
        self.assertTrue(os.path.exists(self._rejected_path()))

    def test_rejected_records_do_not_stop_the_replay(self):
        self._write_spool(_line(reply="400"), _line(reply="ok"), _line(reply="400"))
        with _FakeLedger() as led:
            with warnings.catch_warnings(record=True):
                warnings.simplefilter("always")
                result = runledger.replay_spool(self.spool_path, server=led.addr)
        self.assertEqual(result, runledger.ReplayResult(sent=1, rejected=2, remaining=0))

    # -- unreachable ledger: raises, preserves progress --------------------

    def test_connection_failure_raises_and_keeps_remaining_lines(self):
        self._write_spool(_line(project="a"), _line(project="b"))
        with self.assertRaises(runledger.LedgerUnreachableError):
            runledger.replay_spool(self.spool_path, server="http://127.0.0.1:1", timeout=2.0)
        # Nothing was sent (the ledger was never reached), so both lines
        # stay in the spool for the next attempt.
        self.assertEqual(len(self._read_spool()), 2)

    def test_500_raises_and_preserves_already_sent_progress(self):
        self._write_spool(_line(reply="ok"), _line(reply="500"), _line(reply="ok"))
        with _FakeLedger() as led:
            with self.assertRaises(runledger.LedgerUnreachableError):
                runledger.replay_spool(self.spool_path, server=led.addr)
        # First line succeeded and must not be resent; the failing line and
        # the untried one after it stay in the spool.
        remaining = [json.loads(line) for line in self._read_spool()]
        self.assertEqual([r["_reply"] for r in remaining], ["500", "ok"])
        self.assertEqual(len(led.received), 2, "must not attempt the third line")

    def test_unreachable_error_is_a_ledger_error(self):
        self._write_spool(_line())
        with self.assertRaises(runledger.LedgerError):
            runledger.replay_spool(self.spool_path, server="http://127.0.0.1:1", timeout=2.0)

    # -- default path -------------------------------------------------------

    def test_default_path_matches_runs_default_spool_path(self):
        from runledger._run import DEFAULT_SPOOL_PATH

        self.assertEqual(
            replay_module._resolved_path(None), os.path.expanduser(DEFAULT_SPOOL_PATH)
        )

    def test_tilde_is_expanded(self):
        with mock.patch.dict(os.environ, {"HOME": self.tmpdir}):
            resolved = replay_module._resolved_path(os.path.join("~", "spool.jsonl"))
        self.assertEqual(resolved, os.path.join(self.tmpdir, "spool.jsonl"))


class RewritePreservesConcurrentAppendsTests(unittest.TestCase):
    """A training run can append to the spool while replay is in flight."""

    def setUp(self):
        self.tmpdir = tempfile.mkdtemp()
        self.spool_path = os.path.join(self.tmpdir, "spool.jsonl")

    def tearDown(self):
        shutil.rmtree(self.tmpdir, ignore_errors=True)

    def test_content_appended_after_the_read_offset_survives_a_rewrite(self):
        original = _line(project="a") + "\n"
        with open(self.spool_path, "w", encoding="utf-8") as fh:
            fh.write(original)
        original_size = len(original.encode("utf-8"))

        # Simulate a live Run.start() appending a new record after replay
        # already read the file but before it rewrites it.
        appended = _line(project="b") + "\n"
        with open(self.spool_path, "a", encoding="utf-8") as fh:
            fh.write(appended)

        replay_module._rewrite(self.spool_path, [], original_size=original_size)

        with open(self.spool_path, encoding="utf-8") as fh:
            lines = [json.loads(line) for line in fh if line.strip()]
        self.assertEqual([l["project"] for l in lines], ["b"])

    def test_remaining_lines_precede_a_concurrent_append(self):
        original = _line(project="a") + "\n"
        with open(self.spool_path, "w", encoding="utf-8") as fh:
            fh.write(original)
        original_size = len(original.encode("utf-8"))

        appended = _line(project="c") + "\n"
        with open(self.spool_path, "a", encoding="utf-8") as fh:
            fh.write(appended)

        replay_module._rewrite(self.spool_path, [_line(project="b")], original_size=original_size)

        with open(self.spool_path, encoding="utf-8") as fh:
            lines = [json.loads(line) for line in fh if line.strip()]
        self.assertEqual([l["project"] for l in lines], ["b", "c"])


class MainTests(unittest.TestCase):
    def setUp(self):
        self.tmpdir = tempfile.mkdtemp()
        self.spool_path = os.path.join(self.tmpdir, "spool.jsonl")

    def tearDown(self):
        shutil.rmtree(self.tmpdir, ignore_errors=True)

    def _write_spool(self, *lines):
        with open(self.spool_path, "w", encoding="utf-8") as fh:
            for line in lines:
                fh.write(line + "\n")

    def test_reports_a_summary_and_returns_zero(self):
        self._write_spool(_line(), _line())
        with _FakeLedger() as led:
            out = io.StringIO()
            with redirect_stdout(out):
                code = replay_module._main([self.spool_path, "--server", led.addr])
        self.assertEqual(code, 0)
        self.assertIn("sent 2", out.getvalue())

    def test_dry_run_reports_and_returns_zero(self):
        self._write_spool(_line())
        out = io.StringIO()
        with redirect_stdout(out):
            code = replay_module._main([self.spool_path, "--dry-run"])
        self.assertEqual(code, 0)
        self.assertIn("1 record", out.getvalue())

    def test_unreachable_ledger_prints_to_stderr_and_returns_one(self):
        self._write_spool(_line())
        err = io.StringIO()
        with redirect_stderr(err):
            code = replay_module._main(
                [self.spool_path, "--server", "http://127.0.0.1:1", "--timeout", "2.0"]
            )
        self.assertEqual(code, 1)
        self.assertIn("runledger:", err.getvalue())


if __name__ == "__main__":
    unittest.main()
