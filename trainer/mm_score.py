"""Scores a trained picture-capable student by how close its probabilities come to the teacher's.

For every held-out post the student's probability list is compared with the teacher's whole list
(the same targets train_mm.py trained against), for the broad topic, the subtopic path and the tone:

  overlap          how much probability the two lists share (sum over topics of the smaller of the two):
                   1 = identical, 0 = nothing in common. Equal to 1 minus the total variation distance.
  KL               the extra cost, in nats, of using the student's list where the teacher's is the truth
                   (the validation loss minus the teacher's own entropy, which no student can beat).
  target area      the fewest topics that together hold at least --area of the teacher's probability
                   (one or two for a sharp answer, more for a split one). Reported: how often the
                   student's top pick is inside the area, and how much of the student's probability is.

The exact top-pick match and top-3 are kept as side notes. Everything is split by teacher (text posts
come from Jev, picture posts from Clef) and by how sure the teacher was.

    HF_HUB_OFFLINE=1 python mm_score.py --model /root/models/mm2 --export /root/mm/v21 --images /root/images \\
        --taxonomy /root/mm/taxonomy/v2.1.yaml --out /root/mm/score-mm2-test.json
"""

import argparse
import json
import sys
import time
from pathlib import Path

import numpy as np

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))

BINS = [("teacher under 50% sure", 0.0, 0.5), ("teacher 50-80% sure", 0.5, 0.8), ("teacher 80%+ sure", 0.8, 1.01)]


def closeness(p: np.ndarray, q: np.ndarray, area: float = 0.8) -> dict:
    """Per-post closeness of the student's lists q to the teacher's lists p (both rows of probabilities)."""
    p = p / p.sum(-1, keepdims=True)
    q = q / q.sum(-1, keepdims=True)
    overlap = np.minimum(p, q).sum(-1)
    kl = (p * (np.log(np.clip(p, 1e-12, None)) - np.log(np.clip(q, 1e-9, None)))).sum(-1)
    order = np.argsort(-p, -1)
    size = (np.cumsum(np.take_along_axis(p, order, -1), -1) < area - 1e-9).sum(-1) + 1  # fewest topics reaching `area`
    in_sorted = np.arange(p.shape[1])[None, :] < size[:, None]
    mask = np.zeros(p.shape, bool)
    np.put_along_axis(mask, order, in_sorted, -1)
    rows = np.arange(len(p))
    return {"overlap": overlap, "kl": kl, "area_size": size.astype(float),
            "top_in_area": mask[rows, q.argmax(-1)].astype(float), "mass_in_area": (q * mask).sum(-1),
            "top1": (q.argmax(-1) == p.argmax(-1)).astype(float),
            "top3": (np.argsort(-q, -1)[:, :3] == p.argmax(-1)[:, None]).any(-1).astype(float)}


def summarise(c: dict) -> dict:
    return {"n": int(len(c["overlap"])), "overlap_mean": float(c["overlap"].mean()), "overlap_median": float(np.median(c["overlap"])),
            "overlap_at_least_0.8": float((c["overlap"] >= 0.8).mean()), "overlap_at_least_0.6": float((c["overlap"] >= 0.6).mean()),
            "kl_mean": float(c["kl"].mean()), "area_size_mean": float(c["area_size"].mean()),
            "top_pick_in_area": float(c["top_in_area"].mean()), "student_mass_in_area": float(c["mass_in_area"].mean()),
            "exact_top1": float(c["top1"].mean()), "top3_holds_teacher_top": float(c["top3"].mean())}


def score_head(p: np.ndarray, q: np.ndarray, source: np.ndarray, area: float) -> dict:
    out = {}
    for src in sorted(set(source)):
        m = source == src
        c = closeness(p[m], q[m], area)
        d = summarise(c)
        top_p = p[m].max(-1) / p[m].sum(-1)
        d["by_teacher_confidence"] = {name: summarise({k: v[(top_p >= lo) & (top_p < hi)] for k, v in c.items()})
                                      for name, lo, hi in BINS if ((top_p >= lo) & (top_p < hi)).any()}
        out[src] = d
    return out


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--model", required=True, help="folder with model.pt and config.json")
    ap.add_argument("--export", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--split", default="test", choices=["test", "val"])
    ap.add_argument("--images", default="/root/images")
    ap.add_argument("--taxonomy", default=str(HERE.parent / "taxonomy" / "v2.1.yaml"))
    ap.add_argument("--area", type=float, default=0.8, help="share of the teacher's probability its target area must hold")
    ap.add_argument("--no-temperature", action="store_true", help="use the raw probabilities, not the ones fitted on the validation posts")
    ap.add_argument("--bs", type=int, default=64)
    ap.add_argument("--workers", type=int, default=12)
    ap.add_argument("--limit", type=int, default=0, help="score only this many posts per teacher (for a quick check)")
    a = ap.parse_args()

    import torch
    import torch.nn.functional as F

    import common
    import train_mm as T

    cfg = json.load(open(f"{a.model}/config.json"))
    space = common.Space.from_taxonomy(a.taxonomy)
    assert space.broad == cfg["broad"] and space.paths == cfg["paths"], "the taxonomy differs from the one the model was trained on"
    rows = T.Rows(a.export, space)
    tr, va, te = T.split(rows, 0.08, 0.04)
    idx = te if a.split == "test" else va
    if a.limit:
        idx = np.concatenate([idx[rows.source[idx] == s][: a.limit] for s in sorted(set(rows.source))])
    dev = torch.device("cuda" if torch.cuda.is_available() else "cpu")

    from transformers import AutoProcessor

    processor = AutoProcessor.from_pretrained(cfg["base"])
    model = T.Student(cfg["base"], len(space.broad), len(space.paths), len(T.SIGNALS)).to(dev)
    model.load_state_dict(torch.load(f"{a.model}/model.pt", map_location=dev))
    ns = argparse.Namespace(eval_bs=a.bs, workers=a.workers, images=a.images, max_images=cfg["max_images"])
    t0 = time.time()
    lb, lp, ls, lt = T.predict(model, rows, idx, processor, ns, dev)
    tb, tp = (1.0, 1.0) if a.no_temperature else (cfg["temperature_broad"], cfg["temperature_path"])
    qb = F.softmax(lb / tb, -1).numpy()
    qp = F.softmax(lp / tp, -1).numpy()
    qt = F.softmax(lt, -1).numpy()
    src = rows.source[idx]

    result = {"model": a.model, "split": a.split, "area": a.area, "temperature": [tb, tp], "posts": int(len(idx)), "seconds": round(time.time() - t0),
              "broad": score_head(rows.broad[idx], qb, src, a.area), "path": score_head(rows.path[idx], qp, src, a.area)}
    known = ~np.isnan(rows.tone[idx]).any(-1)
    result["tone"] = score_head(rows.tone[idx][known], qt[known], src[known], a.area)
    json.dump(result, open(a.out, "w"), indent=2)

    for head in ("broad", "path", "tone"):
        print(f"\n== {head} ({a.model}, {a.split} posts)")
        for s, d in result[head].items():
            print(f"  {s:5s} n={d['n']:5d} overlap {d['overlap_mean']:.3f} (median {d['overlap_median']:.3f}, >=0.8: {d['overlap_at_least_0.8']:.0%}) "
                  f"KL {d['kl_mean']:.2f} | area ~{d['area_size_mean']:.1f} topics: top pick inside {d['top_pick_in_area']:.0%}, "
                  f"student mass inside {d['student_mass_in_area']:.0%} | exact top-1 {d['exact_top1']:.0%}")


if __name__ == "__main__":
    main()
