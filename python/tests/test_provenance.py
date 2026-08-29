"""Tests for runledger._provenance.device_name().

ADR 0011 makes "" mean "not recorded" for the scalar provenance fields, and
device_name() is the one function that had not caught up: it returned the
literal "cpu" whether or not any library was actually asked. These tests
pin the two cases apart -- neither library importable ("") vs. torch
imported and reporting no CUDA device ("cpu") -- using sys.modules
substitution so the suite doesn't need torch or jax installed.
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


if __name__ == "__main__":
    unittest.main()
