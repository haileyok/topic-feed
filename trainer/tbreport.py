"""Metrics, charts and tables for TensorBoard, shared by train.py (every epoch) and
tb_report.py (a finished model), so both log the same things under the same names.

Tag groups (TensorBoard groups cards by the part before the first "/"):
  <prefix>                          main scores: broad/path agreement, the lenient path
                                    scores, calibration error, tone, signal correlations
  <prefix>_loss                     each part of the training loss, and the total
  <prefix>_by_label_confidence      scores split by the label's own confidence
  <prefix>_by_label_source          scores on Jev's labels vs relabeled posts
  <prefix>_charts, <prefix>_tables  confusion matrix, calibration chart, per-topic table
  human                             the model's answers on posts people judged
"""

import glob
import json
import os

import matplotlib
import numpy as np
import torch
import torch.nn.functional as F

matplotlib.use("Agg")
import matplotlib.pyplot as plt  # noqa: E402

import common  # noqa: E402
import human_check  # noqa: E402
import losses  # noqa: E402

CONF_BUCKETS = [("ge_0.9", 0.9, 2.0), ("0.6_to_0.9", 0.6, 0.9), ("lt_0.6", -1.0, 0.6)]
SOURCE_NAMES = {"window": "jev", "sample": "jev", "uncertain": "relabeled"}
HUMAN_DIR = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "reference", "human")


@torch.no_grad()
def predict(model, d: common.Data, tok, max_len: int, bs: int = 256, device: str = "cuda") -> list[torch.Tensor]:
    """Logits for all four heads (broad, path, signals, tone), on CPU."""
    model.eval()
    outs = [[], [], [], []]
    for i in range(0, len(d), bs):
        enc = tok(d.texts[i:i + bs], padding=True, truncation=True, max_length=max_len, return_tensors="pt")
        with torch.autocast(device, dtype=torch.bfloat16, enabled=device == "cuda"):
            o = model(enc["input_ids"].to(device), enc["attention_mask"].to(device))
        for k in range(4):
            outs[k].append(o[k].float().cpu())
    return [torch.cat(x) for x in outs]


def probs(outs, t_broad: float = 1.0, t_path: float = 1.0):
    lb, lp, ls, lt = outs
    return (F.softmax(lb / t_broad, -1).numpy(), F.softmax(lp / t_path, -1).numpy(),
            torch.sigmoid(ls).numpy(), F.softmax(lt, -1).numpy())


def loss_means(d: common.Data, outs, conf_floor: float) -> dict[str, float]:
    """Each loss part averaged over posts, and the training objective (weighted total)."""
    lb, lp, ls, lt = outs
    p = losses.parts(lb, lp, ls, lt, torch.tensor(d.broad), torch.tensor(d.path),
                     torch.tensor(d.signals), torch.tensor(d.tone))
    w = torch.tensor(np.maximum(d.conf, conf_floor))
    out = {k: float(v.mean()) for k, v in p.items()}
    out["total"] = float(losses.total(p, w))
    return out


def scores(space, eq, d: common.Data, sb, sp, sig=None, st=None) -> dict[str, float]:
    m = common.evaluate(space, sb, sp, d, sig, st, eq)
    out = {k: m[k] for k in ("broad_top1", "broad_top3", "path_top1", "path_top3", "path_top1_equiv",
                             "path_plausible", "path_plausible_equiv", "broad_ece")}
    if st is not None:
        out["tone_top1"] = m["tone_top1"]
    if sig is not None:
        for k, v in m["signals"].items():
            out[f"signal_corr_{k}"] = v["corr"]
    return out


def groups(space, eq, d: common.Data, sb, sp, min_n: int = 30) -> dict[str, dict[str, float]]:
    """Main scores within slices of the data: by label confidence and by label source."""
    out = {}

    def add(key, idx):
        if len(idx) < min_n:
            return
        sub = d.subset(idx)
        ps = common.path_scores(sp[idx], sub, eq)
        out[key] = {"share": len(idx) / len(d), "broad_top1": common.topk_agreement(sb[idx], sub.broad, 1),
                    "path_top1": common.topk_agreement(sp[idx], sub.path, 1),
                    "path_plausible_equiv": ps["path_plausible_equiv"]}

    for name, lo, hi in CONF_BUCKETS:
        add(f"by_label_confidence/{name}", np.where((d.conf >= lo) & (d.conf < hi))[0])
    src = np.array([SOURCE_NAMES.get(s, s) for s in d.sources])
    if len(set(src)) > 1:
        for s in sorted(set(src)):
            add(f"by_label_source/{s}", np.where(src == s)[0])
    return out


def confusion_figure(space, d: common.Data, sb, title: str):
    t, p = d.broad.argmax(1), sb.argmax(1)
    n = len(space.broad)
    cm = np.zeros((n, n))
    np.add.at(cm, (t, p), 1)
    rows = cm.sum(1, keepdims=True)
    norm = np.divide(cm, rows, out=np.zeros_like(cm), where=rows > 0)
    fig, ax = plt.subplots(figsize=(11, 9.5))
    im = ax.imshow(norm, cmap="Blues", vmin=0, vmax=1)
    ax.set_xticks(range(n), space.broad, rotation=70, ha="right", fontsize=8)
    ax.set_yticks(range(n), [f"{b} ({int(rows[i, 0])})" for i, b in enumerate(space.broad)], fontsize=8)
    for i in range(n):
        for j in range(n):
            if norm[i, j] >= 0.05:
                ax.text(j, i, f"{norm[i, j]:.0%}"[:-1], ha="center", va="center", fontsize=6,
                        color="white" if norm[i, j] > 0.5 else "black")
    ax.set_xlabel("model's top broad topic")
    ax.set_ylabel("label's top broad topic (posts)")
    ax.set_title(title + " · each row sums to 100%")
    fig.colorbar(im, fraction=0.03)
    fig.tight_layout()
    return fig


def calibration_figure(d: common.Data, sb, sp, title: str):
    fig, axes = plt.subplots(1, 2, figsize=(11, 4.5))
    for ax, pred, target, name in ((axes[0], sb, d.broad, "broad topic"), (axes[1], sp, d.path, "subtopic path")):
        conf, hit = pred.max(1), pred.argmax(1) == target.argmax(1)
        edges = np.linspace(0, 1, 11)
        mids, acc, cnt = [], [], []
        for lo, hi in zip(edges[:-1], edges[1:]):
            m = (conf > lo) & (conf <= hi)
            if m.sum() > 0:
                mids.append((lo + hi) / 2); acc.append(hit[m].mean()); cnt.append(m.sum())
        ax.bar(mids, acc, width=0.09, color="#84adff", label="agreement with label")
        ax.plot([0, 1], [0, 1], "--", color="#667085", label="perfect calibration")
        ax2 = ax.twinx()
        ax2.plot(mids, cnt, "o-", color="#f79009", ms=3, label="posts")
        ax2.set_ylabel("posts", color="#f79009")
        ax.set_xlim(0, 1); ax.set_ylim(0, 1)
        ax.set_xlabel(f"model confidence ({name})"); ax.set_ylabel("agreement with label")
        ax.set_title(f"{name} · ECE {common.ece(pred, target):.3f}")
        ax.legend(loc="upper left", fontsize=8)
    fig.suptitle(title)
    fig.tight_layout()
    return fig


def per_topic_markdown(space, d: common.Data, sb) -> str:
    pc = common.per_class(sb, d.broad, space.broad)
    n = len(d)
    lines = ["| broad topic | share of labels | precision | recall | F1 |", "|---|---|---|---|---|"]

    def f(x):
        return "–" if x is None else f"{x:.0%}"

    rows = []
    for b, v in pc.items():
        p, r = v["precision"], v["recall"]
        f1 = 2 * p * r / (p + r) if p and r else None
        rows.append((v["support"], f"| {b} | {v['support'] / n:.1%} | {f(p)} | {f(r)} | {f(f1)} |"))
    lines += [r for _, r in sorted(rows, key=lambda x: -x[0])]
    return "\n".join(lines)


def log(tb, step: int, prefix: str, space, eq, d: common.Data, outs, *, temps=(1.0, 1.0), conf_floor: float = 0.3,
        charts: bool = True) -> dict:
    """Log everything for one split at one step. Returns the numbers logged."""
    sb, sp, sig, st = probs(outs, *temps)
    s = scores(space, eq, d, sb, sp, sig, st)
    for k, v in s.items():
        tb.add_scalar(f"{prefix}/{k}", v, step)
    lm = loss_means(d, outs, conf_floor)
    for k, v in lm.items():
        tb.add_scalar(f"{prefix}_loss/{k}", v, step)
    g = groups(space, eq, d, sb, sp)
    for key, vals in g.items():
        group, name = key.split("/", 1)
        for k, v in vals.items():
            tb.add_scalar(f"{prefix}_{group}/{name}/{k}", v, step)
    if charts:
        tb.add_figure(f"{prefix}_charts/confusion", confusion_figure(space, d, sb, f"{prefix} · step {step}"), step)
        tb.add_figure(f"{prefix}_charts/calibration", calibration_figure(d, sb, sp, f"{prefix} · step {step}"), step)
        tb.add_text(f"{prefix}_tables/per_topic", per_topic_markdown(space, d, sb), step)
    return {"scores": s, "loss": lm, "groups": g}


class Human:
    """Posts people judged (reference/human/*.json), and which answers they accepted."""

    def __init__(self, files: list[str] | None = None):
        self.files = files if files is not None else sorted(glob.glob(os.path.join(HUMAN_DIR, "*.json")))
        self.js = human_check.judgments(self.files) if self.files else {}
        self.uris = {u for u, _ in self.js}
        # Example posts for the table: the randomly sampled reference marks, in file order.
        self.examples = []
        for f in self.files:
            doc = json.load(open(f))
            for m in doc.get("marks", []):
                if m.get("stratum") == "random" and m["uri"] not in self.examples:
                    self.examples.append(m["uri"])

    def subset(self, d: common.Data) -> common.Data | None:
        idx = [i for i, u in enumerate(d.uris) if u in self.uris]
        return d.subset(np.array(idx)) if idx else None

    def log(self, tb, step: int, space, d: common.Data, outs, *, temps=(1.0, 1.0), examples: int = 20) -> dict:
        """Counts of the model's answers people accepted, rejected, or never judged, plus a table."""
        sb, sp, _, _ = probs(outs, *temps)
        top = sp.argmax(1)
        c = {"accepted": 0, "rejected": 0, "unjudged": 0}
        for u, p in zip(d.uris, top):
            j = self.js.get((u, space.paths[p]))
            c["unjudged" if j is None else "accepted" if j["right"] else "rejected"] += 1
        n = len(d)
        out = {"posts": n, **c, "accepted_share": c["accepted"] / n,
               "accepted_share_of_judged": c["accepted"] / max(1, c["accepted"] + c["rejected"])}
        for k, v in out.items():
            tb.add_scalar(f"human/{k}", v, step)
        row = {u: i for i, u in enumerate(d.uris)}
        lines = ["| post | people accepted | label (Jev) | model: top 3 subtopics | verdict |", "|---|---|---|---|---|"]
        for u in [u for u in self.examples if u in row][:examples]:
            i = row[u]
            ok = sorted({p for (uu, p), j in self.js.items() if uu == u and j["right"]}) or ["(none)"]
            t3 = np.argsort(-sp[i])[:3]
            j = self.js.get((u, space.paths[top[i]]))
            verdict = "?" if j is None else ("✓" if j["right"] else "✗")
            text = d.texts[i].replace("\n", " ").replace("|", "/")[:110]
            lines.append(f"| {text} | {', '.join(ok)} | {space.paths[int(d.path[i].argmax())]} | "
                         + ", ".join(f"{space.paths[t]} {sp[i][t]:.2f}" for t in t3) + f" | {verdict} |")
        tb.add_text("human/examples", "\n".join(lines), step)
        return out
