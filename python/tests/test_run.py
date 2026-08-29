"""Tests for runledger.Run.

Runs against a tiny in-process HTTP server standing in for the real
`runledger` server, so these don't need Go, a build, or a live process --
just enough of POST /runs and PATCH /runs/{id}'s contract (echo
run_id/fingerprint on success) to exercise the client honestly.
"""

from __future__ import annotations

import dataclasses
import inspect
import json
import os
import shutil
import signal
import sys
import tempfile
import threading
import unittest
import warnings
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest import mock

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import runledger  # noqa: E402
from runledger import _run as run_module  # noqa: E402


class _Handler(BaseHTTPRequestHandler):
    """Accepts both halves of the two-phase write: POST /runs (start) and
    PATCH /runs/{id} (finish). Always succeeds, echoing a fixed run_id/
    fingerprint the same way for either verb.
    """

    def log_message(self, *args):  # silence the default request logging
        pass

    def _read_json(self):
        length = int(self.headers.get("Content-Length", 0))
        if not length:
            return {}
        return json.loads(self.rfile.read(length).decode("utf-8"))

    def _record(self, method):
        self.server.received.append(
            {
                "method": method,
                "path": self.path,
                "payload": self._read_json(),
                "authorization": self.headers.get("Authorization"),
            }
        )

    def _respond(self, code, out):
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps(out).encode("utf-8"))

    def do_POST(self):
        self._record("POST")
        self._respond(201, {"run_id": "fake-run-id", "fingerprint": "fake-fingerprint"})

    def do_PATCH(self):
        self._record("PATCH")
        run_id = self.path.rsplit("/", 1)[-1]
        self._respond(200, {"run_id": run_id, "fingerprint": "fake-fingerprint"})


class _FakeLedger:
    """A minimal stand-in for POST /runs and PATCH /runs/{id}, on localhost."""

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


class PublicSignatureTests(unittest.TestCase):
    """Run.start() is the package's entire public entry point.

    Its parameters are spelled out rather than swallowed by **kwargs so an
    editor, a type checker, and help() can all see them. That means the
    defaults exist in two places -- the signature and the dataclass field --
    so these tests fail if the two ever drift.
    """

    def test_start_exposes_every_caller_settable_field(self):
        sig = inspect.signature(runledger.Run.start)
        exposed = set(sig.parameters) - {"cls"}
        settable = {
            f.name
            for f in dataclasses.fields(runledger.Run)
            if f.init and not f.name.startswith("_")
        }
        self.assertEqual(
            exposed,
            settable,
            "Run.start() and the Run dataclass disagree about what a caller may set",
        )

    def test_start_defaults_match_the_dataclass(self):
        params = inspect.signature(runledger.Run.start).parameters
        for f in dataclasses.fields(runledger.Run):
            if not f.init or f.name.startswith("_"):
                continue
            if f.default is dataclasses.MISSING:
                continue  # required, or a default_factory checked below
            self.assertEqual(
                params[f.name].default,
                f.default,
                f"default for {f.name}= drifted between start() and the dataclass",
            )

    def test_factory_defaults_are_reachable_through_start(self):
        # params and server carry default_factory values, which cannot be
        # repeated literally in the signature. start() forwards a sentinel
        # instead; these assert the real defaults still arrive.
        with mock.patch.dict(os.environ, {"RUNLEDGER_ADDR": "http://ledger.example"}):
            run = runledger.Run(project="p")
        self.assertEqual(run.server, "http://ledger.example")
        self.assertEqual(runledger.Run(project="p").params, {})

    def test_package_ships_a_py_typed_marker(self):
        # PEP 561: without this file, a consumer's type checker ignores every
        # annotation in the package.
        marker = os.path.join(os.path.dirname(runledger.__file__), "py.typed")
        self.assertTrue(os.path.exists(marker), "runledger/py.typed is missing")


class SpoolPathTests(unittest.TestCase):
    """spool_path is resolved when the file is written, not when the module loads.

    Expanding at import time baked the building machine's home into the
    default -- and left a caller-supplied "~/..." unexpanded, since open()
    does not expand it either. That wrote to a directory literally named "~"
    in the process's cwd while reporting spooled=True, in the one code path
    that only runs when the ledger is already down.
    """

    def test_default_is_left_unexpanded(self):
        self.assertTrue(
            run_module.DEFAULT_SPOOL_PATH.startswith("~"),
            "the default must stay literal so it documents and travels correctly",
        )

    def test_tilde_is_expanded_at_write_time(self):
        home = tempfile.mkdtemp()
        cwd = os.getcwd()
        work = tempfile.mkdtemp()
        try:
            os.chdir(work)
            with mock.patch.dict(os.environ, {"HOME": home}):
                run = runledger.Run(
                    project="p",
                    server="http://127.0.0.1:1",
                    spool_path=os.path.join("~", "runs.jsonl"),
                )
                with warnings.catch_warnings():
                    warnings.simplefilter("ignore")
                    run._spool({"project": "p"})
            self.assertTrue(run.spooled)
            self.assertTrue(
                os.path.exists(os.path.join(home, "runs.jsonl")),
                "record did not land in the home directory it was addressed to",
            )
            self.assertFalse(
                os.path.exists("~"),
                'a directory literally named "~" was created in the cwd',
            )
        finally:
            os.chdir(cwd)
            shutil.rmtree(home, ignore_errors=True)
            shutil.rmtree(work, ignore_errors=True)

    def test_warning_names_the_resolved_path(self):
        # Unreachable for the whole run: both the start POST and the
        # finish-fallback POST fail, and only the second one spools -- see
        # RunDegradesGracefullyTests below for that split in detail. Either
        # warning naming the resolved path satisfies this test.
        home = tempfile.mkdtemp()
        try:
            with mock.patch.dict(os.environ, {"HOME": home}):
                with _clean_git():
                    with warnings.catch_warnings(record=True) as caught:
                        warnings.simplefilter("always")
                        with runledger.Run.start(
                            project="p",
                            server="http://127.0.0.1:1",
                            spool_path=os.path.join("~", "runs.jsonl"),
                            timeout=2.0,
                        ):
                            pass
            messages = [str(w.message) for w in caught]
            self.assertTrue(
                any(home in m for m in messages),
                f"warning should name a path the user can open, got {messages}",
            )
        finally:
            shutil.rmtree(home, ignore_errors=True)

    def test_absolute_paths_are_untouched(self):
        with tempfile.TemporaryDirectory() as tmp:
            target = os.path.join(tmp, "nested", "runs.jsonl")
            run = runledger.Run(
                project="p", server="http://127.0.0.1:1", spool_path=target
            )
            self.assertEqual(run.resolved_spool_path(), target)
            with warnings.catch_warnings():
                warnings.simplefilter("ignore")
                run._spool({"project": "p"})
            self.assertTrue(os.path.exists(target))


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
        self.assertEqual(len(ledger.received), 2, "a start POST and a finish PATCH")
        start_call = ledger.received[0]
        self.assertEqual(start_call["method"], "POST")
        self.assertEqual(start_call["payload"]["config_hash"], "cfg1")
        self.assertTrue(start_call["payload"]["git_dirty"])


class RunRecordingTests(unittest.TestCase):
    """Run.start() now writes the ledger twice: POST /runs with status
    `running` the moment the with-block is entered, and PATCH /runs/{id}
    with the terminal status and final metrics when it ends.
    """

    def test_start_posts_running_then_finish_patches_terminal_status(self):
        with _clean_git(), _FakeLedger() as ledger:
            with runledger.Run.start(
                project="demo", seed=1, params={"lr": 0.001}, server=ledger.addr
            ) as run:
                # run_id/fingerprint are populated from the start-time POST,
                # not only once the run finishes.
                self.assertEqual(run.run_id, "fake-run-id")
                self.assertEqual(run.fingerprint, "fake-fingerprint")
                run.log_metric("loss", 0.9)
                run.log_metric("loss", 0.5)  # overwrites
                run.log_metric("acc", 0.8)

            self.assertEqual(run.status, "succeeded")
            self.assertFalse(run.spooled)

        self.assertEqual(len(ledger.received), 2, "one POST at start, one PATCH at finish")
        start_call, finish_call = ledger.received

        self.assertEqual(start_call["method"], "POST")
        self.assertEqual(start_call["path"], "/v1/runs")
        self.assertEqual(start_call["payload"]["project"], "demo")
        self.assertEqual(start_call["payload"]["seed"], 1)
        self.assertEqual(start_call["payload"]["params"], {"lr": "0.001"})
        self.assertEqual(start_call["payload"]["status"], "running")
        self.assertIn("git_commit", start_call["payload"])
        self.assertIn("started_at", start_call["payload"])
        self.assertNotIn("metrics", start_call["payload"])
        self.assertNotIn("ended_at", start_call["payload"])

        self.assertEqual(finish_call["method"], "PATCH")
        self.assertEqual(finish_call["path"], "/v1/runs/fake-run-id")
        self.assertEqual(finish_call["payload"]["status"], "succeeded")
        self.assertEqual(finish_call["payload"]["metrics"], {"loss": 0.5, "acc": 0.8})
        self.assertIn("ended_at", finish_call["payload"])
        self.assertNotIn(
            "project", finish_call["payload"], "PATCH only carries what changed"
        )

    def test_failure_records_failed_with_partial_metrics_and_reraises(self):
        with _clean_git(), _FakeLedger() as ledger:
            with self.assertRaises(RuntimeError):
                with runledger.Run.start(project="demo", server=ledger.addr) as run:
                    run.log_metric("loss", 0.9)
                    raise RuntimeError("GPU fell over")

            self.assertEqual(run.status, "failed")

        self.assertEqual(len(ledger.received), 2)
        finish_call = ledger.received[1]
        self.assertEqual(finish_call["method"], "PATCH")
        self.assertEqual(finish_call["payload"]["status"], "failed")
        self.assertEqual(finish_call["payload"]["metrics"], {"loss": 0.9})

    def test_bearer_token_sent_when_set(self):
        with _clean_git(), _FakeLedger() as ledger:
            with mock.patch.dict(os.environ, {"RUNLEDGER_TOKEN": "s3cr3t"}):
                with runledger.Run.start(project="demo", server=ledger.addr):
                    pass
        self.assertTrue(ledger.received)
        for call in ledger.received:
            self.assertEqual(call["authorization"], "Bearer s3cr3t")

    def test_no_token_means_no_authorization_header(self):
        with _clean_git(), _FakeLedger() as ledger:
            env = dict(os.environ)
            env.pop("RUNLEDGER_TOKEN", None)
            with mock.patch.dict(os.environ, env, clear=True):
                with runledger.Run.start(project="demo", server=ledger.addr):
                    pass
        self.assertTrue(ledger.received)
        for call in ledger.received:
            self.assertIsNone(call["authorization"])


class RunDegradesGracefullyTests(unittest.TestCase):
    """Never let recording fail the training run -- for either write."""

    def setUp(self):
        self.tmpdir = tempfile.mkdtemp()
        self.spool_path = os.path.join(self.tmpdir, "nested", "spool.jsonl")

    def tearDown(self):
        shutil.rmtree(self.tmpdir, ignore_errors=True)

    def test_unreachable_server_warns_and_spools_instead_of_raising(self):
        # Nothing is listening on this port: connection should be refused
        # promptly rather than hang. Unreachable for the whole run, so both
        # the start POST and the finish-fallback POST fail -- and only the
        # second spools (see test_start_write_failure_does_not_spool below
        # for that distinction in isolation).
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
        self.assertIsNone(run.run_id, "the start write never landed either")
        self.assertTrue(
            any(issubclass(w.category, RuntimeWarning) for w in caught),
            "expected a RuntimeWarning about the unreachable ledger",
        )

        self.assertTrue(os.path.exists(self.spool_path))
        with open(self.spool_path, encoding="utf-8") as fh:
            lines = [json.loads(line) for line in fh if line.strip()]
        self.assertEqual(
            len(lines), 1, "the failed start write must not also spool a line"
        )
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

    def test_start_write_failure_does_not_raise_and_finish_recovers_if_ledger_returns(self):
        """The end-of-run write when the start-time write failed.

        Distinct from total unreachability above: here the ledger rejects
        only the `running` POST and accepts everything else, modelling a
        ledger that came back (or was merely flaky) by the time the run
        ended. `_finish()` must notice there is no run_id and fall back to
        one full POST -- not a PATCH against an id it never learned -- and
        that fallback succeeding means nothing gets spooled.
        """

        class _RejectRunningHandler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_POST(self):
                length = int(self.headers.get("Content-Length", 0))
                payload = json.loads(self.rfile.read(length).decode("utf-8")) if length else {}
                self.server.received.append(payload)
                if payload.get("status") == "running":
                    self.send_response(503)
                    self.send_header("Content-Type", "application/json")
                    self.end_headers()
                    self.wfile.write(b'{"error":"simulated start failure"}')
                    return
                self.send_response(201)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                out = {"run_id": "fallback-run-id", "fingerprint": "fake-fingerprint"}
                self.wfile.write(json.dumps(out).encode("utf-8"))

            def do_PATCH(self):
                # Must never be reached: with no run_id from a successful
                # start write, _finish() has to fall back to a full POST.
                self.send_response(500)
                self.end_headers()

        server = ThreadingHTTPServer(("127.0.0.1", 0), _RejectRunningHandler)
        server.received = []
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            addr = f"http://127.0.0.1:{server.server_port}"
            with _clean_git():
                with warnings.catch_warnings(record=True) as caught:
                    warnings.simplefilter("always")
                    with runledger.Run.start(
                        project="demo", server=addr, timeout=2.0
                    ) as run:
                        self.assertIsNone(run.run_id, "the start write failed")
                        run.log_metric("loss", 0.1)
                    # no exception escaped the with-block

            self.assertEqual(run.status, "succeeded")
            self.assertEqual(run.run_id, "fallback-run-id")
            self.assertFalse(
                run.spooled, "the fallback POST succeeded; nothing to spool"
            )
            self.assertEqual(len(server.received), 2)
            self.assertEqual(server.received[0]["status"], "running")
            self.assertEqual(server.received[1]["status"], "succeeded")
            self.assertEqual(server.received[1]["metrics"], {"loss": 0.1})
            self.assertTrue(
                any(issubclass(w.category, RuntimeWarning) for w in caught),
                "the failed start write should still warn",
            )
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=5)

    def test_patch_failure_after_successful_start_spools_the_full_record(self):
        """The opposite split: the start POST succeeds (a run_id exists on
        the server), but the closing PATCH cannot be delivered. Replay only
        understands full POST /runs bodies (ADR 0008), so the fallback
        spools the complete record rather than the patch -- on replay this
        creates a second, terminal row instead of resurrecting the
        original run_id, leaving that one permanently `running` (ADR 0014).
        """

        class _PatchAlwaysFailsHandler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_POST(self):
                length = int(self.headers.get("Content-Length", 0))
                self.rfile.read(length)
                self.send_response(201)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                out = {"run_id": "orphan-run-id", "fingerprint": "fake-fingerprint"}
                self.wfile.write(json.dumps(out).encode("utf-8"))

            def do_PATCH(self):
                length = int(self.headers.get("Content-Length", 0))
                self.rfile.read(length)
                self.send_response(500)
                self.end_headers()

        server = ThreadingHTTPServer(("127.0.0.1", 0), _PatchAlwaysFailsHandler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            addr = f"http://127.0.0.1:{server.server_port}"
            with _clean_git():
                with warnings.catch_warnings():
                    warnings.simplefilter("ignore")
                    with runledger.Run.start(
                        project="demo",
                        server=addr,
                        timeout=2.0,
                        spool_path=self.spool_path,
                    ) as run:
                        self.assertEqual(run.run_id, "orphan-run-id")
                        run.log_metric("loss", 0.2)

            self.assertTrue(run.spooled)
            with open(self.spool_path, encoding="utf-8") as fh:
                lines = [json.loads(line) for line in fh if line.strip()]
            self.assertEqual(len(lines), 1)
            self.assertEqual(lines[0]["status"], "succeeded")
            self.assertEqual(lines[0]["metrics"], {"loss": 0.2})
            self.assertIn(
                "project", lines[0], "a full record was spooled, not a bare patch"
            )
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=5)


class SignalHandlingTests(unittest.TestCase):
    """SIGTERM is the signal a real scheduler sends before escalating to
    SIGKILL (Slurm's scancel, a Kubernetes eviction, LSF's bkill all work
    this way). This client catches it -- main thread only, since Python
    forbids installing a handler anywhere else -- records the active run,
    and then either chains to whatever handler was already there or lets
    the process die from the signal itself.
    """

    def setUp(self):
        self._original_sigterm = signal.getsignal(signal.SIGTERM)

    def tearDown(self):
        # Belt and suspenders: _finish()/_unregister_hooks should already
        # have restored this once every active run finishes, but a failed
        # assertion mid-test must not leak a handler into later tests.
        signal.signal(signal.SIGTERM, self._original_sigterm)

    def test_sigterm_records_the_run_as_failed(self):
        chained = []

        def previous_handler(signum, frame):
            # A benign stand-in for whatever was installed before
            # Run.start() ran -- it does not kill the process, so this
            # test process survives to make its assertions.
            chained.append(signum)

        signal.signal(signal.SIGTERM, previous_handler)

        with _clean_git(), _FakeLedger() as ledger:
            with runledger.Run.start(project="demo", server=ledger.addr) as run:
                run.log_metric("loss", 0.7)
                signal.raise_signal(signal.SIGTERM)
                # previous_handler doesn't terminate the process, so
                # execution resumes here -- but the run is already
                # finished, recorded by the signal handler above.
                self.assertEqual(run.status, "failed")

            # Falling off the end of the with-block normally would record
            # `succeeded` -- it must not overwrite what the signal already
            # recorded.
            self.assertEqual(run.status, "failed")

        self.assertEqual(
            chained, [signal.SIGTERM], "the previously-installed handler must still run"
        )
        self.assertEqual(len(ledger.received), 2)
        finish_call = ledger.received[1]
        self.assertEqual(finish_call["method"], "PATCH")
        self.assertEqual(finish_call["payload"]["status"], "failed")
        self.assertEqual(finish_call["payload"]["metrics"], {"loss": 0.7})

    def test_signal_handler_chains_to_a_preexisting_handler_instead_of_dropping_it(self):
        calls = []

        def previous_handler(signum, frame):
            calls.append(signum)

        signal.signal(signal.SIGTERM, previous_handler)

        # The ledger address doesn't matter for this test -- it's about
        # whether the pre-existing handler still runs, not what got
        # recorded -- so an unreachable address (fast to fail) keeps it
        # quick, with the resulting warning silenced.
        with _clean_git():
            with warnings.catch_warnings():
                warnings.simplefilter("ignore")
                with runledger.Run.start(
                    project="demo", server="http://127.0.0.1:1", timeout=1.0
                ):
                    signal.raise_signal(signal.SIGTERM)

        self.assertEqual(
            calls,
            [signal.SIGTERM],
            "chaining must invoke the pre-existing handler exactly once, not drop it",
        )
        self.assertIs(
            signal.getsignal(signal.SIGTERM),
            previous_handler,
            "the pre-existing handler must be restored once the run finishes",
        )

    def test_run_started_off_the_main_thread_does_not_install_a_handler(self):
        # signal.signal() raises ValueError anywhere but the main thread;
        # Run.start() must not let that escape into the caller's training
        # code, and must not leave the process's SIGTERM disposition
        # touched by a run it can't actually protect with one.
        original = signal.getsignal(signal.SIGTERM)
        errors = []

        def worker():
            try:
                with _clean_git(), _FakeLedger() as ledger:
                    with runledger.Run.start(project="demo", server=ledger.addr):
                        pass
            except Exception as exc:  # pragma: no cover - failure path
                errors.append(exc)

        thread = threading.Thread(target=worker)
        thread.start()
        thread.join(timeout=10)

        self.assertFalse(errors, f"Run.start() must not raise off the main thread: {errors}")
        self.assertIs(
            signal.getsignal(signal.SIGTERM),
            original,
            "no handler should leak from a non-main-thread run",
        )


if __name__ == "__main__":
    unittest.main()
