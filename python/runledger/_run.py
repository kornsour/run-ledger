"""Run.start(): the in-process client for recording a run's lineage.

Design note -- why this writes the ledger twice, not once
-----------------------------------------------------------
`Run.start()` `POST`s a `running` record the moment a run starts, and
`PATCH`es it to a terminal status (`succeeded`/`failed`) when the `with`
block ends -- the same two-step lifecycle `rlctl start` / `rlctl finish`
already use (`cmd/rlctl/main.go`), and exactly the `created -> running ->
{succeeded, failed, cancelled}` transition the server enforces
(`internal/store/store.go`'s `legalTransitions`).

That used to be one write, always, from `__exit__`, once the outcome was
known (ADR 0005). The reason was analytical, not mechanical: `/v1/fingerprints`
did not filter by status, so a `running` record's mid-training metric would
be counted as a repeat measurement, widen a fingerprint's spread, and could
rank it top of "reproduces worst" -- a false positive on the one claim this
ledger exists to make. That filter now exists (`terminalRuns` in
`internal/api/api.go`, added by #52), so a non-terminal run no longer
reaches `spread.Compute` at all. The reason to write once is gone.

Writing once had a real cost the whole time, though: `__exit__` never runs
for a `SIGKILL`, an OOM kill, or a scheduler escalating past its grace
period -- the single most common way a real training job dies -- so those
produced *zero* trace, not even a `failed` record. `SIGKILL` cannot be
caught by anything running in the process that receives it; the only way to
survive it is to already have a record on the server before it arrives.
That is what the start-time write buys, at the cost described below. See
ADR 0014 (supersedes ADR 0005) for the full reasoning.

Design note -- never let recording fail the training run
----------------------------------------------------------
A ledger that is down, slow, or unreachable must not turn into an exception
in the middle of (or at the end of) an expensive job. Any failure to reach
the server -- a connection error, a timeout, a non-2xx response -- is caught
and turned into a ``RuntimeWarning``. This now applies to *two* calls
instead of one, and they degrade differently:

- If the start-time ``POST`` fails, nothing is spooled. A ``running``
  record with nothing to ever follow it up would sit in the spool forever
  (replay only understands complete records, and a run that failed to
  start recording has no terminal status or final metrics to give it yet).
  Instead ``_finish()`` notices there is no ``run_id`` and falls back to a
  single full ``POST`` at the end -- exactly ADR 0005's original one-shot
  behaviour -- which spools on failure the same way it always did.
- If the start-time ``POST`` succeeds but the closing ``PATCH`` fails, the
  full record is spooled in its place. Replay (``replay.py``, ADR 0008)
  only knows how to resend a complete ``POST /runs`` body, not a partial
  patch, so on replay this creates a second, terminal row rather than
  resurrecting the original ``run_id`` -- leaving an orphaned ``running``
  row behind that never reaches a terminal status. That row is invisible
  to ``spread`` either way (``terminalRuns``, #52), and losing the run's
  final metrics would be worse than one stray non-terminal row.

This is distinct from ``UnreconstructibleRunError``: a run with no git
commit, or a dirty tree with no config hash, is refused immediately, in
``Run.start()``, before any training happens -- the same way ``rlctl record``
refuses it. That is a mistake in how the run was launched, not a ledger
outage, and failing fast is the useful behavior there.

Design note -- recovering from a kill that never reaches ``__exit__``
------------------------------------------------------------------------
``SIGKILL`` cannot be caught -- that is what the start-time ``running``
record above is for. ``SIGTERM`` can be, and is what almost every real
scheduler sends first: Slurm's ``scancel``, Kubernetes evicting a pod,
LSF's ``bkill`` all deliver ``SIGTERM`` and only escalate to ``SIGKILL``
after a grace period if the process is still alive. Catching it here
recovers the common case. An ``atexit`` hook backstops whatever the signal
handler doesn't cover -- a run started off the main thread (Python refuses
to let anything but the main thread install a signal handler), or a
handler further up the chain that calls ``sys.exit()`` instead of letting
the process actually die from the signal.

Two things the signal handler must not do:

1. **Swallow the signal.** A process a scheduler sent ``SIGTERM`` to must
   still die from ``SIGTERM`` -- not linger, and not exit cleanly with
   status 0. A shell script checking ``$?``, or a scheduler checking
   ``WIFSIGNALED``, may depend on that. So after recording, the handler
   either calls whatever handler was installed before ours, or -- if there
   wasn't one worth calling (``SIG_DFL``/``SIG_IGN``) -- restores the
   default disposition and re-sends the signal to this process. That is
   the standard idiom for "run cleanup code, then die exactly as if the
   handler had never been installed."
2. **Clobber a handler that was already there.** A training script that
   also traps ``SIGTERM`` for its own checkpointing would silently stop
   doing that the moment ``Run.start()`` ran, if this just called
   ``signal.signal()`` without saving what was already installed. Instead
   the previous disposition is saved and invoked -- last in, first served,
   the same as any other signal-chaining code.

Alternative considered and rejected: registering only via ``atexit``, with
no signal handler at all. ``atexit`` callbacks do not run when a process
dies from an uncaught signal -- only on ``sys.exit()``, falling off the end
of the program, or an uncaught exception unwinding normally -- which is
exactly the ``SIGTERM`` case. Recording would then depend on user code (or
some other library) happening to convert the signal into a clean exit
instead of letting it kill the process, which is not how any scheduler in
the wild actually behaves. A handler plus ``atexit`` recovers most real
kills; ``atexit`` alone would recover almost none of them.
"""

from __future__ import annotations

import atexit
import contextlib
import dataclasses
import json
import os
import signal
import socket
import threading
import urllib.parse
import urllib.request
import warnings
from datetime import datetime, timezone
from typing import Any, Dict, Iterator, List, Optional

from . import _git, _provenance
from .read import API_VERSION

DEFAULT_SERVER = "http://localhost:8080"
DEFAULT_TIMEOUT = 10.0
# Left unexpanded on purpose. Expanding at import time baked the building
# machine's home directory into the default, which then rendered as an
# absolute path in help(), IDE tooltips, and the published pdoc page -- and,
# worse, meant a caller-supplied "~/..." was never expanded at all, since
# open() does not do it either. Resolution happens in _spool(), at the moment
# of writing.
DEFAULT_SPOOL_PATH = os.path.join("~", ".runledger", "spool.jsonl")


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


# ---------------------------------------------------------------------------
# Signal + atexit bookkeeping.
#
# One handler is shared across every active Run rather than one per run:
# only one disposition can be installed for a given signal at a time, so
# per-run handlers would just overwrite each other. Runs are tracked by
# identity (`is`, not `==`) rather than relying on dataclass equality/
# hashing -- two runs constructed with identical arguments could otherwise
# compare equal and cause the wrong one to be dropped from the active list.
# ---------------------------------------------------------------------------

_HANDLED_SIGNALS = (signal.SIGTERM,)

_hooks_lock = threading.Lock()
_active_runs: List["Run"] = []
# The disposition each handled signal had before runledger installed its
# own -- SIG_DFL, SIG_IGN, or a caller's own handler. Populated the first
# time a Run enters on the main thread, and cleared again once the last
# active run finishes, so a long-lived process that starts and finishes
# many runs over time doesn't accumulate anything.
_previous_handlers: Dict[int, Any] = {}


def _signal_handler(signum: int, frame: Any) -> None:
    """Installed for every signal in ``_HANDLED_SIGNALS`` while at least one
    Run is active. Records every currently-active run as ``failed``, then
    chains to (or restores and re-raises) whatever was installed before --
    see the module docstring's signal-handling design note for why both
    steps matter.
    """
    previous = _previous_handlers.get(signum)
    for run in list(_active_runs):
        try:
            run._finish("failed")
        except Exception:
            # Everything _finish calls already degrades failures to a
            # warning; this is only a last line of defense so that one
            # run's recording trouble can't stop another active run from
            # being recorded, or stop the signal from reaching `previous`.
            pass
    if callable(previous):
        previous(signum, frame)
        return
    # No handler worth calling (SIG_DFL, SIG_IGN, or nothing installed):
    # restore the default disposition and re-send the signal to ourselves.
    # This -- not sys.exit() -- is what makes the process die *from the
    # signal*, the same way it would have if runledger had never installed
    # anything here.
    signal.signal(signum, signal.SIG_DFL)
    os.kill(os.getpid(), signum)


def _register_hooks(run: "Run") -> None:
    """Adds ``run`` to the active set and, on the main thread, installs the
    shared signal handler the first time it's needed. Always registers the
    ``atexit`` backstop, regardless of thread -- ``atexit.register`` itself
    has no main-thread restriction.
    """
    with _hooks_lock:
        _active_runs.append(run)
        if threading.current_thread() is threading.main_thread():
            for sig in _HANDLED_SIGNALS:
                if sig not in _previous_handlers:
                    _previous_handlers[sig] = signal.signal(sig, _signal_handler)
        # Off the main thread, signal.signal() would raise ValueError --
        # Python only allows installing handlers there. Such a run simply
        # has no SIGTERM coverage; the atexit hook below still applies.
    atexit.register(run._on_atexit)


def _unregister_hooks(run: "Run") -> None:
    """Reverses ``_register_hooks``: drops ``run`` from the active set,
    cancels its atexit callback, and -- once no run is left active --
    restores whatever signal dispositions were there before.
    """
    atexit.unregister(run._on_atexit)
    with _hooks_lock:
        _active_runs[:] = [r for r in _active_runs if r is not run]
        if _active_runs:
            return
        restored = []
        for sig, previous in _previous_handlers.items():
            try:
                signal.signal(sig, previous)
                restored.append(sig)
            except ValueError:
                # Not the main thread. Leave the handler installed -- it is
                # harmless with no active runs to record, and a future run
                # entering from the main thread will find this entry still
                # correct and try again when it finishes.
                pass
        for sig in restored:
            del _previous_handlers[sig]


@dataclasses.dataclass
class Run:
    """One experiment run's lineage, recorded once its outcome is known.

    Construct via :meth:`start`, not directly -- that is what captures git
    context and validates it before any training happens.
    """

    project: str
    seed: int = 0
    params: Dict[str, Any] = dataclasses.field(default_factory=dict)
    dataset_version: str = ""
    model_version: str = ""
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
    # Guards _finish() so it runs at most once, however it's triggered --
    # normal __exit__, an exception, the signal handler, or the atexit
    # backstop can all reach it, and only the first should count.
    _finished: bool = dataclasses.field(default=False, init=False, repr=False)

    @classmethod
    @contextlib.contextmanager
    def start(
        cls,
        project: str,
        *,
        seed: int = 0,
        params: Optional[Dict[str, Any]] = None,
        dataset_version: str = "",
        model_version: str = "",
        config_hash: str = "",
        server: Optional[str] = None,
        timeout: float = DEFAULT_TIMEOUT,
        spool_path: str = DEFAULT_SPOOL_PATH,
    ) -> Iterator["Run"]:
        """Context manager: records the run running now, and its outcome later.

        Raises :class:`UnreconstructibleRunError` immediately -- before the
        ``with`` body runs -- if the run would not be reconstructible. Any
        exception raised inside the ``with`` body is recorded as a
        ``failed`` run (with whatever metrics were logged before the
        exception) and then re-raised unchanged. A ``SIGTERM`` delivered
        while the ``with`` body is running is recorded the same way, from
        the main thread only -- see the module docstring.

        Every option is spelled out here rather than collected into
        ``**kwargs``: this is the package's entire public entry point, and a
        signature of ``(project, **kwargs)`` tells an editor, a type checker,
        and ``help()`` nothing about what a caller may actually pass. The
        defaults are duplicated from the dataclass fields below, which
        ``tests/test_run.py`` asserts stays true.

        Identity -- hashed into the run's fingerprint:

        :param project: Project name. Required.
        :param seed: Random seed.
        :param params: Hyperparameters. Values are coerced to strings on the
            wire, so a plain dict of numbers is fine.
        :param dataset_version: Dataset version identifier.
        :param model_version: Model version identifier.
        :param config_hash: Hash of the run's config. Required when the
            working tree is dirty -- see :class:`DirtyTreeError`.

        Client behaviour -- not recorded in the ledger:

        :param server: Ledger base URL. Defaults to ``$RUNLEDGER_ADDR``, or
            ``http://localhost:8080`` when that is unset.
        :param timeout: Per-request timeout in seconds.
        :param spool_path: Where to append the run record if the ledger
            cannot be reached.
        """
        run = cls(
            project=project,
            seed=seed,
            params=params if params is not None else {},
            dataset_version=dataset_version,
            model_version=model_version,
            config_hash=config_hash,
            # server's default is environment-dependent, so it stays on the
            # dataclass: forwarding None would override that default with None.
            **({} if server is None else {"server": server}),
            timeout=timeout,
            spool_path=spool_path,
        )
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
        # Hooks go up before the start-time write below, not after: this is
        # the earliest point a kill would otherwise leave zero trace, and
        # everything from here until _finish() runs must be recorded if the
        # process dies. Registering only after a validation failure (the
        # raises above) would leave a run that never entered the `with`
        # body registered forever, since start()'s try/finally hasn't begun
        # yet at that point -- so this must come after both checks succeed.
        _register_hooks(self)
        self._record_start()

    def log_metric(self, name: str, value: float) -> None:
        """Records a measured metric. Overwrites a prior value for `name`."""
        self._metrics[name] = float(value)

    # -- writing -----------------------------------------------------------

    def _identity_and_provenance(self) -> Dict[str, Any]:
        """Fields shared by the start-time POST and the fallback full POST
        at finish -- everything except status and the timestamps that
        depend on which one is being built.
        """
        payload: Dict[str, Any] = {
            "project": self.project,
            "git_commit": self._git_commit,
            "git_dirty": self._git_dirty,
            "config_hash": self.config_hash,
            "dataset_version": self.dataset_version,
            "model_version": self.model_version,
            "seed": self.seed,
            "host": socket.gethostname(),
            "device": _provenance.device_name(),
            "framework_version": _provenance.framework_version(),
        }
        if self.params:
            # The wire schema's Params is map[string]string (lineage.Run) --
            # the same shape rlctl's --param key=value produces. Coercing
            # here lets callers pass a plain dict of numbers/bools/whatever
            # without hand-stringifying each value themselves.
            #
            # str(v) is not made to match Go's canonical float spelling, and
            # deliberately isn't: two clients agreeing on one exact string
            # for every number is the fragile version of this problem, the
            # one that broke before (rlctl sends the literal "3e-4" a user
            # typed; str(3e-4) here gives "0.0003", a different string for
            # the same value). The server now normalizes any numeric-looking
            # param value before hashing it into the fingerprint (see
            # lineage.Run.Compute and ADR 0013), so this client only has to
            # produce *a* string that parses back to the right number, not
            # *the* string another client would have produced. Duplicating
            # Go's canonicalization here would just be a second
            # implementation of the same rule, with its own chance to drift
            # from the first.
            payload["params"] = {k: str(v) for k, v in self.params.items()}
        return payload

    def _start_payload(self) -> Dict[str, Any]:
        """The record POSTed the moment the run starts. No ``ended_at`` and
        no ``metrics``: neither exists yet, and sending them would claim
        measurements that have not happened.
        """
        payload = self._identity_and_provenance()
        payload["status"] = "running"
        payload["started_at"] = self._started_at
        return payload

    def _payload(self) -> Dict[str, Any]:
        """The complete, final record -- used as the fallback full POST at
        finish (when the start write never landed) and as what gets spooled
        on any finish-time failure, whichever path produced it.
        """
        payload = self._identity_and_provenance()
        payload["status"] = self.status
        payload["started_at"] = self._started_at
        payload["ended_at"] = _utcnow()
        if self._metrics:
            payload["metrics"] = dict(self._metrics)
        return payload

    def _finish_patch(self) -> Dict[str, Any]:
        """The PATCH body that closes out a run whose start-time POST
        already landed -- just what changed, not the identity fields
        (those can't change, and PATCH treats a mismatched one as a
        conflict rather than silently ignoring it).
        """
        patch: Dict[str, Any] = {"status": self.status, "ended_at": _utcnow()}
        if self._metrics:
            patch["metrics"] = dict(self._metrics)
        return patch

    def _record_start(self) -> None:
        """Best-effort POST of a `running` record so a SIGKILL/OOM kill
        still leaves a trace server-side (see the module docstring). Never
        spools on failure -- see `_finish` for why.
        """
        self._send(
            "POST",
            "/runs",
            self._start_payload(),
            warn="record the start of this run",
        )

    def _finish(self, status: str) -> None:
        if self._finished:
            # Reached twice: e.g. a SIGTERM the handler couldn't chain to a
            # process-killing disposition for, so the with-block resumed
            # and reached its own normal else-branch afterward. Whichever
            # got here first wins; the outcome already recorded (the kill)
            # is more accurate than the one that would overwrite it.
            return
        self._finished = True
        self.status = status
        try:
            if self.run_id is not None:
                self._update_finish()
            else:
                self._record_whole_run()
        finally:
            _unregister_hooks(self)

    def _update_finish(self) -> None:
        """The start-time POST landed: PATCH the run it created to a
        terminal status. On failure, spool the *complete* record instead
        of the patch -- see the module docstring for why that's a full
        record and what it means for the run to end up with two rows.
        """
        self._send(
            "PATCH",
            f"/runs/{urllib.parse.quote(self.run_id, safe='')}",
            self._finish_patch(),
            warn=f"update run {self.run_id}",
            spool_payload=self._payload(),
        )

    def _record_whole_run(self) -> None:
        """The start-time POST never landed (ledger was unreachable, or
        this run started on a thread not covered by any of the above): fall
        back to one full record, exactly ADR 0005's original behaviour.
        """
        self._send(
            "POST",
            "/runs",
            self._payload(),
            warn="record this run",
            spool_payload=self._payload(),
        )

    def _on_atexit(self) -> None:
        """Backstop for interpreter shutdown this run's own context manager
        never saw -- e.g. a chained signal handler that calls ``sys.exit()``
        instead of letting the process die from the signal, or a run
        started off the main thread with no SIGTERM coverage at all.
        Idempotent with every other path into ``_finish``, so this is safe
        to call unconditionally.
        """
        self._finish("failed")

    def _send(
        self,
        method: str,
        path: str,
        payload: Dict[str, Any],
        *,
        warn: str,
        spool_payload: Optional[Dict[str, Any]] = None,
    ) -> bool:
        """One HTTP call against the ledger. Returns whether it succeeded;
        never raises.

        A down, slow, or unreachable ledger degrades to a ``RuntimeWarning``
        -- whatever the cause (network error, timeout, bad response,
        non-2xx status) -- recording must never fail the training run. When
        ``spool_payload`` is given, that payload (not necessarily the one
        just sent -- see ``_update_finish``) is appended to the local spool
        file on failure; when it's omitted, nothing is spooled, because the
        caller has determined there is nothing worth spooling yet
        (``_record_start``).
        """
        body = json.dumps(payload).encode("utf-8")
        req = urllib.request.Request(
            self.server.rstrip("/") + API_VERSION + path,
            data=body,
            method=method,
            headers={"Content-Type": "application/json"},
        )
        token = os.environ.get("RUNLEDGER_TOKEN")
        if token:
            req.add_header("Authorization", f"Bearer {token}")
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:
                raw = resp.read()
            out = json.loads(raw.decode("utf-8")) if raw else {}
            if out.get("run_id"):
                self.run_id = out["run_id"]
            if out.get("fingerprint"):
                self.fingerprint = out["fingerprint"]
            return True
        except Exception as exc:
            message = f"runledger: could not {warn} at {self.server} ({exc})"
            if spool_payload is not None:
                message += f"; spooling to {self.resolved_spool_path()} instead"
            # stacklevel=4: _send's own frame, its caller (_record_start /
            # _update_finish / _record_whole_run), *that* caller (_enter /
            # _finish), and finally the frame that invoked one of those --
            # as close as this gets to pointing at the caller's own code
            # through contextlib.contextmanager's generator indirection.
            warnings.warn(message, RuntimeWarning, stacklevel=4)
            if spool_payload is not None:
                self._spool(spool_payload)
            return False

    def resolved_spool_path(self) -> str:
        """``spool_path`` with ``~`` expanded -- the file actually written.

        ``spool_path`` is kept as the caller gave it; this is where it lands.
        """
        return os.path.expanduser(self.spool_path)

    def _spool(self, payload: Dict[str, Any]) -> None:
        try:
            path = self.resolved_spool_path()
            spool_dir = os.path.dirname(path)
            if spool_dir:
                os.makedirs(spool_dir, exist_ok=True)
            with open(path, "a", encoding="utf-8") as fh:
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
