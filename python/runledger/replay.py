"""Replaying the spool: recovering runs a down ledger never received.

Design note -- why a crash mid-replay cannot double-record
------------------------------------------------------------
``started_at`` is client-supplied and part of the spooled payload, and the
server derives ``run_id`` from the fingerprint plus that timestamp (see
``_run.py``). POSTing the same spooled line twice therefore lands on the
same ``run_id`` both times, and the server's own idempotency rule --
identical content against an existing id succeeds again rather than
conflicting -- makes re-recording it a no-op. That is what makes replay
safe to interrupt and re-run: nothing here needs to be transactional across
records, because no individual record needs to be sent exactly once.

Design note -- rejected records are quarantined, not retried forever
------------------------------------------------------------------------
A ``400`` or ``409`` means the server has looked at this exact payload and
will never accept it: a validation error, or a run id that already exists
with *different* content. Leaving a record like that in the spool would
have every future replay retry it forever, alongside records that only need
the server to come back -- and bury the ones that do. Such records are
moved to ``<spool>.rejected.jsonl`` instead, once, and counted separately.

Design note -- why this raises rather than degrades
-------------------------------------------------------
``Run`` degrades to a warning and a spool file when the ledger is
unreachable, because recording must never fail an expensive training job.
Replay has no such constraint -- it is a command a person runs on purpose,
after the fact, specifically to recover spooled records -- so the read
side's convention applies instead (see ``read.py``): a ledger that cannot
be reached raises ``LedgerUnreachableError`` rather than silently reporting
partial progress. Whatever was sent or rejected before the failing record
is not undone; whatever is left, including the record that failed, stays in
the spool for the next attempt.

Design note -- concurrent appends
--------------------------------------
A training run can append to the spool while a replay is in flight. This
module tracks the byte offset read at the start of the call and, when
rewriting the spool, re-reads only what was appended after that offset --
so "read the file, send, rewrite the whole file" cannot silently drop a
line a live process wrote in the meantime. That avoids needing a file lock,
which the standard library has no portable way to take.
"""

from __future__ import annotations

import dataclasses
import os
import sys
import tempfile
import urllib.error
import urllib.request
import warnings
from typing import List, Optional

from ._run import DEFAULT_SERVER, DEFAULT_SPOOL_PATH, DEFAULT_TIMEOUT
from .read import LedgerUnreachableError, _error_detail

# Statuses the server will never reconsider on an unmodified retry: the
# payload itself is what's wrong (400), or a run with this id already
# exists with different content (409). Anything else -- a 5xx, a connection
# refused, a timeout -- says nothing about the payload and is worth trying
# again once the ledger recovers.
_PERMANENT_STATUSES = (400, 409)


@dataclasses.dataclass
class ReplayResult:
    """The outcome of one :func:`replay_spool` call.

    :ivar sent: Records the ledger accepted -- a fresh record, or an
        idempotent re-record of one it already had.
    :ivar rejected: Records the ledger will never accept as sent, moved to
        the ``.rejected.jsonl`` quarantine file beside the spool.
    :ivar remaining: Records still in the spool afterwards: in a dry run,
        every record found; otherwise whatever came after the first
        unreachable response this call hit, plus anything a live process
        appended to the spool while this call was running.
    """

    sent: int = 0
    rejected: int = 0
    remaining: int = 0


def _resolved_path(path: Optional[str]) -> str:
    return os.path.expanduser(path if path is not None else DEFAULT_SPOOL_PATH)


def _rejected_path(spool_path: str) -> str:
    root, ext = os.path.splitext(spool_path)
    return f"{root}.rejected{ext or '.jsonl'}"


def _post(line: str, *, server: str, token: Optional[str], timeout: float) -> None:
    req = urllib.request.Request(
        server.rstrip("/") + "/runs",
        data=line.encode("utf-8"),
        method="POST",
        headers={"Content-Type": "application/json"},
    )
    # Same rule as Run and the read helpers: the token comes from the
    # environment only, never a keyword argument.
    if token:
        req.add_header("Authorization", f"Bearer {token}")
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        resp.read()


def _rewrite(path: str, remaining: List[str], *, original_size: int) -> None:
    """Replace the spool with ``remaining``, keeping anything appended
    to it after this call's initial read."""
    tail = b""
    try:
        with open(path, "rb") as fh:
            fh.seek(original_size)
            tail = fh.read()
    except FileNotFoundError:
        pass

    content = "\n".join(remaining)
    if content:
        content += "\n"
    data = content.encode("utf-8") + tail

    spool_dir = os.path.dirname(path) or "."
    os.makedirs(spool_dir, exist_ok=True)
    fd, tmp_path = tempfile.mkstemp(dir=spool_dir, prefix=".spool-", suffix=".tmp")
    try:
        with os.fdopen(fd, "wb") as fh:
            fh.write(data)
        # Atomic on POSIX and on Windows (as of Python 3.3+), so an
        # interrupted replay leaves either the old spool or the new one,
        # never a half-written file.
        os.replace(tmp_path, path)
    except BaseException:
        try:
            os.remove(tmp_path)
        except OSError:
            pass
        raise


def _quarantine(path: str, lines: List[str]) -> None:
    spool_dir = os.path.dirname(path)
    if spool_dir:
        os.makedirs(spool_dir, exist_ok=True)
    with open(path, "a", encoding="utf-8") as fh:
        for line in lines:
            fh.write(line + "\n")


def replay_spool(
    path: Optional[str] = None,
    *,
    server: Optional[str] = None,
    timeout: float = DEFAULT_TIMEOUT,
    dry_run: bool = False,
) -> ReplayResult:
    """Re-send spooled run records that a down ledger never received.

    Reads from ``$RUNLEDGER_ADDR`` / ``$RUNLEDGER_TOKEN`` exactly as
    ``Run`` and the read helpers do. Records the ledger accepts are removed
    from the spool; records it permanently rejects (see the module
    docstring) are moved to ``<path>.rejected.jsonl``. Both rewrites use a
    temp file and ``os.replace``, so an interrupted replay cannot truncate
    the spool.

    :param path: Spool file to replay. Defaults to the same path ``Run``
        writes to (``~/.runledger/spool.jsonl``), with ``~`` expanded here
        rather than by the caller.
    :param server: Ledger base URL. Defaults to ``$RUNLEDGER_ADDR``.
    :param timeout: Per-request timeout in seconds.
    :param dry_run: Report what is in the spool without contacting the
        ledger or modifying anything.
    :raises LedgerUnreachableError: if a record could not be sent for any
        reason other than the ledger permanently rejecting it (network
        error, timeout, or a non-2xx/400/409 status). Whatever was sent or
        rejected earlier in this call is not undone; the record that
        failed, and everything after it, stays in the spool.
    """
    resolved = _resolved_path(path)
    try:
        with open(resolved, "rb") as fh:
            raw = fh.read()
    except FileNotFoundError:
        return ReplayResult()

    lines = [line for line in raw.decode("utf-8").splitlines() if line.strip()]
    if dry_run:
        return ReplayResult(remaining=len(lines))
    if not lines:
        return ReplayResult()

    base = server or os.environ.get("RUNLEDGER_ADDR") or DEFAULT_SERVER
    token = os.environ.get("RUNLEDGER_TOKEN")

    sent = 0
    rejected: List[str] = []
    remaining: List[str] = []
    error_to_raise: Optional[BaseException] = None

    for i, line in enumerate(lines):
        try:
            _post(line, server=base, token=token, timeout=timeout)
            sent += 1
            continue
        except urllib.error.HTTPError as exc:
            detail = _error_detail(exc)  # also reads and closes exc
            if exc.code in _PERMANENT_STATUSES:
                warnings.warn(
                    f"runledger: spooled record rejected by the ledger "
                    f"({exc.code}: {detail}); moved to {_rejected_path(resolved)}",
                    RuntimeWarning,
                    stacklevel=2,
                )
                rejected.append(line)
                continue
            error_to_raise = LedgerUnreachableError(
                f"ledger at {base} answered {exc.code}: {detail}"
            )
            error_to_raise.__cause__ = exc
        except Exception as exc:  # connection error, timeout, DNS failure, ...
            error_to_raise = LedgerUnreachableError(
                f"could not reach the ledger at {base}: {exc}"
            )
            error_to_raise.__cause__ = exc
        remaining = lines[i:]
        break

    _rewrite(resolved, remaining, original_size=len(raw))
    if rejected:
        _quarantine(_rejected_path(resolved), rejected)

    if error_to_raise is not None:
        raise error_to_raise
    return ReplayResult(sent=sent, rejected=len(rejected), remaining=len(remaining))


def _main(argv: Optional[List[str]] = None) -> int:
    import argparse

    parser = argparse.ArgumentParser(
        prog="python -m runledger.replay",
        description="Re-send spooled run records to the ledger.",
    )
    parser.add_argument(
        "path",
        nargs="?",
        default=None,
        help=f"spool file to replay (default: {DEFAULT_SPOOL_PATH})",
    )
    parser.add_argument("--server", default=None, help="ledger base URL (default: $RUNLEDGER_ADDR)")
    parser.add_argument("--timeout", type=float, default=DEFAULT_TIMEOUT)
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help="report what would be replayed without sending or modifying anything",
    )
    args = parser.parse_args(argv)

    try:
        result = replay_spool(
            args.path, server=args.server, timeout=args.timeout, dry_run=args.dry_run
        )
    except LedgerUnreachableError as exc:
        print(f"runledger: {exc}", file=sys.stderr)
        return 1

    if args.dry_run:
        print(f"{result.remaining} record(s) in the spool")
    else:
        print(
            f"sent {result.sent}, rejected {result.rejected}, "
            f"{result.remaining} remaining"
        )
    return 0


if __name__ == "__main__":
    raise SystemExit(_main())
