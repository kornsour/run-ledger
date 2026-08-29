"""Tests for runledger.hash_dataset().

All against real tmp directories (tempfile.mkdtemp), not mocks -- the whole
point of this helper is what actually happens on a real filesystem: walk
order, path separators, and where a symlink shows up.
"""

from __future__ import annotations

import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import runledger  # noqa: E402
from runledger.content_hash import hash_dataset, SymlinkNotSupportedError  # noqa: E402


def _write(root: str, rel: str, content: bytes) -> None:
    full = os.path.join(root, rel)
    os.makedirs(os.path.dirname(full), exist_ok=True)
    with open(full, "wb") as fh:
        fh.write(content)


class HashDatasetTest(unittest.TestCase):
    def setUp(self):
        self._tmpdirs = []

    def tearDown(self):
        for d in self._tmpdirs:
            # ignore_errors: a test that raises partway through symlink
            # setup can leave a dangling link tempfile's own cleanup trips
            # over on some platforms; the tmp dir itself is disposable.
            import shutil

            shutil.rmtree(d, ignore_errors=True)

    def _mkdir(self) -> str:
        d = tempfile.mkdtemp(prefix="runledger-hash-test-")
        self._tmpdirs.append(d)
        return d

    def test_exported_from_top_level_package(self):
        # hash_dataset and its error are meant to be reached as
        # runledger.hash_dataset(...), not by importing the submodule.
        self.assertIs(runledger.hash_dataset, hash_dataset)
        self.assertIs(runledger.SymlinkNotSupportedError, SymlinkNotSupportedError)

    def test_same_content_same_hash(self):
        a = self._mkdir()
        b = self._mkdir()
        for root in (a, b):
            _write(root, "images/cat.png", b"cat-bytes")
            _write(root, "labels.csv", b"cat,1\ndog,0\n")

        self.assertEqual(hash_dataset(a), hash_dataset(b))

    def test_different_content_different_hash(self):
        a = self._mkdir()
        b = self._mkdir()
        _write(a, "labels.csv", b"cat,1\ndog,0\n")
        _write(b, "labels.csv", b"cat,1\ndog,1\n")  # one byte differs

        self.assertNotEqual(hash_dataset(a), hash_dataset(b))

    def test_different_size_different_hash(self):
        a = self._mkdir()
        b = self._mkdir()
        _write(a, "data.bin", b"x" * 100)
        _write(b, "data.bin", b"x" * 101)

        self.assertNotEqual(hash_dataset(a), hash_dataset(b))

    def test_file_order_irrelevant(self):
        # os.walk's per-directory listing order is filesystem-dependent
        # and not something a test can force directly, so this asserts
        # the property the implementation is supposed to guarantee
        # (sorting before hashing) by building two trees whose files are
        # written in opposite order and checking they still agree.
        a = self._mkdir()
        b = self._mkdir()
        for rel, content in [("a.txt", b"1"), ("m.txt", b"2"), ("z.txt", b"3")]:
            _write(a, rel, content)
        for rel, content in reversed(
            [("a.txt", b"1"), ("m.txt", b"2"), ("z.txt", b"3")]
        ):
            _write(b, rel, content)

        self.assertEqual(hash_dataset(a), hash_dataset(b))

    def test_path_location_irrelevant(self):
        # The same tree, nested at different depths / parent names, must
        # hash the same -- only paths relative to the hashed root count.
        a = self._mkdir()
        nested = os.path.join(self._mkdir(), "some", "deeper", "parent")
        os.makedirs(nested)
        for root in (a, nested):
            _write(root, "train/part-0.parquet", b"parquet-bytes")
            _write(root, "README.txt", b"about this dataset")

        self.assertEqual(hash_dataset(a), hash_dataset(nested))

    def test_renaming_a_file_changes_the_hash(self):
        # A relative path is part of the manifest, so renaming a file is a
        # real change to what the dataset is, not noise to ignore.
        a = self._mkdir()
        b = self._mkdir()
        _write(a, "train.csv", b"same-bytes")
        _write(b, "training.csv", b"same-bytes")

        self.assertNotEqual(hash_dataset(a), hash_dataset(b))

    def test_nested_directories(self):
        a = self._mkdir()
        _write(a, "train/images/0001.png", b"one")
        _write(a, "train/images/0002.png", b"two")
        _write(a, "train/labels.csv", b"labels")
        _write(a, "val/images/0001.png", b"three")

        # Just needs to run without raising and be stable across calls.
        self.assertEqual(hash_dataset(a), hash_dataset(a))

    def test_empty_directory_hashes_to_a_fixed_defined_value(self):
        a = self._mkdir()
        b = self._mkdir()

        digest = hash_dataset(a)
        self.assertEqual(len(digest), 64)  # hex sha256
        self.assertEqual(digest, hash_dataset(b))

    def test_directory_of_only_empty_subdirectories_matches_empty(self):
        a = self._mkdir()
        os.makedirs(os.path.join(a, "empty", "also-empty"))
        b = self._mkdir()

        self.assertEqual(hash_dataset(a), hash_dataset(b))

    def test_missing_path_raises_file_not_found(self):
        a = self._mkdir()
        with self.assertRaises(FileNotFoundError):
            hash_dataset(os.path.join(a, "does-not-exist"))

    def test_symlinked_file_raises(self):
        a = self._mkdir()
        _write(a, "real.bin", b"content")
        link = os.path.join(a, "link.bin")
        try:
            os.symlink(os.path.join(a, "real.bin"), link)
        except (OSError, NotImplementedError):
            self.skipTest("symlinks not supported on this filesystem/platform")

        with self.assertRaises(SymlinkNotSupportedError):
            hash_dataset(a)

    def test_symlinked_directory_raises(self):
        a = self._mkdir()
        target = self._mkdir()
        _write(target, "file.bin", b"content")
        link = os.path.join(a, "link-dir")
        try:
            os.symlink(target, link, target_is_directory=True)
        except (OSError, NotImplementedError):
            self.skipTest("symlinks not supported on this filesystem/platform")

        with self.assertRaises(SymlinkNotSupportedError):
            hash_dataset(a)

    def test_root_itself_a_symlink_raises(self):
        target = self._mkdir()
        _write(target, "file.bin", b"content")
        parent = self._mkdir()
        link = os.path.join(parent, "link-root")
        try:
            os.symlink(target, link, target_is_directory=True)
        except (OSError, NotImplementedError):
            self.skipTest("symlinks not supported on this filesystem/platform")

        with self.assertRaises(SymlinkNotSupportedError):
            hash_dataset(link)

    def test_single_file_path(self):
        a = self._mkdir()
        _write(a, "only.bin", b"just one file")

        digest = hash_dataset(os.path.join(a, "only.bin"))
        self.assertEqual(len(digest), 64)

    def test_single_file_matches_directory_containing_only_that_file(self):
        a = self._mkdir()
        _write(a, "only.bin", b"just one file")
        b = self._mkdir()
        _write(b, "only.bin", b"just one file")

        self.assertEqual(hash_dataset(os.path.join(a, "only.bin")), hash_dataset(b))

    def test_large_file_is_streamed_not_read_whole(self):
        # Exercise the chunked read path with a small chunk_size so this
        # test doesn't need an actually enormous file to prove streaming
        # is happening, then check two files whose bytes differ only past
        # the first chunk are still told apart.
        a = self._mkdir()
        b = self._mkdir()
        _write(a, "big.bin", b"x" * 10_000 + b"A")
        _write(b, "big.bin", b"x" * 10_000 + b"B")

        self.assertNotEqual(
            hash_dataset(a, chunk_size=64), hash_dataset(b, chunk_size=64)
        )
        # And chunk_size is purely a performance knob -- it must not affect
        # the result for identical content.
        _write(a, "same.bin", b"y" * 5_000)
        _write(b, "same.bin", b"y" * 5_000)
        self.assertEqual(
            hash_dataset(a, chunk_size=1), hash_dataset(a, chunk_size=1 << 20)
        )


if __name__ == "__main__":
    unittest.main()
