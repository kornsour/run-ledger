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
"""

from .read import (
    LedgerError,
    LedgerUnreachableError,
    RunNotFoundError,
    run,
    runs,
    spread,
)
from ._run import DirtyTreeError, NoGitCommitError, Run, UnreconstructibleRunError

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
    "LedgerError",
    "LedgerUnreachableError",
    "RunNotFoundError",
]

__version__ = "0.1.0"
