#!/usr/bin/env python3
"""Draft a topic taxonomy from real posts with an LLM through AGW (plan §8.2).

Stage 1: sample posts spread evenly across the hours of the day and ask the LLM, one
chunk at a time, which topics it sees (with rough counts and example post numbers).
Stage 2: merge the chunk lists into one two-level taxonomy YAML that follows the
plan's rules. The output is a draft for human review, not a final taxonomy.

Usage (secrets from ~/.config/topic-feed/env):
    set -a; . ~/.config/topic-feed/env; set +a
    python3 tools/draft_taxonomy.py --out taxonomy/draft1.yaml

Standard library only.
"""

import argparse
import base64
import json
import os
import re
import sys
import urllib.parse
import urllib.request

AGW = os.environ.get("TYPESAFE_BASE_URL", "https://agw.noclues.net")
CLICKHOUSE = os.environ.get("CLICKHOUSE_HTTP", "http://127.0.0.1:8123")


def clickhouse(sql: str) -> list[dict]:
    auth = base64.b64encode(f"topicfeed:{os.environ['CLICKHOUSE_PASSWORD']}".encode()).decode()
    req = urllib.request.Request(
        CLICKHOUSE + "/?" + urllib.parse.urlencode({"database": "topicfeed", "default_format": "JSONEachRow"}),
        data=sql.encode(),
        headers={"Authorization": "Basic " + auth},
    )
    with urllib.request.urlopen(req, timeout=120) as r:
        return [json.loads(line) for line in r.read().decode().splitlines() if line.strip()]


def llm(model: str, prompt: str, max_tokens: int) -> str:
    key = os.environ["TYPESAFE_API_KEY"]
    body = {"model": model, "max_tokens": max_tokens, "messages": [{"role": "user", "content": prompt}]}
    req = urllib.request.Request(
        AGW + "/v1/chat/completions",
        data=json.dumps(body).encode(),
        headers={"x-agw-key": key, "Authorization": "Bearer " + key, "content-type": "application/json",
                 "X-Client": "topic-feed"},
    )
    with urllib.request.urlopen(req, timeout=900) as r:
        out = json.load(r)
    return out["choices"][0]["message"]["content"]


def render(p: dict) -> str:
    """One line per post, close to what Jev will see."""
    parts = [re.sub(r"\s+", " ", p["text"]).strip()[:300]]
    if p["alts"]:
        parts.append("[alt: " + re.sub(r"\s+", " ", p["alts"])[:150] + "]")
    if p["link_domain"] or p["link_title"]:
        parts.append(f"[link: {p['link_domain']} | {p['link_title'][:120]}]")
    if p["quote_text"]:
        parts.append("[quote: " + re.sub(r"\s+", " ", p["quote_text"])[:150] + "]")
    return " ".join(x for x in parts if x)


STAGE1 = """Below are {n} top-level English posts from Bluesky, a social network, sampled across all hours of the day.

Identify the topics these posts are about, as you would when designing a two-level topic taxonomy (broad topic, then subtopic) for recommending posts to people based on their interests.

For each topic you find, give:
- "broad": a broad topic name
- "sub": a more specific subtopic name within it
- "count": roughly how many of these posts belong to it
- "examples": up to 3 post numbers that are clear examples

Be exhaustive: include small topics, and include non-topical categories you see (greetings, personal life updates, jokes with no subject, self-promotion, unclear posts). Output only a JSON array, no prose.

Posts:
{posts}"""

STAGE2 = """You are designing a topic taxonomy for Bluesky posts. It will be used to label posts with a classifier that reads option descriptions literally, and then to recommend posts to people based on the topics they like.

Below are topic lists extracted from {chunks} chunks of real posts ({total} posts in total, spread across all hours of the day). Each entry has a broad topic, a subtopic, an approximate count, and example posts.

Merge them into ONE two-level taxonomy and output it as YAML, following these rules exactly:

1. 20 to 30 broad topics. Each broad topic has 5 to 15 subtopics, except `unclear`, which has none.
2. Every broad topic that has subtopics includes a subtopic with id `other` ("other within this topic").
3. Required broad topics:
   - `personal_life`: personal and everyday life with no other clear topic (greetings, "good morning", life updates, feelings, daily routines). Much of Bluesky is this.
   - `unclear`: no discernible topic, or not enough content to tell. No subtopics.
4. Every topic has: `id` (snake_case), `name` (display name), `description` (one or two sentences, at most ~30 words), and for broad topics `examples` (2 to 3 short example posts, copied from the examples below, trimmed to at most 100 characters).
5. Descriptions must state what the topic does NOT include wherever confusion with another topic is likely (e.g. "Not sports-betting promotions; see promotion."). The classifier reads them literally, so be precise about boundaries.
6. Topics must be mutually exclusive enough that a post has one best broad topic. Prefer topics people would choose to follow. Size broad topics by volume: split very large ones, merge tiny ones.
7. Keep descriptions tight: every question repeats its option list, so length costs money.

Output only YAML in exactly this shape, with no prose and no code fences:

version: draft1
broad:
  - id: sports
    name: Sports
    description: Games, athletes, teams, leagues, and sports media. Not sports-betting promotions (see promotion).
    examples:
      - "..."
    subtopics:
      - id: american_football
        name: American football
        description: NFL, college football, fantasy football.
      - id: other
        name: Other sports
        description: Sports content that fits no other subtopic.

Topic lists:
{lists}"""


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", default="taxonomy/draft1.yaml")
    ap.add_argument("--posts", type=int, default=6000)
    ap.add_argument("--chunk", type=int, default=1200)
    ap.add_argument("--model", default="claude-opus-5-5:api")
    args = ap.parse_args()

    per_hour = -(-args.posts // 24)
    rows = clickhouse(f"""
        SELECT text, arrayStringConcat(media_alts, ' | ') AS alts, link_domain, link_title, quote_text
        FROM posts FINAL
        WHERE uri NOT IN (SELECT uri FROM deletions WHERE collection = 'app.bsky.feed.post')
        ORDER BY cityHash64(uri, 'taxonomy-sample')
        LIMIT {per_hour} BY toHour(indexed_at)
        LIMIT {args.posts}""")
    rows = [r for r in rows if render(r)]
    print(f"sampled {len(rows)} posts", file=sys.stderr)

    out_dir = os.path.dirname(args.out) or "."
    stem = os.path.splitext(os.path.basename(args.out))[0]
    work = os.path.join(out_dir, f"{stem}.work")
    os.makedirs(work, exist_ok=True)

    lists = []
    for ci, start in enumerate(range(0, len(rows), args.chunk)):
        chunk = rows[start:start + args.chunk]
        numbered = "\n".join(f"{i + 1}. {render(p)}" for i, p in enumerate(chunk))
        raw = llm(args.model, STAGE1.format(n=len(chunk), posts=numbered), 16000)
        open(os.path.join(work, f"stage1-chunk{ci}.txt"), "w").write(raw)
        m = re.search(r"\[.*\]", raw, re.S)
        topics = json.loads(m.group(0)) if m else []
        # Replace example numbers with the example text so stage 2 can quote them.
        for t in topics:
            t["examples"] = [render(chunk[n - 1])[:160] for n in t.get("examples", []) if isinstance(n, int) and 0 < n <= len(chunk)]
        lists.append(topics)
        print(f"chunk {ci}: {len(topics)} topics", file=sys.stderr)

    lists_text = "\n\n".join(f"Chunk {i + 1}:\n" + json.dumps(t, ensure_ascii=False) for i, t in enumerate(lists))
    yaml_text = llm(args.model, STAGE2.format(chunks=len(lists), total=len(rows), lists=lists_text), 32000)
    yaml_text = re.sub(r"^```(?:yaml)?\s*|\s*```$", "", yaml_text.strip())
    open(os.path.join(work, "stage2.txt"), "w").write(yaml_text)

    header = (f"# DRAFT taxonomy, generated by tools/draft_taxonomy.py with {args.model}\n"
              f"# from {len(rows)} posts sampled across all hours of the day. Needs human review (plan §8.2).\n")
    with open(args.out, "w") as f:
        f.write(header + yaml_text + "\n")
    print(f"wrote {args.out}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
