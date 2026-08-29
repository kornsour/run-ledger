"""Tests for runledger._provenance.device_name().

ADR 0011 makes "" mean "not recorded" for the scalar provenance fields, and
device_name() is the function that had not caught up: it returned the
literal "cpu" whether or not any library was actually asked, and -- one
level deeper -- whether or not a library that did import ever managed to
answer. A CUDA driver/runtime mismatch is a real case where
`torch.cuda.is_available()` or `get_device_name()` raises instead of
returning; that must read as "no answer", the same as torch not importing
at all, not as a fabricated "cpu" on a machine that may well have a GPU.

These tests use sys.modules substitution and MagicMock stand-ins so the
suite doesn't need torch or jax installed, and doesn't depend on the
machine it runs on actually having (or lacking) a GPU.
"""

from __future__ import annotations

import os
import sys
import unittest
from unittest import mock

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from runledger import _provenance  # noqa: E402


def _unimportable(name: str) -> mock._patch:
    """Make `import <name>` raise ImportError, whether or not it's installed.

    CPython's import machinery treats `sys.modules[name] is None` as a
    standing "this name is known not to import" marker and raises
    ImportError for it, so this forces the "could not find out" branch
    without needing torch or jax actually absent from the test environment.
    """
    return mock.patch.dict(sys.modules, {name: None})


class DeviceNameTests(unittest.TestCase):
    def test_neither_library_importable_returns_empty_string(self):
        with _unimportable("torch"), _unimportable("jax"):
            self.assertEqual(_provenance.device_name(), "")

    def test_torch_present_without_cuda_returns_cpu(self):
        fake_torch = mock.MagicMock()
        fake_torch.cuda.is_available.return_value = False
        with mock.patch.dict(sys.modules, {"torch": fake_torch}), _unimportable(
            "jax"
        ):
            self.assertEqual(_provenance.device_name(), "cpu")

    def test_unobserved_and_observed_cpu_are_distinguishable(self):
        # The whole point of the fix: these two cases must not collapse to
        # the same string, or a caller can no longer tell "no library
        # checked" apart from "checked, no accelerator found".
        with _unimportable("torch"), _unimportable("jax"):
            unobserved = _provenance.device_name()

        fake_torch = mock.MagicMock()
        fake_torch.cuda.is_available.return_value = False
        with mock.patch.dict(sys.modules, {"torch": fake_torch}), _unimportable(
            "jax"
        ):
            observed_cpu = _provenance.device_name()

        self.assertNotEqual(unobserved, observed_cpu)
        self.assertEqual(unobserved, "")
        self.assertEqual(observed_cpu, "cpu")

    def test_torch_reports_cuda_device_takes_priority(self):
        fake_torch = mock.MagicMock()
        fake_torch.cuda.is_available.return_value = True
        fake_torch.cuda.get_device_name.return_value = "NVIDIA H100"
        with mock.patch.dict(sys.modules, {"torch": fake_torch}), _unimportable(
            "jax"
        ):
            self.assertEqual(_provenance.device_name(), "NVIDIA H100")

    def test_is_available_raising_is_not_reported_as_cpu(self):
        # A CUDA driver/runtime mismatch makes is_available() raise on a
        # real GPU machine. That is a failed query, not an answer of "no
        # device" -- reporting "cpu" here would be the exact fabrication
        # the issue is about, just one level deeper than a missing import.
        fake_torch = mock.MagicMock()
        fake_torch.cuda.is_available.side_effect = RuntimeError(
            "CUDA driver/runtime version mismatch"
        )
        with mock.patch.dict(sys.modules, {"torch": fake_torch}), _unimportable(
            "jax"
        ):
            self.assertEqual(_provenance.device_name(), "")

    def test_get_device_name_raising_after_is_available_true_is_not_cpu(self):
        # is_available() answered True (a device exists), but the follow-up
        # query to name it raised -- also a failed query, and also not
        # grounds to fall back to "cpu": there is a device, it's simply
        # unnamed, so nothing licenses "cpu" as the answer either.
        fake_torch = mock.MagicMock()
        fake_torch.cuda.is_available.return_value = True
        fake_torch.cuda.get_device_name.side_effect = RuntimeError(
            "CUDA error: no kernel image is available for execution"
        )
        with mock.patch.dict(sys.modules, {"torch": fake_torch}), _unimportable(
            "jax"
        ):
            self.assertEqual(_provenance.device_name(), "")

    def test_torch_query_failure_falls_through_to_a_working_jax(self):
        # torch importing but failing to answer shouldn't block jax from
        # being asked -- the two libraries are independent sources, and a
        # failed torch query is not evidence about what jax would say.
        fake_torch = mock.MagicMock()
        fake_torch.cuda.is_available.side_effect = RuntimeError("driver mismatch")
        fake_jax = mock.MagicMock()
        fake_jax.devices.return_value = ["TPU v4"]
        with mock.patch.dict(
            sys.modules, {"torch": fake_torch, "jax": fake_jax}
        ):
            self.assertEqual(_provenance.device_name(), "TPU v4")


if __name__ == "__main__":
    unittest.main()
