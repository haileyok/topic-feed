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
import os
from dataclasses import dataclass

import numpy as np
import yaml

# Jev's 0-1 scores. The first four are in every label; the rest came with questions q3
# (2026-09-30) and are missing (NaN) in older labels, which the loss and scores skip.
# New signals go at the end, so older models' outputs line up by position.
SIGNALS = ["substance", "news", "promo", "general_interest",
           "sentiment", "critical", "ad", "engagement_bait", "spam", "self_promo"]
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
    conf: np.ndarray  # (n,) the label's broad confidence (Jev's, or the relabel's for relabeled posts)
    sources: list[str]  # label source per post: "window"/"sample" (Jev's run) or "uncertain" (a relabel)
    configs: list[str] | None = None  # label_config per post

    def __post_init__(self):
        if self.configs is None:
            self.configs = [""] * len(self.uris)

    def subset(self, idx: np.ndarray) -> "Data":
        return Data([self.uris[i] for i in idx], [self.texts[i] for i in idx], [self.windows[i] for i in idx],
                    self.broad[idx], self.path[idx], self.signals[idx], self.tone[idx], self.conf[idx],
                    [self.sources[i] for i in idx], [self.configs[i] for i in idx])

    def concat(self, other: "Data") -> "Data":
        return Data(self.uris + other.uris, self.texts + other.texts, self.windows + other.windows,
                    np.concatenate([self.broad, other.broad]), np.concatenate([self.path, other.path]),
                    np.concatenate([self.signals, other.signals]), np.concatenate([self.tone, other.tone]),
                    np.concatenate([self.conf, other.conf]), self.sources + other.sources, self.configs + other.configs)

    def is_live(self, live_configs) -> np.ndarray:
        """(n,) bool: labeled under one of the live label_configs (posts from the live pipeline)."""
        return np.array([c in live_configs for c in self.configs], dtype=bool)

    def __len__(self) -> int:
        return len(self.uris)


def load(export_dir: str, space: Space) -> Data:
    bi = {b: i for i, b in enumerate(space.broad)}
    pi = {p: i for i, p in enumerate(space.paths)}
    has_subs = {space.broad[space.path_broad[i]] for i, p in enumerate(space.paths) if "/" in p}
    uris, texts, wins, B, P, S, T, C, src, cfg = [], [], [], [], [], [], [], [], [], []
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
            s = np.array([sig.get(k, np.nan) for k in SIGNALS], np.float32).clip(0, 1)  # NaN: not asked
            t = np.array([sig.get("tone." + k, 0.0) for k in TONES], np.float32)
            t = t / t.sum() if t.sum() > 0 else np.full(len(TONES), 1 / len(TONES), np.float32)
            uris.append(r["uri"]); texts.append(r["model_input"]); wins.append(r["window_id"])
            B.append(b); P.append(p); S.append(s); T.append(t); C.append(r["broad_confidence"])
            src.append(r.get("source") or "window"); cfg.append(r.get("label_config") or "")
    return Data(uris, texts, wins, np.stack(B), np.stack(P), np.stack(S), np.stack(T), np.array(C, np.float32), src, cfg)


def live_holdout(d: Data, live_configs, n_test: int, n_val: int) -> tuple[Data, Data, Data]:
    """Set aside live posts from the random sample (source "sample") for testing and
    validation, picked by a hash of the URI so every run holds out the same posts.
    Returns (everything else, live test, live validation). Targeted live labels
    (source "uncertain") are not a fair picture of live posts, so they always train."""
    import hashlib

    live = d.is_live(live_configs)
    cand = [i for i in range(len(d)) if live[i] and d.sources[i] == "sample"]
    cand.sort(key=lambda i: hashlib.sha1(d.uris[i].encode()).hexdigest())
    if len(cand) < n_test + n_val:
        raise SystemExit(f"only {len(cand)} live random-sample posts; need {n_test} test + {n_val} validation")
    te, va = np.array(cand[:n_test], int), np.array(cand[n_test:n_test + n_val], int)
    held = np.zeros(len(d), bool)
    held[te] = held[va] = True
    return d.subset(np.flatnonzero(~held)), d.subset(te), d.subset(va)


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


# A path counts as one of Jev's plausible answers when its probability is at least this
# share of Jev's top path probability. Jev's path_scores are the square roots of these
# joint probabilities, so 0.25 here means "at least half of Jev's best path score".
PLAUSIBLE_RATIO = 0.25


@dataclass
class Equivalence:
    """Paths treated as interchangeable when scoring, from taxonomy/<version>-equivalences.yaml."""

    same: np.ndarray  # (P, P) bool, symmetric, true on the diagonal
    quote_any: np.ndarray  # (P,) bool: on posts that quote another post, matches any path

    @classmethod
    def identity(cls, space: Space) -> "Equivalence":
        n = len(space.paths)
        return cls(np.eye(n, dtype=bool), np.zeros(n, dtype=bool))

    @classmethod
    def from_yaml(cls, path: str, space: Space) -> "Equivalence":
        cfg = yaml.safe_load(open(path))
        eq, pi = cls.identity(space), {p: i for i, p in enumerate(space.paths)}

        def idx(p: str) -> int:
            if p not in pi:
                raise SystemExit(f"{path}: unknown path {p!r}")
            return pi[p]

        for group in cfg.get("groups") or []:
            ids = [idx(p) for p in group]
            for a in ids:
                eq.same[a, ids] = True
        if cfg.get("sibling_other"):
            for i, p in enumerate(space.paths):
                if p.endswith("/other"):
                    b = p.split("/", 1)[0]
                    sib = [j for j, q in enumerate(space.paths) if q.startswith(b + "/")]
                    eq.same[i, sib] = eq.same[sib, i] = True
        for p in cfg.get("quote_reply_paths") or []:
            eq.quote_any[idx(p)] = True
        return eq

    @classmethod
    def for_taxonomy(cls, taxonomy_path: str, space: Space) -> "Equivalence":
        """The equivalence file next to the taxonomy (v1.yaml -> v1-equivalences.yaml), or none."""
        p = taxonomy_path.removesuffix(".yaml") + "-equivalences.yaml"
        return cls.from_yaml(p, space) if os.path.exists(p) else cls.identity(space)


def quotes_a_post(text: str) -> bool:
    return ("\n" + text).find("\n[quote] ") >= 0


def plausible_paths(target: np.ndarray, ratio: float = PLAUSIBLE_RATIO) -> np.ndarray:
    """(n, P) bool: Jev's plausible paths for each post (always includes Jev's top path)."""
    return target >= ratio * target.max(1, keepdims=True)


def matches(answer: np.ndarray, accepted: np.ndarray, eq: Equivalence, quoting: np.ndarray) -> np.ndarray:
    """(n,) bool: answer index matches one of the accepted paths, directly or through eq."""
    ok = (eq.same[answer] & accepted).any(1)
    return ok | (quoting & (eq.quote_any[answer] | (accepted & eq.quote_any[None, :]).any(1)))


def path_scores(path: np.ndarray, d: Data, eq: Equivalence) -> dict:
    """More lenient versions of path_top1: the student's top path counts as right when it
    matches Jev's top path through the equivalences, is one of Jev's plausible paths, or
    matches a plausible path through the equivalences."""
    top, jev_top = path.argmax(1), d.path.argmax(1)
    exact = np.zeros_like(d.path, dtype=bool)
    exact[np.arange(len(d)), jev_top] = True
    plaus = plausible_paths(d.path)
    quoting = np.array([quotes_a_post(t) for t in d.texts], dtype=bool)
    return {
        "path_top1_equiv": float(matches(top, exact, eq, quoting).mean()),
        "path_plausible": float(plaus[np.arange(len(d)), top].mean()),
        "path_plausible_equiv": float(matches(top, plaus, eq, quoting).mean()),
        "jev_plausible_paths_mean": float(plaus.sum(1).mean()),
    }


def signal_scores(pred: np.ndarray, target: np.ndarray, min_n: int = 30) -> dict:
    """Per signal: mean absolute error and correlation with Jev, over posts where Jev
    answered it. pred may have fewer columns (an older model); missing signals are left out."""
    out = {}
    for i, k in enumerate(SIGNALS[:min(pred.shape[1], target.shape[1])]):
        ok = ~np.isnan(target[:, i])
        if ok.sum() < min_n:
            continue
        p, t = pred[ok, i], target[ok, i]
        corr = float(np.corrcoef(p, t)[0, 1]) if p.std() > 0 and t.std() > 0 else 0.0
        out[k] = {"n": int(ok.sum()), "mae": float(np.abs(p - t).mean()), "corr": corr}
    return out


def evaluate(space: Space, broad: np.ndarray, path: np.ndarray, d: Data,
             signals: np.ndarray | None = None, tone: np.ndarray | None = None,
             eq: Equivalence | None = None) -> dict:
    m = {
        "n": len(d),
        "broad_top1": topk_agreement(broad, d.broad, 1), "broad_top3": topk_agreement(broad, d.broad, 3),
        "path_top1": topk_agreement(path, d.path, 1), "path_top3": topk_agreement(path, d.path, 3),
        **path_scores(path, d, eq or Equivalence.identity(space)),
        "broad_ece": ece(broad, d.broad),
        "broad_per_class": per_class(broad, d.broad, space.broad),
    }
    if signals is not None:
        m["signals"] = signal_scores(signals, d.signals)
    if tone is not None:
        m["tone_top1"] = topk_agreement(tone, d.tone, 1)
    return m
