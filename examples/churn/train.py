"""A small churn model: the thing being experimented on.

Pure standard library, on purpose. The ledger's pitch is "one binary, no
external dependencies to try it," and an example that needs a numpy/sklearn
install before it demonstrates anything undercuts that.

This module contains one *real* reproducibility bug, in `split_indices`. It
is not decoration: the whole point of the example is that run-ledger finds
it from the record alone, without being told it is there.
"""

from __future__ import annotations

import math
import random
from typing import Dict, List, Sequence, Tuple

FEATURES = ("tenure_months", "monthly_charges", "support_tickets")

Row = Tuple[List[float], int]


def make_dataset(n: int = 1000, *, dataset_seed: int = 20260801) -> List[Row]:
    """Synthetic monthly-subscription churn data.

    Seeded from a fixed constant, and that constant is what the run records
    as `dataset_version`: the dataset is a stable, named input to the
    experiment, not a source of run-to-run variation. If this were real data
    the version would be a snapshot id or a content hash of the extract.
    """
    rng = random.Random(dataset_seed)
    rows: List[Row] = []
    for _ in range(n):
        tenure = rng.uniform(0.0, 72.0)
        charges = rng.uniform(20.0, 120.0)
        tickets = rng.expovariate(1 / 2.0)
        # Latent churn propensity: long tenure protects, high charges and
        # support contact predict churn.
        z = -1.2 - 0.045 * tenure + 0.022 * charges + 0.35 * tickets
        p = 1.0 / (1.0 + math.exp(-z))
        rows.append(([tenure, charges, tickets], 1 if rng.random() < p else 0))
    return rows


def split_indices(n: int, *, split_seed: int | None) -> Tuple[List[int], List[int]]:
    """Partition row indices into train and test.

    `split_seed=None` is the bug this example exists to demonstrate. A
    `random.Random(None)` seeds itself from OS entropy, so every run trains
    and evaluates on a *different* partition of the same data -- while the
    run's recorded identity (project, commit, config hash, dataset, model,
    seed, params) stays byte-for-byte identical, because nothing in the
    record mentions the split at all.

    That is the exact shape of failure the ledger is built to catch: the
    numbers move and the record says they shouldn't have.
    """
    rng = random.Random(split_seed)
    idx = list(range(n))
    rng.shuffle(idx)
    cut = int(n * 0.75)
    return idx[:cut], idx[cut:]


def _standardizer(rows: Sequence[Row], idx: Sequence[int]):
    """Per-feature mean/stddev, fit on the training fold only."""
    k = len(FEATURES)
    means = [0.0] * k
    for i in idx:
        for j in range(k):
            means[j] += rows[i][0][j]
    means = [m / len(idx) for m in means]
    var = [0.0] * k
    for i in idx:
        for j in range(k):
            d = rows[i][0][j] - means[j]
            var[j] += d * d
    stds = [math.sqrt(v / len(idx)) or 1.0 for v in var]

    def apply(x: Sequence[float]) -> List[float]:
        return [(x[j] - means[j]) / stds[j] for j in range(k)]

    return apply


def _sigmoid(z: float) -> float:
    # Branch to avoid math.exp overflow on large-magnitude logits.
    if z >= 0:
        return 1.0 / (1.0 + math.exp(-z))
    e = math.exp(z)
    return e / (1.0 + e)


def train(
    rows: Sequence[Row],
    train_idx: Sequence[int],
    *,
    seed: int,
    lr: float,
    epochs: int,
) -> Tuple[List[float], float, object]:
    """Full-batch gradient descent on logistic regression.

    `seed` initializes the weights -- and it is the seed the run records.
    Note what it does *not* cover: the data split. A reader of the lineage
    record sees "seed=7" and reasonably assumes the run is pinned.
    """
    norm = _standardizer(rows, train_idx)
    rng = random.Random(seed)
    w = [rng.gauss(0.0, 0.01) for _ in FEATURES]
    b = 0.0
    n = len(train_idx)
    for _ in range(epochs):
        gw = [0.0] * len(FEATURES)
        gb = 0.0
        for i in train_idx:
            x = norm(rows[i][0])
            err = _sigmoid(sum(wj * xj for wj, xj in zip(w, x)) + b) - rows[i][1]
            for j in range(len(FEATURES)):
                gw[j] += err * x[j]
            gb += err
        for j in range(len(FEATURES)):
            w[j] -= lr * gw[j] / n
        b -= lr * gb / n
    return w, b, norm


def evaluate(rows, test_idx, w, b, norm) -> Dict[str, float]:
    """Test-set AUC and log loss."""
    scores, labels = [], []
    for i in test_idx:
        x = norm(rows[i][0])
        scores.append(_sigmoid(sum(wj * xj for wj, xj in zip(w, x)) + b))
        labels.append(rows[i][1])
    return {"auc": _auc(scores, labels), "log_loss": _log_loss(scores, labels)}


def _auc(scores: Sequence[float], labels: Sequence[int]) -> float:
    """Rank-based (Mann-Whitney) AUC, with average ranks for ties."""
    pos = sum(labels)
    neg = len(labels) - pos
    if pos == 0 or neg == 0:
        return float("nan")
    order = sorted(range(len(scores)), key=lambda i: scores[i])
    ranks = [0.0] * len(scores)
    i = 0
    while i < len(order):
        j = i
        while j + 1 < len(order) and scores[order[j + 1]] == scores[order[i]]:
            j += 1
        avg = (i + j) / 2.0 + 1.0
        for k in range(i, j + 1):
            ranks[order[k]] = avg
        i = j + 1
    rank_sum = sum(r for r, y in zip(ranks, labels) if y == 1)
    return (rank_sum - pos * (pos + 1) / 2.0) / (pos * neg)


def _log_loss(scores: Sequence[float], labels: Sequence[int]) -> float:
    eps = 1e-12
    total = 0.0
    for p, y in zip(scores, labels):
        p = min(max(p, eps), 1 - eps)
        total += -(y * math.log(p) + (1 - y) * math.log(1 - p))
    return total / len(labels)


def run_experiment(
    *, seed: int, lr: float, epochs: int, split_seed: int | None, n: int = 1000
) -> Dict[str, float]:
    """One end-to-end training run. Returns the metrics it measured."""
    rows = make_dataset(n)
    train_idx, test_idx = split_indices(len(rows), split_seed=split_seed)
    w, b, norm = train(rows, train_idx, seed=seed, lr=lr, epochs=epochs)
    return evaluate(rows, test_idx, w, b, norm)
