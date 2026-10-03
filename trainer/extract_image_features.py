"""Caches a frozen image encoder's embedding for every picture the training export points to.

Reads the export's rows (build_mm_dataset.py's labels.jsonl.gz), takes the first --max-images pictures of every post (the
ones train_mm.py shows), runs each distinct picture once through the image encoder of a SigLIP 2 checkpoint, and saves

  <out>.npy    float16 array, one row per distinct picture (the encoder's pooled embedding, before any normalising)
  <out>.json   the model name, the embedding size, the picture hashes in row order, and any pictures that could not be read

so the fusion model (train_fusion.py) can be trained without running the image encoder again.

    HF_HUB_OFFLINE=0 python extract_image_features.py --export /root/mm/v21 --images /root/images \\
        --out /root/mm/feats/siglip2-so400m-512
"""

import argparse
import gzip
import json
import time
from pathlib import Path

import numpy as np
import torch
from PIL import Image


class Batches(torch.utils.data.Dataset):
    """Each item is one ready batch of pictures (so DataLoader workers decode and resize them)."""

    def __init__(self, shas, root, processor, bs):
        self.shas, self.root, self.processor, self.bs = shas, root, processor, bs

    def __len__(self):
        return (len(self.shas) + self.bs - 1) // self.bs

    def __getitem__(self, i):
        chunk = self.shas[i * self.bs:(i + 1) * self.bs]
        ims, ok = [], []
        for sha in chunk:
            try:
                with Image.open(Path(self.root) / "1000" / sha[:2] / f"{sha}.jpg") as im:
                    ims.append(im.convert("RGB"))
                ok.append(True)
            except OSError:
                ims.append(Image.new("RGB", (512, 512)))
                ok.append(False)
        return self.processor(images=ims, return_tensors="pt")["pixel_values"], torch.tensor(ok)


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--export", required=True)
    ap.add_argument("--images", default="/root/images")
    ap.add_argument("--out", required=True, help="path prefix; writes <out>.npy and <out>.json")
    ap.add_argument("--model", default="google/siglip2-so400m-patch16-512")
    ap.add_argument("--max-images", type=int, default=2)
    ap.add_argument("--bs", type=int, default=64)
    ap.add_argument("--workers", type=int, default=16)
    ap.add_argument("--limit", type=int, default=0, help="only the first N distinct pictures, for a quick check")
    a = ap.parse_args()

    from transformers import AutoModel, AutoProcessor

    seen = {}
    with gzip.open(f"{a.export}/labels.jsonl.gz", "rt") as f:
        for line in f:
            for sha in json.loads(line)["image_shas"][: a.max_images]:
                seen.setdefault(sha, len(seen))
    shas = list(seen)
    if a.limit:
        shas = shas[: a.limit]
    print(f"{len(shas)} distinct pictures", flush=True)

    dev = torch.device("cuda")
    processor = AutoProcessor.from_pretrained(a.model)
    model = AutoModel.from_pretrained(a.model, dtype=torch.bfloat16).to(dev).eval()
    loader = torch.utils.data.DataLoader(Batches(shas, a.images, processor, a.bs), batch_size=None, shuffle=False,
                                         num_workers=a.workers, prefetch_factor=4)
    chunks, missing, t0 = [], [], time.time()
    with torch.no_grad():
        for i, (pv, ok) in enumerate(loader):
            out = model.get_image_features(pixel_values=pv.to(dev, torch.bfloat16))
            emb = out if torch.is_tensor(out) else out.pooler_output
            chunks.append(emb.float().cpu().numpy().astype(np.float16))
            missing += [shas[i * a.bs + j] for j in range(len(ok)) if not ok[j]]
            if i % 100 == 0:
                print(f"  {min((i + 1) * a.bs, len(shas))} / {len(shas)} pictures, {min((i + 1) * a.bs, len(shas)) / (time.time() - t0):.0f}/s", flush=True)
    feats = np.concatenate(chunks)
    assert len(feats) == len(shas)
    Path(a.out).parent.mkdir(parents=True, exist_ok=True)
    np.save(f"{a.out}.npy", feats)
    json.dump({"model": a.model, "dim": int(feats.shape[1]), "max_images": a.max_images, "shas": shas, "missing": missing},
              open(f"{a.out}.json", "w"))
    print(f"saved {feats.shape} to {a.out}.npy in {time.time() - t0:.0f}s; {len(missing)} pictures could not be read")


if __name__ == "__main__":
    main()
