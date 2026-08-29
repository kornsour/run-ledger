"""Reading the ledger back: runs(), spread(), and run().

Design note -- why these raise where Run does not
-------------------------------------------------
``Run`` degrades to a warning and a spool file when the ledger is
unreachable, because recording lineage must never fail an expensive
training job (see ``run.py``). Reads have no such constraint, and the
opposite default is the safe one: silently returning ``[]`` when the server
is down would answer "how did my experiments do?" with "they didn't", which
is a worse failure than an exception. Every function here raises
``LedgerUnreachableError`` instead.

Design note -- why plain dicts, and no pandas
---------------------------------------------
These return lists of dicts, exactly as the wire delivers them. The package
has no dependencies and that is worth keeping: a caller who wants a frame
writes ``pd.DataFrame(runledger.runs(project="demo"))`` and has lost
nothing, while a caller inside a training container that has no pandas has
not gained a mandatory install.
"""

from __future__ import annotations

import json
import os
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, Dict, List, Optional

DEFAULT_SERVER = "http://localhost:8080"
DEFAULT_TIMEOUT = 10.0

# The server caps a page at 500 whatever a client asks for, and echoes the
# limit it actually applied. Asking for the cap means the fewest round trips
# for a walk; the pagination loop below follows next_cursor regardless, so
# this is a performance choice, not a correctness one.
_PAGE_SIZE = 500


class LedgerError(RuntimeError):
    """A read against the ledger did not succeed."""


class LedgerUnreachableError(LedgerError):
    """The ledger could not be reached, or answered with an error status."""


class RunNotFoundError(LedgerError):
    """No run, or no fingerprint, matches the given id."""


def _base(server: Optional[str]) -> str:
    if server:
        return server.rstrip("/")
    return os.environ.get("RUNLEDGER_ADDR", DEFAULT_SERVER).rstrip("/")


def _get(
    path: str,
    params: Optional[Dict[str, Any]] = None,
    *,
    server: Optional[str] = None,
    timeout: float = DEFAULT_TIMEOUT,
) -> Any:
    """One GET against the ledger, returning decoded JSON.

    Empty and None-valued query parameters are dropped rather than sent as
    ``?project=``: the server treats a zero-valued filter as "do not filter",
    so sending one is at best noise and at worst a filter the caller did not
    intend.
    """
    query = {k: str(v) for k, v in (params or {}).items() if v not in (None, "")}
    url = _base(server) + path
    if query:
        url += "?" + urllib.parse.urlencode(query)

    req = urllib.request.Request(url, method="GET")
    # Same rule as Run and rlctl: the token comes from the environment only,
    # never a keyword argument, so it cannot end up in a notebook cell that
    # gets committed or shared.
    token = os.environ.get("RUNLEDGER_TOKEN")
    if token:
        req.add_header("Authorization", f"Bearer {token}")

    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read()
    except urllib.error.HTTPError as exc:
        detail = _error_detail(exc)
        if exc.code == 404:
            raise RunNotFoundError(detail) from exc
        raise LedgerUnreachableError(
            f"ledger at {_base(server)} answered {exc.code}: {detail}"
        ) from exc
    except Exception as exc:
        raise LedgerUnreachableError(
            f"could not reach the ledger at {_base(server)}: {exc}"
        ) from exc

    return json.loads(raw.decode("utf-8")) if raw else {}


def _error_detail(exc: urllib.error.HTTPError) -> str:
    """The server's own error message, when it sent one it can parse."""
    try:
        raw = exc.read()
    except Exception:
        return exc.reason or "no detail"
    finally:
        # HTTPError is itself a response object; leaving it open raises a
        # ResourceWarning at collection time.
        exc.close()
    try:
        body = json.loads(raw.decode("utf-8"))
    except Exception:
        return exc.reason or "no detail"
    if isinstance(body, dict) and "error" in body:
        return str(body["error"])
    return str(body)


def runs(
    *,
    project: str = "",
    git_commit: str = "",
    fingerprint: str = "",
    status: str = "",
    device: str = "",
    limit: Optional[int] = None,
    server: Optional[str] = None,
    timeout: float = DEFAULT_TIMEOUT,
) -> List[Dict[str, Any]]:
    """Every run matching the given filters, newest first.

    Follows ``next_cursor`` internally, so a caller never handles pagination.
    An unfiltered call against a large ledger walks the whole thing -- pass
    ``limit=`` to bound it.

    :param project: Restrict to one project.
    :param git_commit: Restrict to runs from one commit.
    :param fingerprint: Restrict to one experiment's runs.
    :param status: One of created, running, succeeded, failed, cancelled.
    :param device: Restrict to runs on one device.
    :param limit: Stop after this many runs. ``None`` walks every page.
    :param server: Ledger base URL. Defaults to ``$RUNLEDGER_ADDR``.
    :param timeout: Per-request timeout in seconds.
    :raises LedgerUnreachableError: if the ledger cannot be reached.
    """
    if limit is not None and limit <= 0:
        # The server rejects limit=0 as a bad request; asking for no runs is
        # better answered here than by a round trip that 400s.
        return []
    out: List[Dict[str, Any]] = []
    cursor = ""
    while True:
        want = _PAGE_SIZE if limit is None else min(_PAGE_SIZE, limit - len(out))
        page = _get(
            "/runs",
            {
                "project": project,
                "git_commit": git_commit,
                "fingerprint": fingerprint,
                "status": status,
                "device": device,
                "limit": want,
                "cursor": cursor,
            },
            server=server,
            timeout=timeout,
        )
        out.extend(page.get("runs") or [])
        cursor = page.get("next_cursor") or ""
        # next_cursor's absence is how the server says the traversal is done.
        if not cursor or (limit is not None and len(out) >= limit):
            break
    return out[:limit] if limit is not None else out


def run(run_id: str, *, server: Optional[str] = None, timeout: float = DEFAULT_TIMEOUT) -> Dict[str, Any]:
    """One run by id.

    :raises RunNotFoundError: if no run has that id.
    :raises LedgerUnreachableError: if the ledger cannot be reached.
    """
    return _get(f"/runs/{urllib.parse.quote(run_id, safe='')}", server=server, timeout=timeout)


def spread(
    *,
    project: str = "",
    fingerprint: str = "",
    server: Optional[str] = None,
    timeout: float = DEFAULT_TIMEOUT,
) -> List[Dict[str, Any]]:
    """Fingerprint groups with their per-metric spread.

    With no ``fingerprint``, returns every fingerprint that has more than one
    recorded run, ranked by widest relative metric spread -- "which of my
    experiments reproduce worst?". With a ``fingerprint``, returns that one
    group as a single-element list, including a lone run (reported with
    ``no_repeats: True`` rather than a misleading standard deviation of zero).

    Each group carries ``fingerprint``, ``run_ids``, ``count``, a ``metrics``
    map of count/min/max/mean/stddev, and a ``provenance`` list naming any
    field the group's runs disagree on -- usually the first place to look
    when a group's metrics moved but its identity did not.

    :raises RunNotFoundError: if ``fingerprint`` matches no recorded run.
    :raises LedgerUnreachableError: if the ledger cannot be reached.
    """
    if fingerprint:
        group = _get(
            f"/fingerprints/{urllib.parse.quote(fingerprint, safe='')}",
            server=server,
            timeout=timeout,
        )
        return [group]
    page = _get("/fingerprints", {"project": project}, server=server, timeout=timeout)
    # The collection returns null rather than [] when nothing qualifies.
    return page.get("groups") or []
