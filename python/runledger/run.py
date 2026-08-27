"""Run.start(): the in-process client for recording a run's lineage.

Design note -- why this makes exactly one HTTP call, at the end
-----------------------------------------------------------------
The server's ``POST /runs`` is idempotent only for a byte-identical
re-record of the same run id; recording the same id again with different
content (a different status, new metrics) is a conflict, not an update --
there is no PATCH yet (github.com/Lurking-Walrus/run-ledger issue #1). So a
"record running, then update to succeeded/failed" pair of calls would not
work against the real server today: the second call would fail every time.

Instead, ``Run`` buffers the run's status and metrics locally for its whole
lifetime and writes the ledger exactly once, in ``__exit__``, once the
outcome is known. That produces one accurate record per run against the
API as it exists now, and degrading to a spool file (see below) applies to
that one call.

Design note -- never let recording fail the training run
----------------------------------------------------------
A ledger that is down, slow, or unreachable must not turn into an exception
in the middle of (or at the end of) an expensive job. Any failure to reach
the server -- a connection error, a timeout, a non-2xx response -- is caught,
turned into a ``RuntimeWarning``, and the record is appended as one JSON line
to a local spool file instead. Nothing else about the run is affected.

This is distinct from ``UnreconstructibleRunError``: a run with no git
commit, or a dirty tree with no config hash, is refused immediately, in
``Run.start()``, before any training happens -- the same way ``rlctl record``
refuses it. That is a mistake in how the run was launched, not a ledger
outage, and failing fast is the useful behavior there.
"""

from __future__ import annotations

import contextlib
import dataclasses
import json
import os
import socket
import urllib.request
import warnings
from datetime import datetime, timezone
from typing import Any, Dict, Iterator, Optional

from . import _git, _provenance

DEFAULT_SERVER = "http://localhost:8080"
DEFAULT_TIMEOUT = 10.0
DEFAULT_SPOOL_PATH = os.path.join(
    os.path.expanduser("~"), ".runledger", "spool.jsonl"
)


class UnreconstructibleRunError(RuntimeError):
    """The run would not be reconstructible from what was captured.

    Raised by ``Run.start()`` itself, before any training happens -- the
    same refusal ``rlctl record`` makes locally (it calls ``Run.Validate``
    before ever sending a request), so a bad launch fails immediately
    instead of after an expensive job with nothing to show for it.
    """


class NoGitCommitError(UnreconstructibleRunError):
    """No git commit was found -- run inside a repository."""


class DirtyTreeError(UnreconstructibleRunError):
    """The working tree is dirty and no ``config_hash`` was given.

    Mirrors the server's own rule (ADR 0003): a dirty tree means the commit
    no longer describes the code that ran, so ``config_hash`` is the only
    remaining handle on what actually executed.
    """


def _utcnow() -> str:
    # Go's encoding/json unmarshals time.Time from RFC3339 and specifically
    # tolerates fractional seconds of any precision, so microseconds here
    # round-trip fine against the server's lineage.Run.
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%fZ")


@dataclasses.dataclass
class Run:
    """One experiment run's lineage, recorded once its outcome is known.

    Construct via :meth:`start`, not directly -- that is what captures git
    context and validates it before any training happens.
    """

    project: str
    seed: int = 0
    params: Dict[str, Any] = dataclasses.field(default_factory=dict)
    dataset: str = ""
    model: str = ""
    config_hash: str = ""
    server: str = dataclasses.field(
        default_factory=lambda: os.environ.get("RUNLEDGER_ADDR", DEFAULT_SERVER)
    )
    timeout: float = DEFAULT_TIMEOUT
    spool_path: str = DEFAULT_SPOOL_PATH

    run_id: Optional[str] = dataclasses.field(default=None, init=False)
    fingerprint: Optional[str] = dataclasses.field(default=None, init=False)
    status: str = dataclasses.field(default="running", init=False)
    spooled: bool = dataclasses.field(default=False, init=False)

    _metrics: Dict[str, float] = dataclasses.field(
        default_factory=dict, init=False, repr=False
    )
    _started_at: str = dataclasses.field(default="", init=False, repr=False)
    _git_commit: str = dataclasses.field(default="", init=False, repr=False)
    _git_dirty: bool = dataclasses.field(default=False, init=False, repr=False)

    @classmethod
    @contextlib.contextmanager
    def start(cls, project: str, **kwargs: Any) -> Iterator["Run"]:
        """Context manager: captures lineage now, records the outcome later.

        Raises :class:`UnreconstructibleRunError` immediately -- before the
        ``with`` body runs -- if the run would not be reconstructible. Any
        exception raised inside the ``with`` body is recorded as a
        ``failed`` run (with whatever metrics were logged before the
        exception) and then re-raised unchanged.
        """
        run = cls(project=project, **kwargs)
        run._enter()
        try:
            yield run
        except BaseException:
            run._finish("failed")
            raise
        else:
            run._finish("succeeded")

    def _enter(self) -> None:
        commit, dirty = _git.context()
        if not commit:
            raise NoGitCommitError(
                "no git commit found: run inside a repository, or the "
                "record would not be reconstructible"
            )
        if dirty and not self.config_hash:
            raise DirtyTreeError(
                "git_dirty is set but config_hash is empty: pass "
                "config_hash= so the run stays reconstructible"
            )
        self._git_commit = commit
        self._git_dirty = dirty
        self._started_at = _utcnow()

    def log_metric(self, name: str, value: float) -> None:
        """Records a measured metric. Overwrites a prior value for `name`."""
        self._metrics[name] = float(value)

    def _finish(self, status: str) -> None:
        self.status = status
        self._send(self._payload())

    def _payload(self) -> Dict[str, Any]:
        payload: Dict[str, Any] = {
            "project": self.project,
            "git_commit": self._git_commit,
            "git_dirty": self._git_dirty,
            "config_hash": self.config_hash,
            "dataset_version": self.dataset,
            "model_version": self.model,
            "seed": self.seed,
            "status": self.status,
            "started_at": self._started_at,
            "ended_at": _utcnow(),
            "host": socket.gethostname(),
            "device": _provenance.device_name(),
            "framework_version": _provenance.framework_version(),
        }
        if self.params:
            # The wire schema's Params is map[string]string (lineage.Run) --
            # the same shape rlctl's --param key=value produces. Coercing
            # here lets callers pass a plain dict of numbers/bools/whatever
            # without hand-stringifying each value themselves.
            payload["params"] = {k: str(v) for k, v in self.params.items()}
        if self._metrics:
            payload["metrics"] = dict(self._metrics)
        return payload

    def _send(self, payload: Dict[str, Any]) -> None:
        body = json.dumps(payload).encode("utf-8")
        req = urllib.request.Request(
            self.server.rstrip("/") + "/runs",
            data=body,
            method="POST",
            headers={"Content-Type": "application/json"},
        )
        token = os.environ.get("RUNLEDGER_TOKEN")
        if token:
            req.add_header("Authorization", f"Bearer {token}")
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:
                raw = resp.read()
            out = json.loads(raw.decode("utf-8")) if raw else {}
            self.run_id = out.get("run_id")
            self.fingerprint = out.get("fingerprint")
        except Exception as exc:
            # Recording must never fail the training run: a down, slow, or
            # unreachable ledger degrades to a warning and a local spool
            # file, whatever the cause (network error, timeout, bad
            # response, non-2xx status).
            warnings.warn(
                f"runledger: could not record run at {self.server} "
                f"({exc}); spooling to {self.spool_path} instead",
                RuntimeWarning,
                stacklevel=3,
            )
            self._spool(payload)

    def _spool(self, payload: Dict[str, Any]) -> None:
        try:
            spool_dir = os.path.dirname(self.spool_path)
            if spool_dir:
                os.makedirs(spool_dir, exist_ok=True)
            with open(self.spool_path, "a", encoding="utf-8") as fh:
                fh.write(json.dumps(payload) + "\n")
            self.spooled = True
        except OSError as exc:
            # Even the spool write is best-effort: this must not raise into
            # the caller either.
            warnings.warn(
                f"runledger: could not spool run locally either ({exc}); "
                "this run's lineage was not recorded anywhere",
                RuntimeWarning,
                stacklevel=3,
            )
