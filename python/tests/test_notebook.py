"""Structural checks on the committed notebook.

These do not execute it -- CI's `notebook` job does that against a real
ledger, which is what proves it still works. What these guard is the thing
a green CI run would not catch: outputs quietly committed back into git.

A notebook carrying stored outputs is documentation that asserts a result
nothing verified. It renders as authoritative, diffs as noise, and goes
stale silently -- the exact failure mode `TestRoutesMatchSpec` exists to
prevent on the API side.
"""

from __future__ import annotations

import json
import os
import unittest

NOTEBOOK = os.path.join(
    os.path.dirname(__file__), "..", "examples", "reproducibility.ipynb"
)


class NotebookHygieneTests(unittest.TestCase):
    def setUp(self):
        with open(NOTEBOOK, encoding="utf-8") as fh:
            self.nb = json.load(fh)
        self.code_cells = [
            c for c in self.nb["cells"] if c["cell_type"] == "code"
        ]

    def test_notebook_is_valid_json_with_cells(self):
        self.assertGreater(len(self.code_cells), 0, "no code cells found")

    def test_no_stored_outputs(self):
        offenders = [
            i for i, c in enumerate(self.nb["cells"])
            if c["cell_type"] == "code" and c.get("outputs")
        ]
        self.assertEqual(
            offenders,
            [],
            f"cells {offenders} carry stored outputs; strip them "
            "(jupyter nbconvert --clear-output --inplace) before committing",
        )

    def test_no_execution_counts(self):
        offenders = [
            i for i, c in enumerate(self.nb["cells"])
            if c["cell_type"] == "code" and c.get("execution_count") is not None
        ]
        self.assertEqual(offenders, [], f"cells {offenders} carry execution counts")

    def test_every_cell_has_an_id(self):
        # nbformat warns today and will hard-error on missing ids.
        missing = [i for i, c in enumerate(self.nb["cells"]) if not c.get("id")]
        self.assertEqual(missing, [], f"cells {missing} are missing an id field")

    def test_uses_the_public_api_only(self):
        # The notebook is also the client's worked example: if it reaches for
        # a private helper, the public surface is missing something.
        source = "\n".join(
            "".join(c["source"]) for c in self.code_cells
        )
        self.assertNotIn("runledger._", source, "notebook reaches into private API")
        for public in ("runledger.Run.start", "runledger.runs", "runledger.spread"):
            self.assertIn(public, source, f"notebook never demonstrates {public}")


if __name__ == "__main__":
    unittest.main()
