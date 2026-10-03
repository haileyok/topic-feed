"""Charts and a one-page summary for the fusion topic classifier, from files already on disk (no model is run).

Reads  /data/models/{mm1,mm2,fusion1}  (metrics, score files, test probabilities)
       /data/mm/v21/labels.jsonl.gz     (the training export, for the teachers' answers on the test posts)
       /data/judged-sets/judged-sets.jsonl  (the hand-judged rounds)
Writes PNGs and report.md to --out (default /data/reports/fusion-model). Contains no post text.

    trainer/.venv/bin/python trainer/analysis/fusion_report_plots.py
"""

import argparse
import collections
import gzip
import json
from pathlib import Path

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt  # noqa: E402
import numpy as np  # noqa: E402

TEXT, PICS = "#2f6db5", "#e07b24"
MODEL_COLORS = {"ModernVBERT, 3 epochs": "#c9ced6", "ModernVBERT, 8 epochs": "#8d96a5", "fusion model": "#1b8f6a"}
SRC_NAME = {"jev": "text and link-card posts (vs Jev)", "clef": "picture posts (vs Clef-flash)"}
SRC_COLOR = {"jev": TEXT, "clef": PICS}
plt.rcParams.update({"font.size": 10, "axes.spines.top": False, "axes.spines.right": False, "axes.titlesize": 11,
                     "axes.titleweight": "bold", "figure.dpi": 140, "savefig.bbox": "tight", "axes.grid": True,
                     "grid.alpha": 0.25})


def load_json(p):
    return json.load(open(p))


def export_rows(export, broad):
    """Teacher distributions in training order (rows with no known broad topic probability were skipped, as in training)."""
    bi = {b: i for i, b in enumerate(broad)}
    src, probs = [], []
    with gzip.open(f"{export}/labels.jsonl.gz", "rt") as f:
        for line in f:
            r = json.loads(line)
            b = np.zeros(len(broad), np.float32)
            for k, v in r["broad_probs"].items():
                if k in bi:
                    b[bi[k]] = v
            if b.sum() <= 0:
                continue
            probs.append(b / b.sum())
            src.append(r["source"])
    return np.array(src), np.stack(probs)


def save(fig, out, name):
    fig.savefig(out / name)
    plt.close(fig)
    print("wrote", name)


# 1 ---------------------------------------------------------------------------------------------------------------
def training_curves(out, fus, mm2):
    fig, ax = plt.subplots(2, 2, figsize=(11, 7.5))
    ep = [h["epoch"] for h in fus["history"]]
    best = fus["best_epoch"] + 1
    a = ax[0, 0]
    a.plot(ep, [h["train_loss"] for h in fus["history"]], "o-", color="#555", label="training loss")
    a.plot(ep, [h["val_loss"]["all"]["total"] for h in fus["history"]], "o-", color="#1b8f6a", label="validation loss")
    a.set_title("Loss per epoch (lower is better)")
    a.set_xlabel("epoch")
    a.legend()
    for k, (key, title) in enumerate((("broad_top1", "Broad topic: top pick equals the teacher's top pick"),
                                      ("path_top1", "Subtopic: top pick equals the teacher's top pick"))):
        a = ax[0, 1] if k == 0 else ax[1, 0]
        for src in ("jev", "clef"):
            a.plot(ep, [100 * h["val"][src][key] for h in fus["history"]], "o-", color=SRC_COLOR[src], label=f"fusion, {SRC_NAME[src]}")
            a.plot([h["epoch"] for h in mm2["history"]], [100 * h["val"][src][key] for h in mm2["history"]], "--", color=SRC_COLOR[src],
                   alpha=0.5, label=f"ModernVBERT 8 ep, {SRC_NAME[src]}")
        a.set_title(title + " (validation, %)")
        a.set_xlabel("epoch")
        a.set_ylabel("%")
    a = ax[1, 1]
    a.plot(ep, [100 * h["val"]["clef"]["meme_auc_vs_teacher_at_0.5"] for h in fus["history"]], "o-", color=PICS, label="meme ranking score (x100), pictures")
    for src in ("jev", "clef"):
        a.plot(ep, [100 * h["val"][src]["tone_top1"] for h in fus["history"]], "o-", color=SRC_COLOR[src], alpha=0.6, label=f"tone top pick %, {src}")
    a.set_title("Meme ranking score and tone (validation)")
    a.set_xlabel("epoch")
    for a in ax.ravel():
        a.axvline(best, color="#999", ls=":", lw=1.2)
    ax[0, 1].legend(fontsize=7, loc="lower right")
    ax[1, 1].legend(fontsize=7, loc="lower right")
    fig.suptitle(f"Fusion model training, 8 epochs (dotted line = kept epoch {best}); dashed = ModernVBERT 8 epochs", fontweight="bold")
    fig.tight_layout()
    save(fig, out, "01-training-curves.png")


# 2 ---------------------------------------------------------------------------------------------------------------
def model_comparison(out, scores):
    panels = (("broad", "overlap_mean", "Broad topic: probability shared with the teacher (1 = identical)"),
              ("path", "overlap_mean", "Subtopic: probability shared with the teacher"),
              ("broad", "top_pick_in_area", "Broad: top pick is among the teacher's main topics"),
              ("broad", "exact_top1", "Broad: top pick equals the teacher's top pick"))
    fig, ax = plt.subplots(1, 4, figsize=(16, 4.6), sharey=True)
    width = 0.26
    for a, (head, key, title) in zip(ax, panels):
        for i, (name, sc) in enumerate(scores.items()):
            vals = [sc[head][s][key] for s in ("jev", "clef")]
            xs = np.arange(2) + (i - 1) * width
            bars = a.bar(xs, vals, width, color=MODEL_COLORS[name], label=name)
            for x, v in zip(xs, vals):
                a.text(x, v + 0.01, f"{v:.2f}" if v < 1.5 else f"{v:.0f}", ha="center", fontsize=7.5, rotation=90)
        a.set_xticks(range(2), ["text and link-card\n(8,384 posts)", "pictures\n(2,964 posts)"])
        a.set_title(title, fontsize=9)
        a.set_ylim(0, 1.12)
    ax[0].legend(loc="lower left", fontsize=8)
    fig.suptitle("Three students on the same held-out test posts: text barely moves, pictures improve a lot with the fusion model", fontweight="bold")
    fig.tight_layout()
    save(fig, out, "02-model-comparison.png")


# 3 ---------------------------------------------------------------------------------------------------------------
def closeness_hist(out, src, teach, stu):
    fig, ax = plt.subplots(1, 2, figsize=(11, 4.2), sharey=True)
    for a, s in zip(ax, ("jev", "clef")):
        m = src == s
        ov = np.minimum(teach[m], stu[m]).sum(1)
        a.hist(ov, bins=np.linspace(0, 1, 26), color=SRC_COLOR[s], alpha=0.85, weights=np.full(m.sum(), 100 / m.sum()))
        a.axvline(np.median(ov), color="k", ls="--", lw=1)
        a.text(np.median(ov) - 0.02, a.get_ylim()[1] * 0.92, f"median {np.median(ov):.2f}", ha="right", fontsize=9)
        a.set_title(f"{SRC_NAME[s]}\n{(ov >= 0.8).mean():.0%} of posts share at least 0.8", fontsize=10)
        a.set_xlabel("probability shared between model and teacher on a post (1 = identical answers)")
    ax[0].set_ylabel("% of test posts")
    fig.tight_layout()
    save(fig, out, "03-per-post-closeness.png")


# 4 ---------------------------------------------------------------------------------------------------------------
def reliability(out, src, teach, stu):
    fig, ax = plt.subplots(1, 2, figsize=(11, 4.4), sharey=True)
    edges = np.array([0.0, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0001])
    for a, s in zip(ax, ("jev", "clef")):
        m = src == s
        conf, hit = stu[m].max(1), stu[m].argmax(1) == teach[m].argmax(1)
        ov = np.minimum(teach[m], stu[m]).sum(1)
        mids, rate, share, cnt = [], [], [], []
        for lo, hi in zip(edges[:-1], edges[1:]):
            k = (conf >= lo) & (conf < hi)
            if k.sum() >= 20:
                mids.append((lo + min(hi, 1.0)) / 2)
                rate.append(hit[k].mean())
                share.append(ov[k].mean())
                cnt.append(k.sum())
        a.plot([0.15, 1], [0.15, 1], color="#bbb", ls="--", label="top pick agrees as often as the model is confident")
        a.plot(mids, rate, "o-", color=SRC_COLOR[s], label="top pick equals the teacher's top pick")
        a.plot(mids, share, "s--", color="#444", alpha=0.7, label="probability shared with the teacher")
        for x, c in zip(mids, cnt):
            a.text(x, 0.03, f"{c}", ha="center", fontsize=7, color="#666")
        a.set_title(SRC_NAME[s], fontsize=10)
        a.set_xlabel("model's confidence in its top broad topic")
        a.set_xlim(0.2, 1.0)
        a.set_ylim(0, 1.02)
    ax[0].set_ylabel("agreement with the teacher")
    ax[0].legend(fontsize=7.5, loc="upper left")
    fig.suptitle("When the model is more confident it agrees with its teacher more (small numbers = posts in each bin)", fontweight="bold")
    fig.tight_layout()
    save(fig, out, "04-confidence-vs-agreement.png")


# 5 ---------------------------------------------------------------------------------------------------------------
def confusion(out, src, teach, stu, broad):
    for s, fname in (("jev", "05-confusion-text.png"), ("clef", "06-confusion-pictures.png")):
        m = src == s
        t, p = teach[m].argmax(1), stu[m].argmax(1)
        n = len(broad)
        order = np.argsort(-np.bincount(t, minlength=n))
        cm = np.zeros((n, n))
        for i, j in zip(t, p):
            cm[i, j] += 1
        sup = cm.sum(1)
        norm = cm / np.maximum(sup[:, None], 1)
        norm = norm[np.ix_(order, order)]
        fig, a = plt.subplots(figsize=(10.5, 9))
        im = a.imshow(norm, cmap="Blues", vmin=0, vmax=1)
        names = [f"{broad[i]} ({int(sup[i])})" for i in order]
        a.set_xticks(range(n), [broad[i] for i in order], rotation=90, fontsize=8)
        a.set_yticks(range(n), names, fontsize=8)
        a.grid(False)
        for i in range(n):
            for j in range(n):
                if norm[i, j] >= 0.08:
                    a.text(j, i, f"{100 * norm[i, j]:.0f}", ha="center", va="center", fontsize=6.5,
                           color="white" if norm[i, j] > 0.55 else "#222")
        a.set_xlabel("model's top broad topic")
        a.set_ylabel("teacher's top broad topic (number of test posts)")
        a.set_title(f"{SRC_NAME[s]}: where the model's top pick goes, % of each teacher topic's posts\n(diagonal = agreement; cells under 8% left blank)")
        fig.colorbar(im, ax=a, fraction=0.035, pad=0.02)
        fig.tight_layout()
        save(fig, out, fname)


# 6 ---------------------------------------------------------------------------------------------------------------
def per_topic(out, src, teach, stu, broad):
    fig, ax = plt.subplots(1, 2, figsize=(13, 8), sharey=False)
    for a, s in zip(ax, ("jev", "clef")):
        m = src == s
        t, p = teach[m].argmax(1), stu[m].argmax(1)
        n = len(broad)
        sup = np.bincount(t, minlength=n)
        rec = np.array([(p[t == c] == c).mean() if sup[c] else np.nan for c in range(n)])
        psup = np.bincount(p, minlength=n)
        prec = np.array([(t[p == c] == c).mean() if psup[c] else np.nan for c in range(n)])
        order = np.argsort(sup)
        y = np.arange(n)
        small = sup[order] < 30
        a.barh(y, rec[order], color=[("#cfd3da" if sm else SRC_COLOR[s]) for sm in small], alpha=0.9, label="share of the teacher's posts the model gets (recall)")
        a.plot(prec[order], y, "kD", ms=4, label="share of the model's posts the teacher agrees with (precision)")
        a.set_yticks(y, [f"{broad[i]}  (n={sup[i]})" for i in order], fontsize=8)
        a.set_xlim(0, 1)
        a.set_title(SRC_NAME[s] + "\ngrey = fewer than 30 test posts, read with care", fontsize=10)
        a.set_xlabel("top pick agrees with the teacher's top pick")
    ax[0].legend(fontsize=7.5, loc="lower right")
    fig.tight_layout()
    save(fig, out, "07-per-topic-agreement.png")


# 7 ---------------------------------------------------------------------------------------------------------------
def label_mix(out, src, teach, broad):
    fig, ax = plt.subplots(1, 2, figsize=(12, 7.5), sharey=True)
    tot = np.bincount(teach.argmax(1), minlength=len(broad))
    order = np.argsort(tot)
    for a, s in zip(ax, ("jev", "clef")):
        m = src == s
        cnt = np.bincount(teach[m].argmax(1), minlength=len(broad))
        a.barh(np.arange(len(broad)), 100 * cnt[order] / cnt.sum(), color=SRC_COLOR[s])
        for y, i in enumerate(order):
            a.text(100 * cnt[i] / cnt.sum() + 0.2, y, f"{cnt[i]:,}", va="center", fontsize=7)
        a.set_yticks(range(len(broad)), [broad[i] for i in order], fontsize=8)
        a.set_title(f"{SRC_NAME[s]}\n{cnt.sum():,} training rows")
        a.set_xlabel("% of this teacher's rows (by its top broad topic)")
    fig.suptitle("What the training labels look like", fontweight="bold")
    fig.tight_layout()
    save(fig, out, "08-training-label-mix.png")


# 8 ---------------------------------------------------------------------------------------------------------------
def human_checks(out, rows):
    fig, ax = plt.subplots(1, 3, figsize=(17, 5.2))
    # blind side by side
    groups = collections.OrderedDict([("ModernVBERT 8 ep\ntext (vs Jev)", ("blind-ab-8-epochs-vs-teacher", "Jev")),
                                      ("ModernVBERT 8 ep\npictures (vs Clef)", ("blind-ab-8-epochs-vs-teacher", "Clef-flash")),
                                      ("fusion model\npictures (vs Clef)", ("blind-ab-fusion-vs-clef", "Clef-flash"))])
    cats = [("model", "#1b8f6a", "model better"), ("teacher", "#c0392b", "teacher better"), ("both", "#8fbf9f", "both fine"),
            ("neither", "#888", "neither"), ("unsure", "#d5d5d5", "can't tell")]
    a = ax[0]
    for gi, (label, (rnd, teacher)) in enumerate(groups.items()):
        sel = [r for r in rows if r["round"] == rnd and r["hidden_from_judge"]["teacher"] == teacher]
        c = collections.Counter(r["judgment"]["better"] for r in sel)
        left = 0
        for key, col, name in cats:
            if c[key]:
                a.barh(gi, c[key], left=left, color=col, label=name if gi == 0 else None)
                a.text(left + c[key] / 2, gi, str(c[key]), ha="center", va="center", fontsize=9, color="white" if key in ("model", "teacher") else "#222")
            left += c[key]
        a.text(left + 0.5, gi, f"n={len(sel)}", va="center", fontsize=8)
    a.set_yticks(range(3), list(groups), fontsize=8)
    a.invert_yaxis()
    a.set_title("Blind side by side, posts where the model and teacher disagree", fontsize=10)
    a.legend(fontsize=7, ncol=3, loc="lower center", bbox_to_anchor=(0.5, -0.28))
    # single answer verdicts
    a = ax[1]
    rounds = [("single-answer-3-epochs", "3-epoch student\n100 random posts"), ("single-answer-3-epochs-pictures", "3-epoch student\n50 picture posts"),
              ("changed-answers-8-epochs", "8-epoch student\n48 changed answers")]
    cats2 = [("right", "#1b8f6a"), ("acceptable", "#8fbf9f"), ("unsure", "#d5d5d5"), ("wrong", "#c0392b")]
    for gi, (rnd, label) in enumerate(rounds):
        sel = [r for r in rows if r["round"] == rnd]
        c = collections.Counter(r["judgment"]["topic"] for r in sel)
        left = 0
        for key, col in cats2:
            if c[key]:
                a.barh(gi, 100 * c[key] / len(sel), left=left, color=col, label=key if gi == 0 else None)
                if c[key] / len(sel) > 0.06:
                    a.text(left + 50 * c[key] / len(sel), gi, f"{c[key]}", ha="center", va="center", fontsize=9, color="white" if key in ("right", "wrong") else "#222")
            left += 100 * c[key] / len(sel)
    a.set_yticks(range(3), [r[1] for r in rounds], fontsize=8)
    a.invert_yaxis()
    a.set_xlim(0, 100)
    a.set_xlabel("% of posts (numbers = posts)")
    a.set_title("One answer shown: was the topic right?", fontsize=10)
    a.legend(fontsize=7, ncol=4, loc="lower center", bbox_to_anchor=(0.5, -0.32))
    # early comparison
    a = ax[2]
    early = [r for r in rows if r["round"] == "jev-v1-vs-clef-flash"]
    cats3 = [("jev", "#2f6db5", "Jev closer"), ("clef", "#e07b24", "Clef-flash closer"), ("both", "#8fbf9f", "both"), ("neither", "#888", "neither"), ("skip", "#d5d5d5", "skipped")]
    for gi, (label, pick) in enumerate((("text posts", lambda r: not r["post"].get("picture_hashes")), ("picture posts", lambda r: bool(r["post"].get("picture_hashes"))))):
        sel = [r for r in early if pick(r)]
        c = collections.Counter(r["judgment"]["closer_to_right"] for r in sel)
        left = 0
        for key, col, name in cats3:
            if c[key]:
                a.barh(gi, c[key], left=left, color=col, label=name if gi == 0 else None)
                a.text(left + c[key] / 2, gi, str(c[key]), ha="center", va="center", fontsize=9, color="white" if key in ("jev", "clef") else "#222")
            left += c[key]
        a.text(left + 0.5, gi, f"n={len(sel)}", va="center", fontsize=8)
    a.set_yticks(range(2), ["text posts", "picture posts"])
    a.invert_yaxis()
    a.set_title("Early check: Jev (v1 topics) vs first Clef-flash run, not blinded", fontsize=10)
    a.legend(fontsize=7, ncol=3, loc="lower center", bbox_to_anchor=(0.5, -0.28))
    fig.suptitle("Hand checks (one rater, the project owner)", fontweight="bold")
    fig.tight_layout()
    save(fig, out, "09-hand-checks.png")


def facts(src, teach, stu, broad):
    """Plain-language findings computed from the test posts, for the report."""
    out = []
    for s in ("jev", "clef"):
        m = src == s
        t, p = teach[m].argmax(1), stu[m].argmax(1)
        pair = collections.Counter()
        for i, j in zip(t, p):
            if i != j:
                pair[tuple(sorted((broad[i], broad[j])))] += 1
        top = ", ".join(f"{a} and {b} ({n})" for (a, b), n in pair.most_common(4))
        conf, hit = stu[m].max(1), p == t
        bins = []
        for lo, hi in ((0, 0.5), (0.5, 0.7), (0.7, 0.9), (0.9, 1.01)):
            k = np.logical_and(conf >= lo, conf < hi)
            bins.append(f"{100 * hit[k].mean():.0f}% ({int(k.sum())} posts) at confidence {lo:.1f} to {min(hi, 1):.1f}")
        out.append(f"- {SRC_NAME[s]}: the top pick differs from the teacher's on {int((t != p).sum())} of {int(m.sum())} posts. "
                   f"Topic pairs that trade the most posts (either direction): {top}.")
        out.append(f"- {SRC_NAME[s]}: the top pick equals the teacher's top pick on " + "; ".join(bins) + ".")
    return out


def write_report(out, scores, fus, counts, found):
    f = scores["fusion model"]
    lines = [
        "# Fusion topic classifier: results at a glance", "",
        "All numbers are on the held-out test posts: the newest 8% of each teacher's posts (8,384 text and link-card posts against Jev, 2,964 picture posts against Clef-flash).",
        "They measure how closely the model reproduces its teacher, not whether the answer is right. No post text is in any chart.", "",
        "| | text, 3 ep | text, 8 ep | text, fusion | pictures, 3 ep | pictures, 8 ep | pictures, fusion |", "|---|---|---|---|---|---|---|",
    ]
    for label, head, key, pct in (("broad topic, probability shared with teacher", "broad", "overlap_mean", False),
                                  ("subtopic, probability shared with teacher", "path", "overlap_mean", False),
                                  ("broad, top pick among teacher's main topics", "broad", "top_pick_in_area", True),
                                  ("broad, top pick equals teacher's top pick", "broad", "exact_top1", True),
                                  ("subtopic, top pick equals teacher's top pick", "path", "exact_top1", True)):
        cells = []
        for s in ("jev", "clef"):
            for name in scores:
                v = scores[name][head][s][key]
                cells.append(f"{100 * v:.1f}%" if pct else f"{v:.3f}")
        lines.append(f"| {label} | " + " | ".join(cells) + " |")
    lines += ["", "## Charts", "",
              "1. `01-training-curves.png`: loss and validation scores per epoch; the best epoch is kept (dotted line). Validation loss flattens after about epoch 5 (2.60 to 2.59) while the top-pick score still creeps up by about a point.",
              "2. `02-model-comparison.png`: the three students side by side. Text barely moves between models; pictures improve with the fusion model.",
              "3. `03-per-post-closeness.png`: how close the model's whole list is to the teacher's, post by post.",
              "4. `04-confidence-vs-agreement.png`: the more confident the model, the more it agrees with its teacher.",
              "5. `05-confusion-text.png`, `06-confusion-pictures.png`: for each teacher topic, where the model's top pick goes.",
              "6. `07-per-topic-agreement.png`: which topics the model gets and which it misses, with the number of test posts behind each bar.",
              "7. `08-training-label-mix.png`: what the labels it was trained on look like.",
              "8. `09-hand-checks.png`: the owner's judgments by round (one rater, small samples).", "",
              "## What the test posts show", "", *found, "",
              "## Reading them", "",
              "- A teacher's own uncertainty caps agreement: Clef-flash (calibrated) shares only 0.683 of its broad-topic probability with Jev on the same 5,000 text posts.",
              "- Hand-check samples are 25 to 100 posts each; differences of a few posts are noise.",
              f"- Fusion training: {fus['train_seconds']:.0f} s on one RTX 5090, best epoch {fus['best_epoch'] + 1} of 8, single seed.", ""]
    (out / "report.md").write_text("\n".join(lines))
    print("wrote report.md")


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--models", default="/data/models")
    ap.add_argument("--export", default="/data/mm/v21")
    ap.add_argument("--judged", default="/data/judged-sets/judged-sets.jsonl")
    ap.add_argument("--out", default="/data/reports/fusion-model")
    a = ap.parse_args()
    out = Path(a.out)
    out.mkdir(parents=True, exist_ok=True)
    fus, mm2 = load_json(f"{a.models}/fusion1/metrics.json"), load_json(f"{a.models}/mm2/metrics.json")
    cfg = load_json(f"{a.models}/fusion1/config.json")
    scores = {"ModernVBERT, 3 epochs": load_json(f"{a.models}/mm2/score/score-mm1-test.json"),
              "ModernVBERT, 8 epochs": load_json(f"{a.models}/mm2/score/score-mm2-test.json"),
              "fusion model": {"broad": fus["closeness"]["broad"], "path": fus["closeness"]["path"]}}
    src_all, teach_all = export_rows(a.export, cfg["broad"])
    z = np.load(f"{a.models}/fusion1/test-probs.npz")
    idx = z["idx"]
    src, teach, stu = src_all[idx], teach_all[idx], z["broad"]
    print("test posts", len(idx), dict(collections.Counter(src)))
    sanity = float(np.minimum(teach[src == "jev"], stu[src == "jev"]).sum(1).mean())
    print("recomputed text broad shared", round(sanity, 4), "vs metrics", round(fus["closeness"]["broad"]["jev"]["overlap_mean"], 4))
    rows = [json.loads(line) for line in open(a.judged)]

    training_curves(out, fus, mm2)
    model_comparison(out, scores)
    closeness_hist(out, src, teach, stu)
    reliability(out, src, teach, stu)
    confusion(out, src, teach, stu, cfg["broad"])
    per_topic(out, src, teach, stu, cfg["broad"])
    label_mix(out, src_all, teach_all, cfg["broad"])
    human_checks(out, rows)
    write_report(out, scores, fus, collections.Counter(src_all), facts(src, teach, stu, cfg["broad"]))


if __name__ == "__main__":
    main()
