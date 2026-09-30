"""Build a reference set: test posts labeled independently by a strong LLM, next to
Jev's and the student's answers, for human review (plan §8.3's reference set).

Sample: posts from the test windows only (the student never trained on them):
  - "random":   uniformly random test posts, for an unbiased accuracy estimate
  - "disagree": random posts where the student's and Jev's broad topics differ
The LLM sees the post and the taxonomy, never Jev's or the student's answer.

    uv run python reference.py --model /data/models/v1 --name v1-ref300

Writes ../reference/<name>.jsonl (committed), and a review page at
/data/reports/<name>/index.html. LLM answers are cached in /data/reference/<name>.llm.jsonl
so a rerun only labels what's missing.
"""

import argparse
import concurrent.futures as cf
import json
import os
import re
import time
import urllib.request

import numpy as np
import torch
import torch.nn.functional as F
import yaml
from transformers import AutoTokenizer

import common
from train import Student, predict

AGW = os.environ.get("TYPESAFE_BASE_URL", "https://agw.noclues.net")

PROMPT = """You are labeling a Bluesky post with a topic taxonomy used to recommend posts to people by the topics they follow.

Pick the single best broad topic and the single best subtopic path for the post. Judge what the post is actually about, as a person following that topic would see it, not surface keywords, hashtags, or link domains alone. The post may include [alt] image descriptions, a [link] card, a [quote] of another post, and [tags]; use them as context.

Rules:
- "path" must be one of the path ids listed below, exactly. For broad topics without subtopics (like "unclear"), the path is just the broad id.
- "broad" must be the part of the path before the slash.
- Use personal_life for personal and everyday life with no other clear topic, and unclear when there's no discernible topic or too little content.

TAXONOMY
{taxonomy}

POST
<<<
{post}
>>>

Reply with only a JSON object: {{"broad": "...", "path": "...", "confidence": 0.0 to 1.0, "reason": "at most 20 words"}}"""


def taxonomy_text(path: str) -> str:
    t = yaml.safe_load(open(path))
    lines = []
    for b in t["broad"]:
        lines.append(f"- {b['id']}: {b['description']}")
        for s in b.get("subtopics") or []:
            lines.append(f"    - {b['id']}/{s['id']}: {s['description']}")
    return "\n".join(lines)


def call_llm(model: str, prompt: str) -> dict:
    key = os.environ["TYPESAFE_API_KEY"]
    body = {"model": model, "max_completion_tokens": 4000, "messages": [{"role": "user", "content": prompt}]}
    req = urllib.request.Request(AGW + "/v1/chat/completions", data=json.dumps(body).encode(),
                                 headers={"x-agw-key": key, "Authorization": "Bearer " + key,
                                          "content-type": "application/json", "X-Client": "topic-feed"})
    with urllib.request.urlopen(req, timeout=300) as r:
        out = json.load(r)
    return {"content": out["choices"][0]["message"]["content"], "usage": out.get("usage", {})}


def label_one(model, tax_text, space, post_text):
    """Ask the LLM, validate the answer, retry up to twice on invalid output or errors."""
    last = None
    for attempt in range(3):
        try:
            r = call_llm(model, PROMPT.format(taxonomy=tax_text, post=post_text))
            m = re.search(r"\{.*\}", r["content"], re.S)
            ans = json.loads(m.group(0)) if m else {}
            path = ans.get("path", "")
            if path in space.paths:
                ans["broad"] = path.split("/", 1)[0]
                ans["usage"] = r["usage"]
                return ans
            last = f"invalid path {path!r}"
        except Exception as e:  # network or parse error: retry
            last = repr(e)[:200]
            time.sleep(2 * (attempt + 1))
    return {"error": last}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--model", required=True, help="student model directory")
    ap.add_argument("--taxonomy", default="../taxonomy/v1.yaml")
    ap.add_argument("--name", default="v1-ref300")
    ap.add_argument("--random", type=int, default=200)
    ap.add_argument("--disagree", type=int, default=100)
    ap.add_argument("--llm", default="gpt-6-luna:api")
    ap.add_argument("--concurrency", type=int, default=8)
    ap.add_argument("--seed", type=int, default=20260928)
    a = ap.parse_args()

    cfg = json.load(open(f"{a.model}/config.json"))
    export = json.load(open(f"{a.model}/metrics.json"))["args"]["export"]
    space = common.Space.from_taxonomy(a.taxonomy)
    _, _, te, _ = common.split(common.load(export, space))

    # Student predictions on the whole test split.
    tok = AutoTokenizer.from_pretrained(a.model)
    student = Student(cfg["base"], len(space.broad), len(space.paths), n_signals=len(cfg["signals"])).cuda()
    student.load_state_dict(torch.load(f"{a.model}/model.pt", map_location="cuda"))
    lb, lp, _, _ = predict(student, te, tok, cfg["max_len"])
    sb = F.softmax(lb / cfg["temperature_broad"], -1).numpy()
    sp = F.softmax(lp / cfg["temperature_path"], -1).numpy()

    # Sample: random first, then disagreements not already chosen.
    rng = np.random.default_rng(a.seed)
    idx = np.arange(len(te))
    rand = rng.choice(idx, a.random, replace=False)
    dis_pool = np.setdiff1d(idx[sb.argmax(1) != te.broad.argmax(1)], rand)
    dis = rng.choice(dis_pool, min(a.disagree, len(dis_pool)), replace=False)
    picked = [(int(i), "random") for i in rand] + [(int(i), "disagree") for i in dis]

    # LLM labels, cached.
    os.makedirs("/data/reference", exist_ok=True)
    cache_path = f"/data/reference/{a.name}.llm.jsonl"
    cache = {}
    if os.path.exists(cache_path):
        for line in open(cache_path):
            r = json.loads(line)
            if "error" not in r["llm"]:
                cache[r["uri"]] = r["llm"]
    todo = [(i, s) for i, s in picked if te.uris[i] not in cache]
    print(f"{len(picked)} posts sampled ({a.random} random, {len(dis)} disagreements); {len(todo)} need LLM labels", flush=True)
    tax_text = taxonomy_text(a.taxonomy)
    with open(cache_path, "a") as cf_out, cf.ThreadPoolExecutor(a.concurrency) as pool:
        futs = {pool.submit(label_one, a.llm, tax_text, space, te.texts[i]): i for i, _ in todo}
        for n, fut in enumerate(cf.as_completed(futs), 1):
            i = futs[fut]
            ans = fut.result()
            cf_out.write(json.dumps({"uri": te.uris[i], "llm": ans}) + "\n")
            cf_out.flush()
            if "error" not in ans:
                cache[te.uris[i]] = ans
            if n % 25 == 0 or n == len(todo):
                print(f"  LLM labeled {n}/{len(todo)}", flush=True)

    # Assemble the reference rows.
    rows = []
    for i, stratum in picked:
        u = te.uris[i]
        llm = cache.get(u)
        if not llm:
            continue
        rows.append({
            "uri": u, "stratum": stratum, "text": te.texts[i], "window": te.windows[i],
            "jev": {"broad": space.broad[int(te.broad[i].argmax())], "broad_p": round(float(te.broad[i].max()), 3),
                    "path": space.paths[int(te.path[i].argmax())], "path_p": round(float(te.path[i].max()), 3),
                    "top3": [[space.broad[j], round(float(te.broad[i][j]), 3)] for j in te.broad[i].argsort()[::-1][:3]]},
            "student": {"broad": space.broad[int(sb[i].argmax())], "broad_p": round(float(sb[i].max()), 3),
                        "path": space.paths[int(sp[i].argmax())], "path_p": round(float(sp[i].max()), 3),
                        "top3": [[space.broad[j], round(float(sb[i][j]), 3)] for j in sb[i].argsort()[::-1][:3]]},
            "llm": {"model": a.llm, "broad": llm["broad"], "path": llm["path"],
                    "confidence": llm.get("confidence"), "reason": llm.get("reason", "")},
        })

    os.makedirs("../reference", exist_ok=True)
    with open(f"../reference/{a.name}.jsonl", "w") as f:
        for r in rows:
            f.write(json.dumps(r, ensure_ascii=False) + "\n")

    stats = summarize(rows)
    json.dump(stats, open(f"/data/reference/{a.name}.stats.json", "w"), indent=2)
    print(json.dumps(stats, indent=2))

    import reference_viewer
    page_dir = f"/data/reports/{a.name}"
    os.makedirs(page_dir, exist_ok=True)
    reference_viewer.write(f"{page_dir}/index.html", a.name, rows, stats)
    print(f"wrote ../reference/{a.name}.jsonl ({len(rows)} rows) and {page_dir}/index.html")


def summarize(rows):
    def rate(sub, f):
        return round(sum(f(r) for r in sub) / len(sub), 3) if sub else None

    out = {}
    for stratum in ("random", "disagree"):
        sub = [r for r in rows if r["stratum"] == stratum]
        out[stratum] = {
            "n": len(sub),
            "broad: jev == student": rate(sub, lambda r: r["jev"]["broad"] == r["student"]["broad"]),
            "broad: llm == jev": rate(sub, lambda r: r["llm"]["broad"] == r["jev"]["broad"]),
            "broad: llm == student": rate(sub, lambda r: r["llm"]["broad"] == r["student"]["broad"]),
            "path: llm == jev": rate(sub, lambda r: r["llm"]["path"] == r["jev"]["path"]),
            "path: llm == student": rate(sub, lambda r: r["llm"]["path"] == r["student"]["path"]),
            "broad: llm matches neither": rate(sub, lambda r: r["llm"]["broad"] not in (r["jev"]["broad"], r["student"]["broad"])),
        }
    return out


if __name__ == "__main__":
    main()
