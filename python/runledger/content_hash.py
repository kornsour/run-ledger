"""hash_dataset(): content-address a dataset so dataset_version can be derived.

Design note -- why this exists
-------------------------------
``dataset_version`` (and ``model_version``, ``config_hash``) are free strings
a caller supplies; the server hashes them into the fingerprint exactly as
given and has no way to check that a run labelled ``"v1"`` saw the same bytes
as another run labelled ``"v1"`` -- it never sees the dataset at all, only
the label. That is not a gap the server can close: ADR 0001's reasoning
against trusting a caller-asserted *run* fingerprint does not extend here,
because the caller is the only party who can observe the dataset in the
first place. See the root README's note on the ``unattributable`` verdict
for what this costs -- a relabeled or re-exported dataset under an unchanged
label produces the identical signature as real nondeterminism.

``hash_dataset()`` gives a caller who wants the label to mean something a way
to derive it instead of typing it: point it at a file or directory and get
back a hash of its content, so two runs only share a ``dataset_version`` when
they provably saw the same bytes, and relabeling a snapshot without touching
its contents does not change the hash.

This is opt-in and entirely client-side. Nothing here touches the wire
schema, ``internal/lineage``, or the fingerprint contract in ADR 0004 -- the
server goes on storing whatever string it is given, unchanged. Calling this
is a choice a caller makes instead of typing ``dataset_version="v1"`` by
hand; nothing in the client or the server requires it.

Design note -- what is in the manifest, and what is deliberately not
-----------------------------------------------------------------------
The hash is over a manifest of ``(relative path, size, content digest)`` per
file, sorted by path. Specifically excluded, on purpose:

- **Absolute paths.** The same tree checked out to two different locations,
  or on two different machines, must hash the same -- only the path
  *relative to the root being hashed* goes into the manifest.
- **mtimes, permissions, ownership.** These change on copy, checkout, or a
  bare ``chmod`` without the bytes changing, and would make the hash flap
  for reasons that have nothing to do with the data.
- **Directory entries themselves.** An empty directory contributes nothing
  to the manifest -- only files carry content, so a tree that is nothing but
  empty directories hashes the same as a genuinely empty one.
- **Symlinks.** Resolving one is ambiguous input for a content hash: does
  the manifest describe the link or its target, and what happens when the
  target lives outside the tree, differs in size on another machine, or is
  simply broken there? Rather than guess, ``hash_dataset()`` refuses --
  see :class:`SymlinkNotSupportedError`.

File content is hashed streaming, ``chunk_size`` bytes at a time, so a
multi-gigabyte file is never read into memory whole.

Manifest entries are sorted by relative path before hashing, so the walk
order ``os.walk`` happens to produce on a given filesystem (which is
unspecified, and differs across platforms) cannot change the result -- see
``sorted(...)`` in :func:`hash_dataset` below.
"""

from __future__ import annotations

import hashlib
import os
from pathlib import Path
from typing import Iterator, Tuple, Union

PathLike = Union[str, "os.PathLike[str]"]

DEFAULT_CHUNK_SIZE = 1 << 20  # 1 MiB: large enough to be efficient, small
# enough that hashing a multi-gigabyte file never holds more than one chunk
# in memory at a time.

# Folded into every hash so a future change to what the manifest contains,
# or how entries are delimited, is never silently indistinguishable from a
# change in the data itself -- the same reason ADR 0004 versions the
# server's fingerprint input. There is no migration path implied by this:
# a caller who upgrades and gets a different digest for the same bytes was
# always going to, since the digest was never a promise to stay stable
# across releases of this helper -- only across machines and directory
# listing orders for one given release.
_MANIFEST_VERSION = b"runledger-dataset-hash-v1"


class SymlinkNotSupportedError(ValueError):
    """``hash_dataset()`` found a symlink and refuses to guess what it means.

    A symlink is exactly the case where "the data" is ambiguous -- the link
    itself, or whatever it currently resolves to, possibly outside the tree
    being hashed and possibly not even present on another machine. Resolve
    it to a real file, copy the target into the tree, or exclude the path,
    whichever actually reflects what the dataset is -- then hash that.
    """


def hash_dataset(path: PathLike, *, chunk_size: int = DEFAULT_CHUNK_SIZE) -> str:
    """Content-addresses a file or directory tree; returns a hex digest.

    Deterministic across machines and across runs on the same machine: two
    trees with the same files (same relative paths, same bytes) hash the
    same regardless of where they live on disk, what order the filesystem
    happens to list them in, or what their mtimes/permissions are. Two
    trees that differ in any file's content, size, name, or relative
    location hash differently.

    An empty directory hashes to a fixed, defined value (the manifest is
    simply empty) rather than raising -- "the dataset has no files" is a
    legitimate, representable state, distinct from "the path does not
    exist".

    :param path: File or directory to hash.
    :param chunk_size: Bytes read per chunk while hashing file content.
        The default (1 MiB) bounds peak memory regardless of file size;
        raise it for a modest speed gain on very large files at the cost of
        a larger read buffer.
    :raises FileNotFoundError: ``path`` does not exist.
    :raises SymlinkNotSupportedError: a symlink -- ``path`` itself, or
        anything under it -- was found.
    """
    root = Path(path)
    if root.is_symlink():
        raise SymlinkNotSupportedError(str(root))
    if not root.exists():
        raise FileNotFoundError(f"hash_dataset: no such path: {path}")

    manifest = hashlib.sha256()
    manifest.update(_MANIFEST_VERSION)

    # sorted() forces the whole generator to run before hashing starts, so
    # every file is hashed and its (path, size, digest) known before the
    # manifest order is fixed -- entries then go into the running hash in
    # relative-path order, independent of the filesystem's own walk order.
    for rel, size, digest in sorted(_entries(root, chunk_size=chunk_size)):
        rel_bytes = rel.encode("utf-8")
        # Length-prefixed so ("ab", "c...") and ("a", "bc...") cannot
        # collide by simple concatenation -- the same reasoning
        # internal/lineage's Compute uses for the server-side fingerprint
        # (see the root README's "Hashed fields are length-prefixed" note).
        manifest.update(len(rel_bytes).to_bytes(8, "big"))
        manifest.update(rel_bytes)
        manifest.update(size.to_bytes(8, "big"))
        manifest.update(digest.encode("ascii"))

    return manifest.hexdigest()


def _entries(root: Path, *, chunk_size: int) -> Iterator[Tuple[str, int, str]]:
    """Yields (relative posix path, size, hex digest) for every file under root.

    ``root`` itself may be a single file, so a caller can content-address
    one file (e.g. a single parquet snapshot) the same way as a directory.
    """
    if root.is_file():
        yield _hash_file(root, root.name, chunk_size)
        return

    # followlinks=False so os.walk never descends into a symlinked
    # directory; that alone would silently drop everything under it from
    # the manifest, which is exactly the kind of quiet omission this
    # function refuses to make -- the explicit checks below turn that
    # silent skip into a raised SymlinkNotSupportedError instead.
    for dirpath, dirnames, filenames in os.walk(root, followlinks=False):
        for name in dirnames:
            if (Path(dirpath) / name).is_symlink():
                raise SymlinkNotSupportedError(str(Path(dirpath) / name))
        for name in filenames:
            full = Path(dirpath) / name
            if full.is_symlink():
                raise SymlinkNotSupportedError(str(full))
            rel = full.relative_to(root).as_posix()
            yield _hash_file(full, rel, chunk_size)


def _hash_file(full: Path, rel: str, chunk_size: int) -> Tuple[str, int, str]:
    h = hashlib.sha256()
    size = 0
    with open(full, "rb") as fh:
        for chunk in iter(lambda: fh.read(chunk_size), b""):
            size += len(chunk)
            h.update(chunk)
    return (rel, size, h.hexdigest())
