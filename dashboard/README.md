# runledger dashboard

A local, read-only browser over the ledger, built on the [Python
client](../python/README.md): a project picker feeding the `spread()`
ranking (worst-reproducing first), a group's runs and provenance
disagreements, and a `compare()` verdict between two of them.

It is strictly a *consumer* of `runledger` -- no new API surface. If a view
here needs something the client cannot express, that is a gap in the
client, not something to work around with a hand-rolled HTTP call.

## Why this is a separate package

`python/` promises "standard library only" and means it -- a training
container should never need to install a dashboard's dependencies to record
a run. This is the first thing in the repo that carries a real dependency
tree (marimo), so it lives in its own directory with its own
`pyproject.toml` rather than becoming an optional extra on the client.

## Why marimo, not Streamlit or Jupyter

See [issue #55](https://github.com/kornsour/run-ledger/issues/55) for the
full reasoning. In short: the app is a plain `.py` file with no hidden
execution-order state (marimo re-runs cells by dependency, not by click
order), so `git diff` on it is a code diff, not a notebook-output diff --
the same problem `python/tests/test_notebook.py` already enforces for the
worked notebook, solved structurally instead of by a test here.

## Run it

```bash
make build && ./bin/runledger &          # from the repo root -- the ledger
pip install -e ./python                  # the client, editable
pip install -e ./dashboard               # the dashboard's own dependency (marimo)
make dashboard                           # from the repo root
```

Or directly:

```bash
marimo run dashboard/app.py              # read-only view
marimo edit dashboard/app.py             # to change it
```

Point it at a different ledger with the "Ledger server" field in the app,
or by setting `RUNLEDGER_ADDR` before launching.

## Why this only runs locally

The server sets no `Access-Control-Allow-*` headers anywhere (checked
across `internal/`, `cmd/`, and `docs/openapi.yaml`), so a page served from
somewhere other than the ledger's own origin cannot `fetch` it -- including
`marimo export html-wasm` published to a static site making a Pyodide
`fetch` back to a reader's `localhost`, which is blocked by both CORS and
mixed content. Publishing a live version of this dashboard is a separate
decision that starts with "should the server grow a configurable CORS
allowlist?", not an export flag here.

## Layout

- `app.py` -- the marimo app: thin cells that wire UI elements to
  `runledger_dashboard`.
- `runledger_dashboard/` -- the data layer the cells call into
  (`list_projects`, `ranked_groups`, `group_runs`, `pair_diff`,
  `widest_spread`). Split out so it is testable with plain `pytest` /
  `unittest`, without a marimo runtime.
- `tests/` -- exercises `runledger_dashboard` against an in-process fake
  ledger, the same approach `python/tests/test_read.py` uses for the
  client itself.

## Test

```bash
pip install -e ./python -e './dashboard[dev]'
python3 -m unittest discover -s dashboard/tests -v
```
