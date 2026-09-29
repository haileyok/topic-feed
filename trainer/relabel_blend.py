"""Blend a relabel with Jev's original labels, as rows for jev_labels.

For each relabeled post the path distribution becomes an even mix of Jev's and the
LLM's (by default): broad probabilities are averaged, and each broad topic's subtopic
distribution is the mix of the two models' subtopic distributions weighted by how much
broad-topic probability each gave it, so the joint P(broad) * P(sub | broad) is the
average of the two joints. Ranking signals stay Jev's. Rows get their own label_config
and a newer labeled_at, so an export accepting Jev's config and this one takes the blend.

    uv run python relabel_blend.py --name v1-luna-le05
    make relabel-load RELABEL=v1-luna-le05-blend
    make export LABEL_CONFIG=5697660f73fc,<printed config> EXPORT=/data/exports/v1-blend
"""

import argparse
import datetime as dt
import gzip
import hashlib
import json
import math


def norm(d: dict) -> dict:
    s = sum(d.values())
    return {k: v / s for k, v in d.items() if v > 0} if s > 0 else {}


def conditionals(sub: dict) -> dict:
    """{"broad/sub": p} -> {broad: {"broad/sub": p}} normalized per broad."""
    out: dict = {}
    for k, v in (sub or {}).items():
        if v > 0:
            out.setdefault(k.split("/", 1)[0], {})[k] = v
    return {b: norm(d) for b, d in out.items()}


def blend(jev: dict, llm: dict, w_llm: float) -> tuple[dict, dict]:
    jb, lb = norm(jev["broad_probs"]), norm(llm["broad_probs"])
    jc, lc = conditionals(jev["sub_probs"]), conditionals(llm["sub_probs"])
    broad = {b: (1 - w_llm) * jb.get(b, 0) + w_llm * lb.get(b, 0) for b in set(jb) | set(lb)}
    sub = {}
    for b in broad:
        # A model that didn't ask subtopics for this broad topic contributes no opinion.
        wj = (1 - w_llm) * jb.get(b, 0) if b in jc else 0
        wl = w_llm * lb.get(b, 0) if b in lc else 0
        if wj + wl <= 0:
            continue
        for k in set(jc.get(b, {})) | set(lc.get(b, {})):
            sub[k] = (wj * jc.get(b, {}).get(k, 0) + wl * lc.get(b, {}).get(k, 0)) / (wj + wl)
    return {k: v for k, v in broad.items() if v > 0}, {k: v for k, v in sub.items() if v > 0}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--name", required=True, help="relabel run name (reads /data/relabel/<name>.rows.jsonl)")
    ap.add_argument("--export", default="/data/exports/v1-final", help="export holding Jev's original labels")
    ap.add_argument("--llm-weight", type=float, default=0.5)
    a = ap.parse_args()

    llm = {}
    for line in open(f"/data/relabel/{a.name}.rows.jsonl"):
        r = json.loads(line)
        llm[r["uri"]] = r
    configs = {r["label_config"] for r in llm.values()}
    jev, jev_configs = {}, set()
    with gzip.open(f"{a.export}/labels.jsonl.gz", "rt") as f:
        for line in f:
            r = json.loads(line)
            if r["uri"] in llm:
                jev[r["uri"]] = r
                jev_configs.add(r["label_config"])
    s = "|".join(["blend", ",".join(sorted(jev_configs)), ",".join(sorted(configs)), f"w{a.llm_weight:g}"])
    config = hashlib.sha256(s.encode()).hexdigest()[:12]
    model = next(iter(llm.values()))["jev_model"]

    now = dt.datetime.now(dt.timezone.utc).strftime("%Y-%m-%d %H:%M:%S.%f")[:-3]
    out_path = f"/data/relabel/{a.name}-blend.rows.jsonl"
    n = 0
    with open(out_path, "w") as f:
        for uri, lr in sorted(llm.items()):
            jr = jev.get(uri)
            if not jr:
                continue
            broad, sub = blend(jr, lr, a.llm_weight)
            f.write(json.dumps({
                "uri": uri, "taxonomy_version": lr["taxonomy_version"], "label_config": config,
                "jev_model": f"blend:{model}", "source": "uncertain", "batch_size": 1, "labeled_at": now,
                "broad_probs": broad, "broad_confidence": max(broad.values()), "sub_probs": sub,
                "path_scores": {k: math.sqrt(broad.get(k.split("/", 1)[0], 0) * v) for k, v in sub.items()},
                "signals": jr["signals"] or {}, "request_ids": lr["request_ids"],
            }) + "\n")
            n += 1
    print(f"blended {n} posts (LLM weight {a.llm_weight}) from Jev {sorted(jev_configs)} and {sorted(configs)}")
    print(f"label_config {config}; wrote {out_path}")


if __name__ == "__main__":
    main()
