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
"""

from .run import DirtyTreeError, NoGitCommitError, Run, UnreconstructibleRunError

__all__ = [
    "Run",
    "UnreconstructibleRunError",
    "NoGitCommitError",
    "DirtyTreeError",
]

__version__ = "0.1.0"
