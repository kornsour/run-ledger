"""Tests for runledger.Run.

Runs against a tiny in-process HTTP server standing in for the real
`runledger` server, so these don't need Go, a build, or a live process --
just enough of POST /runs's contract (echo run_id/fingerprint on success) to
exercise the client honestly.
"""

from __future__ import annotations

import json
import os
import shutil
import sys
import tempfile
import threading
import unittest
import warnings
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest import mock

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import runledger  # noqa: E402
from runledger import run as run_module  # noqa: E402


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
        self.send_response(201)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        out = {"run_id": "fake-run-id", "fingerprint": "fake-fingerprint"}
        self.wfile.write(json.dumps(out).encode("utf-8"))


class _FakeLedger:
    """A minimal stand-in for POST /runs, listening on localhost."""

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


def _clean_git(commit="abcdef1234567890", dirty=False):
    return mock.patch.object(run_module._git, "context", return_value=(commit, dirty))


class RunStartValidationTests(unittest.TestCase):
    def test_no_git_commit_raises_before_body_runs(self):
        entered = False
        with _clean_git(commit="", dirty=False):
            with self.assertRaises(runledger.NoGitCommitError):
                with runledger.Run.start(project="demo"):
                    entered = True
        self.assertFalse(entered, "the with-body must not run when validation fails")

    def test_dirty_tree_without_config_hash_raises(self):
        with _clean_git(commit="abc123", dirty=True):
            with self.assertRaises(runledger.DirtyTreeError):
                with runledger.Run.start(project="demo"):
                    pass

    def test_dirty_tree_with_config_hash_is_accepted(self):
        with _clean_git(commit="abc123", dirty=True), _FakeLedger() as ledger:
            with runledger.Run.start(
                project="demo", config_hash="cfg1", server=ledger.addr
            ):
                pass
        self.assertEqual(len(ledger.received), 1)
        self.assertEqual(ledger.received[0]["payload"]["config_hash"], "cfg1")
        self.assertTrue(ledger.received[0]["payload"]["git_dirty"])


class RunRecordingTests(unittest.TestCase):
    def test_success_records_once_with_final_metrics(self):
        with _clean_git(), _FakeLedger() as ledger:
            with runledger.Run.start(
                project="demo", seed=1, params={"lr": 0.001}, server=ledger.addr
            ) as run:
                run.log_metric("loss", 0.9)
                run.log_metric("loss", 0.5)  # overwrites
                run.log_metric("acc", 0.8)

            self.assertEqual(run.status, "succeeded")
            self.assertEqual(run.run_id, "fake-run-id")
            self.assertEqual(run.fingerprint, "fake-fingerprint")
            self.assertFalse(run.spooled)

        self.assertEqual(len(ledger.received), 1, "exactly one HTTP call per run")
        payload = ledger.received[0]["payload"]
        self.assertEqual(payload["project"], "demo")
        self.assertEqual(payload["seed"], 1)
        self.assertEqual(payload["params"], {"lr": "0.001"})
        self.assertEqual(payload["status"], "succeeded")
        self.assertEqual(payload["metrics"], {"loss": 0.5, "acc": 0.8})
        self.assertIn("git_commit", payload)
        self.assertIn("started_at", payload)
        self.assertIn("ended_at", payload)

    def test_failure_records_failed_with_partial_metrics_and_reraises(self):
        with _clean_git(), _FakeLedger() as ledger:
            with self.assertRaises(RuntimeError):
                with runledger.Run.start(project="demo", server=ledger.addr) as run:
                    run.log_metric("loss", 0.9)
                    raise RuntimeError("GPU fell over")

            self.assertEqual(run.status, "failed")

        self.assertEqual(len(ledger.received), 1)
        payload = ledger.received[0]["payload"]
        self.assertEqual(payload["status"], "failed")
        self.assertEqual(payload["metrics"], {"loss": 0.9})

    def test_bearer_token_sent_when_set(self):
        with _clean_git(), _FakeLedger() as ledger:
            with mock.patch.dict(os.environ, {"RUNLEDGER_TOKEN": "s3cr3t"}):
                with runledger.Run.start(project="demo", server=ledger.addr):
                    pass
        self.assertEqual(ledger.received[0]["authorization"], "Bearer s3cr3t")

    def test_no_token_means_no_authorization_header(self):
        with _clean_git(), _FakeLedger() as ledger:
            env = dict(os.environ)
            env.pop("RUNLEDGER_TOKEN", None)
            with mock.patch.dict(os.environ, env, clear=True):
                with runledger.Run.start(project="demo", server=ledger.addr):
                    pass
        self.assertIsNone(ledger.received[0]["authorization"])


class RunDegradesGracefullyTests(unittest.TestCase):
    """Never let recording fail the training run."""

    def setUp(self):
        self.tmpdir = tempfile.mkdtemp()
        self.spool_path = os.path.join(self.tmpdir, "nested", "spool.jsonl")

    def tearDown(self):
        shutil.rmtree(self.tmpdir, ignore_errors=True)

    def test_unreachable_server_warns_and_spools_instead_of_raising(self):
        # Nothing is listening on this port: connection should be refused
        # promptly rather than hang.
        unreachable = "http://127.0.0.1:1"
        with _clean_git():
            with warnings.catch_warnings(record=True) as caught:
                warnings.simplefilter("always")
                with runledger.Run.start(
                    project="demo",
                    server=unreachable,
                    timeout=2.0,
                    spool_path=self.spool_path,
                ) as run:
                    run.log_metric("loss", 0.42)
                # no exception escaped the with-block

        self.assertTrue(run.spooled)
        self.assertIsNone(run.run_id)
        self.assertTrue(
            any(issubclass(w.category, RuntimeWarning) for w in caught),
            "expected a RuntimeWarning about the unreachable ledger",
        )

        self.assertTrue(os.path.exists(self.spool_path))
        with open(self.spool_path, encoding="utf-8") as fh:
            lines = [json.loads(line) for line in fh if line.strip()]
        self.assertEqual(len(lines), 1)
        self.assertEqual(lines[0]["project"], "demo")
        self.assertEqual(lines[0]["status"], "succeeded")
        self.assertEqual(lines[0]["metrics"], {"loss": 0.42})

    def test_server_error_response_also_spools_instead_of_raising(self):
        class _FailingHandler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_POST(self):
                length = int(self.headers.get("Content-Length", 0))
                self.rfile.read(length)
                self.send_response(500)
                self.end_headers()
                self.wfile.write(b'{"error":"boom"}')

        server = ThreadingHTTPServer(("127.0.0.1", 0), _FailingHandler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            addr = f"http://127.0.0.1:{server.server_port}"
            with _clean_git():
                with warnings.catch_warnings(record=True):
                    warnings.simplefilter("always")
                    with runledger.Run.start(
                        project="demo", server=addr, spool_path=self.spool_path
                    ) as run:
                        pass
            self.assertTrue(run.spooled)
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=5)


if __name__ == "__main__":
    unittest.main()
