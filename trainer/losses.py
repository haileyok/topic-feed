"""The student's training loss, split into its parts so training and reports log the same
numbers. Weights match train.py's objective: broad + path + 0.5 * signals + 0.3 * tone,
each example weighted by Jev's broad confidence (floored)."""

import torch
import torch.nn.functional as F

WEIGHTS = {"broad": 1.0, "path": 1.0, "signals": 0.5, "tone": 0.3}


def soft_ce(logits, target):
    return -(target * F.log_softmax(logits.float(), -1)).sum(-1)


def parts(lb, lp, ls, lt, yb, yp, ys, yt) -> dict[str, torch.Tensor]:
    """Per-example loss for each head, unweighted."""
    return {
        "broad": soft_ce(lb, yb),
        "path": soft_ce(lp, yp),
        "signals": F.binary_cross_entropy_with_logits(ls.float(), ys, reduction="none").mean(-1),
        "tone": soft_ce(lt, yt),
    }


def total(p: dict[str, torch.Tensor], w: torch.Tensor) -> torch.Tensor:
    """The training objective: weighted sum of the parts, averaged with example weights w."""
    per = sum(WEIGHTS[k] * v for k, v in p.items())
    return (per * w).sum() / w.sum()
