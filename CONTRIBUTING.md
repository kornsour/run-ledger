# Contributing

Open a focused pull request against `main` and explain the behavior being
changed, its verification, and any compatibility implications.

Before submitting:

```bash
make lint
make test
make build
python3 -m unittest discover -s python/tests -v
python3 -m unittest discover -s examples/churn -v
python3 -m unittest discover -s dashboard/tests -v
```

Changes to the HTTP surface must update `docs/openapi.yaml`. Architectural
changes should add or supersede an ADR under `docs/adr/`; do not rewrite the
history in `archive/`.

Never commit tokens, generated documentation, experiment data, DuckDB files, or
notebook outputs.
