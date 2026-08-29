"""runledger — record an experiment run's lineage from inside training code.

The alternative is shelling out to ``rlctl`` or hand-rolling an HTTP call,
which puts the friction in exactly the wrong place: the moment the lineage
(git commit, seed, params, framework, device) is cheaply available is inside
the training script itself.

    import runledger

    with runledger.Run.start(project="demo", seed=1, params={"lr": 3e-4}) as run:
        for step in range(steps):
            loss = train_step()
            run.log_metric("loss", loss)

See the package README (``python/README.md``) for the full worked example,
including what happens when the loop raises partway through.

Reading it back is the other half, and lives beside it:

    for group in runledger.spread(project="demo"):
        print(group["fingerprint"], group["metrics"])

Note the asymmetry in how the two halves fail. Writing degrades to a warning
and a local spool file, because recording lineage must never fail an
expensive training job. Reading raises, because silently returning nothing
when the ledger is down answers "how did my experiments do?" with "they
didn't" -- a worse outcome than an exception.

Getting a spool back into the ledger is a third thing, done later and on
purpose, and lives in ``replay``:

    result = runledger.replay_spool()
    print(result.sent, result.rejected, result.remaining)

or from a shell: ``python -m runledger.replay``. See ``replay.py`` for why
this is safe to interrupt and re-run.

``dataset_version`` is a free string the ledger never verifies; a caller who
wants it to mean something can derive it from the data instead of typing it:

    digest = runledger.hash_dataset("data/train")
    with runledger.Run.start(project="demo", dataset_version=digest) as run:
        ...

Opt-in and entirely client-side -- see ``content_hash.py``.
"""

from .read import (
    LedgerError,
    LedgerUnreachableError,
    RunNotFoundError,
    compare,
    run,
    runs,
    spread,
)
from ._run import DirtyTreeError, NoGitCommitError, Run, UnreconstructibleRunError
from .content_hash import SymlinkNotSupportedError, hash_dataset
from .replay import ReplayResult, replay_spool

__all__ = [
    # writing
    "Run",
    "UnreconstructibleRunError",
    "NoGitCommitError",
    "DirtyTreeError",
    # reading
    "runs",
    "run",
    "spread",
    "compare",
    "LedgerError",
    "LedgerUnreachableError",
    "RunNotFoundError",
    # recovering a spool
    "replay_spool",
    "ReplayResult",
    # deriving dataset_version
    "hash_dataset",
    "SymlinkNotSupportedError",
]

__version__ = "0.1.0"
