"""Label posts with Cloudflare's Clef-flash, locally, the way internal/labeler labels them with Jev.

Same two passes as the Go labeler (plan section 10):
  pass 1  the broad topic plus the ranking signals, one request per post
  pass 2  the subtopic question for each of the top-k broad topics (probability >= min-broad-p)
and the same path score, sqrt(P(broad) * P(sub | broad)), so the answers can be scored with
gold_check.py next to Jev's and the student's. The post is rendered exactly as postdoc.Jev()
renders it, and the questions are ports of internal/labeler/questions.go (QuestionsVersion q3).

This needs the Clef release (weights, joint_schema_model.py) and its own Python environment,
not the trainer's:

    /data/clef/.venv/bin/python clef_labels.py --posts /data/clef/gold_posts.jsonl \\
        --out /data/clef/gold-clef-flash

--posts is JSON lines with the columns of a `posts` row (uri, text, media_alts, link_domain,
link_title, link_description, quote_text, tags, media_kinds) and optionally image_texts,
image_text_sources, labels, as post_pipeline has them, and image_urls (pictures to download
and show the model itself). Writes <out>.jsonl (every probability,
the token count and timings per post) and <out>.paths.json ({uri: top path}, for gold_check.py
--answers). --check-render compares our rendering with the stored text instead of labeling.
"""

import argparse
import json
import math
import sys
import time
from pathlib import Path

import yaml

MAX_LINK_DESCRIPTION, MAX_QUOTE, MAX_IMAGE_TEXT = 300, 500, 300


# --- the post document, a port of internal/postdoc ---------------------------------------


def one_line(s: str) -> str:
    return " ".join(s.split())


def truncate(s: str, n: int) -> str:
    if len(s) <= n:
        return s
    return s[: n - 1].rstrip() + "…"


def lines(xs, n: int = 0) -> list[str]:
    out = []
    for s in xs or []:
        s = one_line(s)
        if s:
            out.append(truncate(s, n) if n else s)
    return out


def media_summary(kinds) -> str:
    order, count = [], {}
    for k in kinds or []:
        k = one_line(k)
        if not k:
            continue
        if k not in count:
            order.append(k)
        count[k] = count.get(k, 0) + 1
    return ", ".join(f"1 {k}" if count[k] == 1 else f"{count[k]} {k}s" for k in order)


def make_doc(row: dict) -> dict:
    """postdoc.New for a posts row (plus, optionally, what the pipeline found in the images)."""
    ocr, desc = [], []
    for t, src in zip(row.get("image_texts") or [], row.get("image_text_sources") or []):
        if src == "ocr":
            ocr.append(t)
        elif src == "luna":
            desc.append(t)
    labels = []
    for l in row.get("labels") or []:
        l = one_line(l)
        if l and not l.startswith("!") and l != "needs-review" and l not in labels:
            labels.append(l)
    link = {
        "domain": one_line(row.get("link_domain") or ""),
        "title": one_line(row.get("link_title") or ""),
        "description": truncate(one_line(row.get("link_description") or ""), MAX_LINK_DESCRIPTION),
    }
    tags, seen = [], set()
    for t in row.get("tags") or []:
        t = one_line(t).lstrip("#")
        if t and t.lower() not in seen:
            seen.add(t.lower())
            tags.append(t)
    return {
        "text": (row.get("text") or "").strip(),
        "alt_text": lines(row.get("media_alts")),
        "image_text": lines(ocr, MAX_IMAGE_TEXT),
        "image_descriptions": lines(desc, MAX_IMAGE_TEXT),
        "media": media_summary(row.get("media_kinds")),
        "labels": labels,
        "link": link if any(link.values()) else None,
        "quote": truncate(one_line(row.get("quote_text") or ""), MAX_QUOTE),
        "tags": tags,
    }


def jev_post(d: dict) -> dict:
    """Doc.Jev: one entry of the state's "posts" array; empty fields are left out."""
    out = {}
    for key, v in (("text", d["text"]), ("alt_text", d["alt_text"]), ("attachments", d["media"]),
                   ("text_in_images", d["image_text"]), ("image_descriptions", d["image_descriptions"]),
                   ("labels", d["labels"]), ("link", {k: x for k, x in (d["link"] or {}).items() if x} or None),
                   ("quote", d["quote"]), ("tags", d["tags"])):
        if v:
            out[key] = v
    return out


def student_string(d: dict) -> str:
    """Doc.Student, to check our rendering against the stored model input."""
    out = []
    if d["text"]:
        out.append(d["text"])
    if d["tags"]:
        out.append("[tags] " + " ".join("#" + t for t in d["tags"]))
    if d["media"]:
        out.append("[media] " + d["media"])
    if d["labels"]:
        out.append("[labels] " + ", ".join(d["labels"]))
    if d["alt_text"]:
        out.append("[alt] " + " | ".join(d["alt_text"]))
    if d["image_text"]:
        out.append("[image text] " + " | ".join(d["image_text"]))
    if d["image_descriptions"]:
        out.append("[image description] " + " | ".join(d["image_descriptions"]))
    if d["link"]:
        out.append("[link] " + " | ".join(p for p in (d["link"]["domain"], d["link"]["title"], d["link"]["description"]) if p))
    if d["quote"]:
        out.append("[quote] " + d["quote"])
    return "\n".join(out)


# --- the questions, a port of internal/labeler/questions.go (q3) ------------------------

SENTIMENT = ["Very negative", "Somewhat negative", "Neutral or mixed", "Somewhat positive", "Very positive"]
SUBSTANCE = [
    "Low effort: a few words, a reaction, or filler",
    "Some substance: a clear thought, opinion, or share",
    "Substantive: informative, insightful, creative, or detailed",
]
TONE = {
    "informative": "Mainly conveys information, news, facts, or explanation.",
    "humorous": "Mainly a joke, meme, wordplay, or playful.",
    "personal": "Mainly about the author's own life, feelings, or experiences.",
    "outraged": "Mainly angry, indignant, or outraged.",
    "supportive": "Mainly encouraging, grateful, celebratory, or kind.",
    "other": "None of the above describes the main tone.",
}
YES_NO = ["news", "promo", "general_interest", "critical", "ad", "engagement_bait", "spam", "self_promo", "meme"]


def choice(instructions: str, options: dict) -> dict:
    return {"type": "choice", "instructions": instructions, "criteria": {k: (v or None) for k, v in options.items()}}


def score(instructions: str, levels: list[str]) -> dict:
    return {"type": "score", "instructions": instructions, "criteria": levels}


def noul(instructions: str) -> dict:
    return {"type": "noul", "instructions": instructions}


def pass1_questions(tax: dict, i: int = 0) -> dict:
    p = f"`posts[{i}]`"
    q = {}
    q[f"p{i}_broad"] = choice(
        f"Which broad topic best describes the post {p} (its text, alt text, attachments, text in images, "
        "image descriptions, labels, link, quoted post, and tags)?",
        {b["id"]: b.get("description", "") for b in tax["broad"]})
    q[f"p{i}_substance"] = score(f"How substantive is the post {p}?", SUBSTANCE)
    q[f"p{i}_news"] = noul(f"Is the post {p} about a current event or breaking news?")
    q[f"p{i}_promo"] = noul(f"Is the post {p} mainly self-promotion, an advertisement, or a request for follows, likes, or reposts?")
    q[f"p{i}_general_interest"] = noul(f"Would someone who doesn't know the author find the post {p} interesting?")
    q[f"p{i}_tone"] = choice(f"What is the main tone of the post {p}?", TONE)
    q[f"p{i}_sentiment"] = score(f"What is the overall sentiment of the post {p}?", SENTIMENT)
    q[f"p{i}_critical"] = noul(f"Is the post {p} negative about, critical of, or mocking the main thing it is about "
                               "(for example a post about AI that says AI is bad)?")
    q[f"p{i}_ad"] = noul(f"Is the post {p} an advertisement, sales pitch, or deal for a product or service?")
    q[f"p{i}_engagement_bait"] = noul(f"Does the post {p} mainly ask for follows, likes, reposts, or replies "
                                      "(follow trains, \"like if you agree\", repost-to-win giveaways)?")
    q[f"p{i}_spam"] = noul(f"Is the post {p} spam: a scam, a crypto or money scheme, repetitive or automated junk, "
                           "a link farm, or piles of unrelated hashtags?")
    q[f"p{i}_self_promo"] = noul(f"Is the post {p} the author sharing or promoting their own work "
                                 "(their art, writing, music, stream, shop, or research)?")
    # Added after q3 and not in internal/labeler/questions.go yet: a format, not a topic, so a post
    # of any topic can be one. Keep the wording identical there.
    q[f"p{i}_meme"] = noul(f"Is the post {p} mainly a meme: a captioned, edited, or AI-generated joke image, a reaction "
                           "image, or a recycled joke format (\"me when...\", copy-pasted text) made to be reshared? "
                           "Original photos, artwork, editorial cartoons, and news are not memes.")
    return q


def pass2_question(b: dict, i: int = 0) -> dict:
    return choice(f"Which kind of {b['name']} content is the post `posts[{i}]` about?",
                  {s["id"]: s.get("description", "") for s in b["subtopics"]})


# --- labeling ----------------------------------------------------------------------------


def ranked(probs: dict) -> list[tuple[str, float]]:
    return sorted(probs.items(), key=lambda kv: (-kv[1], kv[0]))


def fetch_images(urls: list[str], limit: int = 4):
    """Download a post's pictures (Clef takes at most 4) as RGB PIL images."""
    import io
    import urllib.request

    from PIL import Image

    out = []
    for u in urls[:limit]:
        req = urllib.request.Request(u, headers={"User-Agent": "topic-feed-clef-eval"})
        with urllib.request.urlopen(req, timeout=30) as r:
            out.append(Image.open(io.BytesIO(r.read())).convert("RGB"))
    return out


def label_post(model, processor, systemone, tax: dict, state: dict, top_k: int, min_broad_p: float, images=None) -> dict:
    by_id = {b["id"]: b for b in tax["broad"]}
    media = {"images": images} if images else {}
    t0 = time.time()
    r1 = systemone(model, processor, {"model": "clef-flash", "state": state, "questions": pass1_questions(tax), **media})
    broad = r1["answers"]["p0_broad"]["probabilities"]
    sig = {}
    for name in ("substance", "sentiment"):
        a = r1["answers"][f"p0_{name}"]
        sig[name] = a["score"] / (len(a["legend"]) - 1)
    for name in YES_NO:
        sig[name] = r1["answers"][f"p0_{name}"]["noul"]
    # The tone question was always asked; its answer was not kept until now. Same keys as Jev's
    # signals in jev_labels ("tone.humorous", ...).
    for option, prob in r1["answers"]["p0_tone"]["probabilities"].items():
        sig[f"tone.{option}"] = prob
    tokens, t1 = r1["usage"]["input_tokens"], time.time()

    pairs = []
    for rank, (b, p) in enumerate(ranked(broad)):
        if rank >= top_k or p < min_broad_p:
            break
        if by_id.get(b, {}).get("subtopics"):
            pairs.append(by_id[b])
    sub, path_scores = {}, {}
    if pairs:
        q2 = {f"p0_{b['id']}_sub": pass2_question(b) for b in pairs}
        r2 = systemone(model, processor, {"model": "clef-flash", "state": state, "questions": q2, **media})
        tokens += r2["usage"]["input_tokens"]
        for b in pairs:
            for s, ps in r2["answers"][f"p0_{b['id']}_sub"]["probabilities"].items():
                sub[f"{b['id']}/{s}"] = ps
                path_scores[f"{b['id']}/{s}"] = math.sqrt(broad[b["id"]] * ps)
    t2 = time.time()

    # The top path, as gold_check.py reads Jev's labels: path scores, plus broad topics without
    # subtopics scored by sqrt(P(broad)).
    cand = dict(path_scores)
    for b in tax["broad"]:
        if not b.get("subtopics"):
            cand[b["id"]] = math.sqrt(broad.get(b["id"], 0.0))
    top = max(cand.items(), key=lambda kv: kv[1])[0] if cand else None
    return {"broad_probs": broad, "sub_probs": sub, "path_scores": path_scores, "signals": sig, "top_path": top,
            "input_tokens": tokens, "pass1_s": round(t1 - t0, 3), "pass2_s": round(t2 - t1, 3)}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--posts", required=True)
    ap.add_argument("--out", help="output prefix (<out>.jsonl and <out>.paths.json)")
    ap.add_argument("--model", default="/data/clef/clef-flash", help="the Clef release directory")
    ap.add_argument("--taxonomy", default=str(Path(__file__).resolve().parent.parent / "taxonomy" / "v1.yaml"))
    ap.add_argument("--stored", help="gold labels JSON, with --check-render: compare renderings with its stored text")
    ap.add_argument("--check-render", action="store_true")
    ap.add_argument("--top-k", type=int, default=3)
    ap.add_argument("--min-broad-p", type=float, default=0.05)
    ap.add_argument("--limit", type=int, default=0)
    ap.add_argument("--gpu-fraction", type=float, default=0.86, help="cap on this process's share of GPU memory")
    a = ap.parse_args()

    tax = yaml.safe_load(open(a.taxonomy))
    rows = [json.loads(l) for l in open(a.posts) if l.strip()]
    if a.limit:
        rows = rows[: a.limit]

    if a.check_render:
        stored = {l["uri"]: l["text"] for l in json.load(open(a.stored))["labels"]}
        bad = 0
        for r in rows:
            mine = student_string(make_doc(r))
            if mine != stored.get(r["uri"]):
                bad += 1
                print("MISMATCH", r["uri"], "\n  mine:  ", repr(mine[:200]), "\n  stored:", repr((stored.get(r["uri"]) or "")[:200]))
        print(f"{len(rows) - bad}/{len(rows)} posts render exactly as stored")
        sys.exit(1 if bad else 0)

    import torch

    torch.cuda.set_per_process_memory_fraction(a.gpu_fraction)
    sys.path.insert(0, a.model)
    from joint_schema_model import load_release_model, systemone

    t0 = time.time()
    model, processor = load_release_model(a.model, device="cuda")
    print(f"loaded in {time.time() - t0:.0f}s; GPU memory in use {torch.cuda.memory_allocated() / 2**30:.1f} GiB", flush=True)

    out_jsonl = open(f"{a.out}.jsonl", "w")
    paths = {}
    for n, r in enumerate(rows, 1):
        state = {"posts": [jev_post(make_doc(r))]}
        images = fetch_images(r["image_urls"]) if r.get("image_urls") else None
        res = label_post(model, processor, systemone, tax, state, a.top_k, a.min_broad_p, images)
        res["images"] = len(images or [])
        res["uri"] = r["uri"]
        out_jsonl.write(json.dumps(res, ensure_ascii=False) + "\n")
        out_jsonl.flush()
        paths[r["uri"]] = res["top_path"]
        if n % 10 == 0 or n == len(rows):
            print(f"{n}/{len(rows)}  peak GPU {torch.cuda.max_memory_allocated() / 2**30:.1f} GiB  "
                  f"{res['input_tokens']} tokens, {res['pass1_s'] + res['pass2_s']:.2f}s for the last post", flush=True)
    json.dump(paths, open(f"{a.out}.paths.json", "w"), indent=1)


if __name__ == "__main__":
    main()
