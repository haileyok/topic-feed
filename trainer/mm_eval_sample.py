"""Draws random posts from the picture-capable student's held-out test set, for a hand check.

The test set is what train_mm.py never trained on or validated against: per teacher, the newest
--test-frac of the posts that have a timestamp. This picks --n of them at random (seeded, so the
draw can be repeated) from both teachers together, so the mix of text and picture posts follows the
test set. It writes

  <out>/sample.json            the drawn posts in a fixed random order, each with what the teacher said
                               (kept out of the page that shows the student's answers)
  <out>/sample-export/         those posts as a small training-format export that mm_predict.py reads

    CUDA_VISIBLE_DEVICES= /data/clef/.venv/bin/python mm_eval_sample.py --export /data/mm/v21 \\
        --out /data/models/mm1/eval --n 100
"""

import argparse
import gzip
import json
import random
import re
import shutil
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import common  # noqa: E402
import train_mm as T  # noqa: E402

URI_AT_START = re.compile(r'^\{"uri": ?"((?:[^"\\]|\\.)*)"')


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--export", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--n", type=int, default=100)
    ap.add_argument("--seed", type=int, default=20261003)
    ap.add_argument("--pictures-only", action="store_true", help="draw only from posts the student was shown pictures for")
    ap.add_argument("--exclude", action="append", default=[], help="a sample.json of an earlier draw whose posts are not drawn again; repeat")
    ap.add_argument("--taxonomy", default=str(HERE.parent / "taxonomy" / "v2.1.yaml"))
    ap.add_argument("--test-frac", type=float, default=0.08, help="as used for training")
    ap.add_argument("--val-frac", type=float, default=0.04, help="as used for training")
    a = ap.parse_args()

    space = common.Space.from_taxonomy(a.taxonomy)
    rows = T.Rows(a.export, space)
    tr, va, te = T.split(rows, a.test_frac, a.val_frac)
    assert not (set(te) & set(tr)) and not (set(te) & set(va)), "the test set overlaps training or validation"
    skip = {p["uri"] for path in a.exclude for p in json.load(open(path))["posts"]}
    pool = [int(i) for i in te if rows.uris[int(i)] not in skip and (not a.pictures_only or rows.shas[int(i)])]
    assert len(pool) >= a.n, f"only {len(pool)} posts to draw from"
    picked = random.Random(a.seed).sample(pool, a.n)  # the order of this list is the fixed random order

    out = Path(a.out)
    (out / "sample-export").mkdir(parents=True, exist_ok=True)
    sample = []
    for i in picked:
        pb, pp = rows.broad[i], rows.path[i]
        sample.append({
            "uri": rows.uris[i], "source": str(rows.source[i]), "t": int(rows.t[i]),
            "teacher": {"broad": space.broad[int(pb.argmax())], "broad_p": float(pb.max()),
                        "path": space.paths[int(pp.argmax())], "path_p": float(pp.max()),
                        "meme": None if rows.signals[i][T.SIGNALS.index("meme")] != rows.signals[i][T.SIGNALS.index("meme")]
                        else float(rows.signals[i][T.SIGNALS.index("meme")])},
        })
    # test_size is the number of posts drawn from (the page says "drawn from N posts the model never trained on")
    json.dump({"seed": a.seed, "n": a.n, "export": a.export, "test_size": len(pool), "pictures_only": a.pictures_only,
               "excluded": len(skip), "posts": sample}, open(out / "sample.json", "w"), indent=1)

    want, kept = {s["uri"] for s in sample}, 0
    with gzip.open(Path(a.export) / "labels.jsonl.gz", "rt") as f, gzip.open(out / "sample-export" / "labels.jsonl.gz", "wt") as g:
        for line in f:
            m = URI_AT_START.match(line)
            if m and json.loads('"' + m.group(1) + '"') in want:
                g.write(line)
                kept += 1
    assert kept == len(want), f"found {kept} of {len(want)} drawn posts in the export"
    shutil.copy(Path(a.export) / "manifest.json", out / "sample-export" / "manifest.json")

    by = {s: sum(1 for p in sample if p["source"] == s) for s in ("jev", "clef")}
    print(f"test set {len(te)} posts, {len(pool)} to draw from; drew {len(sample)} (seed {a.seed}): text/link-card {by['jev']}, with pictures {by['clef']}")
    print(f"wrote {out / 'sample.json'} and {out / 'sample-export'}")


if __name__ == "__main__":
    main()
