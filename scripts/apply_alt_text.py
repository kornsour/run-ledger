"""Replace nbconvert's placeholder image alt text with the notebook's own.

Why this exists
---------------
nbconvert's lab template never emits an ``alt`` attribute for an output
image at all -- ``data_png`` reads width, height, unconfined and
needs_background from output metadata, but not alt. The HTML exporter then
notices the gap after the fact and fills in a hardcoded string:

    "No description has been provided for this image"

which satisfies the validator and tells a screen-reader user nothing. There
is no configuration trait to change it (checked against nbconvert 7.17).

So the real description is authored where it belongs -- next to the plot, in
the notebook cell's own metadata under ``alt_text`` -- and applied here,
after conversion. Cell metadata survives ``--clear-output``, so it stays in
git even though the image it describes does not.

Usage: apply_alt_text.py <notebook.ipynb> <rendered.html>

Exits non-zero if the notebook and the rendered page disagree about how many
images there are. A silent mismatch would mean a plot shipped with the
placeholder, or an alt text landed on the wrong image -- both worse than a
failed build, because both look fine.
"""

from __future__ import annotations

import json
import sys

PLACEHOLDER = "No description has been provided for this image"


def main(argv: list[str]) -> int:
    if len(argv) != 3:
        print(__doc__.strip().splitlines()[-3], file=sys.stderr)
        return 2
    notebook_path, html_path = argv[1], argv[2]

    with open(notebook_path, encoding="utf-8") as fh:
        nb = json.load(fh)

    # Document order: the rendered page lays images out in the order their
    # cells appear, so the Nth declared alt text belongs to the Nth image.
    declared = [
        cell["metadata"]["alt_text"]
        for cell in nb["cells"]
        if cell["cell_type"] == "code" and cell["metadata"].get("alt_text")
    ]

    try:
        from bs4 import BeautifulSoup
    except ImportError:
        print(
            "apply_alt_text: beautifulsoup4 is required "
            "(it ships with nbconvert; pip install -e './python[docs]')",
            file=sys.stderr,
        )
        return 1

    with open(html_path, encoding="utf-8") as fh:
        soup = BeautifulSoup(fh.read(), "html.parser")

    placeholders = [
        img for img in soup.find_all("img") if img.get("alt") == PLACEHOLDER
    ]

    if len(placeholders) != len(declared):
        print(
            f"apply_alt_text: {len(placeholders)} image(s) in {html_path} need "
            f"alt text but {len(declared)} cell(s) declare it.\n"
            "Add an \"alt_text\" entry to the metadata of every code cell that "
            "renders an image.",
            file=sys.stderr,
        )
        return 1

    for img, text in zip(placeholders, declared):
        img["alt"] = text

    with open(html_path, "w", encoding="utf-8") as fh:
        fh.write(str(soup))

    print(f"apply_alt_text: described {len(declared)} image(s) in {html_path}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
