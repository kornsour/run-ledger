"""Is the *client* at fault? Flagging runs whose record is under-captured.

The ledger's one claim is "same fingerprint + different metrics = something
real is going unrecorded." It deliberately does not say what. But there is a
cheap signal it can offer about *where to look first*, and it needs no
schema change and no new field: compare a run against its peers.

The reasoning
-------------
ADR 0011 settled what an empty value means: for these fields "" and "not
recorded" are the same claim, because none of them has a meaningful empty
value. So "was this captured?" is now answerable exactly -- an empty field
was not captured, full stop.

What stays unanswerable from a single record is *why*: whether this run
genuinely had no dataset version, or the client that submitted it failed
to send one. That is a fact about the recording process rather than about
the experiment, and no amount of widening the field's type would recover
it.

Across a *project*, though, it is recoverable statistically. If 11 of 12
runs in a project record `framework_version` and one does not, the odds that
the twelfth run genuinely had no framework are poor. The peer group supplies
the prior that the individual record lacks.

Two signals fall out, and they mean different things:

*   **Odd-one-out** -- the field is well covered by peers and missing here.
    Points at a single bad launch: a script invoked without an env var, a
    notebook run by hand, a job submitted from a stale branch.

*   **Blind spot** -- the field is empty for *every* run in the project.
    Points at the pipeline, not the run: this client never captures that
    field at all. Harmless on its own, and damning when the group also shows
    unattributable spread, because the blind spot is then a live candidate
    for the thing going unrecorded.

The blind-spot signal is escalated only for fingerprint groups whose metrics
actually disagree. A project that never records `device` and reproduces
perfectly has nothing wrong with it.
"""

from __future__ import annotations

import statistics
from typing import Any, Dict, List, Sequence

# The six fields where an empty value means "not recorded" (ADR 0011) and
# a client is therefore capable of silently omitting one. Identity fields
# are marked because a gap there is worse: it changes which experiment the
# run is grouped with, not just how well it is described.
CAPTURABLE = [
    ("config_hash", "identity"),
    ("dataset_version", "identity"),
    ("model_version", "identity"),
    ("host", "provenance"),
    ("device", "provenance"),
    ("framework_version", "provenance"),
]

# A field must be present on at least this share of a project's runs, and on
# at least MIN_PEERS of them, before its absence on one run is called odd.
# Both guards matter: a share alone would flag 1-of-2 as a 50% outlier.
ODD_ONE_OUT_SHARE = 0.6
MIN_PEERS = 3


def coverage(runs: Sequence[Dict[str, Any]]) -> Dict[str, float]:
    """Share of runs recording a non-empty value, per capturable field."""
    if not runs:
        return {}
    out = {}
    for field, _kind in CAPTURABLE:
        present = sum(1 for r in runs if str(r.get(field, "")).strip())
        out[field] = present / len(runs)
    return out


def odd_ones_out(runs: Sequence[Dict[str, Any]]) -> List[Dict[str, Any]]:
    """Runs missing a field their peers overwhelmingly record."""
    cov = coverage(runs)
    findings = []
    for run in runs:
        missing = []
        for field, kind in CAPTURABLE:
            share = cov.get(field, 0.0)
            peers = round(share * len(runs))
            if share >= ODD_ONE_OUT_SHARE and peers >= MIN_PEERS:
                if not str(run.get(field, "")).strip():
                    missing.append(
                        {
                            "field": field,
                            "kind": kind,
                            "peers": peers,
                            "total": len(runs),
                        }
                    )
        if missing:
            findings.append({"run_id": run["run_id"], "missing": missing})
    return findings


def blind_spots(runs: Sequence[Dict[str, Any]]) -> List[Dict[str, str]]:
    """Fields no run in the project ever records."""
    cov = coverage(runs)
    return [
        {"field": f, "kind": k} for f, k in CAPTURABLE if cov.get(f, 0.0) == 0.0
    ]


def unattributable_groups(runs: Sequence[Dict[str, Any]]) -> List[Dict[str, Any]]:
    """Fingerprint groups with repeats whose metrics disagree.

    Mirrors what `GET /v1/fingerprints` ranks, computed here so the report
    can join it against the capture findings in one pass.
    """
    by_fp: Dict[str, List[Dict[str, Any]]] = {}
    for r in runs:
        by_fp.setdefault(r["fingerprint"], []).append(r)
    out = []
    for fp, group in by_fp.items():
        if len(group) < 2:
            continue
        widest, worst = 0.0, None
        for key in {k for r in group for k in (r.get("metrics") or {})}:
            vals = [r["metrics"][key] for r in group if key in (r.get("metrics") or {})]
            if len(vals) < 2:
                continue
            mean = statistics.mean(vals)
            if mean == 0:
                continue
            cv = abs(statistics.pstdev(vals) / mean)
            if cv > widest:
                widest, worst = cv, key
        if worst is not None and widest > 0:
            out.append(
                {
                    "fingerprint": fp,
                    "count": len(group),
                    "metric": worst,
                    "cv": widest,
                    "runs": group,
                }
            )
    return sorted(out, key=lambda g: -g["cv"])


def report(runs: Sequence[Dict[str, Any]]) -> str:
    """Human-readable capture report for a project's runs."""
    lines: List[str] = []
    cov = coverage(runs)
    lines.append(f"capture coverage across {len(runs)} runs")
    for field, kind in CAPTURABLE:
        share = cov.get(field, 0.0)
        bar = "#" * round(share * 20)
        lines.append(f"  {field:<18} {kind:<10} {share:6.0%} {bar}")

    odd = odd_ones_out(runs)
    lines.append("")
    if odd:
        lines.append("LIKELY CLIENT FAULT -- a run is missing what its peers record")
        for f in odd:
            lines.append(f"  {f['run_id']}")
            for m in f["missing"]:
                lines.append(
                    f"    {m['field']:<18} {m['kind']:<10} empty here, "
                    f"recorded by {m['peers']}/{m['total']} runs"
                )
        lines.append(
            "  -> this record was probably produced by a different launch path\n"
            "     (a hand-run notebook, a stale script, a missing env var),\n"
            "     not by an experiment that genuinely had no such value."
        )
    else:
        lines.append("no odd-one-out capture gaps")

    spots = blind_spots(runs)
    bad = unattributable_groups(runs)
    lines.append("")
    if spots and bad:
        names = ", ".join(s["field"] for s in spots)
        lines.append("BLIND SPOTS, AND THEY MATTER HERE")
        lines.append(f"  never recorded by any run in this project: {names}")
        lines.append(
            f"  and {len(bad)} fingerprint group(s) show metric spread that the\n"
            f"  record cannot explain -- widest {bad[0]['metric']} CV "
            f"{bad[0]['cv']:.2%} over {bad[0]['count']} runs."
        )
        lines.append(
            "  -> an unrecorded field is a candidate explanation by construction.\n"
            "     Capture these before hunting for subtler causes."
        )
    elif spots:
        names = ", ".join(s["field"] for s in spots)
        lines.append(f"blind spots (never recorded): {names}")
        lines.append("  -> no unexplained spread in this project, so nothing to chase.")
    else:
        lines.append("no blind spots: every capturable field is recorded somewhere")
    return "\n".join(lines)
