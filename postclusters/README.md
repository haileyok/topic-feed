# postclusters

An exploration: embed posts, group them into topics, have an LLM name them, and put every post on a map a person can
browse to judge whether the topics make sense. The Python here only reads ClickHouse and writes files under `data/`;
the map is served by `cmd/postmap`, which needs no database.

There are two ways to make the topics. The one the map now uses is the delve atlas's (`~/bluesky/delve-atlas`):

```
 ClickHouse ─► pull.py ─► embed.py ─► atlas_pilot.py ──────────► atlas_extend.py ─► label.py ─► build_snapshot.py
 (posts, last  distinct   Ollama      on a sample (shards 0-3):   every other post   Haiku 5.5   --layout post
  5 days)      texts in   nomic-embed  exact kNN on the GPU,      gets the topic and  titles each  ─► data/snapshot/<UTC>/
               16 shards  -text        UMAP 2d + 12d, HDBSCAN,     the place of its    topic and    ─► cmd/postmap
                                       noise attached to a topic   nearest sampled     each region
                                                                   posts (GPU)
```

The first way, `cluster.py` (k-means segments of about 350 posts, a nearest-neighbour graph, Leiden areas, a UMAP of
the segment centres, `build_snapshot.py --layout pack`), is kept for comparison.

```sh
cd postclusters
uv run python pull.py --shards 0-3          # a quarter of the posts (about 855,000 distinct texts); seconds per shard
uv run python pull.py --shards 4-15 --window-of 0-3   # the rest, over the same stretch of time as shards 0-3
uv run python embed.py --shards 0-15        # about 150 posts/s, so about 1.6 h per 4 shards; resumable

# The atlas way. The GPU steps run through `sg render` (see gpu_knn.py): torch with a GPU is in the trainer's environment.
uv run python atlas_pilot.py --shards 0-3 --name atlas-sample --subsample 250000 --mcs 12   # about 55 min: kNN 2, UMAPs 45, HDBSCAN 6
uv run python atlas_extend.py --sample atlas-sample/sub250000-mcs12 --shards 0-15 --out atlas   # about 3 min
uv run python label.py --model haiku55 --dir atlas --workers 12   # about 10 min for 2,896 topics
uv run python label.py --model haiku55 --dir atlas --areas
uv run python build_snapshot.py --shards 0-15 --dir atlas --layout post --models haiku55   # about 1 min; data/snapshot/<UTC>/ and "latest"

# The first way
uv run python cluster.py --shards 0-15      # about 100 min and 14 GB for all 16; 9,768 segments, 75 areas
uv run python label.py --model haiku        # about 27 min for 9,768 segments (--workers 12)
uv run python label.py --model haiku --areas
uv run python report.py                     # data/clusters/report.html and map.png
uv run python build_snapshot.py --shards 0-15  # about 35 min; the posts are placed by pack() and PCA, not by UMAP

uv run python pilot_compare.py && uv run python pilot_sample.py   # the trial of the two ways on one shard, and a page to read it
uv run python -m unittest                   # tests of the logic
```

Everything lands in `data/` (ignored by git). The report contains the text of public posts and is meant to be
read locally.

## The map

`build_snapshot.py` writes an immutable snapshot directory (the file list is at the top of the script) and repoints
`data/snapshot/latest`. `cmd/postmap` serves the newest one: every post is a point, coloured by area, by how
coherent the labelling model found its segment, by the segment's kind, or by the existing classifier's critical,
sentiment or outraged score for that post. Areas are named when zoomed out and segments when zoomed in. Click a
segment for its description, tone, who posts it, example posts, search inside it and the segments next to it; click
a post for its text, its neighbours on the map and a link to it on Bluesky. The search box finds topics by name and
posts by the words in them (at least 3 characters; matching posts are ringed on the map). Filters hide bots, spam
and ads, adult content, segments without opposing sides, or segments below a coherence.

```sh
make postmap                     # container on port 8740 of the internal network; picks up a new snapshot within a minute
go run ./cmd/postmap             # or without a container (POSTMAP_DIR, HTTP_ADDR; listens on 127.0.0.1 unless HTTP_ADDR says otherwise)
node tools/cdp.mjs tools/scenarios/postmap.mjs        # headless checks of the page (needs the service; CHROMIUM=...)
node tools/cdp.mjs tools/scenarios/postmap-perf.mjs   # draw and drag speed at several zooms, to compare versions of the page
```

The page keeps panning and zooming smooth with 3.4 million points: the points are sorted into blocks of the map so a
view draws only the blocks it shows; while the view moves only a share of each block's points are drawn, as many as the
last frames' speed allows (and all of them again when it stops); labels are laid out once for a zoom and a stretch of
map, drawn from cached images, so moving the view only copies the ones on screen.

The service shows the text of public posts and has no access control: `make postmap` publishes it on the internal network only (the way the labelling report viewer is), and it must not be exposed publicly. It loads every post's lower-cased text into memory for search (about 470 MB for 3.4 million
posts) and never serves `posts.jsonl` or `texts.txt` whole.

Positions mean two things. Where a segment sits among the others comes from a UMAP of the segment centres, nudged so that segments sized by their posts do not overlap, so
distances between segments are as meaningful as that UMAP and no more. Inside a segment a post is placed by the first two
principal components of the segment's own embeddings: posts that sit close are alike along the directions that
vary most in that segment, and that is all it says.

## Things that are easy to get wrong

- **HDBSCAN is the slow step and cannot be watched.** On the 855,000 posts of shards 0-3 it ran for over 3 hours without
  finishing (UMAP took 40 minutes). On a random 250,000 of them it takes 6 minutes, so `--subsample` does that and
  `atlas_extend.py` gives the other posts a topic by their nearest labelled posts. On the held-out labelled posts, that
  vote picks the HDBSCAN topic 71% of the time.
- **Haiku 5.5 takes no `temperature`** (the gateway refuses it); `label.py` leaves it out for that model.
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

## What the runs found

The atlas way, on all 16 shards (the map now in use): 2,896 topics (median 654 posts, 10th to 90th percentile 239 to 2,367;
the biggest, books and writing, has 58,586) in 75 regions. Haiku 5.5 titled all of them in 10 minutes with no failures
and rated 67% coherent (4 or 5) and 10% incoherent (1 or 2). Of a post's 10 nearest on the map, 34% are in its topic
and 59% in its region (by chance: 0.1% and 2%). The trial on one shard (213,772 posts, 428 clusters each, rated by
Haiku 5.5 on 150 random clusters): HDBSCAN topics 59% coherent against 45% for k-means; the existing classifier's
subtopics agree slightly better with k-means (0.437 against 0.421).

The k-means way, on all 16 shards:

The last five days (2026-10-04 to 10-09), all 16 shards: 3,418,707 distinct texts, 3,638,264 posts (counting repeats),
9,768 segments (median 334 posts), 75 areas. Labelled with Claude Haiku 4.5 through the AI gateway (27 minutes, no
failures).

- Haiku rates 72% of segments coherent (4 or 5) and 19% incoherent (1 or 2), holding the same shares of the posts.
- Of the posts, 29% are discussion, 23% personal updates, 16% news links; 13% of segments (13% of posts) are bots, spam
  or ads.
- 2,091 segments (21%) were described as having posts on opposing sides; 126 segment titles mention AI. Some of the
  axes are filler ("easy vs hard to do").

The first run, on a quarter of the posts (shards 0 to 3): 854,907 distinct texts, 2,443 segments, 56 areas. Haiku rated
63% of segments coherent and 25% incoherent. Where the existing 117-subtopic classifier's top subtopic was at least half
of a segment, Haiku rated it coherent more often (52% of coherent segments against 15% of incoherent ones), so the
ratings are not just the model agreeing with itself (not re-measured on all 16 shards). Luna (`gpt-5.6-luna`) was compared
with Haiku on 80 of those segments: near-identical titles, Luna more lenient on coherence (3.8 against 3.05 on average)
and about twice as slow. Haiku was used for everything.
