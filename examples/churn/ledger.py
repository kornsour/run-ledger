"""A minimal, standard-library ledger client for this example.

`python/runledger` is the real client and is what training code should use.
This example deliberately does not import it, for two reasons:

1. It would need `pip install -e ./python` before the example demonstrated
   anything, and the point of the example is to be runnable immediately.
2. `Run.start()` captures device and framework provenance automatically.
   This example needs to *withhold* some of that, on purpose, to show what
   an under-captured record looks like -- see scenario D.

So this is a thin POST wrapper, not a second implementation of anything.
"""

from __future__ import annotations

import hashlib
import json
import os
import subprocess
import urllib.error
import urllib.request
from datetime import datetime, timezone
from typing import Any, Dict, Optional

DEFAULT_SERVER = "http://localhost:8080"


def server() -> str:
    return os.environ.get("RUNLEDGER_ADDR", DEFAULT_SERVER).rstrip("/")


def _git(*args: str) -> str:
    try:
        return subprocess.run(
            ["git", *args], capture_output=True, text=True, check=True
        ).stdout.strip()
    except (OSError, subprocess.CalledProcessError):
        return ""


def git_commit() -> str:
    return _git("rev-parse", "HEAD")


def git_dirty() -> bool:
    return bool(_git("status", "--porcelain"))


def config_hash(config: Dict[str, Any]) -> str:
    """Content hash of the run's configuration.

    This is the fix for the bug the example demonstrates, and it is worth
    being explicit about why: a knob that is not in this dict is not in the
    fingerprint, and a knob that is not in the fingerprint cannot explain a
    difference between two runs. "Put the knob in the config and hash the
    config" is what turns an unrecorded variable into a recorded one.

    Sorted keys, because two configs that differ only in key order are the
    same config -- the same reason lineage.Run.Compute sorts params.
    """
    canonical = json.dumps(config, sort_keys=True, separators=(",", ":"))
    return hashlib.sha256(canonical.encode()).hexdigest()[:16]


def record(
    *,
    project: str,
    seed: int,
    params: Dict[str, Any],
    cfg_hash: str,
    metrics: Dict[str, float],
    dataset_version: str = "",
    model_version: str = "",
    device: str = "",
    framework_version: str = "",
    host: str = "",
    status: str = "succeeded",
) -> str:
    """POST one finished run. Returns its server-assigned run id.

    Every field here is sent as a string or number the server understands;
    the server assigns `run_id` and `fingerprint` itself (ADR 0001), so
    neither appears in this body.
    """
    now = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%fZ")
    body: Dict[str, Any] = {
        "project": project,
        "git_commit": git_commit(),
        "git_dirty": git_dirty(),
        "config_hash": cfg_hash,
        "dataset_version": dataset_version,
        "model_version": model_version,
        "seed": seed,
        "params": {k: str(v) for k, v in params.items()},
        "host": host,
        "device": device,
        "framework_version": framework_version,
        "status": status,
        "started_at": now,
        "ended_at": now,
        "metrics": metrics,
    }
    req = urllib.request.Request(
        f"{server()}/v1/runs",
        data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            return json.load(resp)["run_id"]
    except urllib.error.HTTPError as exc:
        raise RuntimeError(f"ledger refused the run: {exc.read().decode()}") from exc


def get(path: str) -> Any:
    with urllib.request.urlopen(f"{server()}{path}", timeout=10) as resp:
        return json.load(resp)


def compare(a: str, b: str) -> Dict[str, Any]:
    return get(f"/v1/comparisons?a={a}&b={b}")


def fingerprints(project: str) -> Any:
    return get(f"/v1/fingerprints?project={project}")


def runs(project: str, limit: int = 500) -> Any:
    return get(f"/v1/runs?project={project}&limit={limit}")


def wait_until_up(timeout: float = 10.0) -> None:
    """Block until the ledger answers /healthz, or give a useful error."""
    import time

    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            urllib.request.urlopen(f"{server()}/healthz", timeout=1).read()
            return
        except OSError:
            time.sleep(0.2)
    raise SystemExit(
        f"no ledger at {server()}\n"
        "  start one with:  make build && ./bin/runledger &\n"
        "  or run the whole example with:  make example"
    )
