"""Add the site-wide light/dark toggle to the executed notebook's HTML.

Why this exists
----------------
nbconvert's lab template renders the notebook using JupyterLab's `--jp-*` CSS
custom properties, but only ever emits their light-theme values -- there is
no dark variant and no toggle, on this version of nbconvert (checked against
7.17). The same toggle is baked into docs/index.html directly and into
pdoc's output via docs/pdoc-template/ (see frame.html.jinja2 there for the
shared design); this script gets the notebook page to the same place by
post-processing nbconvert's output, the same way apply_alt_text.py already
does for image alt text.

All three pages read and write the same "theme" localStorage key, so a
choice made on one page is the choice already in effect on the next.

Usage: apply_theme_toggle.py <rendered.html>
"""

from __future__ import annotations

import sys

# JupyterLab's own dark-theme values for the subset of --jp-* variables this
# notebook page actually renders with (layout, borders, code, links, syntax
# highlighting). The rest of JupyterLab's ~250 variables style toolbar chrome
# that a static nbconvert export never includes.
DARK_CSS = """
:root[data-theme="dark"] {
  --jp-layout-color0: #111;
  --jp-layout-color1: #212121;
  --jp-layout-color2: #424242;
  --jp-layout-color3: #616161;
  --jp-layout-color4: #757575;
  --jp-content-font-color0: rgba(255, 255, 255, 1);
  --jp-content-font-color1: rgba(255, 255, 255, 1);
  --jp-content-font-color2: rgba(255, 255, 255, 0.7);
  --jp-content-font-color3: rgba(255, 255, 255, 0.5);
  --jp-border-color0: #616161;
  --jp-border-color1: #616161;
  --jp-border-color2: #424242;
  --jp-border-color3: #212121;
  --jp-cell-editor-background: #212121;
  --jp-cell-editor-border-color: #616161;
  --jp-cell-editor-active-background: #111;
  --jp-cell-editor-active-border-color: #2196f3;
  --jp-input-background: #424242;
  --jp-input-hover-background: #424242;
  --jp-toolbar-background: #212121;
  --jp-toolbar-border-color: #424242;
  --jp-content-link-color: #64b5f6;
  --jp-brand-color0: #1976d2;
  --jp-brand-color1: #2196f3;
  --jp-brand-color2: #64b5f6;
  --jp-brand-color3: #bbdefb;
  --jp-mirror-editor-comment-color: #6a9955;
  --jp-mirror-editor-keyword-color: #4caf50;
  --jp-mirror-editor-string-color: #ff7070;
  --jp-mirror-editor-number-color: #66bb6a;
  --jp-mirror-editor-variable-color: #e0e0e0;
  --jp-mirror-editor-operator-color: #d48fff;
  --jp-mirror-editor-punctuation-color: #42a5f5;
  --jp-mirror-editor-error-color: #f00;
  --jp-ui-font-color0: rgba(255, 255, 255, 1);
  --jp-ui-font-color1: rgba(255, 255, 255, 0.87);
  --jp-ui-font-color2: rgba(255, 255, 255, 0.54);
  --jp-ui-font-color3: rgba(255, 255, 255, 0.38);
  --jp-rendermime-table-row-background: #212121;
  --jp-rendermime-table-row-hover-background: rgba(3, 169, 244, 0.2);
  --jp-rendermime-error-background: rgba(244, 67, 54, 0.28);
  --jp-scrollbar-background-color: #3f4244;
  --jp-warn-color0: #f57c00;
  --jp-error-color0: #d32f2f;
  --jp-success-color0: #388e3c;
  --jp-info-color0: #0097a7;
}

#rl-theme-toggle {
  display: none;
  position: fixed;
  top: 1rem;
  right: 1rem;
  width: 2.25rem;
  height: 2.25rem;
  align-items: center;
  justify-content: center;
  border: 1px solid #d1d9e0;
  border-radius: 999px;
  background: #f6f8fa;
  color: #1b1f24;
  cursor: pointer;
  z-index: 1000;
}
.js #rl-theme-toggle { display: inline-flex; }
#rl-theme-toggle svg { width: 1.1rem; height: 1.1rem; }
#rl-theme-toggle:hover { border-color: #0969da; }
#rl-theme-toggle .icon-sun { display: none; }
:root[data-theme="dark"] #rl-theme-toggle .icon-sun { display: block; }
:root[data-theme="dark"] #rl-theme-toggle .icon-moon { display: none; }
:root[data-theme="dark"] #rl-theme-toggle {
  background: #151b23;
  border-color: #3d444d;
  color: #e6edf3;
}
:root[data-theme="dark"] #rl-theme-toggle:hover { border-color: #4493f8; }
"""

# Runs synchronously in <head>, before the body paints, so the page never
# flashes the wrong theme.
INIT_JS = """
(function () {
  document.documentElement.classList.add("js");
  var KEY = "theme";
  function systemTheme() {
    try {
      return window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches
        ? "dark" : "light";
    } catch (e) {
      return "dark";
    }
  }
  var stored = null;
  try { stored = localStorage.getItem(KEY); } catch (e) {}
  document.documentElement.setAttribute("data-theme", stored || systemTheme());
})();
"""

TOGGLE_JS = """
(function () {
  var KEY = "theme";
  var btn = document.getElementById("rl-theme-toggle");
  if (!btn) return;
  btn.addEventListener("click", function () {
    var current = document.documentElement.getAttribute("data-theme") === "dark" ? "dark" : "light";
    var next = current === "dark" ? "light" : "dark";
    try { localStorage.setItem(KEY, next); } catch (e) {}
    document.documentElement.setAttribute("data-theme", next);
  });
  if (window.matchMedia) {
    try {
      window.matchMedia("(prefers-color-scheme: dark)").addEventListener("change", function (e) {
        var stored = null;
        try { stored = localStorage.getItem(KEY); } catch (err) {}
        if (!stored) document.documentElement.setAttribute("data-theme", e.matches ? "dark" : "light");
      });
    } catch (e) {}
  }
})();
"""

TOGGLE_BUTTON = """
<button id="rl-theme-toggle" type="button" aria-label="Toggle dark mode" title="Toggle dark mode">
  <svg class="icon-sun" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.93 4.93l1.41 1.41M17.66 17.66l1.41 1.41M2 12h2M20 12h2M4.93 19.07l1.41-1.41M17.66 6.34l1.41-1.41"/></svg>
  <svg class="icon-moon" viewBox="0 0 24 24" fill="currentColor"><path d="M20.5 14.7A8.5 8.5 0 0 1 9.3 3.5a.5.5 0 0 0-.6-.7A10 10 0 1 0 21.2 15.3a.5.5 0 0 0-.7-.6z"/></svg>
</button>
"""


def main(argv: list[str]) -> int:
    if len(argv) != 2:
        print("usage: apply_theme_toggle.py <rendered.html>", file=sys.stderr)
        return 2
    html_path = argv[1]

    try:
        from bs4 import BeautifulSoup
    except ImportError:
        print(
            "apply_theme_toggle: beautifulsoup4 is required "
            "(it ships with nbconvert; pip install -e './python[docs]')",
            file=sys.stderr,
        )
        return 1

    with open(html_path, encoding="utf-8") as fh:
        soup = BeautifulSoup(fh.read(), "html.parser")

    init_script = soup.new_tag("script")
    init_script.string = INIT_JS
    soup.head.insert(0, init_script)

    style = soup.new_tag("style")
    style.string = DARK_CSS
    soup.head.append(style)

    button = BeautifulSoup(TOGGLE_BUTTON, "html.parser")
    soup.body.insert(0, button)

    toggle_script = soup.new_tag("script")
    toggle_script.string = TOGGLE_JS
    soup.body.append(toggle_script)

    with open(html_path, "w", encoding="utf-8") as fh:
        fh.write(str(soup))

    print(f"apply_theme_toggle: added the theme toggle to {html_path}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
