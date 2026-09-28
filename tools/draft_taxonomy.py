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

STAGE2 = """You are designing a topic taxonomy for Bluesky posts. It will be used to label posts with a classifier that reads option descriptions literally, then to train a smaller model on those labels, and then to recommend posts to people based on the topics they like.

Below are topic lists extracted from {chunks} chunks of real posts ({total} posts in total, spread evenly across {span}). Each entry has a broad topic, a subtopic, an approximate count (an LLM estimate), and example posts.
{measured}
Merge them into ONE two-level taxonomy and output it as YAML, following these rules exactly:

1. 20 to 30 broad topics. `unclear` has no subtopics. Every other broad topic has at least 2 subtopics (counting `other`).
2. SUBTOPIC BUDGET: {min_subs} to {max_subs} subtopics in total across all broad topics, counting every `other`. This is a hard limit. Allocate subtopics in proportion to each broad topic's volume: a broad topic with ~1% of posts gets 2-3 subtopics, a large one (~10%) may get up to 10. Each subtopic should be expected to hold at least ~0.1% of all posts; the model will be trained on ~175,000 labeled posts and needs ~200 examples per subtopic. Merge thin subtopics rather than keeping them.
3. Subtopics must be durable categories that will still make sense in a month. Do NOT create subtopics for a single news event, person, scandal, or product launch (e.g. no `epstein` subtopic); those posts belong in a durable subtopic such as `us_politics/scandals_investigations` or the topic's `other`.
4. Every broad topic that has subtopics includes a subtopic with id `other` ("other within this topic"). Where a topic from a previous draft still fits, keep its id.
5. Required broad topics:
   - `personal_life`: personal and everyday life with no other clear topic (greetings, "good morning", life updates, feelings, daily routines). Much of Bluesky is this.
   - `unclear`: no discernible topic, or not enough content to tell. No subtopics.
6. Every topic has: `id` (snake_case), `name` (display name), `description` (one or two sentences, at most ~30 words), and for broad topics `examples` (2 to 3 short example posts, copied from the examples below, trimmed to at most 100 characters).
7. Descriptions must state what the topic does NOT include wherever confusion with another topic is likely (e.g. "Not sports-betting promotions; see promotion."). The classifier reads them literally, so be precise about boundaries.
8. Topics must be mutually exclusive enough that a post has one best broad topic. Prefer topics people would choose to follow. Size broad topics by volume: split very large ones, merge tiny ones.
9. Keep descriptions tight: every question repeats its option list, so length costs money.
10. Quote any YAML string value that contains a colon followed by a space.

Output only YAML in exactly this shape, with no prose and no code fences:

version: {version}
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


def measured_volumes(ref: str) -> str:
    """Topic volumes measured by Jev on a previous labeling run, as a prompt section.

    ref is "<taxonomy_version>:<label_config>". Counts use each post's top broad topic
    and its best path (highest path score), as in the review report.
    """
    version, config = ref.split(":", 1)
    rows = clickhouse(f"""
        SELECT broad_probs, path_scores FROM jev_labels FINAL
        WHERE taxonomy_version = '{version}' AND label_config = '{config}'""")
    if not rows:
        return ""
    n = len(rows)
    broad, paths = {}, {}
    for r in rows:
        top = max(r["broad_probs"].items(), key=lambda kv: kv[1])[0]
        broad[top] = broad.get(top, 0) + 1
        best = max(r["path_scores"].items(), key=lambda kv: kv[1])[0] if r["path_scores"] else top
        paths[best] = paths.get(best, 0) + 1
    lines = [f"\nMEASURED VOLUMES. A previous draft taxonomy ({version}) was used to label {n} random posts with the classifier. "
             "These shares are measured, so trust them over the LLM estimates when sizing topics. "
             "Subtopics under ~0.1% here are too thin to keep on their own. "
             "Previous-draft subtopics with no posts at all are not listed.\n",
             "Broad topic shares:"]
    for k, v in sorted(broad.items(), key=lambda kv: -kv[1]):
        lines.append(f"- {k}: {100 * v / n:.1f}%")
    lines.append("\nSubtopic (best path) shares:")
    for k, v in sorted(paths.items(), key=lambda kv: -kv[1]):
        lines.append(f"- {k}: {100 * v / n:.2f}%")
    return "\n".join(lines) + "\n"


def parse_topics(raw: str):
    """The JSON array in a stage-1 answer, or None if it doesn't parse."""
    m = re.search(r"\[.*\]", raw, re.S)
    if not m:
        return None
    try:
        topics = json.loads(m.group(0))
    except json.JSONDecodeError:
        return None
    return topics if isinstance(topics, list) else None


def count_subtopics(yaml_text: str) -> tuple[int, int]:
    """(broad topics, subtopics) in the expected indentation of the output shape."""
    broad = len(re.findall(r"^  - id: ", yaml_text, re.M))
    subs = len(re.findall(r"^      - id: ", yaml_text, re.M))
    return broad, subs


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", default="taxonomy/draft1.yaml")
    ap.add_argument("--version", default=None, help="taxonomy version (default: output file name)")
    ap.add_argument("--posts", type=int, default=9000)
    ap.add_argument("--chunk", type=int, default=1200)
    ap.add_argument("--model", default="claude-opus-5-5:api")
    ap.add_argument("--min-subtopics", type=int, default=100)
    ap.add_argument("--max-subtopics", type=int, default=120)
    ap.add_argument("--measured-from", default="", help="<taxonomy_version>:<label_config> of a labeled run to size topics by")
    ap.add_argument("--until", default="", help="only sample posts at or before this UTC time (YYYY-MM-DD HH:MM:SS); "
                    "pins the sample so a re-run reuses finished chunks")
    args = ap.parse_args()
    version = args.version or os.path.splitext(os.path.basename(args.out))[0]
    until = f"AND indexed_at <= toDateTime64('{args.until}', 6, 'UTC')" if args.until else ""

    # Spread the sample evenly over every hour in the data (all backfill days), not
    # just the hours of one day.
    hours = clickhouse(f"SELECT uniqExact(toStartOfHour(indexed_at)) AS h, min(indexed_at) AS lo, max(indexed_at) AS hi FROM posts WHERE 1 {until}")[0]
    per_hour = -(-args.posts // int(hours["h"]))
    span = f"{hours['h']} hours ({hours['lo'][:16]} to {hours['hi'][:16]} UTC)"
    rows = clickhouse(f"""
        SELECT text, arrayStringConcat(media_alts, ' | ') AS alts, link_domain, link_title, quote_text
        FROM posts FINAL
        WHERE uri NOT IN (SELECT uri FROM deletions WHERE collection = 'app.bsky.feed.post') {until}
        ORDER BY cityHash64(uri, 'taxonomy-sample-{version}')
        LIMIT {per_hour} BY toStartOfHour(indexed_at)
        LIMIT {args.posts}""")
    rows = [r for r in rows if render(r)]
    print(f"sampled {len(rows)} posts over {span}", file=sys.stderr)
    measured = measured_volumes(args.measured_from) if args.measured_from else ""

    out_dir = os.path.dirname(args.out) or "."
    stem = os.path.splitext(os.path.basename(args.out))[0]
    work = os.path.join(out_dir, f"{stem}.work")
    os.makedirs(work, exist_ok=True)

    # A finished chunk is reused only when the sample is pinned (--until) and its
    # posts are identical, recorded as a hash next to the chunk's output.
    import hashlib

    lists = []
    for ci, start in enumerate(range(0, len(rows), args.chunk)):
        chunk = rows[start:start + args.chunk]
        numbered = "\n".join(f"{i + 1}. {render(p)}" for i, p in enumerate(chunk))
        key = hashlib.sha256(numbered.encode()).hexdigest()[:16]
        raw_path, key_path = os.path.join(work, f"stage1-chunk{ci}.txt"), os.path.join(work, f"stage1-chunk{ci}.key")
        topics = None
        if args.until and os.path.exists(key_path) and open(key_path).read().strip() == key:
            topics = parse_topics(open(raw_path).read())
            if topics is not None:
                print(f"chunk {ci}: reusing finished output", file=sys.stderr)
        for attempt in range(3):
            if topics is not None:
                break
            raw = llm(args.model, STAGE1.format(n=len(chunk), posts=numbered), 16000)
            open(raw_path, "w").write(raw)
            topics = parse_topics(raw)
            if topics is None:
                print(f"chunk {ci}: unparseable JSON on attempt {attempt}; retrying", file=sys.stderr)
        if topics is None:
            raise SystemExit(f"chunk {ci}: no valid JSON after 3 attempts; see {raw_path}")
        open(key_path, "w").write(key)
        # Replace example numbers with the example text so stage 2 can quote them.
        for t in topics:
            t["examples"] = [render(chunk[n - 1])[:160] for n in t.get("examples", []) if isinstance(n, int) and 0 < n <= len(chunk)]
        lists.append(topics)
        print(f"chunk {ci}: {len(topics)} topics", file=sys.stderr)

    lists_text = "\n\n".join(f"Chunk {i + 1}:\n" + json.dumps(t, ensure_ascii=False) for i, t in enumerate(lists))
    prompt = STAGE2.format(chunks=len(lists), total=len(rows), span=span, measured=measured, lists=lists_text,
                           min_subs=args.min_subtopics, max_subs=args.max_subtopics, version=version)
    open(os.path.join(work, "stage2-prompt.txt"), "w").write(prompt)
    yaml_text = ""
    for attempt in range(2):
        yaml_text = re.sub(r"^```(?:yaml)?\s*|\s*```$", "", llm(args.model, prompt, 32000).strip())
        open(os.path.join(work, f"stage2-attempt{attempt}.txt"), "w").write(yaml_text)
        nb, ns = count_subtopics(yaml_text)
        print(f"merge attempt {attempt}: {nb} broad topics, {ns} subtopics", file=sys.stderr)
        if args.min_subtopics <= ns <= args.max_subtopics:
            break
        prompt += (f"\n\nYour previous answer had {ns} subtopics, outside the required {args.min_subtopics} to "
                   f"{args.max_subtopics}. Here it is; revise it to fit the budget and output the full YAML again.\n\n{yaml_text}")
    else:
        print("WARNING: subtopic count still outside the budget; review before use", file=sys.stderr)

    header = (f"# DRAFT taxonomy, generated by tools/draft_taxonomy.py with {args.model}\n"
              f"# from {len(rows)} posts sampled evenly over {span}.\n"
              f"# Subtopic budget {args.min_subtopics}-{args.max_subtopics}"
              + (f"; sized using measured volumes from {args.measured_from}" if args.measured_from else "")
              + ". Needs human review (plan §8.2).\n")
    with open(args.out, "w") as f:
        f.write(header + yaml_text + "\n")
    print(f"wrote {args.out}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
