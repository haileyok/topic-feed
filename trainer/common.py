"""Shared data loading, targets, splits, and metrics for the topic classifier (plan §11).

Label spaces come from the taxonomy YAML:
  - broad: every broad topic id, in taxonomy order
  - paths: "broad/sub" for every subtopic, plus the bare broad id for broad topics
    without subtopics (e.g. "unclear"), so every post has a most likely path

Targets are Jev's full distributions (distillation, plan D8):
  - broad: Jev's broad-topic probabilities
  - path: the joint P(broad) * P(sub | broad) over the pairs Jev scored, plus P(broad)
    for broad topics without subtopics, renormalized
  - signals: substance/news/promo/general_interest in 0..1, and the tone distribution
"""

from __future__ import annotations

import gzip
import json
from dataclasses import dataclass

import numpy as np
import yaml

SIGNALS = ["substance", "news", "promo", "general_interest"]
TONES = ["informative", "humorous", "personal", "outraged", "supportive", "other"]
TEST_WINDOWS, VAL_WINDOWS = 3, 3  # newest 3 windows test, the 3 before them validation


@dataclass
class Space:
    version: str
    broad: list[str]
    paths: list[str]
    path_broad: np.ndarray  # index into broad for each path

    @classmethod
    def from_taxonomy(cls, path: str) -> "Space":
        t = yaml.safe_load(open(path))
        broad, paths, pb = [], [], []
        for i, b in enumerate(t["broad"]):
            broad.append(b["id"])
            subs = b.get("subtopics") or []
            if subs:
                for s in subs:
                    paths.append(f"{b['id']}/{s['id']}")
                    pb.append(i)
            else:
                paths.append(b["id"])
                pb.append(i)
        return cls(t["version"], broad, paths, np.array(pb))


@dataclass
class Data:
    uris: list[str]
    texts: list[str]
    windows: list[str]
    broad: np.ndarray  # (n, B) target distribution
    path: np.ndarray  # (n, P) target distribution
    signals: np.ndarray  # (n, 4) in 0..1
    tone: np.ndarray  # (n, 6) distribution
    conf: np.ndarray  # (n,) Jev broad confidence

    def subset(self, idx: np.ndarray) -> "Data":
        return Data([self.uris[i] for i in idx], [self.texts[i] for i in idx], [self.windows[i] for i in idx],
                    self.broad[idx], self.path[idx], self.signals[idx], self.tone[idx], self.conf[idx])

    def __len__(self) -> int:
        return len(self.uris)


def load(export_dir: str, space: Space) -> Data:
    bi = {b: i for i, b in enumerate(space.broad)}
    pi = {p: i for i, p in enumerate(space.paths)}
    has_subs = {space.broad[space.path_broad[i]] for i, p in enumerate(space.paths) if "/" in p}
    uris, texts, wins, B, P, S, T, C = [], [], [], [], [], [], [], []
    with gzip.open(f"{export_dir}/labels.jsonl.gz", "rt") as f:
        for line in f:
            r = json.loads(line)
            b = np.zeros(len(space.broad), np.float32)
            for k, v in r["broad_probs"].items():
                if k in bi:
                    b[bi[k]] = v
            if b.sum() <= 0:
                continue
            b /= b.sum()
            p = np.zeros(len(space.paths), np.float32)
            for k, v in (r["sub_probs"] or {}).items():
                if k in pi:
                    p[pi[k]] = b[bi[k.split("/", 1)[0]]] * v
            for name, i in bi.items():
                if name not in has_subs:
                    p[pi[name]] = b[i]
            if p.sum() <= 0:  # no subtopics asked: fall back to the broad distribution's "other"
                top = space.broad[int(b.argmax())]
                key = f"{top}/other" if f"{top}/other" in pi else top
                p[pi[key]] = 1.0
            p /= p.sum()
            sig = r["signals"] or {}
            s = np.array([sig.get(k, 0.0) for k in SIGNALS], np.float32).clip(0, 1)
            t = np.array([sig.get("tone." + k, 0.0) for k in TONES], np.float32)
            t = t / t.sum() if t.sum() > 0 else np.full(len(TONES), 1 / len(TONES), np.float32)
            uris.append(r["uri"]); texts.append(r["model_input"]); wins.append(r["window_id"])
            B.append(b); P.append(p); S.append(s); T.append(t); C.append(r["broad_confidence"])
    return Data(uris, texts, wins, np.stack(B), np.stack(P), np.stack(S), np.stack(T), np.array(C, np.float32))


def split(d: Data) -> tuple[Data, Data, Data, dict]:
    """Split by labeling window (plan D9): newest windows are test, the ones before are
    validation, the rest train. Window ids sort in time order."""
    ws = sorted({w for w in d.windows if w})
    test_w, val_w = set(ws[-TEST_WINDOWS:]), set(ws[-TEST_WINDOWS - VAL_WINDOWS:-TEST_WINDOWS])
    idx = np.arange(len(d))
    w = np.array(d.windows)
    te, va = idx[np.isin(w, list(test_w))], idx[np.isin(w, list(val_w))]
    tr = idx[~np.isin(w, list(test_w | val_w))]
    info = {"train_windows": sorted(set(ws) - test_w - val_w), "val_windows": sorted(val_w), "test_windows": sorted(test_w),
            "n_train": len(tr), "n_val": len(va), "n_test": len(te)}
    return d.subset(tr), d.subset(va), d.subset(te), info


def topk_agreement(pred: np.ndarray, target: np.ndarray, k: int) -> float:
    """Share of rows where the target's argmax is in the prediction's top k."""
    t = target.argmax(1)
    top = np.argsort(-pred, 1)[:, :k]
    return float((top == t[:, None]).any(1).mean())


def ece(pred: np.ndarray, target: np.ndarray, bins: int = 15) -> float:
    """Expected calibration error of the top prediction against the target's argmax."""
    conf, hit = pred.max(1), (pred.argmax(1) == target.argmax(1))
    edges = np.linspace(0, 1, bins + 1)
    e = 0.0
    for lo, hi in zip(edges[:-1], edges[1:]):
        m = (conf > lo) & (conf <= hi)
        if m.any():
            e += m.mean() * abs(hit[m].mean() - conf[m].mean())
    return float(e)


def per_class(pred: np.ndarray, target: np.ndarray, names: list[str]) -> dict:
    p, t = pred.argmax(1), target.argmax(1)
    out = {}
    for i, n in enumerate(names):
        tp, fp, fn = int(((p == i) & (t == i)).sum()), int(((p == i) & (t != i)).sum()), int(((p != i) & (t == i)).sum())
        out[n] = {"support": int((t == i).sum()), "precision": tp / (tp + fp) if tp + fp else None,
                  "recall": tp / (tp + fn) if tp + fn else None}
    return out


def evaluate(space: Space, broad: np.ndarray, path: np.ndarray, d: Data,
             signals: np.ndarray | None = None, tone: np.ndarray | None = None) -> dict:
    m = {
        "n": len(d),
        "broad_top1": topk_agreement(broad, d.broad, 1), "broad_top3": topk_agreement(broad, d.broad, 3),
        "path_top1": topk_agreement(path, d.path, 1), "path_top3": topk_agreement(path, d.path, 3),
        "broad_ece": ece(broad, d.broad),
        "broad_per_class": per_class(broad, d.broad, space.broad),
    }
    if signals is not None:
        m["signals"] = {k: {"mae": float(np.abs(signals[:, i] - d.signals[:, i]).mean()),
                            "corr": float(np.corrcoef(signals[:, i], d.signals[:, i])[0, 1])}
                        for i, k in enumerate(SIGNALS)}
    if tone is not None:
        m["tone_top1"] = topk_agreement(tone, d.tone, 1)
    return m
