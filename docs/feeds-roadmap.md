# Feeds roadmap

**Owner:** Hailey (`hailey.at`)
**Status:** Planned, not started. Agreed with Hailey on 2026-09-29.
**Last updated:** 2026-09-29

Five improvements to the feed generator (`cmd/feedgen`, `internal/feedgen`), written down
so they can be picked up later. Each section says what exists today, what to build, and
what's still open.

## Where things stand

- **Feeds** are defined in `config/feeds.yaml` and served from `https://feeds.hailey.at`
  (`did:web:feeds.hailey.at`), owned by `hailey.at`: NFL, AI, Video Games, Cats & Pets,
  Anime & Manga, Software Dev, Tabletop Games. `make feeds` restarts the server with the
  config; `make feeds-publish` writes the feed records (interactive; asks for the emailed
  sign-in code when needed).
- **Selection:** a post is a candidate when the model's probability for any of the feed's
  subtopic paths is at least `min_prob`, within the last 24 hours (`post_pipeline`, written
  by `cmd/pipeline`). Deleted posts, inactive accounts, and posts the label policy drops are
  left out at serve time; `allow_adult` admits adult-only posts.
- **Ranking** (`internal/feedgen/rank.go`): `(prior + weighted likes, reposts, replies,
  quotes) / (age_hours + 2)^gravity`, with the prior from the model's substance,
  general-interest, and promo signals plus optional tone weights; every 4th slot is the
  newest post; an author's posts are 10 slots apart. All per feed in `feeds.yaml`.
- **Tone rules** per feed: `tone.max` / `tone.min` cutoffs and `tone.weights` nudges over
  the six tones (informative, humorous, personal, outraged, supportive, other). AI uses
  `max: {outraged: 0.5}`.
- **Feed context:** every served post carries `feedContext` JSON: `id` (post URI), `topic`
  and `p` (top subtopic and probability), `top` (three most likely subtopics), `tone` (all
  six tones). Every page carries a `reqId` starting with the feed's rkey.
- **Interactions:** all feeds declare `acceptsInteractions`; `app.bsky.feed.sendInteractions`
  stores every event in `feed_interactions` (`viewer_did`, `feed`, `item`, `event`,
  `feed_context`, `req_id`, `received_at`). Events: `interactionSeen`, `interactionLike`,
  `interactionRepost`, `interactionReply`, `interactionQuote`, `interactionShare`, the
  `clickthrough*` events, `requestMore`, `requestLess`.
- **Engagement counts:** likes (`like_counts_hourly`) and reposts, replies, quotes
  (`engagement_hourly`; replies and quotes counted since 2026-09-29 06:04 UTC).

## 1. Feeds that span every topic ("Good vibes", "Funny", "Deep reads")

**Why:** feeds chosen by tone or quality rather than topic, which keyword feeds can't do.

**Build:**
- Let a feed match any subtopic: `paths: any` (or omit `paths`). Candidates then come from
  every classified post in the window, so `min_prob` doesn't apply; the selection is the
  cutoffs.
- Add signal cutoffs next to tone cutoffs: `signals.min` / `signals.max` over substance,
  news, promo, general_interest, the same shape as `tone`.
- Add `exclude_paths` so a cross-topic feed can leave out politics, adult topics, or
  automated feeds.
- Candidate volume is ~20k posts/hour across all topics, so push tone and signal cutoffs
  into the ClickHouse query (bind the tone/signal name as a parameter) instead of filtering
  in Go, and set `max_posts` high enough to reach back a useful number of hours.

**Examples:**
```yaml
- rkey: good-vibes
  display_name: Good Vibes
  paths: any
  exclude_paths: [us_politics/*, world_news/*, adult_content/*, automated_feeds/*]
  tone:
    max: {outraged: 0.2}
    weights: {supportive: 2, humorous: 1}
- rkey: deep-reads
  display_name: Deep Reads
  paths: any
  signals: {min: {substance: 0.8}}
  tone: {max: {outraged: 0.4}}
```

**Open:** whether `exclude_paths` supports `broad/*` wildcards (likely yes); a sensible
default `max_posts` for cross-topic feeds.

## 2. Learning from interactions

**Why:** use what readers do to improve each feed without retraining the model.

**Signals available** (per feed, from `feed_interactions`):
- `requestLess` / `requestMore`: explicit.
- seen-but-not-engaged: `interactionSeen` without a like/repost/reply/quote/clickthrough
  from the same viewer on the same item.

**Build:**
- A periodic job (in feedgen, every few minutes) aggregates per feed over a trailing window
  (e.g. 7 days): show-less and show-more counts per author, per subtopic (from
  `feed_context.topic`), and per dominant tone; plus seen and engaged counts.
- Turn these into ranking adjustments: multiply a candidate's score by a per-feed factor
  for its author, subtopic, and tone, e.g. `factor = exp(-k * (less - more) / (seen + c))`,
  clamped to [0.2, 1.5], with smoothing `c` so a single tap doesn't bury an author.
- Per-post: a post with enough show-less taps in a feed drops out of that feed.
- Expose the factors in the tuning page (section 5) and in metrics.

**Not in scope:** anything per viewer ("you already saw this", personal preferences). That
is the personal feed ("For you", built 2026-10-01: see the README's "Personal feeds"), which
keeps its own per-viewer history; these adjustments are per feed.

**Open:** trailing window length; the weight of seen-without-engagement relative to an
explicit show-less; whether show-less on one feed should affect other feeds.

## 3. Interactions dashboard

**Why:** see how each feed performs and which thresholds are too loose.

**Build:** a page served by feedgen on the metrics listener (localhost only, reachable over
the tunnel or SSH), or a static report like the labeling reports on :8090. Per feed, over a
chosen window:
- viewers, pages served, posts seen;
- like, repost, reply, quote, share, and clickthrough rates per seen post;
- show-more and show-less rates;
- the same rates broken down by subtopic, by probability band (e.g. 0.5-0.6, 0.6-0.7, ...),
  by tone, and by slot position (ranked slots vs fresh slots);
- the posts with the most show-less taps, with their text and feed context.

The probability-band view is the direct check on `min_prob`: if 0.5-0.6 gets much more
show-less than 0.8+, raise the threshold.

**Open:** where it's served (feedgen vs the reports viewer); whether to add Prometheus
counters for the headline rates so they can be graphed.

## 4. Hard examples for retraining

**Why:** posts people mark "show less" in an on-topic feed are likely misclassified. They
are a small, human-picked set of hard cases for the next model.

**Build:**
- A table (or export) of candidate mistakes: items with `requestLess` in a feed, with the
  feed's paths, the model's top 3 at serve time (from `feed_context`), and the post
  document (`post_pipeline.model_input`).
- A review page in the style of the gold-set page: for each post, confirm the right
  subtopic(s) or mark "fine, just not interesting". Confirmed labels go to
  `reference/hard/` as JSON, like `reference/gold/`.
- Feed them into training as a small high-weight set, and track them as their own
  evaluation slice (did the next model fix them?).
- Also sample posts just above each feed's threshold that got no engagement, as a second
  source of likely mistakes.

**Open:** how to weight them in training (`trainer/train_fusion.py`); whether Jev relabels them
first or the review page is the only label source.

## 5. Ranking tuning page

**Why:** tune ranking weights, gravity, fresh slots, and tone rules by eye before changing
`feeds.yaml`.

**Build:** a page (same place as the dashboard) that shows, for a chosen feed, the current
ranking next to a ranking with edited settings, each row with: slot, age, likes, reposts,
replies, quotes, prior, tone, score, author, and the post text. Settings are edited in the
page and recomputed against the feed's current candidates; nothing is saved. A "copy as
YAML" button produces the `ranking` / `tone` block to paste into `feeds.yaml`.

Needs a debug endpoint in feedgen that returns a feed's candidates with every scoring
input (already in memory for the current build), and the scoring code exposed so the page
recomputes exactly what the server would.

**Open:** whether the page recomputes in the browser (port `Score`/`Rank` to JS) or calls
feedgen with the edited settings (simpler, one implementation).

## Suggested order

1. Dashboard (3): it measures everything else.
2. Cross-topic feeds (1).
3. Tuning page (5): shares the debug endpoint with the dashboard.
4. Learning from interactions (2): needs a few days of interactions first.
5. Hard examples (4): needs show-less data, and a retraining run to use it.
