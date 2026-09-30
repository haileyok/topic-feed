# topic-feed

Topic feeds for Bluesky, picked by a trained classifier instead of keyword lists.

Every English post on the network is classified seconds after it's posted: which of 25 broad
topics and 117 subtopics it's about, what tone it has, and ten scores such as how substantive
it is, whether it's news, whether it's an ad or spam, and whether it's critical of its own
subject. Feeds are defined in one YAML file as "posts about these topics, above this
confidence, with these score rules", ranked by engagement and freshness, and served to the
Bluesky app from `https://feeds.hailey.at`. A web page at the same address builds and previews
feeds interactively.

The classifier is a ModernBERT-base model distilled from **Jev** (`jev-1.13.0`), a hosted
model that answers structured questions about text. Jev labels a sample of posts; the small
model learns to reproduce its answers and runs on one GPU at about 770 posts a second, for
roughly 1/2,000th of Jev's cost per post. The model is on Hugging Face:
[haileyok/microblog-topic-classifier-v4](https://huggingface.co/haileyok/microblog-topic-classifier-v4).

- [How it works](#how-it-works)
- [Running it](#running-it)
- [Feeds](#feeds)
- [The feed builder](#the-feed-builder)
- [The classifier](#the-classifier)
- [Training a new model](#training-a-new-model)
- [Data](#data)
- [Operations](#operations)
- [Further reading](#further-reading)

## How it works

```
                        ┌────────────────────────── one machine ───────────────────────────┐
 Jetstream ── posts, ──►│ ingest ──► ClickHouse ◄── modlabels ◄── Bluesky moderation labels │
 (Bluesky     likes,    │               │  ▲                                                 │
  firehose)   reposts,  │               ▼  │                                                 │
              deletes   │            pipeline ── label policy, OCR, image descriptions ──►   │
                        │               │  ▲         (tesseract; vision model via the        │
                        │               ▼  │          AI gateway)                            │
                        │        classifier service (GPU, trainer/serve.py)                  │
                        │               │                                                    │
                        │               ▼                                                    │
                        │   post_pipeline: topics, tone, scores per post                     │
                        │               │                                                    │
                        │               ▼                                                    │
                        │   feedgen ── ranked feeds ──► Bluesky app (via Cloudflare tunnel)  │
                        │      └────── feed builder page, previews                           │
                        │                                                                    │
                        │   labeler ◄──► Jev (AI gateway)   export ──► trainer (PyTorch, GPU)│
                        └────────────────────────────────────────────────────────────────────┘
```

1. **Ingest** (`cmd/ingest`) follows Bluesky's Jetstream and writes posts, likes, reposts,
   deletions, and account status changes to ClickHouse. It keeps English posts (the post's
   language tags, checked by a local language detector), resolves the text of quoted posts, and
   records self-labels and attachments. Its position in the stream is saved, so restarts
   resume.
2. **Moderation labels** (`cmd/modlabels`) follows the Bluesky moderation service's label
   stream, including label removals, into `mod_labels`.
3. **The pipeline** (`cmd/pipeline`) takes each post about 10 seconds after ingest, when labels
   have usually arrived:
   - applies the label policy (`config/label_policy.yaml`): posts are `ok`, `adult_only`
     (shown only in feeds that allow adult content), or `drop` (never shown);
   - finds text for images without alt text: tesseract OCR first, and if that finds fewer
     than 7 confident words, a one- or two-sentence description from a vision model (Luna,
     `gpt-6-luna`) through the AI gateway, within a daily budget;
   - sends the post, rendered as a *post document* (below), to the classifier service and
     writes everything to `post_pipeline`.
4. **The classifier service** (`trainer/serve.py`) runs on the host under systemd, serves the
   current model on the GPU at `127.0.0.1:8700`, and answers batches of post documents with
   topic, tone, and score probabilities.
5. **The feed generator** (`cmd/feedgen`) rebuilds every feed every 20 seconds from the last 48
   hours of `post_pipeline`, ranks it, and serves `app.bsky.feed.getFeedSkeleton` to Bluesky.
   It also records the interactions Bluesky sends back (seen, liked, "show more", "show less")
   and serves the feed builder page.

Training happens offline: the **labeler** (`cmd/labeler`) asks Jev about a sample of posts,
**export** (`cmd/export`) turns the labels into a training set, and the **trainer**
(`trainer/`) fine-tunes the model on the GPU.

## Running it

Everything runs on one Linux machine with an NVIDIA GPU, rootless Docker with Compose, Go, and
[uv](https://docs.astral.sh/uv/). Data lives under `/data` (ClickHouse, exports, models,
reports). Long-running services run from the main checkout (`~/bluesky/topic-feed`, kept at
`main`), not from a worktree.

### Configuration

Secrets and deployment settings live in `~/.config/topic-feed/env` (the Makefile reads it;
override with `ENV_FILE=`):

| variable | used by | what |
|---|---|---|
| `CLICKHOUSE_PASSWORD` | everything | database password |
| `JETSTREAM_API_KEY` | ingest, verifyingest | Jetstream archive replay |
| `TYPESAFE_API_KEY` | labeler, pipeline | the AI gateway key (Jev, image descriptions) |
| `FEEDGEN_HOSTNAME` | feedgen | public hostname, e.g. `feeds.hailey.at` |
| `FEEDGEN_OWNER_DID` | feedgen | the account that owns the feed records |
| `FEEDGEN_HANDLE`, `FEEDGEN_APP_PASSWORD` | `make feeds-publish` | login for writing feed records |
| `FEEDGEN_BUILDER_ADULT_KEY` | feedgen | optional; the owner's key for adult content in the feed builder (at least 24 characters) |

Each command documents its other settings (all with defaults) at the top of its `main.go`;
for example the pipeline's `LLM_DAILY_BUDGET_USD`, `LLM_TIMEOUT_SECONDS`, `OCR_WORKERS`, and
feedgen's `FEEDGEN_WINDOW_HOURS`, `FEEDGEN_REFRESH_SECONDS`, `FEEDGEN_MAX_POSTS`.

### Starting everything

```sh
make up                   # ClickHouse, ingest, modlabels, pipeline, report viewer, TensorBoard
make schema               # apply schema/*.sql (safe to re-run)
make install-classifier   # the GPU classifier service (systemd user unit)
make feeds                # the feed generator (compose profile "feeds")
make feeds-publish        # write the feed records to the owner's account (asks before writing)
make install-backup       # nightly backup timer
```

The pipeline waits for the classifier service and refuses to start if the model expects a
post document version it can't render. Enable linger (`loginctl enable-linger $USER`) so
rootless containers keep running without a login session.

### Services and ports

| service | where | port |
|---|---|---|
| ClickHouse | compose `clickhouse` | `127.0.0.1:9000` (native), `127.0.0.1:8123` (HTTP) |
| ingest | compose `ingest` | metrics `127.0.0.1:9101` |
| modlabels | compose `modlabels` | metrics `127.0.0.1:9102` |
| pipeline | compose `pipeline` (host network) | metrics `127.0.0.1:9103` |
| feed generator | compose `feedgen` (profile `feeds`) | `127.0.0.1:8710`, metrics `9104`; public through a Cloudflare tunnel |
| classifier service | systemd user unit `topic-feed-classifier` | `127.0.0.1:8700` |
| labeling reports | compose `viewer` | `:8090` |
| TensorBoard | compose `tensorboard` | `:6006` |
| Jev labeling | compose `labeler` (profile `labeling`, `make label`) | none |

Every Go service exposes Prometheus metrics at `/metrics` and logs JSON to stdout
(`docker logs topic-feed-<service>`); the classifier logs to the journal
(`make classifier-logs`).

### Make targets

| target | what |
|---|---|
| `make up` / `down` / `ps` / `logs` | the compose services |
| `make schema` | apply the schema |
| `make ch` | interactive ClickHouse client |
| `make test` | Go tests |
| `make feeds` | rebuild and restart the feed generator with `config/feeds.yaml` |
| `make feeds-publish` | write feed records (`DRY=1` to only print, `CODE=` for an emailed sign-in code) |
| `make label` | start or resume the Jev labeling run over the labeling windows |
| `make export` | export a training set (`LABEL_CONFIG=`, `FULL_CONTEXT=`, `EXPORT=`) |
| `make train` | train a model (`EXPORT=`, `RUN=`, `EPOCHS=`, `TRAIN_ARGS=`) |
| `make baseline` | the embedding baseline on an export |
| `make relabel` / `relabel-load` | relabel uncertain posts with a second model and load the result |
| `make install-classifier` / `classifier-logs` | the classifier service |
| `make backup` / `install-backup` | backups |

## Feeds

Feeds are defined in `config/feeds.yaml`; the comment at the top of the file documents every
field. For example, an AI feed without the angry, anti-AI, or spammy posts:

```yaml
  - rkey: upbeat-ai
    display_name: Upbeat AI
    description: Posts about AI, picked by a topic classifier. No keyword lists.
    paths: [technology/ai]
    min_prob: 0.6
    tone:
      max: {outraged: 0.5}
    signals:
      max: {critical: 0.5, spam: 0.3}
      weights: {substance: 1}
```

- **Selection.** A post is a candidate when the model's probability for any of the feed's
  `paths` (subtopics like `sports/american_football`, or whole broad topics like `world_news`)
  is at least `min_prob`, within the last 48 hours. `paths: ["*"]` matches every topic, for
  feeds chosen by tone or scores alone. `exclude` leaves out posts above a probability for a
  topic. Deleted posts, inactive accounts, and posts the label policy drops are always left out;
  `adult_only` posts only appear with `allow_adult: true`.
- **Rules on tone and scores.** `tone` and `signals` each take `max` and `min` (hard cutoffs:
  posts outside the range are removed) and `weights` (a nudge: weight × score is added to the
  post's ranking prior, nothing is removed). Tones: `informative`, `humorous`, `personal`,
  `outraged`, `supportive`, `other`. Signals: see [the classifier](#the-classifier).
- **Ranking.** Each post scores
  `(prior + like·likes + repost·reposts + reply·replies + quote·quotes) / (age_hours + 2)^gravity`,
  where the prior comes from the model's substance, general-interest, and promotion scores (and
  any weights), so posts without engagement yet are still ordered sensibly. Every
  `fresh_every`-th slot goes to the newest post not yet placed, and an author's posts are kept
  `author_gap` slots apart. All of it can be set per feed under `ranking`.
- **Serving.** Pages are cut from one ranked build, so paging through a feed never repeats or
  skips posts even as it's rebuilt. Every served post carries a `feedContext` with its top
  subtopics, tone, and scores, and every page a request ID, so interactions map back to what
  was shown.
- **Changing feeds.** Edit `config/feeds.yaml`, then `make feeds` (settings take effect
  immediately) and, for new feeds or changed names and descriptions, `make feeds-publish`.
  Changing a feed's `rkey` makes a new feed.

There are 13 feeds today: NFL, Baseball, AI, Video Games, Cats & Pets, Anime & Manga, Software
Dev, Tabletop Games, Photography, Art, Calm World News, Chill Tech, and Funny.

## The feed builder

`https://feeds.hailey.at/` is a public page for building feeds without editing YAML. It
previews a feed from live data as you pick topics, move tone and score sliders ("Allowed"
ranges are cutoffs, "Boost" is a nudge), and adjust ranking; it lists the served feeds for
browsing or remixing; and **Copy YAML** produces the entry for `config/feeds.yaml`. A link to
the page reproduces the exact settings.

With **Scores** on, each post shows why it's there: its top subtopics and how well it matches
the feed, every signal and tone as a bar (with the ones the current settings use highlighted),
and its ranking score, slot, age, engagement, and labels.

The page never shows adult content to the public. With `FEEDGEN_BUILDER_ADULT_KEY` set, the
owner visits `https://feeds.hailey.at/adult-access?key=<key>` once to get a cookie; that
browser then has an **Adult** switch that offers the adult topics, includes adult-only posts,
and adds `allow_adult: true` to copied YAML. `/adult-access?off=1` removes the cookie.

Previews look at the newest 5,000 matching posts; the builder rate-limits each visitor and
caches repeated settings for 30 seconds.

## The classifier

### What it predicts

- **Broad topic**: one of 25 (`us_politics`, `world_news`, `personal_life`, `entertainment`,
  `gaming`, `automated_feeds`, `humor`, `sports`, `music`, `adult_content`, `art`, `society`,
  `technology`, `animals_nature`, `books_writing`, `online_culture`, `health`, `promotion`,
  `history_religion`, `work_education`, `lifestyle`, `economy_finance`, `food_drink`,
  `science`, `unclear`).
- **Subtopic path**: one of the 117 subtopics in `taxonomy/v1.yaml` (plus `unclear`), e.g.
  `sports/american_football`, `technology/ai`. The taxonomy has a description and examples for
  every topic; `taxonomy/v1-equivalences.yaml` lists subtopics treated as interchangeable when
  scoring.
- **Tone**: `informative`, `humorous`, `personal`, `outraged`, `supportive`, or `other`.
- **Signals**, each 0-1, one per question Jev answers:

| signal | question |
|---|---|
| `substance` | how substantive is the post (low effort / some substance / substantive) |
| `news` | is it about a current event or breaking news |
| `promo` | is it mainly self-promotion, an ad, or a request for follows, likes, or reposts |
| `general_interest` | would someone who doesn't know the author find it interesting |
| `sentiment` | overall sentiment, very negative (0) to very positive (1) |
| `critical` | is it negative about, critical of, or mocking the main thing it's about |
| `ad` | is it an advertisement, sales pitch, or deal |
| `engagement_bait` | does it mainly ask for follows, likes, reposts, or replies |
| `spam` | is it a scam, money scheme, automated junk, link farm, or hashtag pile |
| `self_promo` | is the author sharing their own work (art, writing, music, stream, shop, research) |

`critical` is what makes feeds like AI usable: many posts against a topic are sarcastic or
personal rather than angry, so tone alone doesn't catch them.

### The post document

Jev and the model must see exactly the same content, or the model learns to guess from
information it never gets. `internal/postdoc` builds the one canonical rendering; for the model
it's plain text:

```
{post text}
[tags] #tag1 #tag2
[media] 2 images, 1 video
[labels] porn, nudity
[alt] {author's alt text}
[image text] {words read from images without alt text}
[image description] {descriptions of images without alt text}
[link] {domain} | {title} | {description}
[quote] {quoted post text}
```

Empty lines are left out. The format has a version (`pd2`) recorded in every label
configuration and model config; any change to either rendering bumps it, and golden files in
`internal/postdoc/testdata` catch accidental changes. The pipeline renders whichever version the
loaded model expects.

### How it's trained

- **Teacher labels.** The labeler sends each post to Jev with questions: which broad topic,
  then which subtopic within the likeliest broad topics, plus the tone and signal questions.
  Jev answers with probability distributions, and the model is trained to match them (soft
  targets), so it learns Jev's uncertainty as well as its top answer. Requests stay within a
  rate budget (500 a minute by default) shared with other users of the account.
- **Training data.** About 180k posts from 24 fifteen-minute labeling windows spread over
  three days (`config/labeling_windows.yaml`, each hour of the day once); a relabel of the
  ~23k posts Jev was least sure about by a second model, blended with Jev's labels; and 60k
  recent live posts labeled with the full post document and every signal question (40k random,
  20k hard cases), of which 10k random posts are held out for testing and 3k for validation.
- **Model.** ModernBERT-base with mean pooling and four heads (broad, subtopic, signals, tone),
  temperature-scaled on the validation set. Splits are by time, so test scores measure later,
  unseen posts.
- **Current model.** On 10k held-out live posts it agrees with Jev on the broad topic 83.8% of
  the time (top-3: 96.3%), on the exact subtopic 75.7%, and its subtopic is one of Jev's
  plausible answers 90.4% of the time. It's published at
  [haileyok/microblog-topic-classifier-v4](https://huggingface.co/haileyok/microblog-topic-classifier-v4),
  with a model card covering evaluation, calibration, feed thresholds, signals, and cost, and a
  standalone loader (`modeling.py`) for using it outside this repo.

## Training a new model

`docs/training.md` has the details; in short:

```sh
set -a; . ~/.config/topic-feed/env; set +a   # commands run with go run need the env file

# 1. Label posts with Jev (runs for hours; resumable)
go run ./cmd/labeler -live random -source sample -limit 40000 \
  -from 2026-09-29T05:00:00Z -to 2026-09-30T00:00:00Z

# 2. Export a training set: the label configurations to include, and the ones labeled
#    with the full post document (image text, attachments, labels)
make export LABEL_CONFIG=5697660f73fc,6a350cf6d994,ea4d660431d6 FULL_CONTEXT=ea4d660431d6 \
  EXPORT=/data/exports/v4

# 3. Train on the GPU (about 80 minutes; watch at :6006)
make train EXPORT=/data/exports/v4 RUN=v4 EPOCHS=8 \
  TRAIN_ARGS="--live-configs ea4d660431d6 --live-weight 3 --live-test 10000 --live-val 3000"

# 4. Compare with the model in production
cd trainer && uv run python compare_live.py --export /data/exports/v4 --live-configs ea4d660431d6 \
  --production-inputs /data/exports/v4/production_inputs.jsonl.gz --models <current> v4
```

To switch models: point `MODEL_DIR` in `deploy/systemd/topic-feed-classifier.service` at the
new model, `make install-classifier`, rebuild and restart the pipeline, then re-score the posts
feeds can still show with `go run ./cmd/rescore -from-model <old> -before <switch time>`
(it reuses stored image text, so no image fetches or model calls).

Long jobs (labeling, training, rescoring) should run in `tmux`, a systemd unit, or similar, so
a dropped session doesn't stop them. The labeler and rescore both resume where they left off.

## Data

Everything is in ClickHouse, database `topicfeed` (`make ch`):

| table | what |
|---|---|
| `posts` | English posts: text, alt text, link card, quoted text, tags, self-labels, attachments |
| `post_texts` | every post create ingest saw (any language), for quote lookups and ingest checks |
| `likes`, `reposts`, `like_counts_hourly`, `engagement_hourly`, `post_refs` | engagement for ranking |
| `deletions`, `account_status` | deleted posts and deactivated or suspended accounts, applied on every read |
| `mod_labels` | moderation labels and label removals |
| `post_pipeline` | per post: label policy decision, labels, image text and its source, model, topic probabilities, tone, signals |
| `jev_labels`, `jev_requests` | teacher labels and the request log (tokens, latency, status) |
| `feed_interactions` | what Bluesky reports people did with feed posts |
| `ingest_cursor` | stream positions for ingest, modlabels, and the pipeline |
| `image_retry_queue` | posts whose image step failed, waiting for a retry, with attempts and the last error |

Tables that are re-written (`posts`, `post_pipeline`, …) are `ReplacingMergeTree`s: read them
with `FINAL` to get one row per post.

## Operations

- **Backups.** `topic-feed-backup.timer` runs `deploy/backup.sh` nightly at 03:30 UTC: Jev
  labels, request logs, and model files, to the OS drive, kept 14 days. Ingest tables aren't
  backed up; the Jetstream archive can replay them.
- **Costs.** Jev labeling is about $0.11 per 1,000 posts. Image descriptions are capped per day
  (`LLM_DAILY_BUDGET_USD`, $25 in `docker-compose.yml`) and typically run about $10 a day. The
  model itself only costs GPU time.
- **Resilience.** If the AI gateway slows down, image descriptions time out after 15 seconds,
  and when most recent calls fail they pause for a minute (images are then marked
  `unavailable`), so classification keeps up. The pipeline's lag is `pipeline_lag_seconds` on
  its metrics endpoint; posts are normally classified about 15 seconds after they're posted.
- **Failed images are retried.** Posts whose image step failed or was skipped during a pause
  go into `image_retry_queue`; the pipeline retries just those images after about 2 min,
  10 min, 30 min, 2 h, and 6 h, re-classifies the post when anything changes, and gives up
  after 5 failures. Posts keep appearing in feeds meanwhile with their text-only
  classification. `SELECT status, count() FROM image_retry_queue FINAL GROUP BY status` shows
  the queue.
- **Checking ingest.** `go run ./cmd/verifyingest -after <seq> -before <seq>` re-reads a range
  from the Jetstream archive and reports any post missing from `post_texts`.
- **Reports.** Labeling runs, comparison pages, and taxonomy reviews are written under
  `/data/reports` and served at `:8090`.

## Further reading

- `docs/plan.md`: the original planning document (goals, measured facts about the network and
  Jev, decisions, data model, phases). Parts of its architecture have since changed; this README
  describes what runs today.
- `docs/training.md`: exporting, training, and checking a model.
- `docs/feeds-roadmap.md`: planned feed features (interaction dashboard, learning from "show
  less", ranking tuning).
- The header comments of `config/feeds.yaml`, `config/label_policy.yaml`, and each `cmd/*/main.go`.
