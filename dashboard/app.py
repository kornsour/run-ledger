import marimo

__generated_with = "0.24.0"
app = marimo.App(width="medium", app_title="run-ledger dashboard")


@app.cell(hide_code=True)
def _():
    import marimo as mo

    mo.md(
        """
        # run-ledger dashboard

        A local, read-only browser over `runledger` -- **which of my
        experiments reproduce worst?**, drilled down into a group's runs and
        a pairwise diff. Nothing here talks to the ledger except through the
        Python client; if a view needs something `runledger` cannot express,
        that is a gap in the client, not something to work around here.

        Run with `marimo run app.py` for the read-only view, or
        `marimo edit app.py` to poke at it.
        """
    )
    return (mo,)


@app.cell(hide_code=True)
def _(mo):
    import os

    server_input = mo.ui.text(
        value=os.environ.get("RUNLEDGER_ADDR", "http://localhost:8080"),
        label="Ledger server",
        full_width=True,
    )
    server_input
    return (server_input,)


@app.cell(hide_code=True)
def _(mo, server_input):
    from runledger import LedgerError as _LedgerError

    from runledger_dashboard import list_projects

    # Both the project list and the ranked groups below fail the same way,
    # by design (runledger.read raises rather than degrading) -- one banner
    # here stands in for both rather than duplicating the try/except.
    ledger_error = None
    projects = []
    try:
        projects = list_projects(server=server_input.value)
    except _LedgerError as exc:
        ledger_error = exc

    if ledger_error is not None:
        mo.stop(
            True,
            mo.md(f"**Could not reach the ledger at `{server_input.value}`:** {ledger_error}").callout(
                kind="danger"
            ),
        )

    project_picker = mo.ui.dropdown(
        options=["(all projects)"] + projects,
        value="(all projects)",
        label="Project",
    )
    project_picker
    return (project_picker,)


@app.cell(hide_code=True)
def _(mo, project_picker, server_input):
    from runledger_dashboard import ranked_groups, widest_spread

    selected_project = "" if project_picker.value == "(all projects)" else project_picker.value
    groups = ranked_groups(project=selected_project, server=server_input.value)

    def _row(g):
        worst = widest_spread(g)
        return {
            "fingerprint": g["fingerprint"][:16] + "...",
            "runs": g["count"],
            "widest metric": worst["metric"] if worst else "-",
            "relative spread": f"{worst['cv']:.1%}" if worst else "-",
            "provenance disagreements": ", ".join(d["field"] for d in g.get("provenance") or []) or "-",
        }

    groups_table = mo.ui.table(
        [_row(g) for g in groups],
        selection="single",
        label="Fingerprint groups, worst-reproducing first",
        page_size=15,
    )

    mo.vstack(
        [
            mo.md(f"**{len(groups)}** group(s) with repeats" + (f" in `{selected_project}`" if selected_project else " across every project")),
            groups_table,
        ]
    )
    return groups, groups_table


@app.cell(hide_code=True)
def _(groups, groups_table, mo, server_input):
    from runledger_dashboard import diff_cell, group_runs

    if not groups_table.value:
        mo.stop(True, mo.md("_Select a group above to see its runs._"))

    selected_index = groups_table.value[0]
    # marimo's table selection returns the selected row dicts, not their
    # index into the source list, so the source group is recovered by
    # matching on the one column both the table row and the group carry:
    # the full fingerprint's own prefix.
    selected_group = next(
        g for g in groups if g["fingerprint"][:16] + "..." == selected_index["fingerprint"]
    )

    runs = group_runs(selected_group, server=server_input.value)

    provenance = selected_group.get("provenance") or []
    # A ProvenanceDiff's values are plain strings, never JSON null: Go's
    # []string has no "absent" element, so a run that never recorded the
    # field comes through as "" rather than None. Every field spread
    # surfaces here (device, framework_version, submitter_claim, job_id) is
    # a scalar provenance field where ADR 0011 gives "" exactly one
    # meaning, "not recorded" -- unlike the pair_diff fields below, nothing
    # here can be a genuinely-recorded empty string. `v or None` maps that
    # "" back to diff_cell's None case (an em dash) instead of its ""
    # case (a quoted empty string, meant for a value a run actually
    # recorded as empty) -- reusing diff_cell's existing convention rather
    # than inventing a second one, so the two views read the same way.
    # Before this, ", ".join(d["values"]) rendered the unrecorded run's ""
    # verbatim, leaving a dangling comma that both looked like a formatting
    # bug and erased the fact that some run never recorded the field --
    # precisely the interesting half of a submitter_claim/job_id
    # disagreement, since those two go unrecorded far more often than
    # device or framework_version do.
    provenance_md = (
        "\n".join(
            f"- **{d['field']}**: {', '.join(diff_cell(v or None) for v in d['values'])}"
            for d in provenance
        )
        if provenance
        else "_This group's runs agree on every recorded provenance field._"
    )

    mo.vstack(
        [
            mo.md(f"## Group `{selected_group['fingerprint'][:16]}...`"),
            mo.md("**Provenance disagreements** -- usually the first place to look:"),
            mo.md(provenance_md),
            mo.md("**Runs in this group:**"),
            mo.ui.table(
                [
                    {
                        "run_id": r["run_id"],
                        "status": r.get("status"),
                        **{f"metrics.{k}": v for k, v in (r.get("metrics") or {}).items()},
                    }
                    for r in runs
                ],
                selection=None,
            ),
        ]
    )
    return runs, selected_group


@app.cell(hide_code=True)
def _(mo, runs):
    if len(runs) < 2:
        mo.stop(True, mo.md("_Need at least two runs in this group to compare._"))

    run_options = {r["run_id"]: r for r in runs}
    a_picker = mo.ui.dropdown(options=list(run_options), value=runs[0]["run_id"], label="Run A")
    b_picker = mo.ui.dropdown(options=list(run_options), value=runs[1]["run_id"], label="Run B")
    mo.md("## Compare two runs")
    mo.hstack([a_picker, b_picker])
    return a_picker, b_picker


@app.cell(hide_code=True)
def _(a_picker, b_picker, mo, server_input):
    from runledger import LedgerError as _LedgerError

    from runledger_dashboard import diff_cell as _cell
    from runledger_dashboard import pair_diff

    if a_picker.value == b_picker.value:
        mo.stop(True, mo.md("_Pick two different runs._"))

    try:
        diff = pair_diff(a_picker.value, b_picker.value, server=server_input.value)
    except _LedgerError as exc:
        mo.stop(True, mo.md(f"**Comparison failed:** {exc}").callout(kind="danger"))

    verdict = (
        "Same experiment, but it measured differently -- **unattributable**: nothing in the record explains the gap."
        if diff["unattributable"]
        else ("Same experiment, same result." if diff["same_experiment"] else "Different experiments -- differing metrics are expected.")
    )

    mo.vstack(
        [
            mo.md(verdict),
            mo.ui.table(
                [
                    {
                        "field": f["name"],
                        "kind": f["kind"],
                        # null means the run never recorded the field; ""
                        # means it recorded an empty value. Passing null
                        # straight through renders a blank cell, which reads
                        # as the empty value it is not -- the same
                        # conflation rlctl's `cell` avoids. See ADR 0011.
                        "a": _cell(f["a"]),
                        "b": _cell(f["b"]),
                    }
                    for f in diff["fields"]
                ],
                selection=None,
            )
            if diff["fields"]
            else mo.md("_No fields differ between these two runs._"),
        ]
    )
    return


if __name__ == "__main__":
    app.run()
