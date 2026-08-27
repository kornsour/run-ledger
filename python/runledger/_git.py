"""Git context capture, mirroring cmd/rlctl's gitContext().

Kept in its own module so tests can monkeypatch context() without touching
an actual git repository.
"""

from __future__ import annotations

import subprocess
from typing import Optional, Tuple


def context(cwd: Optional[str] = None) -> Tuple[str, bool]:
    """Returns (commit, dirty), the same two fields cmd/rlctl captures.

    Any failure to invoke git (not installed, not a repository, timeout)
    reports an empty commit -- the caller decides whether that is fatal, the
    same way rlctl's ``record`` command does.
    """

    def run(*args: str) -> str:
        try:
            proc = subprocess.run(
                ["git", *args],
                cwd=cwd,
                capture_output=True,
                text=True,
                timeout=10,
            )
        except (OSError, subprocess.SubprocessError):
            return ""
        if proc.returncode != 0:
            return ""
        return proc.stdout.strip()

    commit = run("rev-parse", "HEAD")
    dirty = run("status", "--porcelain") != ""
    return commit, dirty
