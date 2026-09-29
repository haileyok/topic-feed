"""Relabel posts Jev was unsure about with an LLM, in Jev's label format.

Picks posts from an export whose Jev broad-topic confidence is at or below a cutoff,
asks the LLM for a probability distribution over its top broad topics and their
subtopics, and writes rows for the jev_labels table under their own label_config, so
Jev's labels stay untouched and an export can accept either:

    make export LABEL_CONFIG=5697660f73fc               # Jev only
    make export LABEL_CONFIG=5697660f73fc,<this config>  # this run wins where it relabeled

Ranking signals (substance, news, promo, general interest, tone) are not topic calls,
so each row keeps Jev's. Rows use source 'uncertain' (plan §10.1).

LLM answers are appended to /data/relabel/<name>.llm.jsonl as they arrive, so a rerun
resumes. The rows for ClickHouse go to /data/relabel/<name>.rows.jsonl.

    uv run python relabel.py --export /data/exports/v1-final --max-confidence 0.5 --name v1-luna-le05
"""

import argparse
import concurrent.futures as cf
import datetime as dt
import gzip
import hashlib
import json
import math
import os
import random
import re
import threading
import time
import urllib.error
import urllib.request

import yaml

import common

AGW = os.environ.get("TYPESAFE_BASE_URL", "https://agw.noclues.net")
PROMPT_VERSION = "dist2"  # dist2: instructions and taxonomy in a system message (cacheable)

# The fixed part (a system message, so the gateway can cache it); the post follows in a
# user message.
PROMPT_HEAD = """You are labeling a Bluesky post with a topic taxonomy used to recommend posts to people by the topics they follow.

Judge what the post is actually about, as a person following that topic would see it, not surface keywords, hashtags, or link domains alone. The post may include [alt] image descriptions, a [link] card, a [quote] of another post, and [tags]; use them as context.

Give a probability distribution over the broad topics the post plausibly belongs to (at most 3, most likely first), and for each of them a distribution over its subtopics.

Rules:
- "broad" must be one of the broad ids below, exactly.
- Subtopic keys must be full path ids listed below under that broad topic, exactly (e.g. "sports/basketball"). Broad topics without subtopics (like "unclear") get an empty subtopics object.
- Broad probabilities sum to at most 1. Subtopic probabilities within a broad topic sum to 1.
- Use personal_life for personal and everyday life with no other clear topic, and unclear when there's no discernible topic or too little content.
- Be calibrated: if a post clearly has one topic, give it most of the probability; if it is genuinely ambiguous, spread it.

Reply with only a JSON object:
{{"topics": [{{"broad": "...", "p": 0.0, "subtopics": {{"<broad>/<sub>": 0.0}}}}], "reason": "at most 20 words"}}

TAXONOMY
{taxonomy}"""
PROMPT_TAIL = "\n>>>"


def taxonomy_text(path: str) -> str:
    t = yaml.safe_load(open(path))
    lines = []
    for b in t["broad"]:
        lines.append(f"- {b['id']}: {b['description']}")
        for s in b.get("subtopics") or []:
            lines.append(f"    - {b['id']}/{s['id']}: {s['description']}")
    return "\n".join(lines)


def label_config(space: common.Space, tax_hash: str, model: str) -> str:
    s = "|".join([space.version, tax_hash, "llm-relabel", PROMPT_VERSION, "pd1", model])
    return hashlib.sha256(s.encode()).hexdigest()[:12]


def call_llm(model: str, head: str, post: str, cache_key: str) -> dict:
    """The fixed instructions and taxonomy go in their own message: the gateway caches up
    to the end of a message, so a single message holding the post never hits the cache."""
    key = os.environ["TYPESAFE_API_KEY"]
    body = {"model": model, "max_completion_tokens": 4000, "prompt_cache_key": cache_key,
            "messages": [{"role": "system", "content": head},
                         {"role": "user", "content": "POST\n<<<\n" + post + PROMPT_TAIL}]}
    req = urllib.request.Request(AGW + "/v1/chat/completions", data=json.dumps(body).encode(),
                                 headers={"x-agw-key": key, "Authorization": "Bearer " + key,
                                          "content-type": "application/json", "X-Client": "topic-feed"})
    with urllib.request.urlopen(req, timeout=300) as r:
        out = json.load(r)
    return {"content": out["choices"][0]["message"]["content"], "usage": out.get("usage", {}), "id": out.get("id", "")}


def parse(content: str, space: common.Space, has_subs: set[str]) -> dict:
    """Validate the LLM's answer and turn it into Jev-format maps. Raises ValueError."""
    m = re.search(r"\{.*\}", content, re.S)
    if not m:
        raise ValueError("no JSON object")
    ans = json.loads(m.group(0))
    topics = ans.get("topics") or []
    if not topics:
        raise ValueError("no topics")
    broad, sub, filled = {}, {}, 0
    for t in topics[:3]:
        b, p = t.get("broad"), float(t.get("p") or 0)
        if b not in space.broad:
            raise ValueError(f"unknown broad {b!r}")
        if p <= 0:
            continue
        broad[b] = broad.get(b, 0) + p
        if b in has_subs:
            subs = {k: float(v) for k, v in (t.get("subtopics") or {}).items() if float(v) > 0}
            for k in subs:
                if not k.startswith(b + "/") or k not in space.paths:
                    raise ValueError(f"unknown subtopic {k!r} for {b}")
            if not subs:  # a broad topic with subtopics but none given: its "other"
                subs, filled = {f"{b}/other": 1.0}, filled + 1
            total = sum(subs.values())
            for k, v in subs.items():
                sub[k] = sub.get(k, 0) + v / total
    s = sum(broad.values())
    if s <= 0:
        raise ValueError("zero probability")
    if s > 1:
        broad = {k: v / s for k, v in broad.items()}
    return {"broad": broad, "sub": sub, "reason": ans.get("reason", ""), "filled_other": filled}


def label_one(model: str, head: str, text: str, space, has_subs, cache_key: str) -> dict:
    """Ask the LLM, validate, retry on invalid output, rate limits and errors."""
    last = None
    for attempt in range(6):
        try:
            r = call_llm(model, head, text, cache_key)
            ans = parse(r["content"], space, has_subs)
            return {**ans, "usage": r["usage"], "id": r["id"]}
        except urllib.error.HTTPError as e:
            last = f"HTTP {e.code}"
            wait = float(e.headers.get("Retry-After") or 0) if e.code == 429 else 0
            time.sleep(max(wait, min(60, 2 ** attempt)) + random.random())
        except Exception as e:  # network error or invalid answer
            last = repr(e)[:200]
            time.sleep(min(30, 2 ** attempt) + random.random())
    return {"error": last}


# List prices per 1M tokens for gpt-6-luna (OpenAI API docs, 2026-09): input, cached
# input, cache writes, output. The gateway's own billing may differ.
PRICE = {"input": 0.10, "cached": 0.01, "cache_write": 0.125, "output": 0.50}


def cost(u: dict) -> float:
    pt = u.get("prompt_tokens", 0)
    d = u.get("prompt_tokens_details") or {}
    cached, written = d.get("cached_tokens", 0), d.get("cache_write_tokens", 0)
    plain = max(0, pt - cached - written)
    return (plain * PRICE["input"] + cached * PRICE["cached"] + written * PRICE["cache_write"]
            + u.get("completion_tokens", 0) * PRICE["output"]) / 1e6


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--export", required=True, help="export to pick posts from (its model_input is what the LLM sees)")
    ap.add_argument("--taxonomy", default="../taxonomy/v1.yaml")
    ap.add_argument("--max-confidence", type=float, default=0.5, help="relabel posts with Jev broad confidence <= this")
    ap.add_argument("--llm", default="gpt-6-luna:api")
    ap.add_argument("--name", required=True)
    ap.add_argument("--limit", type=int, default=0, help="only the first N selected posts (smoke test)")
    ap.add_argument("--concurrency", type=int, default=32)
    a = ap.parse_args()

    space = common.Space.from_taxonomy(a.taxonomy)
    has_subs = {p.split("/", 1)[0] for p in space.paths if "/" in p}
    manifest = json.load(open(f"{a.export}/manifest.json"))
    config = label_config(space, manifest["taxonomy_hash"], a.llm)
    head = PROMPT_HEAD.format(taxonomy=taxonomy_text(a.taxonomy))

    rows = []
    with gzip.open(f"{a.export}/labels.jsonl.gz", "rt") as f:
        for line in f:
            r = json.loads(line)
            if r["broad_confidence"] <= a.max_confidence:
                rows.append({"uri": r["uri"], "text": r["model_input"], "signals": r["signals"] or {},
                             "jev_broad_confidence": r["broad_confidence"]})
    rows.sort(key=lambda r: r["uri"])
    if a.limit:
        rows = random.Random(0).sample(rows, min(a.limit, len(rows)))
    print(f"{len(rows)} posts with Jev confidence <= {a.max_confidence}; label_config {config} ({a.llm}, prompt {PROMPT_VERSION})", flush=True)

    os.makedirs("/data/relabel", exist_ok=True)
    cache_path = f"/data/relabel/{a.name}.llm.jsonl"
    done = {}
    if os.path.exists(cache_path):
        for line in open(cache_path):
            x = json.loads(line)
            if "error" not in x["llm"]:
                done[x["uri"]] = x["llm"]
    todo = [r for r in rows if r["uri"] not in done]
    print(f"{len(done)} already answered, {len(todo)} to go", flush=True)

    lock, spent, errors, t0 = threading.Lock(), [sum(cost(v.get("usage", {})) for v in done.values())], [0], time.time()
    n_done = [0]
    with open(cache_path, "a") as out, cf.ThreadPoolExecutor(a.concurrency) as pool:
        futs = {pool.submit(label_one, a.llm, head, r["text"], space, has_subs, f"topic-feed-{config}"): r for r in todo}
        for fut in cf.as_completed(futs):
            r, ans = futs[fut], fut.result()
            with lock:
                out.write(json.dumps({"uri": r["uri"], "llm": ans}) + "\n")
                out.flush()
                n_done[0] += 1
                if "error" in ans:
                    errors[0] += 1
                else:
                    done[r["uri"]] = ans
                    spent[0] += cost(ans.get("usage", {}))
                k = n_done[0]
                if k % 500 == 0 or k == len(todo):
                    el = time.time() - t0
                    rate = k / max(el, 1e-9) * 60
                    eta = (len(todo) - k) / max(rate, 1e-9)
                    fin = (dt.datetime.now(dt.timezone.utc) + dt.timedelta(minutes=eta)).strftime("%H:%M")
                    print(f"  {k}/{len(todo)} · {rate:.0f}/min · failed {errors[0]} · ${spent[0]:.2f} so far"
                          f" · ETA {eta:.0f} min (~{fin} UTC)", flush=True)

    # Rows for jev_labels.
    now = dt.datetime.now(dt.timezone.utc).strftime("%Y-%m-%d %H:%M:%S.%f")[:-3]
    rows_path = f"/data/relabel/{a.name}.rows.jsonl"
    n, cached, prompt, filled = 0, 0, 0, 0
    with open(rows_path, "w") as f:
        for r in rows:
            ans = done.get(r["uri"])
            if not ans:
                continue
            path_scores = {k: math.sqrt(ans["broad"].get(k.split("/", 1)[0], 0) * v) for k, v in ans["sub"].items()}
            f.write(json.dumps({
                "uri": r["uri"], "taxonomy_version": space.version, "label_config": config, "jev_model": a.llm,
                "source": "uncertain", "batch_size": 1, "labeled_at": now,
                "broad_probs": ans["broad"], "broad_confidence": max(ans["broad"].values()),
                "sub_probs": ans["sub"], "path_scores": path_scores, "signals": r["signals"],
                "request_ids": [ans.get("id", "")],
            }) + "\n")
            n += 1
            u = ans.get("usage", {})
            prompt += u.get("prompt_tokens", 0)
            cached += (u.get("prompt_tokens_details") or {}).get("cached_tokens", 0)
            filled += ans.get("filled_other", 0)
    summary = {"name": a.name, "label_config": config, "llm": a.llm, "prompt_version": PROMPT_VERSION,
               "export": a.export, "max_confidence": a.max_confidence, "selected": len(rows), "labeled": n,
               "missing": len(rows) - n, "cost_usd_list_price": round(spent[0], 2),
               "cached_prompt_share": round(cached / max(prompt, 1), 3), "subtopics_filled_with_other": filled}
    json.dump(summary, open(f"/data/relabel/{a.name}.summary.json", "w"), indent=2)
    print(json.dumps(summary, indent=2))
    print(f"wrote {rows_path}")


if __name__ == "__main__":
    main()
