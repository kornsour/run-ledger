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
    """The active accelerator's name; "cpu" if a library checked and found
    none; "" if neither library could be asked.

    ADR 0011: "" means "not recorded". Returning "cpu" when neither
    ``torch`` nor ``jax`` imports would assert an observation this process
    never made -- and it would be indistinguishable downstream from a run
    that genuinely imported one of them and got told there was no
    accelerator. Only the latter earns "cpu": at least one library
    imported and answered, and the answer was no.
    """
    observed = False
    try:
        import torch  # type: ignore

        observed = True
        if torch.cuda.is_available():
            return torch.cuda.get_device_name(0)
    except Exception:
        pass
    try:
        import jax  # type: ignore

        observed = True
        devices = jax.devices()
        if devices:
            return str(devices[0])
    except Exception:
        pass
    return "cpu" if observed else ""
