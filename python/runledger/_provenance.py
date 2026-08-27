"""Framework and device provenance -- cheap and unambiguous only.

Only ``torch`` and ``jax`` are recognized, and only via their own
``__version__`` / device APIs -- never guessed or inferred from, say,
installed packages the run never touched.
"""

from __future__ import annotations


def framework_version() -> str:
    """Best-effort ``"torch X, jax Y"`` for whichever of the two import."""
    parts = []
    try:
        import torch  # type: ignore

        parts.append(f"torch {torch.__version__}")
    except Exception:
        pass
    try:
        import jax  # type: ignore

        parts.append(f"jax {jax.__version__}")
    except Exception:
        pass
    return ", ".join(parts)


def device_name() -> str:
    """The active accelerator's name, or "cpu" if none is visible."""
    try:
        import torch  # type: ignore

        if torch.cuda.is_available():
            return torch.cuda.get_device_name(0)
    except Exception:
        pass
    try:
        import jax  # type: ignore

        devices = jax.devices()
        if devices:
            return str(devices[0])
    except Exception:
        pass
    return "cpu"
