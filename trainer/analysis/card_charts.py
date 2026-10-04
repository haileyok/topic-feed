"""Charts for the Hugging Face model card of microblog-topic-classifier-v5, from files already on disk (no model is run).

Reads  --model   the packaged model folder (metrics.json, config.json), as uploaded: /data/models/v5
       --probs   the model's probabilities on the held-out test posts: /data/models/fusion1/test-probs.npz
       --export  the training export, for the teachers' answers on those posts: /data/mm/v21
Writes PNGs to --out (default: the card's images/ folder next to its README). They contain no post text.

Before drawing, the scores recomputed from the test probabilities are checked against metrics.json, so a chart
can't quietly disagree with the numbers in the card.

    trainer/.venv/bin/python trainer/analysis/card_charts.py
"""

import argparse
import collections
import json
import sys
from pathlib import Path

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt  # noqa: E402
import numpy as np  # noqa: E402

HERE = Path(__file__).resolve().parent
TEXT, PICS, GREY, INK = "#2f6db5", "#e07b24", "#9aa3b0", "#222222"
SOURCES = ("jev", "clef")
NAME = {"jev": "text and link-card posts", "clef": "picture posts"}
COLOR = {"jev": TEXT, "clef": PICS}
TEACHER = {"jev": "Jev", "clef": "Clef-flash"}

plt.rcParams.update({
    "font.size": 10.5, "axes.titlesize": 12, "axes.titleweight": "bold", "axes.titlelocation": "left",
    "axes.spines.top": False, "axes.spines.right": False, "axes.grid": True, "grid.alpha": 0.25,
    "axes.axisbelow": True, "legend.frameon": False, "figure.dpi": 150, "savefig.dpi": 150,
    "savefig.bbox": "tight", "savefig.facecolor": "white", "figure.facecolor": "white",
})


def load_json(p):
    with open(p) as f:
        return json.load(f)


def teacher_rows(export, taxonomy, broad, paths):
    """The teachers' broad and subtopic targets, built by the training code itself (train_mm.Rows), so they are the
    targets the model was trained and tested against, in the order test-probs.npz's indexes refer to."""
    sys.path.insert(0, str(HERE.parent))
    import common  # noqa: E402
    import train_mm  # noqa: E402

    space = common.Space.from_taxonomy(taxonomy)
    if list(space.broad) != list(broad) or list(space.paths) != list(paths):
        raise SystemExit(f"{taxonomy} lists its topics in another order than the model's config.json")
    rows = train_mm.Rows(export, space)
    return np.asarray(rows.source), rows.broad, rows.path


def save(fig, out, name):
    fig.savefig(out / name)
    plt.close(fig)
    print("wrote", out / name)


def bar_labels(a, bars, fmt):
    for b in bars:
        a.annotate(fmt(b.get_height()), (b.get_x() + b.get_width() / 2, b.get_height()), xytext=(0, 3),
                   textcoords="offset points", ha="center", va="bottom", fontsize=9)


# 1. Headline results ------------------------------------------------------------------------------------------------
def results(out, m):
    cl, test = m["closeness"], m["test"]
    rows = [
        ("broad topic:\nprobability\nshared", lambda s: cl["broad"][s]["overlap_mean"]),
        ("broad topic:\ntop pick in the\nteacher's main\ntopics", lambda s: cl["broad"][s]["top_pick_in_area"]),
        ("broad topic:\nsame\ntop pick", lambda s: cl["broad"][s]["exact_top1"]),
        ("subtopic:\nprobability\nshared", lambda s: cl["path"][s]["overlap_mean"]),
        ("subtopic:\ntop pick in the\nteacher's main\nsubtopics", lambda s: cl["path"][s]["top_pick_in_area"]),
        ("subtopic:\nsame\ntop pick", lambda s: cl["path"][s]["exact_top1"]),
        ("tone:\nprobability\nshared", lambda s: cl["tone"][s]["overlap_mean"]),
    ]
    fig, a = plt.subplots(figsize=(11.5, 4.8))
    x, w = np.arange(len(rows)), 0.38
    for k, s in enumerate(SOURCES):
        bars = a.bar(x + (k - 0.5) * w, [f(s) for _, f in rows], w, color=COLOR[s],
                     label=f"{NAME[s]} ({test[s]['n']:,}, vs {TEACHER[s]})")
        bar_labels(a, bars, lambda v: f"{v:.2f}")
    a.set_xticks(x, [r[0] for r in rows], fontsize=9)
    a.set_ylim(0, 1.08)
    a.set_ylabel("agreement with the teacher (1 = identical)")
    a.set_title("How closely the model reproduces its teachers on held-out test posts")
    a.legend(loc="upper center", bbox_to_anchor=(0.5, -0.3), ncol=2)
    save(fig, out, "results.png")


# 2. Training --------------------------------------------------------------------------------------------------------
def training(out, m):
    h, best = m["history"], m["best_epoch"] + 1
    ep = [r["epoch"] for r in h]
    fig, ax = plt.subplots(1, 3, figsize=(13, 3.9))
    a = ax[0]
    a.plot(ep, [r["train_loss"] for r in h], "o-", color="#555", label="training")
    a.plot(ep, [r["val_loss"]["all"]["total"] for r in h], "o-", color="#1b8f6a", label="validation")
    a.set_title("Loss (lower is better)")
    a.legend()
    for a, key, title in ((ax[1], "broad_top1", "Broad topic: same top pick"), (ax[2], "path_top1", "Subtopic: same top pick")):
        for s in SOURCES:
            a.plot(ep, [100 * r["val"][s][key] for r in h], "o-", color=COLOR[s], label=NAME[s])
        a.set_title(title + " (%)")
    ax[1].legend(loc="lower right", fontsize=9)
    for a in ax:
        a.axvline(best, color="#888", ls=":", lw=1.2)
        a.set_xlabel("epoch")
        a.set_xticks(ep)
    ax[0].annotate(f"kept: epoch {best}", (best, ax[0].get_ylim()[1]), xytext=(-4, -12), textcoords="offset points",
                   ha="right", fontsize=8.5, color="#666")
    fig.suptitle("Validation scores per epoch (the kept epoch has the best average broad-topic top pick over both teachers)",
                 x=0.01, ha="left", fontsize=10.5, color="#444", y=1.03)
    fig.tight_layout()
    save(fig, out, "training.png")


# 3. Per topic -------------------------------------------------------------------------------------------------------
def per_topic(out, src, tb, sb, broad):
    fig, ax = plt.subplots(1, 2, figsize=(12.5, 8.6))
    for a, s in zip(ax, SOURCES):
        m = src == s
        t, p = tb[m].argmax(1), sb[m].argmax(1)
        n = len(broad)
        sup, psup = np.bincount(t, minlength=n), np.bincount(p, minlength=n)
        rec = np.array([(p[t == c] == c).mean() if sup[c] else np.nan for c in range(n)])
        prec = np.array([(t[p == c] == c).mean() if psup[c] else np.nan for c in range(n)])
        order = np.argsort(sup)
        y = np.arange(n)
        small = sup[order] < 30
        a.barh(y, rec[order], color=[("#d5d9e0" if sm else COLOR[s]) for sm in small], height=0.72,
               label="share of the teacher's posts on the topic\nwhere the model's top pick agrees")
        a.plot(prec[order], y, "D", color=INK, ms=4.5,
               label="share of the model's picks of the topic\nthat the teacher's top pick agrees with")
        a.set_yticks(y, [f"{broad[i]}  ({sup[i]:,})" for i in order], fontsize=8.5)
        a.set_xlim(0, 1)
        a.set_ylim(-0.7, n - 0.3)
        a.set_title(f"{NAME[s]} (vs {TEACHER[s]})")
        a.set_xlabel("agreement on the top broad topic")
    fig.suptitle("Per broad topic, on held-out test posts (in brackets: test posts the teacher put there; grey bars: fewer than 30)",
                 x=0.01, ha="left", fontsize=10.5, color="#444")
    handles, labels = ax[0].get_legend_handles_labels()
    fig.legend(handles, labels, loc="lower center", ncol=2, bbox_to_anchor=(0.5, -0.06), fontsize=9)
    fig.tight_layout()
    save(fig, out, "per-topic.png")


# 4. Confidence ------------------------------------------------------------------------------------------------------
def confidence(out, src, tb, sb):
    edges = np.array([0.0, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0001])
    fig, ax = plt.subplots(1, 2, figsize=(11.5, 4.3), sharey=True)
    for a, s in zip(ax, SOURCES):
        m = src == s
        conf, hit = sb[m].max(1), sb[m].argmax(1) == tb[m].argmax(1)
        mids, rate, cnt = [], [], []
        for lo, hi in zip(edges[:-1], edges[1:]):
            k = (conf >= lo) & (conf < hi)
            if k.sum() >= 20:
                mids.append(float(conf[k].mean()))  # drawn at the bin's average probability, not its middle
                rate.append(hit[k].mean())
                cnt.append(int(k.sum()))
        a.plot([0.2, 1], [0.2, 1], color="#bbb", ls="--", lw=1, label="agrees exactly as often as it is confident")
        a.plot(mids, rate, "o-", color=COLOR[s], lw=2, label="same top broad topic as the teacher")
        for x, c in zip(mids, cnt):
            a.annotate(f"{c:,}", (x, 0.04), ha="center", fontsize=7.5, color="#777")
        a.set_title(f"{NAME[s]} (vs {TEACHER[s]})")
        a.set_xlabel("model's probability for its top broad topic")
        a.set_xlim(0.2, 1.0)
        a.set_ylim(0, 1.03)
    ax[0].set_ylabel("share of posts")
    ax[0].legend(loc="upper left", fontsize=8.5)
    fig.suptitle("The more confident the model, the more often it agrees with its teacher (small numbers: test posts in each bin)",
                 x=0.01, ha="left", fontsize=10.5, color="#444", y=1.02)
    fig.tight_layout()
    save(fig, out, "confidence.png")


# 5. Teacher confidence ----------------------------------------------------------------------------------------------
def teacher_confidence(out, m):
    buckets = ["teacher under 50% sure", "teacher 50-80% sure", "teacher 80%+ sure"]
    short = ["under 50%", "50-80%", "80% or more"]
    fig, ax = plt.subplots(1, 2, figsize=(11.5, 4.1), sharey=True)
    for a, head, title in ((ax[0], "broad", "Broad topic: same top pick"), (ax[1], "path", "Subtopic: same top pick")):
        x, w = np.arange(len(buckets)), 0.38
        for k, s in enumerate(SOURCES):
            d = m["closeness"][head][s]["by_teacher_confidence"]
            bars = a.bar(x + (k - 0.5) * w, [d[b]["exact_top1"] for b in buckets], w, color=COLOR[s], label=NAME[s])
            bar_labels(a, bars, lambda v: f"{v:.0%}")
            for xi, b in zip(x, buckets):
                a.annotate(f"{d[b]['n']:,}", (xi + (k - 0.5) * w, 0.03), ha="center", fontsize=7.5, color="white")
        a.set_xticks(x, short)
        a.set_xlabel("teacher's probability for its own top pick")
        a.set_title(title)
        a.set_ylim(0, 1.1)
    ax[0].set_ylabel("share of test posts")
    ax[1].legend(loc="upper left", fontsize=9)
    fig.suptitle("Most disagreement is on posts the teacher itself was unsure about (numbers in the bars: test posts)",
                 x=0.01, ha="left", fontsize=10.5, color="#444", y=1.02)
    fig.tight_layout()
    save(fig, out, "teacher-confidence.png")


# 6. Topics most often swapped ---------------------------------------------------------------------------------------
def swaps(out, src, tb, sb, broad, top=8):
    fig, ax = plt.subplots(1, 2, figsize=(12, 4.2))
    for a, s in zip(ax, SOURCES):
        m = src == s
        t, p = tb[m].argmax(1), sb[m].argmax(1)
        pair = collections.Counter()
        for i, j in zip(t, p):
            if i != j:
                pair[tuple(sorted((broad[i], broad[j])))] += 1
        items = pair.most_common(top)[::-1]
        a.barh(range(len(items)), [100 * n / m.sum() for _, n in items], color=COLOR[s], height=0.65)
        for y, (_, n) in enumerate(items):
            a.annotate(f"{n:,}", (100 * n / m.sum(), y), xytext=(3, 0), textcoords="offset points", va="center", fontsize=8.5)
        a.set_yticks(range(len(items)), [f"{x} / {y}" for (x, y), _ in items], fontsize=9)
        a.set_xlabel("% of test posts (number: posts)")
        differ = int((t != p).sum())
        a.set_title(f"{NAME[s]} (vs {TEACHER[s]})\ntop picks differ on {differ:,} of {int(m.sum()):,} ({differ / m.sum():.0%})")
        a.margins(x=0.12)
    fig.suptitle("The topic pairs the model and its teacher trade most often (either direction)",
                 x=0.01, ha="left", fontsize=10.5, color="#444", y=1.02)
    fig.tight_layout()
    save(fig, out, "swaps.png")


# 7. Signals ---------------------------------------------------------------------------------------------------------
def signals(out, m, names):
    fig, a = plt.subplots(figsize=(11, 3.9))
    x, w = np.arange(len(names)), 0.38
    for k, s in enumerate(SOURCES):
        mae = m["test"][s]["signal_mae"]
        vals = [mae.get(n, np.nan) for n in names]
        a.bar(x + (k - 0.5) * w, vals, w, color=COLOR[s], label=NAME[s])
    meme = names.index("meme") if "meme" in names else None
    if meme is not None:
        a.annotate("only trained on\npicture posts", (meme - 0.5 * w, 0.005), xytext=(0, 18), textcoords="offset points",
                   ha="center", fontsize=8, color="#666", arrowprops={"arrowstyle": "-", "color": "#aaa"})
    a.set_xticks(x, [n.replace("_", "\n") for n in names], fontsize=9)
    a.set_ylabel("mean absolute difference\nfrom the teacher (0 to 1)")
    a.set_title("Signal scores: how far the model's 0-1 score is from its teacher's, on average (lower is better)")
    a.legend(loc="upper right", fontsize=9)
    save(fig, out, "signals.png")


# 8. Training data ---------------------------------------------------------------------------------------------------
def training_data(out, src, tb, broad):
    fig, ax = plt.subplots(1, 2, figsize=(12, 7.6), sharey=True)
    order = np.argsort(np.bincount(tb.argmax(1), minlength=len(broad)))
    for a, s in zip(ax, SOURCES):
        m = src == s
        cnt = np.bincount(tb[m].argmax(1), minlength=len(broad))
        pct = 100 * cnt[order] / cnt.sum()
        a.barh(np.arange(len(broad)), pct, color=COLOR[s], height=0.72)
        for y, (i, v) in enumerate(zip(order, pct)):
            a.annotate(f"{cnt[i]:,}", (v, y), xytext=(3, 0), textcoords="offset points", va="center", fontsize=7.5)
        a.set_yticks(range(len(broad)), [broad[i] for i in order], fontsize=8.5)
        a.set_title(f"{NAME[s]}\nlabelled by {TEACHER[s]}: {cnt.sum():,} posts")
        a.set_xlabel("% of this teacher's posts (by its top broad topic)")
        a.set_xlim(0, pct.max() * 1.16)
    fig.suptitle("What the training labels look like (all splits; number: posts)", x=0.01, ha="left", fontsize=10.5, color="#444")
    fig.tight_layout()
    save(fig, out, "training-data.png")


def check(m, src, tb, sb, tp, sp):
    """The scores recomputed from the test probabilities must be the ones metrics.json reports."""
    for s in SOURCES:
        k = src == s
        for head, t, p in (("broad", tb, sb), ("path", tp, sp)):
            want = m["closeness"][head][s]
            got_overlap = float(np.minimum(t[k], p[k]).sum(1).mean())
            got_top1 = float((t[k].argmax(1) == p[k].argmax(1)).mean())
            if int(k.sum()) != want["n"] or abs(got_overlap - want["overlap_mean"]) > 2e-3 or abs(got_top1 - want["exact_top1"]) > 2e-3:
                raise SystemExit(f"{head} {s}: recomputed n={int(k.sum())} overlap={got_overlap:.4f} top1={got_top1:.4f}, but metrics.json "
                                 f"says n={want['n']} overlap={want['overlap_mean']:.4f} top1={want['exact_top1']:.4f}")
            print(f"checked {head} {s}: n={want['n']} overlap {got_overlap:.4f} top pick {got_top1:.4f}")


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--model", default="/data/models/v5")
    ap.add_argument("--probs", default="/data/models/fusion1/test-probs.npz")
    ap.add_argument("--export", default="/data/mm/v21", help="the training export folder (labels.jsonl.gz)")
    ap.add_argument("--taxonomy", default=str(HERE.parent.parent / "taxonomy" / "v2.1.yaml"))
    ap.add_argument("--out", default=str(HERE.parent / "hf" / "microblog-topic-classifier-v5" / "images"))
    a = ap.parse_args()
    out = Path(a.out)
    out.mkdir(parents=True, exist_ok=True)
    m, cfg = load_json(f"{a.model}/metrics.json"), load_json(f"{a.model}/config.json")
    broad, paths = cfg["broad"], cfg["paths"]

    src_all, tb_all, tp_all = teacher_rows(a.export, a.taxonomy, broad, paths)
    z = np.load(a.probs)
    idx = z["idx"]
    src, tb, tp, sb, sp = src_all[idx], tb_all[idx], tp_all[idx], z["broad"], z["path"]
    check(m, src, tb, sb, tp, sp)

    results(out, m)
    training(out, m)
    per_topic(out, src, tb, sb, broad)
    confidence(out, src, tb, sb)
    teacher_confidence(out, m)
    swaps(out, src, tb, sb, broad)
    signals(out, m, cfg["signals"])
    training_data(out, src_all, tb_all, broad)


if __name__ == "__main__":
    main()
