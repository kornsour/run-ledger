"""runledger_dashboard -- data layer for the marimo browse-and-drill-down app.

Split out from app.py so it can be tested with plain pytest, without a
marimo runtime: marimo cells are still just functions, but exercising them
means either driving the whole notebook or reaching into internals that are
not the module's public contract. The functions here are.

Everything in this package is a thin, read-only consumer of ``runledger``
(see the repo root README's `python/` client) -- no new API surface. If a
view here needs something the client cannot express, that is a gap in
``runledger``, not something to work around with a hand-rolled HTTP call.
"""

from ._data import (
    diff_cell,
    group_runs,
    list_projects,
    pair_diff,
    ranked_groups,
    widest_spread,
)

__all__ = [
    "list_projects",
    "ranked_groups",
    "widest_spread",
    "group_runs",
    "pair_diff",
    "diff_cell",
]
