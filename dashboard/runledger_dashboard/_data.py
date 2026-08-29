"""Read-only queries the dashboard's cells wire up to marimo UI elements.

Design note -- raise, don't degrade
------------------------------------
Same convention as ``runledger.read``: a function here raises
``runledger.LedgerError`` (unreachable ledger, or an id that matches
nothing) rather than swallowing it. The dashboard is what decides how to
show that to a person -- an error banner instead of a crash -- but the data
layer is not the place to paper over "the ledger did not answer."

Design note -- no project-listing endpoint
-------------------------------------------
The API has no ``GET /v1/projects``, and adding one would be exactly the
kind of new API surface this app is not supposed to need (see the package
docstring). ``list_projects`` answers it with the client it already has:
every distinct ``project`` seen in a bounded scan of ``runs()``. Bounded
because an unfiltered walk of a large ledger is the expensive kind of
"list projects" -- ``scan_limit`` trades completeness on a ledger with more
runs than that for a picker that opens promptly; pass a larger one, or
``None`` to walk the whole thing, if that trade is wrong for your ledger.
"""

from __future__ import annotations

from typing import Any, Dict, List, Optional

import runledger

DEFAULT_SCAN_LIMIT = 5000


def list_projects(
    *,
    server: Optional[str] = None,
    timeout: float = runledger.read.DEFAULT_TIMEOUT,
    scan_limit: Optional[int] = DEFAULT_SCAN_LIMIT,
) -> List[str]:
    """Distinct project names, sorted, from a bounded scan of ``runs()``.

    :raises runledger.LedgerUnreachableError: if the ledger cannot be reached.
    """
    rows = runledger.runs(limit=scan_limit, server=server, timeout=timeout)
    return sorted({r["project"] for r in rows if r.get("project")})


def ranked_groups(
    *,
    project: str = "",
    server: Optional[str] = None,
    timeout: float = runledger.read.DEFAULT_TIMEOUT,
) -> List[Dict[str, Any]]:
    """Fingerprint groups with repeats, worst relative spread first.

    A thin pass-through to ``runledger.spread()`` -- the server already does
    the ranking. ``project=""`` (the default) ranks across every project,
    the same as the client itself.

    :raises runledger.LedgerUnreachableError: if the ledger cannot be reached.
    """
    return runledger.spread(project=project, server=server, timeout=timeout)


def widest_spread(group: Dict[str, Any]) -> Optional[Dict[str, Any]]:
    """The one metric responsible for a group's ranking, for display.

    Mirrors the server's own ranking key (widest coefficient of variation,
    |stddev / mean|, skipping a zero mean or a metric only one run
    reported) -- see ``internal/spread.Group.Widest`` -- but the *value*
    of that computation is not on the wire, only its effect on ordering.
    Recomputing it here is what lets a group's row in the table say which
    metric, and by how much, rather than just where it landed.

    Returns ``None`` for a group with nothing measurable (``no_repeats``,
    or no metric with a nonzero mean across more than one run).
    """
    best: Optional[Dict[str, Any]] = None
    best_cv = -1.0
    for name, stat in (group.get("metrics") or {}).items():
        if stat.get("count", 0) < 2 or stat.get("mean") == 0:
            continue
        cv = abs(stat["stddev"] / stat["mean"])
        if cv > best_cv:
            best_cv, best = cv, {"metric": name, "cv": cv, **stat}
    return best


def group_runs(
    group: Dict[str, Any],
    *,
    server: Optional[str] = None,
    timeout: float = runledger.read.DEFAULT_TIMEOUT,
) -> List[Dict[str, Any]]:
    """The full run record for each of a group's ``run_ids``.

    ``spread()`` reports the group's aggregate metrics and where its runs'
    provenance disagrees, but not each run's own metrics -- that needs one
    ``run()`` per id. A group is a handful of repeats, not a page of
    results, so one call each is the whole cost of a drill-down here.

    :raises runledger.RunNotFoundError: if a run named in the group was
        deleted from the ledger after the group was fetched.
    :raises runledger.LedgerUnreachableError: if the ledger cannot be reached.
    """
    return [
        runledger.run(run_id, server=server, timeout=timeout)
        for run_id in group.get("run_ids") or []
    ]


def pair_diff(
    a: Any,
    b: Any,
    *,
    server: Optional[str] = None,
    timeout: float = runledger.read.DEFAULT_TIMEOUT,
) -> Dict[str, Any]:
    """What differs between two runs -- a thin pass-through to
    ``runledger.compare()``, kept here only so every ledger call the
    dashboard makes goes through this module and not app.py directly.

    :raises runledger.RunNotFoundError: if ``a`` or ``b`` matches no run.
    :raises runledger.LedgerUnreachableError: if the ledger cannot be reached.
    """
    return runledger.compare(a, b, server=server, timeout=timeout)


def diff_cell(value: Optional[str]) -> str:
    """Render one side of a ``pair_diff`` field for display.

    ``None`` means the run never recorded the field; ``""`` means it
    recorded an empty value, which only a param can be (ADR 0011). Passing
    ``None`` straight into a table renders a blank cell, which reads as the
    empty value it is not -- the same conflation ``rlctl``'s ``cell``
    avoids, and the reason both spellings used to be indistinguishable.
    """
    if value is None:
        return "\u2014"
    if value == "":
        return '""'
    return value
