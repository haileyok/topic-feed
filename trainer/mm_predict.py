"""Runs a trained picture-capable student (train_mm.py's output) over a small export and writes its answers.

For every post in the export: the probability of each broad topic and of the best subtopic paths
(both divided by the temperatures fitted on the validation posts, as train_mm.py saved them), the
signals including meme, the tone, how many pictures it was shown, and the text it read. One JSON
line per post.

    HF_HUB_OFFLINE=1 python mm_predict.py --model /root/models/mm1 --export /root/mm/eval/sample-export \\
        --images /root/images --out /root/mm/eval/predictions.jsonl
"""

import argparse
import json
import sys
import time
from pathlib import Path

import numpy as np
import torch
import torch.nn.functional as F

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import common  # noqa: E402
import train_mm as T  # noqa: E402


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--model", required=True, help="folder with model.pt and config.json")
    ap.add_argument("--export", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--images", default="/root/images", help="picture archive root (holds 1000/)")
    ap.add_argument("--taxonomy", default=str(HERE.parent / "taxonomy" / "v2.1.yaml"))
    ap.add_argument("--bs", type=int, default=16)
    ap.add_argument("--workers", type=int, default=4)
    ap.add_argument("--top-paths", type=int, default=8)
    a = ap.parse_args()

    cfg = json.load(open(f"{a.model}/config.json"))
    space = common.Space.from_taxonomy(a.taxonomy)
    assert space.broad == cfg["broad"] and space.paths == cfg["paths"], "the taxonomy differs from the one the model was trained on"
    assert T.SIGNALS == cfg["signals"], "the signal list differs from the one the model was trained on"
    rows = T.Rows(a.export, space)
    dev = torch.device("cuda" if torch.cuda.is_available() else "cpu")

    from transformers import AutoProcessor

    processor = AutoProcessor.from_pretrained(cfg["base"])
    model = T.Student(cfg["base"], len(space.broad), len(space.paths), len(T.SIGNALS)).to(dev)
    model.load_state_dict(torch.load(f"{a.model}/model.pt", map_location=dev))
    ns = argparse.Namespace(eval_bs=a.bs, workers=a.workers, images=a.images, max_images=cfg["max_images"])

    t0 = time.time()
    lb, lp, ls, lt = T.predict(model, rows, np.arange(len(rows)), processor, ns, dev)
    pb = F.softmax(lb / cfg["temperature_broad"], -1).numpy()
    pp = F.softmax(lp / cfg["temperature_path"], -1).numpy()
    ps, pt = torch.sigmoid(ls).numpy(), F.softmax(lt, -1).numpy()

    with open(a.out, "w") as f:
        for i in range(len(rows)):
            top = np.argsort(-pp[i])[: a.top_paths]
            f.write(json.dumps({
                "uri": rows.uris[i], "source": str(rows.source[i]),
                "broad": {space.broad[k]: round(float(pb[i][k]), 4) for k in np.argsort(-pb[i])},
                "paths": {space.paths[k]: round(float(pp[i][k]), 4) for k in top},
                "signals": {name: round(float(ps[i][k]), 4) for k, name in enumerate(T.SIGNALS)},
                "tone": {name: round(float(pt[i][k]), 4) for k, name in enumerate(T.TONES)},
                "images_shown": min(len(rows.shas[i]), cfg["max_images"]),
                "model_input": rows.texts[i],
            }) + "\n")
    print(f"{len(rows)} posts in {time.time() - t0:.0f}s on {dev}; wrote {a.out}")


if __name__ == "__main__":
    main()
