# postclusters

An exploration, not a service: embed posts, group them into small segments of the embedding space, link
the segments into a graph, have an LLM name them, and write a report for a person to judge whether the
segments make sense. Nothing here serves anything or writes to the database.

```
 ClickHouse ──► pull.py ──► embed.py ──► cluster.py ──► label.py ──► report.py
 (posts, last   distinct    Ollama       segments (k-means),  Haiku names   review report
  5 days)       texts in    nomic-embed  post nearest-        each segment  (HTML + map)
                16 shards   -text        neighbour graph,     and each area
                                         areas (Leiden)
```

```sh
cd postclusters
uv run python pull.py --shards 0-3          # a quarter of the posts (about 855,000 distinct texts)
uv run python embed.py --shards 0-3         # about 150 posts/s, so about 1.6 h; resumable
uv run python cluster.py --shards 0-3       # about 8 min; ~2,400 segments, ~55 areas
uv run python label.py --model haiku        # about 10 min, about $3 at list price
uv run python label.py --model haiku --areas
uv run python report.py                     # data/clusters/report.html and map.png
uv run python -m unittest                   # tests of the logic
```

Everything lands in `data/` (ignored by git). The report contains the text of public posts and is meant to be
read locally.

## Things that are easy to get wrong

- **Never pull a shard again once it has embeddings.** ClickHouse does not promise the order of grouped rows, so a
  second pull can reorder posts relative to the embeddings. `embed.py` stores a fingerprint of each shard's
  documents and `cluster.py` refuses to run if they differ.
- **Run anything longer than an hour as a service** (`shell_service`), not a background command: those are
  killed after an hour. `embed.py` is resumable (finished chunks are skipped), so a killed run loses nothing.
- **Embeddings group by language as well as subject.** Posts mostly in a non-Latin script are left out when
  clustering; other languages written in Latin script (German, Spanish, Portuguese) still form segments of
  their own.
- **Bots that put a random id in every post** escape exact-duplicate removal (`macro: <20 random characters>` is
  1.6% of all posts). The report flags segments that are mostly templated posts; collapsing them before
  clustering is the next improvement.
- **Tell the labelling model the date.** It assumes the year from its training data: a few titles say "2024" for
  the 2026 Texas Senate race.
- `label.py` asks for JSON and checks the answer's shape; post text is passed as data with an instruction to
  ignore instructions inside it.

## What the first run found

The last five days (2026-10-04 to 10-09), a quarter of the posts (shards 0 to 3 of 16): 854,907 distinct texts, 910,003 posts
(counting repeats), 2,443 segments (median 335 posts), 56 areas. Labelled with Claude Haiku 4.5 through the AI
gateway.

- Haiku rates 63% of segments coherent (4 or 5), 25% incoherent (1 or 2), holding about the same shares of the
  posts. Where the existing 117-subtopic classifier's top subtopic is at least half of a segment, Haiku rates it
  coherent more often (52% of coherent segments against 15% of incoherent ones), so the ratings are not just the
  model agreeing with itself.
- Of the posts, 27% are discussion, 26% personal updates, 16% news links; about 8% of segments (8% of posts) are
  bots, spam or templates.
- 554 segments (23%) were described as having posts on opposing sides; 70 segments mention AI, 29 of those with
  an opposing-sides axis. Some of the axes are filler ("easy vs hard to do").
- Luna (`gpt-5.6-luna`) was compared with Haiku on 80 segments: near-identical titles, Luna more lenient on
  coherence (3.8 against 3.05 on average) and about twice as slow. Haiku was used for everything.
