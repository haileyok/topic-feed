"""Builds a hand-check batch of only the posts whose answer a newer model changed.

Takes an earlier hand check (sample.json, predictions.jsonl and verdicts.jsonl in one or more folders) and the newer
model's answers for the same posts (predictions-<name>.jsonl in each folder). A post is kept when the newer model's
best broad topic or best subtopic path differs from the older model's. Posts whose answer did not change keep the
verdict you already gave. Writes sample.json and predictions.jsonl in --out, in the shape mm_eval_app.py reads;
the older answer and the earlier verdict are left out so the judging stays blind to them.

    python3 mm_eval_changed.py --dirs /data/models/mm1/eval /data/models/mm1/eval-pictures --new mm2 --out /data/models/mm2/eval-changed
"""

import argparse
import json
import os
import random


def top(p, key):
    return max(p[key].items(), key=lambda kv: kv[1])[0]


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--dirs", nargs="+", required=True)
    ap.add_argument("--new", required=True, help="name of the newer model: predictions-<name>.jsonl in each folder")
    ap.add_argument("--out", required=True)
    ap.add_argument("--seed", type=int, default=20261005)
    a = ap.parse_args()

    posts, preds, kept_old = [], {}, {}
    for d in a.dirs:
        sample = json.load(open(os.path.join(d, "sample.json")))["posts"]
        old = {json.loads(l)["uri"]: json.loads(l) for l in open(os.path.join(d, "predictions.jsonl"))}
        new = {json.loads(l)["uri"]: json.loads(l) for l in open(os.path.join(d, f"predictions-{a.new}.jsonl"))}
        for p in sample:
            u = p["uri"]
            if top(old[u], "broad") != top(new[u], "broad") or top(old[u], "paths") != top(new[u], "paths"):
                posts.append(p)
                preds[u] = new[u]
                kept_old[u] = {"broad": top(old[u], "broad"), "path": top(old[u], "paths")}
    random.Random(a.seed).shuffle(posts)
    os.makedirs(a.out, exist_ok=True)
    json.dump({"seed": a.seed, "n": len(posts), "test_size": len(posts), "posts": posts,
               "description": f"only the {len(posts)} posts you already judged where the new 8-epoch model's topic or subtopic differs from the first 3-epoch model's"},
              open(os.path.join(a.out, "sample.json"), "w"), indent=1)
    with open(os.path.join(a.out, "predictions.jsonl"), "w") as f:
        for p in posts:
            f.write(json.dumps(preds[p["uri"]]) + "\n")
    json.dump(kept_old, open(os.path.join(a.out, "old-answers.json"), "w"), indent=1)  # for the report afterwards, not shown on the page
    print(f"{len(posts)} changed posts written to {a.out}")


if __name__ == "__main__":
    main()
