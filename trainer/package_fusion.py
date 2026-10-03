"""Builds the folder that is uploaded to Hugging Face for the fusion model (no user posts go in it).

Reads a training output folder (model.pt, config.json, metrics.json) and writes, next to a copy of topic_classifier.py:

  model.safetensors     all trained weights (the fine-tuned text encoder, the picture projection, the heads), float32
  config.json           labels, signal and tone names, temperatures, limits, which text and image encoders were used
  text_encoder/         the text encoder's architecture config (so loading needs no extra download of its weights)
  tokenizer/            the text encoder's tokenizer files
  topics.json           topic ids, names and descriptions from the taxonomy (the taxonomy's example posts are left out)
  metrics.json          the training run's validation history and held-out test scores, without file paths
  NOTICE.md             the licences and sources of the parts that came from elsewhere

    python package_fusion.py --model /data/models/fusion1 --taxonomy ../taxonomy/v2.1.yaml --out /data/hf/microblog-topic-classifier-v5
"""

import argparse
import json
import shutil
from pathlib import Path

import torch
import yaml
from safetensors.torch import save_file

HERE = Path(__file__).resolve().parent


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--model", required=True, help="folder with model.pt, config.json, metrics.json")
    ap.add_argument("--taxonomy", default=str(HERE.parent / "taxonomy" / "v2.1.yaml"))
    ap.add_argument("--module", default=str(HERE / "topic_classifier.py"), help="topic_classifier.py to copy into --out")
    ap.add_argument("--out", required=True)
    a = ap.parse_args()
    out, src = Path(a.out), Path(a.model)
    out.mkdir(parents=True, exist_ok=True)

    cfg = json.load(open(src / "config.json"))
    state = torch.load(src / "model.pt", map_location="cpu", weights_only=True)
    state = {k: v.contiguous().float() for k, v in state.items()}
    save_file(state, out / "model.safetensors", metadata={"format": "pt"})
    print(f"model.safetensors: {len(state)} tensors, {sum(v.numel() for v in state.values()) / 1e6:.1f} M parameters")

    from transformers import AutoConfig, AutoTokenizer

    AutoConfig.from_pretrained(cfg["text_model"]).save_pretrained(out / "text_encoder")
    AutoTokenizer.from_pretrained(cfg["text_model"]).save_pretrained(out / "tokenizer")

    cfg["postdoc_version"] = "pd2"  # the post document format (internal/postdoc) the Go pipeline renders for this model
    json.dump(cfg, open(out / "config.json", "w"), indent=2)

    tax = yaml.safe_load(open(a.taxonomy))
    assert tax["version"] == cfg["taxonomy_version"], (tax["version"], cfg["taxonomy_version"])
    topics = []
    for b in tax["broad"]:
        topics.append({"id": b["id"], "name": b["name"], "description": b["description"],
                       "subtopics": [{"id": f"{b['id']}/{s['id']}", "name": s["name"], "description": s.get("description", "")}
                                     for s in b.get("subtopics") or []]})
    json.dump({"taxonomy_version": tax["version"], "broad": topics}, open(out / "topics.json", "w"), indent=1, ensure_ascii=False)
    n_sub = sum(len(t["subtopics"]) for t in topics)
    print(f"topics.json: {len(topics)} broad topics, {n_sub} subtopics")

    m = json.load(open(src / "metrics.json"))
    m["args"] = {k: v for k, v in m["args"].items() if k not in ("export", "feats", "out", "taxonomy", "tb")}
    json.dump(m, open(out / "metrics.json", "w"), indent=1)

    shutil.copy(a.module, out / "topic_classifier.py")
    print("done:", sorted(p.name for p in out.iterdir()))


if __name__ == "__main__":
    main()
