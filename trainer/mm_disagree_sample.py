"""Draws held-out posts where a trained picture-capable student really disagrees with its teacher, for a blind review.

Runs the student over the whole test set (the newest posts of each teacher, never trained on). A post counts as a real
disagreement when the student's best broad topic falls outside the teacher's target area: the fewest topics that together
hold at least --area of the teacher's probability (near ties between two close topics are not counted). From those it
draws --n-text text posts (teacher Jev) and --n-pictures posts with pictures (teacher Clef) at random, skipping posts that
earlier hand checks used (--exclude). Writes

  <out>/sample.json        the drawn posts in a random order, each with the teacher's answer and which of the two
                           answers (model, teacher) is shown first, to be used only when the verdicts are analysed
  <out>/predictions.jsonl  the student's answers for those posts, in mm_predict.py's format

    HF_HUB_OFFLINE=1 python mm_disagree_sample.py --model /root/models/mm2 --export /root/mm/v21 --images /root/images \\
        --taxonomy /root/mm/taxonomy/v2.1.yaml --out /root/mm/eval-disagree --exclude /root/mm/eval/sample.json
"""

import argparse
import json
import random
import sys
import time
from pathlib import Path

import numpy as np

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--model", required=True, help="folder with model.pt and config.json")
    ap.add_argument("--export", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--images", default="/root/images")
    ap.add_argument("--taxonomy", default=str(HERE.parent / "taxonomy" / "v2.1.yaml"))
    ap.add_argument("--n-text", type=int, default=25)
    ap.add_argument("--n-pictures", type=int, default=25)
    ap.add_argument("--seed", type=int, default=20261006)
    ap.add_argument("--area", type=float, default=0.8)
    ap.add_argument("--exclude", action="append", default=[], help="a sample.json of an earlier hand check; its posts are not drawn")
    ap.add_argument("--bs", type=int, default=64)
    ap.add_argument("--workers", type=int, default=12)
    ap.add_argument("--probs", help="test-probs.npz saved by train_fusion.py; use it instead of running a student model")
    a = ap.parse_args()

    import torch
    import torch.nn.functional as F

    import common
    import mm_score as M
    import train_mm as T

    cfg = json.load(open(f"{a.model}/config.json"))
    space = common.Space.from_taxonomy(a.taxonomy)
    assert space.broad == cfg["broad"] and space.paths == cfg["paths"], "the taxonomy differs from the one the model was trained on"
    rows = T.Rows(a.export, space)
    tr, va, te = T.split(rows, 0.08, 0.04)
    skip = {p["uri"] for path in a.exclude for p in json.load(open(path))["posts"]}
    t0 = time.time()
    if a.probs:  # a model that saved its test-set probabilities (train_fusion.py): no model needs to run
        saved = np.load(a.probs)
        assert np.array_equal(saved["idx"], te), "the saved probabilities are for a different test set"
        qb, qp, qs, qt = saved["broad"].astype(float), saved["path"].astype(float), None, None
    else:
        dev = torch.device("cuda" if torch.cuda.is_available() else "cpu")

        from transformers import AutoProcessor

        processor = AutoProcessor.from_pretrained(cfg["base"])
        model = T.Student(cfg["base"], len(space.broad), len(space.paths), len(T.SIGNALS)).to(dev)
        model.load_state_dict(torch.load(f"{a.model}/model.pt", map_location=dev))
        ns = argparse.Namespace(eval_bs=a.bs, workers=a.workers, images=a.images, max_images=cfg["max_images"])
        lb, lp, ls, lt = T.predict(model, rows, te, processor, ns, dev)
        qb = F.softmax(lb / cfg["temperature_broad"], -1).numpy()
        qp = F.softmax(lp / cfg["temperature_path"], -1).numpy()
        qs, qt = torch.sigmoid(ls).numpy(), F.softmax(lt, -1).numpy()
    print(f"scored {len(te)} test posts in {time.time() - t0:.0f}s", flush=True)

    c = M.closeness(rows.broad[te], qb, a.area)
    outside = c["top_in_area"] == 0
    src = rows.source[te]
    pool = {s: [k for k in range(len(te)) if outside[k] and src[k] == s and rows.uris[te[k]] not in skip] for s in ("jev", "clef")}
    for s in pool:
        total = int((src == s).sum())
        print(f"{s}: {int((outside & (src == s)).sum())} of {total} test posts ({100 * (outside & (src == s)).sum() / total:.0f}%) have the model's top pick outside the teacher's target area; "
              f"{len(pool[s])} left after leaving out earlier hand checks", flush=True)
    rng = random.Random(a.seed)
    picks = rng.sample(pool["jev"], a.n_text) + rng.sample(pool["clef"], a.n_pictures)
    rng.shuffle(picks)

    def top(vec, names, k):
        order = np.argsort(-vec)[:k]
        return {names[j]: round(float(vec[j]), 4) for j in order}

    posts = []
    with open(Path(a.out).mkdir(parents=True, exist_ok=True) or f"{a.out}/predictions.jsonl", "w") as f:
        for k in picks:
            i = int(te[k])
            posts.append({"uri": rows.uris[i], "source": str(rows.source[i]), "t": int(rows.t[i]),
                          "teacher": {"broad": top(rows.broad[i], space.broad, 3), "paths": top(rows.path[i], space.paths, 3),
                                      "tone": top(rows.tone[i], T.TONES, 2) if not np.isnan(rows.tone[i]).any() else {}},
                          "model_top": {"broad": space.broad[int(qb[k].argmax())], "path": space.paths[int(qp[k].argmax())]},
                          "order": rng.choice([["model", "teacher"], ["teacher", "model"]])})
            f.write(json.dumps({
                "uri": rows.uris[i], "source": str(rows.source[i]),
                "broad": {space.broad[j]: round(float(qb[k][j]), 4) for j in np.argsort(-qb[k])},
                "paths": {space.paths[j]: round(float(qp[k][j]), 4) for j in np.argsort(-qp[k])[:8]},
                "signals": {name: round(float(qs[k][j]), 4) for j, name in enumerate(T.SIGNALS)} if qs is not None else {},
                "tone": {name: round(float(qt[k][j]), 4) for j, name in enumerate(T.TONES)} if qt is not None else {},
                "images_shown": min(len(rows.shas[i]), cfg["max_images"]), "model_input": rows.texts[i]}) + "\n")
    json.dump({"seed": a.seed, "n": len(posts), "test_size": sum(len(v) for v in pool.values()), "area": a.area,
               "pool": {s: len(v) for s, v in pool.items()},
               "description": " and ".join(x for x in (f"{a.n_text} text posts" if a.n_text else "", f"{a.n_pictures} picture posts" if a.n_pictures else "") if x)
                              + " the model never trained on, where its top topic falls outside the teacher's main area; "
                              "two answers per post in random order, you are not told which is which",
               "posts": posts}, open(f"{a.out}/sample.json", "w"), indent=1)
    print(f"wrote {len(posts)} posts to {a.out}")


if __name__ == "__main__":
    main()
