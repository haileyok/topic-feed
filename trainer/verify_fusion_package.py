"""Checks the standalone fusion-model package (topic_classifier.py) against what training produced.

Two checks, run separately:

  --render   topic_classifier.render_post on the raw posts must give exactly the stored model_input of the training export
             (needs --posts, the raw posts file; runs on a CPU, no model loaded)
  --predict  the package's predictions on held-out test posts must match test-probs.npz written by train_fusion.py
             (needs a GPU box with the export, the picture archive and the SigLIP 2 weights)

    python verify_fusion_package.py --package /data/hf/microblog-topic-classifier-v5 --export /data/mm/v21 \\
        --render --posts /data/clef/full_posts.jsonl
    python verify_fusion_package.py --package /root/pkg --export /root/mm/v21 --images /root/images \\
        --predict --probs /root/models/fusion1/test-probs.npz --n 60
"""

import argparse
import gzip
import json
import random
import sys
from pathlib import Path

import numpy as np


def export_rows(export, broad):
    """The export rows training used, in training order (rows with no known broad topic probability were skipped)."""
    known = set(broad)
    rows = []
    with gzip.open(f"{export}/labels.jsonl.gz", "rt") as f:
        for line in f:
            r = json.loads(line)
            if sum(v for k, v in r["broad_probs"].items() if k in known) > 0:
                rows.append(r)
    return rows


def check_render(a):
    sys.path.insert(0, a.package)
    from topic_classifier import render_post

    cfg = json.load(open(f"{a.package}/config.json"))
    rows = export_rows(a.export, cfg["broad"])
    rng = random.Random(a.seed)
    pick = {}
    for r in rng.sample(rows, min(a.n, len(rows))):
        pick[r["uri"]] = r
    posts = {}
    with open(a.posts) as f:
        for line in f:
            if not line.startswith('{"uri":"'):
                continue
            u = json.loads('"' + line[8:line.index('"', 8)] + '"')
            if u in pick:
                posts[u] = json.loads(line)
    bad, n = [], 0
    for u, r in pick.items():
        if u not in posts:
            continue
        n += 1
        mine = render_post(posts[u], with_pictures=bool(r["image_shas"]))
        if mine != r["model_input"]:
            bad.append(u)
    print(json.dumps({"checked": n, "different": len(bad), "examples": bad[:3]}))
    return not bad


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--package", required=True)
    ap.add_argument("--export", required=True)
    ap.add_argument("--images", default="/root/images")
    ap.add_argument("--posts")
    ap.add_argument("--probs")
    ap.add_argument("--render", action="store_true")
    ap.add_argument("--predict", action="store_true")
    ap.add_argument("--n", type=int, default=2000, help="posts to check (per source for --predict)")
    ap.add_argument("--seed", type=int, default=0)
    ap.add_argument("--device", default=None)
    ap.add_argument("--from-hub", help="load the model from this Hugging Face repository id instead of --package")
    a = ap.parse_args()
    ok = True
    if a.render:
        ok &= check_render(a)
    if a.predict:
        sys.path.insert(0, a.package)
        from topic_classifier import TopicClassifier

        cfg = json.load(open(f"{a.package}/config.json"))
        rows = export_rows(a.export, cfg["broad"])
        z = np.load(a.probs)
        idx = z["idx"]
        pos = {int(i): k for k, i in enumerate(idx)}
        rng = random.Random(a.seed)
        chosen = []
        for src in ("jev", "clef"):
            have = [int(i) for i in idx if rows[int(i)]["source"] == src]
            chosen += rng.sample(have, min(a.n, len(have)))
        items = []
        for i in chosen:
            r = rows[i]
            items.append({"text": r["model_input"],
                          "pictures": [f"{a.images}/1000/{s[:2]}/{s}.jpg" for s in r["image_shas"][: cfg["max_images"]]]})
        clf = TopicClassifier.from_pretrained(a.from_hub or a.package, device=a.device)
        out = clf.predict_arrays(items, batch_size=32)
        res = {}
        for name, key in (("broad", "broad"), ("paths", "path")):
            want = z[key][[pos[i] for i in chosen]]
            diff = np.abs(out[name] - want)
            pic = np.array([bool(rows[i]["image_shas"]) for i in chosen])
            res[name] = {
                "posts": len(chosen), "picture_posts": int(pic.sum()),
                "max_abs_diff": float(diff.max()), "mean_abs_diff": float(diff.mean()),
                "top1_same": float((out[name].argmax(1) == want.argmax(1)).mean()),
                "top1_same_pictures": float((out[name].argmax(1) == want.argmax(1))[pic].mean()) if pic.any() else None,
                "top1_same_text": float((out[name].argmax(1) == want.argmax(1))[~pic].mean()) if (~pic).any() else None,
                "max_abs_diff_pictures": float(diff[pic].max()) if pic.any() else None,
            }
        print(json.dumps(res, indent=1))
        ok &= res["broad"]["top1_same"] >= 0.97 and res["broad"]["mean_abs_diff"] < 0.01
    print("OK" if ok else "FAILED")
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
