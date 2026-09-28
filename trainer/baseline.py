"""Embedding baseline (plan §11.4): a small sentence-embedding model plus logistic
regression per level. It sets the bar the fine-tuned model has to beat.

    uv run python baseline.py --export /data/exports/v1-v0 --taxonomy ../taxonomy/v1.yaml --out /data/models/baseline-v0
"""

import argparse
import json
import os
import time

import numpy as np
import torch
from sklearn.linear_model import LogisticRegression
from transformers import AutoModel, AutoTokenizer

import common

EMBED_MODEL = "BAAI/bge-small-en-v1.5"


@torch.no_grad()
def embed(texts: list[str], bs: int = 512) -> np.ndarray:
    tok = AutoTokenizer.from_pretrained(EMBED_MODEL)
    model = AutoModel.from_pretrained(EMBED_MODEL, torch_dtype=torch.float16).cuda().eval()
    out = []
    for i in range(0, len(texts), bs):
        enc = tok(texts[i:i + bs], padding=True, truncation=True, max_length=256, return_tensors="pt").to("cuda")
        cls = model(**enc).last_hidden_state[:, 0]  # bge uses the CLS token
        out.append(torch.nn.functional.normalize(cls.float(), dim=-1).cpu().numpy())
    return np.concatenate(out)


def fit_predict(x_tr, y_tr, x_ev, n_classes):
    clf = LogisticRegression(max_iter=2000, C=4.0, n_jobs=-1)
    clf.fit(x_tr, y_tr)
    proba = np.zeros((len(x_ev), n_classes), np.float32)
    proba[:, clf.classes_] = clf.predict_proba(x_ev)
    return proba


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--export", required=True)
    ap.add_argument("--taxonomy", required=True)
    ap.add_argument("--out", required=True)
    a = ap.parse_args()

    space = common.Space.from_taxonomy(a.taxonomy)
    tr, va, te, info = common.split(common.load(a.export, space))
    print(json.dumps(info))
    t0 = time.time()
    x_tr, x_te = embed(tr.texts), embed(te.texts)
    print(f"embedded {len(tr) + len(te)} posts in {time.time() - t0:.0f}s")

    t0 = time.time()
    broad = fit_predict(x_tr, tr.broad.argmax(1), x_te, len(space.broad))
    path = fit_predict(x_tr, tr.path.argmax(1), x_te, len(space.paths))
    print(f"fit in {time.time() - t0:.0f}s")

    m = common.evaluate(space, broad, path, te)
    m["split"] = info
    m["embed_model"] = EMBED_MODEL
    os.makedirs(a.out, exist_ok=True)
    json.dump(m, open(f"{a.out}/metrics.json", "w"), indent=2)
    print(json.dumps({k: v for k, v in m.items() if k not in ("broad_per_class", "split")}, indent=2))


if __name__ == "__main__":
    main()
