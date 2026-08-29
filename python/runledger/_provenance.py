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
    """The active accelerator's name; "cpu" if a library was asked and
    answered "none"; "" if nothing could be asked, or the asking itself
    failed.

    ADR 0011: "" means "not recorded". Returning "cpu" whenever neither
    library imports would assert an observation this process never made.
    The same fabrication can happen one level deeper than "did it import":
    a library can import fine and still fail to answer -- a CUDA
    driver/runtime mismatch is a real-world case where ``is_available()``
    or ``get_device_name()`` raises instead of returning. That failure
    carries no information about the device either, and is exactly the
    case a broken-driver machine hits, so it must not read as "cpu" any
    more than a missing import does.

    "cpu" is earned only at the point a query *returns* an answer of "no
    accelerator" -- ``torch.cuda.is_available()`` returning ``False``, or
    ``jax.devices()`` returning empty -- not merely at import. Import
    failure and query failure both fall through to the next library, and
    if nothing ever returns an answer, the result is "".
    """
    observed = False

    try:
        import torch  # type: ignore
    except Exception:
        torch = None  # type: ignore

    if torch is not None:
        try:
            if torch.cuda.is_available():
                return torch.cuda.get_device_name(0)
            observed = True  # asked, and it answered: no CUDA device
        except Exception:
            pass  # asked, but the query itself failed -- no answer to trust

    try:
        import jax  # type: ignore
    except Exception:
        jax = None  # type: ignore

    if jax is not None:
        try:
            devices = jax.devices()
            if devices:
                return str(devices[0])
            observed = True  # asked, and it answered: no device
        except Exception:
            pass  # asked, but the query itself failed -- no answer to trust

    return "cpu" if observed else ""
