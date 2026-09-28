"""Compare a trained student with Jev's raw outputs on the test windows.

Goes beyond top-1 agreement: how close the probability distributions are, and how
agreement depends on how sure Jev itself was.

    uv run python compare.py --model /data/models/v1
"""

import argparse
import json
import os

import numpy as np
import torch
import torch.nn.functional as F
from transformers import AutoTokenizer

import common
import compare_viewer
from train import Student, predict


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--model", required=True)
    ap.add_argument("--export", default=None, help="default: the export the model was trained on")
    ap.add_argument("--taxonomy", default="../taxonomy/v1.yaml")
    ap.add_argument("--page", action="store_true", help="also write the side-by-side review page to /data/reports/<model>-vs-jev")
    a = ap.parse_args()

    cfg = json.load(open(f"{a.model}/config.json"))
    metrics = json.load(open(f"{a.model}/metrics.json"))
    export = a.export or metrics["args"]["export"]
    space = common.Space.from_taxonomy(a.taxonomy)
    _, _, te, _ = common.split(common.load(export, space))

    tok = AutoTokenizer.from_pretrained(a.model)
    model = Student(cfg["base"], len(space.broad), len(space.paths)).cuda()
    model.load_state_dict(torch.load(f"{a.model}/model.pt", map_location="cuda"))
    lb, lp, ls, lt = predict(model, te, tok, cfg["max_len"])
    sb = F.softmax(lb / cfg["temperature_broad"], -1).numpy()
    sp = F.softmax(lp / cfg["temperature_path"], -1).numpy()
    sig = torch.sigmoid(ls).numpy()

    jb, jp = te.broad, te.path
    jev_top, stu_top = jb.argmax(1), sb.argmax(1)
    jev_conf = jb.max(1)
    agree = stu_top == jev_top
    jev_second = np.argsort(-jb, 1)[:, 1]

    def tvd(p, q):  # total variation distance: half the L1 distance, 0..1
        return 0.5 * np.abs(p - q).sum(1)

    out = {"n_test": len(te), "export": export}
    out["broad"] = {
        "top1_agreement": float(agree.mean()),
        "mean_total_variation": float(tvd(sb, jb).mean()),
        "mean_abs_prob_diff_on_jev_top": float(np.abs(sb[np.arange(len(te)), jev_top] - jev_conf).mean()),
        "disagreements_where_student_picked_jevs_2nd": float((stu_top[~agree] == jev_second[~agree]).mean()),
        "corr_student_vs_jev_top_prob": float(np.corrcoef(sb.max(1), jev_conf)[0, 1]),
    }
    out["path"] = {"top1_agreement": float((sp.argmax(1) == jp.argmax(1)).mean()),
                   "mean_total_variation": float(tvd(sp, jp).mean())}

    buckets = [("Jev >= 0.9", jev_conf >= 0.9), ("Jev 0.6-0.9", (jev_conf >= 0.6) & (jev_conf < 0.9)), ("Jev < 0.6", jev_conf < 0.6)]
    out["by_jev_confidence"] = []
    for name, m in buckets:
        out["by_jev_confidence"].append({
            "bucket": name, "share_of_posts": float(m.mean()),
            "broad_top1": float(agree[m].mean()),
            "path_top1": float((sp.argmax(1)[m] == jp.argmax(1)[m]).mean()),
            "student_top_in_jevs_top2": float(((stu_top == jev_top) | (stu_top == jev_second))[m].mean()),
        })

    out["signals"] = {k: {"mae": float(np.abs(sig[:, i] - te.signals[:, i]).mean()),
                          "corr": float(np.corrcoef(sig[:, i], te.signals[:, i])[0, 1])}
                      for i, k in enumerate(common.SIGNALS)}

    # Most common confident disagreements (Jev >= 0.9): which topic pairs.
    m = (jev_conf >= 0.9) & ~agree
    pairs = {}
    for j, s in zip(jev_top[m], stu_top[m]):
        key = f"Jev {space.broad[j]} -> student {space.broad[s]}"
        pairs[key] = pairs.get(key, 0) + 1
    out["confident_disagreement_pairs"] = sorted(pairs.items(), key=lambda kv: -kv[1])[:8]
    json.dump(out, open(f"{a.model}/compare_jev.json", "w"), indent=2)
    print(json.dumps(out, indent=2))

    if a.page:
        name = os.path.basename(a.model.rstrip("/"))
        page_dir = os.path.join("/data/reports", f"{name}-vs-jev")
        os.makedirs(page_dir, exist_ok=True)
        compare_viewer.write(os.path.join(page_dir, "index.html"), name, space, te, sb, sp, sig)
        print(f"wrote {page_dir}/index.html")


if __name__ == "__main__":
    main()
