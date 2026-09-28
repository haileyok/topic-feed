# Topic-Classified Personalized Feed: Planning Document

**Owner:** Hailey (`haileyok`, `hailey.at`)
**Status:** Planning. No code written yet.
**Working title:** "topic feed". The project name and repo location are still open (see [Open questions](#18-open-questions)).
**Last updated:** 2026-09-28

This document is meant to stand on its own. It collects everything decided, measured, and researched so far, so work can continue on a different machine without the original conversation. Anything marked **(estimate)** has not been measured yet. Anything marked **(measured)** or **(verified)** was checked directly.

---

## Table of contents

1. [Summary](#1-summary)
2. [Goals and non-goals](#2-goals-and-non-goals)
3. [Terms used in this document](#3-terms-used-in-this-document)
4. [Background: measured facts and research](#4-background-measured-facts-and-research)
5. [Decisions made so far](#5-decisions-made-so-far)
6. [Architecture](#6-architecture)
7. [Data model (ClickHouse)](#7-data-model-clickhouse)
8. [Phase 0: repo, local stack, taxonomy, Jev test run](#8-phase-0-repo-local-stack-taxonomy-jev-test-run)
9. [Phase 1: ingest and backfill](#9-phase-1-ingest-and-backfill)
10. [Phase 2: labeling with Jev](#10-phase-2-labeling-with-jev)
11. [Phase 3: training the classifier on the 4090](#11-phase-3-training-the-classifier-on-the-4090)
12. [Phase 4: serving the classifier](#12-phase-4-serving-the-classifier)
13. [Phase 5: regular retraining and drift monitoring](#13-phase-5-regular-retraining-and-drift-monitoring)
14. [Phase 6 (later): the feed itself](#14-phase-6-later-the-feed-itself)
15. [Observability](#15-observability)
16. [Cost and capacity](#16-cost-and-capacity)
17. [Risks and mitigations](#17-risks-and-mitigations)
18. [Open questions](#18-open-questions)
19. [Milestone checklist](#19-milestone-checklist)
- [Appendix A: Jev request examples](#appendix-a-jev-request-examples)
- [Appendix B: Jetstream event shapes](#appendix-b-jetstream-event-shapes)
- [Appendix C: Reference links](#appendix-c-reference-links)

---

## 1. Summary

We're building an experimental Bluesky feed generator that recommends recent posts based on the topics a user has liked.

The core technical work is **topic classification of posts, from broad topics down to subtopics**. We do it in two stages:

1. **Hosted Jev labels posts at high quality.** Jev is TypeSafe's hosted classification model. These labels are the training data, and Jev keeps labeling a sample of new posts as ground truth.
2. **A small classifier trained on those labels classifies every post.** It's fine-tuned on an RTX 4090 and runs on CPU for free, with no rate limits.

The data comes from **Jetstream**: 2–3 days of backfill, then live. We keep **top-level English posts only**, plus likes, reposts, and deletions. Everything goes into **ClickHouse**. For the initial training set, Jev labels **six hours' worth of posts, taken from 24 short windows spread across the backfill days** (~175k posts), not every backfilled post. The first 2,500 labeled posts are reviewed by hand before the rest are labeled. The classifier is retrained regularly as labeled data accumulates.

Jev and the classifier see **exactly the same content for each post**, built by one shared function (the *post document*, §10.2).

The feed itself comes after classification works: interest profiles from likes, candidates under 24h old, and filtering out posts already seen (tracked through `app.bsky.feed.sendInteractions`). It's outlined in Phase 6 but not detailed yet.

## 2. Goals and non-goals

### Goals
- Classify every top-level English post on the network into a **two-level topic taxonomy** (broad topic → subtopic), with calibrated probabilities.
- Produce per-post **ranking signals** alongside topics: substance, time-sensitive news, self-promotion or engagement bait, tone.
- Keep classification **cheap, fast, and independent of hosted Jev's rate limits** by distilling Jev into a small model we own.
- **Retrain regularly** so the classifier keeps up with new events, slang, and topics.
- Eventually serve a **personalized feed**: posts under 24h old (preferably under 12h), matched to each user's liked topics, excluding posts they've already seen.

### Non-goals (for now)
- Replies, and non-English posts. We may revisit later.
- A production deployment. This is an experiment on one or two machines, though the design shouldn't block productionizing later.
- A more granular emergent topic level (e.g. "today's Eagles game") built by clustering embeddings. Optional, after the core works.
- Image or video understanding beyond the alt text, link card text, and quoted text that come with a post.

## 3. Terms used in this document

| Term | Meaning |
|---|---|
| **Jev** | TypeSafe's hosted classification model, called through the "System One" API (`POST /v1/systemone`). You send a *state* (the content) and typed *questions* (choice, score, yes/no); it returns calibrated probabilities. |
| **OpenJev** | An independent open-weights model (27B parameters) that imitates Jev's API. Evaluated, but not used at runtime. See §4.5. |
| **Teacher / student** | Standard model-distillation terms. The *teacher* (hosted Jev) produces labels; the *student* (our small classifier) learns to reproduce them. |
| **Taxonomy** | Our versioned list of broad topics, each with subtopics, all with descriptions. It lives in the repo as YAML. |
| **Broad topic / subtopic** | Level 1 and level 2 of the taxonomy (e.g. `sports` → `american_football`). |
| **Ranking signals** | Extra per-post judgments that aren't topics (substance, newsworthiness, self-promotion, tone), used later for ranking. |
| **Top-level post** | An `app.bsky.feed.post` record with no `reply` field. |
| **Backfill** | Reading historical Jetstream events, starting from a point in the past. We backfill 2–3 days. |
| **Labeling windows** | The 24 short time windows (15 minutes each, six hours in total) spread across the backfill days. Jev labels every post in them, and they form the initial training set. See §10.1. |
| **Post document** | The one canonical representation of a post's content (text, alt text, link card, quoted text, tags). Jev's state and the student's input string are both rendered from it by the same code. See §10.2. |
| **AGW** | Bluesky's internal AI gateway at `https://agw.noclues.net`. It proxies Jev and LLMs using a personal AGW key. |

## 4. Background: measured facts and research

### 4.1 Network volume (measured)

Sampled from the public Jetstream on 2026-09-28: one 30-second sample and one 60-second sample. These are single samples at one time of day. **Re-measure across a full day** before relying on them for capacity planning.

| Slice | Rate | Per day | Share of posts |
|---|---|---|---|
| All posts (creates) | ~30–32/s | ~2.6–2.75M | 100% |
| Likes (creates) | ~200/s | ~17M | n/a |
| Top-level posts | ~18/s | ~1.54M | ~59% |
| **Top-level + English (`langs` tag)** | **~8/s** | **~0.69M** | **~26%** |
| Direct replies to a thread's first post, English (for reference) | ~5/s | ~0.44M | ~17% |

- Average text length of top-level English posts: **~109 bytes** (~25–35 tokens).
- **Six hours** of top-level English posts (the labeling windows) ≈ **170–180k posts**. Two days ≈ 1.4M; three days ≈ 2.1M.

### 4.2 Jev: TypeSafe System One (verified from docs, 2026-09)

**Endpoint and shape**
- `POST /v1/systemone` with body `{model, state, questions}`. Response: `{model, answers, usage}`.
- `GET /v1/models` lists model names.
- Question types:
  - `noul`: returns the probability of yes (0–1). Has no confidence field.
  - `choice`: needs `criteria` mapping option → description (description may be null). Returns `choice`, `probabilities` over all options, and `confidence`.
  - `score`: needs ordered level descriptions. Returns `score` (a position along the levels, can fall between two), `probabilities`, `legend`, and `confidence`.
- Question IDs are the map keys. **They are not sent to the model**, so the instructions must contain the complete question.
- The response `model` field reports the versioned model that actually answered (e.g. `jev-1.13.0`).

**Model, limits, price**

| | |
|---|---|
| Current model | `jev-1.13.0` (aliases `jev-latest` and `jev-preview` both point to it). **Pin the versioned ID.** Aliases move when new releases ship. |
| Price | **$0.042 per million input tokens.** Output tokens are free. |
| Rate limits | **250,000 tokens/s** and **1,200 requests/min**. The docs say limits "are adjusting dynamically" and can change without notice. |
| Context | **64k tokens per request** (state + all questions). **32k for the state plus the single longest question.** |
| Choice options | Up to **255** per question. |
| Score levels | 2 to 10. |
| Input | Text only: a string, JSON object, or array. English is the primary training language. |

**Guidance from the docs that shapes our design**
- **The state is read once, and every question is evaluated against it in parallel.** Extra questions cost only their own tokens and barely add latency. The docs strongly recommend putting every question about a state into one request, including questions you might not need (their "speculative fan-out" pattern). The parallel-questions cookbook measured that batching 13 questions into one request did not change any answer compared with asking them one per request.
- **Refer to parts of the state with backticked paths** in the instructions, e.g. `` `posts[3].text` ``. This lets many posts share one state with one question per post. TypeSafe's docs use this pattern (an `items` array with one question per `items[i]`) in their counting example. *Verified live:* a two-post request with one choice question per post classified both correctly.
- **Accuracy drops as the state fills with material unrelated to the question.** The Jev 1.13 known-weaknesses page says: "Accuracy falls as the state grows with content unrelated to the decision… Jev suffers from context rot," and recommends "Filter first; send only what the question needs." For a question about one post, the other posts in the batch count as unrelated.
- **Batch size: the docs give no number** (rechecked 2026-09-28 across the state, models, fan-out, jaggedness, and cookbook pages). What they do show:
  - TypeSafe's own per-item cookbooks send **one item per request**. Classifying RAG passages: "Nothing batches passages into one request, because each question is about one pair." Re-ranking: "one request per candidate · no request sees another" (1,200 calls used 1.54M input tokens, ~1,280 tokens per call).
  - The examples that put many items in one state (line-by-line search: 218 lines, ~44k characters) ask one question about the whole collection. None of them classifies each item separately.
  - So the docs lean toward **small batches**, and we treat batching as something to justify with the Phase 0 test, not as the default. Because each post's questions dominate its tokens, batching saves requests, not tokens (§16).
- **Jev reads instructions literally.** Write exact conditions, and put boundary cases in the option descriptions.
- **Dependent questions need a second request.** If question B's options depend on question A's answer (subtopic depends on broad topic), that takes two requests.
- **Hierarchical classification cookbook:** keep the top **K=3** paths at each level instead of only the best one, and score each path by the geometric mean of its step probabilities. In their examples this got 4 of 4 correct vs 2 of 4 for taking only the best step.
- Choice probabilities always sum to 1, so include an explicit "none of these / other" option wherever the list might not cover every input.

### 4.3 Accessing Jev through AGW (verified)

- AGW proxies Jev's API unchanged at **`https://agw.noclues.net/v1/systemone`** and swaps in TypeSafe credentials on the way out.
- **Authentication:** a personal AGW key works as either `Authorization: Bearer <key>` or `x-agw-key: <key>`. Both were verified with a live request.
- The same gateway also serves LLMs through OpenAI- and Anthropic-compatible endpoints with the same key. We use these for drafting the taxonomy (Phase 0).
- **Shared account:** AGW uses **Bluesky's production TypeSafe account**, which Attie also uses for its Jev checks and feed labeling. **Agreed budget: we may use about 1/3 to 1/2 of the account's rate limits** (~400–600 requests/min, ~80–125k tokens/s). Make this configurable.
- Set an attribution header on every request (e.g. `X-Client: topic-feed`) so our traffic is identifiable.
- **Never commit the AGW key.** It lives in `~/.config/topic-feed/env` (mode 600, outside the repo) as `TYPESAFE_API_KEY`, which the Go client reads by default. *Verified 2026-09-28:* a one-question request through AGW returned `jev-1.13.0`'s answer for 395 input tokens.

### 4.4 Go client for Jev

Unofficial Go client: **`github.com/haileyok/typesafe-client/go`**, package `typesafe`. It uses only the standard library and needs Go 1.22+.

```go
import typesafe "github.com/haileyok/typesafe-client/go"

client, err := typesafe.NewClient(
    typesafe.WithAPIKey(os.Getenv("TYPESAFE_API_KEY")), // AGW key
    typesafe.WithBaseURL("https://agw.noclues.net"),
    typesafe.WithModel("jev-1.13.0"),                    // pin the version
    typesafe.WithTimeout(60*time.Second),                // per attempt; the 10s default is too short for big batches
    typesafe.WithHeader("X-Client", "topic-feed"),
)

resp, err := client.SystemOne(ctx, typesafe.Request{
    State: map[string]any{"posts": posts},
    Questions: typesafe.Questions{
        "p0_broad": typesafe.Choice("Which broad topic best describes `posts[0]`?",
            typesafe.Opt("sports", "Games, athletes, teams, leagues. Not sports-betting ads."),
            typesafe.Opt("other", "No clear topic: greetings, personal chatter, unclear."),
        ),
        "p0_news": typesafe.Noul("Is `posts[0]` about a current event or breaking news?"),
    },
})
ans, _ := resp.Choice("p0_broad") // ans.Choice, ans.Probabilities, ans.Confidence, ans.Ranked()
```

Client behavior worth knowing:
- **Retries:** by default up to 2 retries with backoff (500ms, doubling, capped at 5s) on 408, 429, 5xx, and connection errors. It honors `Retry-After` up to 60s and has a 30s total budget. Configure with `WithRetryPolicy`, or per request with `RequestRetryPolicy`.
- **Every question gets an answer:** a successful response is guaranteed to contain an answer for every question ID asked. Otherwise the client returns a validation error.
- **Other constructors:** `ChoiceNames` (options with no descriptions), `Score(instr, levels...)`, `Noul(instr).WithCriteria(yes, no)`, `Bare(name)`. `Choice` keeps options in the order given.
- **Errors:** match with `errors.Is(err, typesafe.ErrRateLimit)` and similar. `APIError.RequestID` holds TypeSafe's request ID. On successful responses the ID arrives in the `x-typesafe-request-id` header (verified through AGW); check whether the client exposes it on success, since `jev_labels.request_ids` and `jev_requests.request_id` need it.

### 4.5 OpenJev (evaluated, not chosen for runtime)

- `huggingface.co/openjev/openjev`: an independent open-weights project, **not affiliated with TypeSafe**. It's a **27B dense model** fine-tuned from Qwen3.8-27B, and it's the only size available. Its helper server exposes the same `/v1/systemone` API, so our Go client works against it unchanged.
- **License: CC BY-NC 4.0 (non-commercial).** Fine for experiments. Production use would need a commercial license from the OpenJev project.
- **Accuracy (the OpenJev authors' own benchmark: 10k held-out questions, 34 public datasets):**
  - Overall: hosted Jev 85.4%, OpenJev 84.0%.
  - **Intent / routing / topic: tied at 92.8%.**
  - Sentiment 82.1% vs Jev's 81.5%; spam/hate 80.4% vs 81.2%.
  - Jev leads clearly on commonsense (88.3 vs 85.8) and facts/science (89.0 vs 82.5).
- **Limits:** 52 options per single pass (more options take multiple passes), 16k-token prompts.
- **Builds:** 16-bit (54GB), FP8 (29GB, matches the 16-bit model), GGUF Q4_K_M (16.5GB, 82.8% vs 83.2% on their check), MLX.
- **How it scores:** one forward pass per question, reading the scores of the option letters. The prompt layout is state first, then the question, then the options, so every question pays again for its option list.
- **Why it's not the plan:** it needs datacenter-GPU throughput to keep up (see §4.6). It's kept as a possible fallback teacher, or for comparison.

### 4.6 Hardware evaluated for running a 27B classifier (for the record)

Our workload only reads each prompt and produces one token, so **compute limits it, not memory**. Measured prompt-processing speed for Qwen3.8-27B-class models:

| Hardware | Tokens/s | Verdict for all top-level English posts |
|---|---|---|
| Framework Desktop (Ryzen AI Max+ 395, 128GB) | ~240–290 (measured by others) | Too slow, even for the leanest setup |
| RTX 4090 (4-bit GGUF) | ~1,500–1,860 (measured by others) | Fits only a lean single-pass setup |
| RTX 5090 | ~2,300+ (measured by others) | Similar to the 4090, somewhat better |
| H100 (FP8) | ~8k+ (OpenJev's published numbers) | Comfortable |

The full-quality setup (§10) needs ~16k tokens/s, which is why **we distill into a small model instead of running a 27B model**. A ~150M-parameter student needs a tiny fraction of that compute and runs on CPU.

### 4.7 Jetstream

Docs: https://bsky.network/docs/jetstream/ and https://bsky.network/docs/jetstream-replay/ (read 2026-09-28).

- **We use Jetstream v2**, Bluesky's public instances: `wss://jetstream.us-east.bsky.network` (or `jetstream.us-west`). Replay (history) is only available on v2. The v1 instances (`jetstream1.us-west`, `jetstream2.us-east`) are live-only.
- **Live tail:** a WebSocket to `/xrpc/network.bsky.jetstream.subscribeEvents` with subprotocol `xrpc.v1.json`. No authentication and not metered. Server-side filters: `collections` (repeatable, up to 100, `app.bsky.feed.*`-style wildcards allowed), `dids` (up to 10,000), and `kinds` (`commit`, `identity`, `account`, `sync`). A collection filter applies to commits only; account, identity, and sync events always come through, and we need the account ones.
- **Replay (our backfill):** the Go SDK (`github.com/bluesky-social/jetstream`) does it in one loop: `jetstream.Subscribe(host, jetstream.WithAPIKey(key), jetstream.WithCollections(...), jetstream.WithAfterSeq(seq))`. It plans the history over HTTP (`planSnapshot`), downloads archive blocks in parallel, then connects the live socket at the archive tip, removing duplicates at the seam. Persist `batch.LastCursor()` to resume.
  - The HTTP side needs an **API key** (`Authorization: Bearer`, which the SDK adds). Ours is `JETSTREAM_API_KEY` in `~/.config/topic-feed/env`. *Verified 2026-09-28:* `listSegments` returned 200 with our key.
  - **Metered by bytes downloaded** (compressed). Over the limit returns 429 with `Retry-After`; downloads resume with HTTP `Range` requests, so nothing is charged twice.
  - **The archive goes back to 2026-08-04** (first segment's time), so 72h of history is well within reach. Segments are ~256MB compressed; the segment list is paginated with a `cursor`.
  - Replay starts from a **sequence number** (`afterSeq`), not a time. To start 72h back, find the first segment whose time range (`minWitnessedAt`/`maxWitnessedAt` from `listSegments`) covers that point and use its `minSeq`. (The live socket alone also accepts a unix-microsecond `cursor`, but only within its 36h window.)
  - If a backfill takes longer than the live socket's **36-hour** lookback, the live connect fails with a 400 carrying the new floor, and the SDK re-plans from its last position. No data is skipped.
- **Delivery:** at least once, in `seq` order per account. Cursors are inclusive. Writes must be idempotent (key on the record's `at://` URI).
- **Folding:** replay delivers creates, updates, and deletes as they happened. An `account` event with `active: false` and `status: "deleted"`, or a `sync` divergence marker, removes **all** of that account's records.
- Event shapes: see [Appendix B](#appendix-b-jetstream-event-shapes). v2 wraps each event in an envelope and uses `seq` plus an RFC 3339 `time`, not v1's `time_us`.

### 4.8 Feed generator protocol pieces (verified against lexicons)

- **`app.bsky.feed.getFeedSkeleton`**
  - Params: `feed` (at-uri), `limit` (1–100, default 50), `cursor`.
  - Output: `feed[]` of `{post, reason?, feedContext?}`, plus `cursor` and `reqId` (max 100 chars, passed back with interactions).
  - The request carries a service-auth JWT that identifies the viewer.
- **`app.bsky.feed.sendInteractions`**
  - Input: `{feed?, interactions[]}`. Each interaction is `{item (at-uri), event, feedContext? (≤2000 chars), reqId? (≤100 chars)}`.
  - Events: `requestLess`, `requestMore`, `clickthroughItem`, `clickthroughAuthor`, `clickthroughReposter`, `clickthroughEmbed`, `interactionSeen`, `interactionLike`, `interactionRepost`, `interactionReply`, `interactionQuote`, `interactionShare`. Each is prefixed `app.bsky.feed.defs#`.
- **Generator record (`app.bsky.feed.generator`)**
  - Required: `did`, `displayName`, `createdAt`.
  - Set **`acceptsInteractions: true`** so the app sends interactions to us.
  - Also: `contentMode` (unspecified or video), `description`, `avatar`, `labels`.
- **Moderation:** feed generators return only post URIs. The AppView fills in the posts and applies the viewer's blocks, mutes, and moderation settings.

## 5. Decisions made so far

| # | Decision | Why |
|---|---|---|
| D1 | **Top-level English posts only** (no replies) | Cuts volume ~4× and matches what a feed shows. Checked by the `langs` tag **and** a local language detector. |
| D2 | **Jev labels six hours of posts, taken from 24 fifteen-minute windows spread over 2–3 days**; ingest keeps *everything* from those days | Covers every hour of the day and several days of news at ~175k posts (~$15). Variety across time matters more to the student than raw volume. The student classifies everything else, including the <24h feed candidates. Interest profiles start with 2–3 days of likes and grow as live data accumulates. |
| D3 | **Hosted Jev is the teacher**, through AGW | Best available quality. Cost is small (~$15 for the labeling windows at full quality). |
| D4 | **Spend freely on labeling quality**: option descriptions, top-3 broad-topic candidates, ranking questions | Money isn't the constraint. Label quality caps student quality. |
| D5 | **Rate budget: ~1/3–1/2 of Jev's limits** | Shared account with Attie. Make it configurable. |
| D6 | **Small batches of posts per Jev request** (possibly one). Batch size picked by the Phase 0 test and never above what fits the 64k context limit | TypeSafe's docs give no batch size, and their own per-item cookbooks send one item per request. Other posts in the state count as unrelated content, which the docs say costs accuracy. Batching saves requests, not tokens, and at our volumes even batch size 1 fits the rate budget (§16). |
| D7 | **Distill into a small student model trained on the RTX 4090**; retrain regularly | Removes per-post cost and rate limits for live traffic, and runs on CPU. |
| D8 | **Train on Jev's full probability distributions**, not only its top choice | Keeps Jev's uncertainty. The standard, stronger form of distillation. |
| D9 | **Evaluate on later labeling windows** than the training windows | Tests whether the model generalizes to news it hasn't seen. A random holdout from the same windows would flatter it. |
| D10 | **ClickHouse** is the main store; **Redis** is added only in the feed phase | ClickHouse handles large appends, time-window queries, and aggregations like likes × topics well. Redis holds per-user serving state. |
| D11 | **Go** for ingest, labeling, export, orchestration, and the feed server. **Python** for training and classifier inference | Go matches existing Bluesky tooling (indigo, the Jev client). PyTorch has no practical Go equivalent. |
| D12 | **Stop after the first 2,500 labeled posts for a human review** before labeling the rest | Checks the taxonomy and the labels on real posts for ~$0.20 before committing to the full set. |
| D13 | **Honor deletions** everywhere, including exports and candidates | Required for any Bluesky data product. |
| D14 | **One shared post document** renders both Jev's state and the student's input | The student can only learn what it sees. If Jev sees quoted text or tags that the student doesn't, the student learns to guess from missing information. |

## 6. Architecture

```
                         ┌──────────────────── one machine: Threadripper 7960X + RTX 4090 ────────────────────┐
 Jetstream ──(posts, likes, reposts, deletes, account events)──► ingest (Go)                                 │
                         │                                          │                                         │
                         │                                          ▼                                         │
                         │     hosted Jev ◄──► labeler (Go) ◄──► ClickHouse ◄──► classify (Go) worker        │
                         │     via AGW          (teacher labels)     │  ▲                 │                   │
                         │                                          │  │ predictions     │ HTTP              │
                         │                   export (Go) → Parquet  │  │                 ▼                   │
                         │                                          │  └────────── classifier service       │
                         │                                          ▼               (Python, ONNX, CPU)     │
                         │                     train (PyTorch, GPU) → evaluate → promote ──► models/current  │
                         └─────────────────────────────────────────────────────────────────────────────────────┘
```

### Components

| Component | Language | Responsibility |
|---|---|---|
| `ingest` | Go | Consume Jetstream (backfill, then live); filter; resolve quoted-post text; write posts, likes, reposts, deletions, and account status to ClickHouse; save the stream position. |
| `labeler` | Go | Pick posts that need Jev labels (labeling windows, random sample, uncertain), build batched requests from post documents, call Jev under the rate budget, write labels and cost records. |
| `export` | Go | Select one training label per post, render each post's student input from its post document, and write Parquet for the trainer. |
| `classify` worker | Go | Render post documents for posts without a current-model prediction, send them to the classifier service, write predictions. Re-score the last 24h when a new model is promoted. |
| `internal/postdoc` | Go | The post document: builds it from a `posts` row and renders it as Jev state JSON and as the student input string. The only place either format is produced. |
| classifier service | Python | Load the current ONNX model and serve `POST /classify` on already-rendered input strings. Hot-reload when a new model is published. |
| `trainer` | Python | Train → evaluate → promote on exported Parquet. Runs on the GPU, on a schedule. |
| taxonomy tool | Go or Python | Sample posts, ask an LLM (via AGW) to draft the taxonomy, and output YAML for human review. |
| `feedgen` (Phase 6) | Go | The feed-generator endpoints, interest profiles, ranking, and seen tracking. |

Suggested repo layout: `cmd/{ingest,labeler,export,classify,feedgen}`, `internal/postdoc`, `internal/...`, `taxonomy/v1.yaml`, `config/labeling_windows.yaml`, `trainer/` (a Python project managed with `uv`), `serving/` (Python), `deploy/docker-compose.yml`, `schema/*.sql`.

### Deployment: one machine (decided 2026-09-28)

Everything runs on one machine, `penguin` (inspected 2026-09-28):

| Part | What it has | Enough? |
|---|---|---|
| CPU | AMD Ryzen Threadripper 7960X, 24 cores / 48 threads, up to 5.36GHz, AVX-512 with VNNI and BF16 | Yes. ClickHouse, the Go services, and CPU inference together need a fraction of it. AVX-512 VNNI speeds up int8 ONNX inference. |
| Memory | 256GB DDR5-5200 (4 × 64GB, all four channels filled), ECC reporting present | Yes. ClickHouse at this scale wants ~32–64GB. |
| GPU | NVIDIA RTX 4090, 24GB, PCIe 4.0 x16, driver 610.57 (CUDA 13.3) | Yes. ModernBERT-base and -large fine-tuning fit comfortably, and so does OpenJev's 4-bit build (16.5GB) for the optional comparison. |
| Storage | 2 × 8TB WD_BLACK SN850X NVMe. `nvme0n1` holds the OS (6.9TB free); **`nvme1n1` is blank: no partition, not mounted** | Yes, by a wide margin (the plan needs ~100GB/month). |
| Network | 1Gb Ethernet (`eno1`) | Yes. Live Jetstream is well under 1MB/s; the 3-day backfill is tens of GB and takes minutes to an hour. |
| Software | Ubuntu 26.04.1, Go 1.26.7, uv 0.12.17, system Python 3.14.4, Docker 29.8.1 (rootless) with Compose v5.5.1 | Yes. PyTorch wheels bundle the CUDA runtime, so no CUDA toolkit (`nvcc`) is needed. |

**Setup results (2026-09-28):**
- `/data`: the second NVMe, XFS, mounted by UUID with `nofail`. Holds `clickhouse/`, `backups/`, `exports/`, `models/`.
- ClickHouse `26.8.13.2` (an LTS release) runs from `deploy/docker-compose.yml`, bound to localhost, with data on `/data/clickhouse` and memory capped at 64GB. Under rootless Docker its files are owned by uid 100100 on the host (the container's `clickhouse` user).
- PyTorch `2.14.0+cu132` in `trainer/` (Python 3.12.14) sees the RTX 4090. Under load: PCIe Gen 4 x16, 61°C, ~274W, ~161 TFLOPS bf16 matmul, ~27GB/s host-to-GPU copies.

How the work is divided:
- **GPU:** training, plus bulk re-scoring when a new model is promoted (0.7M posts takes minutes on the GPU vs up to an hour on CPU). Training and bulk re-scoring never run at the same time.
- **CPU:** ClickHouse, the Go services, and the classifier service for live posts (~8 posts/s).
- **Model files** move through a local directory (`models/<version>/`, with `models/current` as a symlink). No rsync.

Setup needed (part of M0.1):
1. **Format and mount the second NVMe** (e.g. ext4 or XFS at `/data`) and put ClickHouse's data directory there, so database I/O doesn't share a disk with the OS and Docker.
2. **ClickHouse:** run the container with its data bind-mounted to `/data/clickhouse`. Docker here is rootless, so raise the container's open-file limit (`ulimits: nofile`) and expect harmless warnings about missing capabilities. If rootless networking ever limits insert throughput, running ClickHouse natively (the official `.deb`) is the fallback.
3. **Python for training:** a `uv` project pinned to Python 3.12 (`uv python install 3.12`), with a CUDA build of PyTorch. The system Python 3.14 is too new for some ML wheels.
4. **Backups:** one machine means one point of failure. Nightly, copy `jev_labels`, the taxonomy, the labeling-window file, and promoted models to the OS disk (ClickHouse `BACKUP TABLE … TO Disk(…)`), and occasionally off the machine. Jev labels are cheap to redo (~$15) but slow to redo within the rate budget, and their history is the long-term training set.
5. **Check under load:** during the first training run, confirm the GPU link reports PCIe Gen 4 x16 (it idles at Gen 1 to save power, which is normal) and watch GPU temperature.

## 7. Data model (ClickHouse)

The DDL below is a starting point. Note these choices:
- **Deduplication:** Jetstream can deliver an event more than once, so tables use `ReplacingMergeTree`, and queries use `FINAL` or `argMax` where exact counts matter.
- **Time columns:** `indexed_at` is the Jetstream event time. We use it (not the client-supplied `createdAt`, which can be wrong or in the future) for freshness, partitioning, and time-based splits.

```sql
-- Top-level English posts (plus a few rejected posts for debugging the language filter, optional).
-- Every column the post document uses is stored here, so Jev and the student always see the same content.
CREATE TABLE posts
(
    uri               String,                       -- at://did/app.bsky.feed.post/rkey
    did               String,
    rkey              String,
    cid               String,
    created_at        DateTime64(3, 'UTC'),         -- client-supplied createdAt, stored but not trusted
    indexed_at        DateTime64(6, 'UTC'),         -- Jetstream event time
    text              String,
    langs             Array(LowCardinality(String)),
    detected_lang     LowCardinality(String),       -- from the local language detector
    embed_type        LowCardinality(String),       -- none|images|video|external|record|recordWithMedia
    media_alts        Array(String),                -- alt text of images and video
    link_uri          String,
    link_domain       LowCardinality(String),
    link_title        String,
    link_description  String,
    quote_uri         String,
    quote_text        String,                       -- text of the quoted post, resolved at ingest (§9.1); empty if unavailable
    tags              Array(String),                -- hashtags from facets and the tags field
    link_domains      Array(LowCardinality(String)),-- domains of links in facets
    has_labels        UInt8                         -- self-labels present (e.g. adult content)
)
ENGINE = ReplacingMergeTree(indexed_at)
PARTITION BY toDate(indexed_at)
ORDER BY uri;

-- Text of every post on the network (any language, replies included), kept briefly
-- so quoted posts can be resolved without calling the AppView. ~300MB/day raw.
CREATE TABLE post_texts
(
    uri         String,
    text        String,
    indexed_at  DateTime64(6, 'UTC')
)
ENGINE = ReplacingMergeTree(indexed_at)
PARTITION BY toDate(indexed_at)
ORDER BY uri
TTL toDateTime(indexed_at) + INTERVAL 7 DAY;

CREATE TABLE likes
(
    actor_did    String,
    rkey         String,
    subject_uri  String,
    created_at   DateTime64(3, 'UTC'),
    indexed_at   DateTime64(6, 'UTC')
)
ENGINE = ReplacingMergeTree(indexed_at)
PARTITION BY toDate(indexed_at)
ORDER BY (actor_did, rkey)
TTL toDateTime(indexed_at) + INTERVAL 14 DAY;

-- Same shape as likes, for app.bsky.feed.repost.
CREATE TABLE reposts AS likes;

-- Like counts per post per hour, for engagement and velocity signals.
CREATE TABLE like_counts_hourly
(
    subject_uri  String,
    hour         DateTime('UTC'),
    likes        UInt64
)
ENGINE = SummingMergeTree
PARTITION BY toDate(hour)
ORDER BY (subject_uri, hour)
TTL hour + INTERVAL 14 DAY;

CREATE MATERIALIZED VIEW like_counts_hourly_mv TO like_counts_hourly AS
SELECT subject_uri, toStartOfHour(indexed_at) AS hour, count() AS likes
FROM likes GROUP BY subject_uri, hour;

-- Deletes of posts, likes, and reposts. Unlikes arrive as deletes on app.bsky.feed.like.
CREATE TABLE deletions
(
    did         String,
    collection  LowCardinality(String),
    rkey        String,
    uri         String,
    indexed_at  DateTime64(6, 'UTC')
)
ENGINE = ReplacingMergeTree(indexed_at)
ORDER BY (collection, did, rkey);

-- Latest known account status (deactivated, taken down, etc.).
CREATE TABLE account_status
(
    did         String,
    active      UInt8,
    status      LowCardinality(String),
    indexed_at  DateTime64(6, 'UTC')
)
ENGINE = ReplacingMergeTree(indexed_at)
ORDER BY did;

-- Teacher labels from hosted Jev. Kept forever; this is the training set.
-- A post can have several rows (one per taxonomy_version × label_config). The export (§11.2)
-- picks exactly one training label per post and never uses `eval` rows.
CREATE TABLE jev_labels
(
    uri               String,
    taxonomy_version  LowCardinality(String),       -- e.g. "v1"
    label_config      LowCardinality(String),       -- hash of the question set, post document version, batch size, top-K, etc.
    jev_model         LowCardinality(String),       -- versioned model that answered, e.g. "jev-1.13.0"
    source            LowCardinality(String),       -- window|sample|uncertain|eval
    batch_size        UInt16,
    labeled_at        DateTime64(3, 'UTC'),
    broad_probs       Map(LowCardinality(String), Float32),   -- full distribution over broad topics
    broad_confidence  Float32,
    sub_probs         Map(LowCardinality(String), Float32),   -- key "broad/sub": P(sub | broad) for each broad topic asked
    path_scores       Map(LowCardinality(String), Float32),   -- key "broad/sub": geometric-mean path score
    signals           Map(LowCardinality(String), Float32),   -- ranking signals, normalized to 0..1
    request_ids       Array(String)                           -- TypeSafe request IDs, for debugging
)
ENGINE = ReplacingMergeTree(labeled_at)
ORDER BY (taxonomy_version, label_config, uri);

-- One row per Jev request, for cost and throughput tracking.
CREATE TABLE jev_requests
(
    request_id     String,
    ts             DateTime64(3, 'UTC'),
    pass           LowCardinality(String),          -- broad|sub
    n_posts        UInt16,
    n_questions    UInt16,
    input_tokens   UInt32,
    latency_ms     UInt32,
    status         LowCardinality(String),          -- ok|rate_limited|error
    error          String
)
ENGINE = MergeTree
PARTITION BY toDate(ts)
ORDER BY ts;

-- Student predictions.
CREATE TABLE predictions
(
    uri               String,
    model_version     LowCardinality(String),
    taxonomy_version  LowCardinality(String),
    predicted_at      DateTime64(3, 'UTC'),
    broad_probs       Map(LowCardinality(String), Float32),
    sub_probs         Map(LowCardinality(String), Float32),   -- key "broad/sub": joint probability
    signals           Map(LowCardinality(String), Float32),
    broad_confidence  Float32
)
ENGINE = ReplacingMergeTree(predicted_at)
PARTITION BY toDate(predicted_at)
ORDER BY (model_version, uri)
TTL toDateTime(predicted_at) + INTERVAL 30 DAY;

-- Model versions and their evaluation results.
CREATE TABLE models
(
    model_version     String,
    taxonomy_version  LowCardinality(String),
    trained_at        DateTime('UTC'),
    data_from         DateTime('UTC'),
    data_to           DateTime('UTC'),
    n_train           UInt32,
    metrics           String,                        -- JSON
    promoted          UInt8,
    promoted_at       Nullable(DateTime('UTC'))
)
ENGINE = ReplacingMergeTree(trained_at)
ORDER BY model_version;

-- Jetstream stream position, one row per consumer.
CREATE TABLE ingest_cursor
(
    consumer    String,
    cursor      UInt64,
    updated_at  DateTime64(3, 'UTC')
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY consumer;
```

**Rough storage** (estimates): posts ~0.4GB/day raw; `post_texts` ~0.3GB/day raw (7-day TTL); likes ~2.5GB/day raw, much less after ClickHouse compression. Months of data fit in well under 100GB.

**Handling deletions:**
- Every read path (candidates, exports, labeling) excludes URIs in `deletions` and authors whose `account_status` is inactive.
- A daily job physically removes deleted posts' text (`ALTER TABLE posts DELETE WHERE uri IN (…)`, or a lightweight delete), and blanks `quote_text` on posts whose quoted post was deleted.
- Retraining on a sliding window ages deleted content out of the model over time.

## 8. Phase 0: repo, local stack, taxonomy, Jev test run

### 8.1 Repo and stack
- Create the repo (name and location open). Per Hailey's conventions, do the work in a git worktree at `~/worktrees/<repo>/<slug>` on a branch named `hailey/<slug>`.
- `deploy/docker-compose.yml`: ClickHouse (a current stable server image), with a mounted volume. Redis comes later.
- `schema/*.sql` holds the DDL from §7, plus a small migration runner or a `make schema` target.
- Configuration comes from environment variables. Secrets live in `~/.config/topic-feed/env` (mode 600, outside the repo), which the Makefile passes to docker compose: `TYPESAFE_API_KEY` (the AGW key), `JETSTREAM_API_KEY`, and `CLICKHOUSE_PASSWORD`. Other keys: `TYPESAFE_BASE_URL=https://agw.noclues.net`, `JEV_MODEL=jev-1.13.0`, `CLICKHOUSE_DSN`, `JETSTREAM_URL`, the rate budget (`JEV_MAX_RPM`, `JEV_MAX_TPS`), and the batch size (`JEV_BATCH_SIZE`, set from the Phase 0 test).
- `internal/postdoc` with golden-file tests: a fixed set of example posts (plain text, images, video, link card, quote, quote with media, tags, empty text) and their expected Jev JSON and student string. Any change to either rendering bumps the post document version, which is part of `label_config` and of each model's `config.json`.

### 8.2 Drafting the taxonomy (v1)
1. **Sample** ~5–10k top-level English posts spread evenly across the hours of a day, taken from Jetstream directly or from ClickHouse once Phase 1 has data.
2. **Draft with an LLM through AGW.** Ask it to cluster the sample into **~20–30 broad topics**, each with **~5–15 subtopics**. Every topic needs:
   - a short ID (`snake_case`)
   - a display name
   - a one- to two-sentence description that includes **what it does *not* include**, since Jev reads descriptions literally
   - 2–3 example posts from the sample
3. **Required topics:**
   - A broad topic for **personal and everyday life** (greetings, "good morning", life updates). Much of Bluesky is this.
   - An explicit **"unclear / no topic"** option at the broad level.
   - An **"other within this topic"** subtopic under every broad topic.
4. **Human review (Hailey):** merge, split, rename, and tighten descriptions.
5. **Save as `taxonomy/v1.yaml`:**
   ```yaml
   version: v1
   broad:
     - id: sports
       name: Sports
       description: Games, athletes, teams, leagues, and sports media. Not sports-betting promotions (see promotions).
       subtopics:
         - id: american_football
           name: American football
           description: NFL, college football, fantasy football.
         - id: other
           name: Other sports
           description: Sports content that fits no other subtopic.
   ```
6. **Limits to respect:** at most 255 options per question for hosted Jev (52 if OpenJev is ever used), so keep each level well under 52 to stay portable. Keep descriptions tight. Every question repeats its option list, which is the main cost per post, and it also caps how many posts fit in one request (§8.3).

### 8.3 Jev test run (sets batch size and top-K, and confirms costs)

Use ~2,000 posts spread over time, labeled with source `eval`. Each run costs about **$0.20** (~2,000 posts × ~2k tokens ≈ 4M tokens); the whole test is ~$1–2.

**First, compute the largest batch that fits.** Every post in a request adds its own questions, and the whole request must stay under 64k tokens:
- Measure the question tokens per post from the real taxonomy (pass 1: the broad-topic question plus the ranking questions; pass 2: up to 3 subtopic questions). Rough estimate for pass 1: ~1,000–1,100 tokens per post with ~25 broad topics at ~35 tokens per description.
- `max_batch = floor(0.8 × 64,000 / (question tokens per post + ~40 tokens of post content))`. The 0.8 leaves room for token-estimate error. With the rough estimate above that's **~45 posts** for pass 1.
- Never test or run above `max_batch`. Recompute it whenever the taxonomy or question set changes.

Then run these:

1. **Reference:** each post alone (batch size 1), full question set. Run it **twice** to measure Jev's run-to-run noise, so we don't chase noise.
2. **Batch sizes:** 5, 10, 25, and `max_batch` (if it's above 25), same questions. Shuffle posts randomly within each batch so neighbors are unrelated to each other and to time.
3. **Metrics per batch size, measured against the batch-size-1 reference:**
   - Top-1 agreement on broad topic and on subtopic.
   - Mean absolute difference in probabilities.
   - Change in average confidence.
   - Agreement by position in the batch (first posts vs last posts), to catch position effects.
   - Real tokens per post and time per request.
4. **Top-3 vs top-1 broad topic** for the subtopic pass: how often the best final path goes through the 2nd or 3rd broad candidate, and whether spot-checks show those cases were right.
5. **Ranking questions:** check each one for spread. A question that's nearly always 0 or 1 isn't useful. Drop questions that don't discriminate.
6. **Optional, OpenJev comparison:** run the same posts through OpenJev (the Q4 GGUF build fits on the 4090; at ~1,500–1,800 tokens/s, 2,000 posts take roughly an hour or two). Measure agreement with Jev, and have an LLM or a human review the disagreements.

**Decision rule for batch size:** use the largest batch size whose broad-topic agreement with the reference is within ~1 percentage point (tune this after seeing the noise from the repeated reference run) and shows no position effect. If no batch size passes, **use batch size 1**. That's affordable at our volumes: tokens barely change, and the request count stays within budget (§16).

Also create a **fixed reference set of ~500 posts** reviewed by a human (LLM-assisted), stored as a file in the repo. It's also labeled by Jev with source `eval`, so the three-way comparison (student vs Jev vs human) is possible and so the export can exclude it from training. It guards against the student inheriting Jev's systematic mistakes, and it's used for regression checks throughout.

**Done when:** taxonomy v1 is approved; batch size and top-K are chosen; `max_batch` and tokens per post are measured; §16's cost numbers are updated with real numbers; the reference set exists.

## 9. Phase 1: ingest and backfill

### 9.1 Ingest service (Go)
- **Collections:** `app.bsky.feed.post`, `app.bsky.feed.like`, `app.bsky.feed.repost`. Also account events, and deletes for all three collections.
- **Start** 2–3 days back (default 72h, configurable) via the backfill/archive path, then switch to live. Save the stream position to `ingest_cursor` every few seconds and resume from it on restart. Ingest keeps **everything** from these days, not only the labeling windows: the student classifies all of it, and the likes feed interest profiles.
- **Every post's text** (any language, replies included) goes into `post_texts`, for resolving quotes.
- **Post filter:**
  1. `operation == create`, and the record has **no `reply`** field.
  2. `langs` contains `en` or `en-*`, **and** a local language detector agrees (e.g. `github.com/pemistahl/lingua-go` restricted to a handful of common languages for speed; or `whatlanggo`). Skip the detector check for very short text (<~20 characters) and trust the tag. Log how often the tag and detector disagree.
  3. Extract the embed data:
     - `app.bsky.embed.images`: image alt texts → `media_alts`
     - `app.bsky.embed.video`: alt text → `media_alts`
     - `app.bsky.embed.external`: link URI, title, description
     - `app.bsky.embed.record`: quoted post URI
     - `app.bsky.embed.recordWithMedia`: both the media and the quote
  4. Facets and `tags`: hashtags and link domains.
  5. **Resolve quoted text:** for posts that quote another post, look up the quoted post's text in `post_texts`. If it's missing (older than our data, or skipped), fetch it from the public AppView with `app.bsky.feed.getPosts` (up to 25 URIs per call) and store it in `post_texts`. If it still can't be found (deleted, or hidden by the AppView), leave `quote_text` empty. Resolution runs in a small batching step so a slow AppView call doesn't stall ingest; posts wait at most a few seconds before being written with whatever was found.
- **Likes and reposts:** store **all** of them network-wide, not only likes on top-level English posts. It's cheap, and it serves engagement counts and interest profiles for any user.
- **Deletes:** write to `deletions`. **Account events:** write to `account_status`.
- **Throughput:** batch inserts into ClickHouse (e.g. every 1s or every 5–10k rows). Backfill replay should run far above live speed. Live is ~230 events/s total.
- **Idempotency:** the same event twice produces the same row, collapsed by the table engine.

### 9.2 Checks
- The backfill covers the full range (check `min(indexed_at)` / `max(indexed_at)`).
- Top-level English posts per hour look plausible: ~25–35k/hour, varying by time of day.
- Quote resolution: the share of quote posts with non-empty `quote_text` is high (expect well above 90%), and AppView fetches stay a small fraction of lookups.
- Live lag (now minus the latest event time) stays under ~5s.

**Done when:** ~2M filtered posts over 72h (or ~1.4M over 48h) are stored along with likes, reposts, and deletions, and live ingest keeps up.

## 10. Phase 2: labeling with Jev

### 10.1 What gets labeled (the `source` column)

| Source | What | When |
|---|---|---|
| `window` | Every post in the **labeling windows**: 24 windows of 15 minutes (six hours in total) spread across the backfill days, ~175k posts | Once. The first 2,500 are a review sample (§10.2a). |
| `sample` | A **random ~20%** of new live posts (configurable, 0–100%) | Continuously; unbiased data for evaluation and drift checks |
| `uncertain` | New posts where the student is unsure (broad-topic top probability < ~0.6, or high entropy; tune later) | Continuously once the student exists. The most informative training examples. **Never used for evaluation** (biased toward hard cases). |
| `eval` | The Phase 0 test runs and the reference set | As needed. **Never used for training.** |

**Choosing the labeling windows:** a small script picks the windows once, records its random seed, and writes them to `config/labeling_windows.yaml` (window ID, start, end). Constraints:
- Each hour of the day (UTC) appears in exactly one window, so the set covers the whole daily cycle.
- The windows are spread evenly across the backfill days (8 per day for 3 days, or 12 per day for 2).
- Each window starts at a random minute inside its hour.

The labeler and the trainer both read this file. The trainer uses it to split data by window (§11.2).

**Open question (default shown):** ongoing labeling at ~20% random plus uncertain posts (~$15–25/day), or 100% of posts (~$60/day)?

### 10.2 The post document: how a post is presented to Jev and to the student

Jev's state and the student's input are both rendered by `internal/postdoc` from the same `posts` row. No other code builds either format. The fields:

| Field | Source column | Notes |
|---|---|---|
| `text` | `text` | As written. |
| `alt_text` | `media_alts` | Image and video alt text. |
| `link` | `link_domain`, `link_title`, `link_description` | Description truncated to ~300 characters. |
| `quote` | `quote_text` | Text of the quoted post, truncated to ~500 characters. |
| `tags` | `tags` | Hashtags from facets and the `tags` field, deduplicated. |

Empty fields are left out of both renderings. Posts with no text and no alt text still get labeled; "unclear" is the expected answer.

**Jev rendering** (one entry of the `posts` array in the state):
```json
{"posts": [
  {"text": "…post text…",
   "alt_text": ["alt text 1", "alt text 2"],
   "link": {"domain": "example.com", "title": "…", "description": "…"},
   "quote": "…text of the quoted post…",
   "tags": ["…"]}
]}
```

**Student rendering** (one string):
```
{text}
[tags] #tag1 #tag2
[alt] {alt 1} | {alt 2}
[link] {domain} | {title} | {description}
[quote] {quote text}
```

The Go code renders the student string for both training (via `export`) and serving (via the `classify` worker). Python only tokenizes it, so the two formats can't drift apart between training and serving.

### 10.2a First 2,500 posts: stop and review

Before labeling the full window set, the labeler labels **2,500 posts** (~104 per window, chosen at random within each window) with the production configuration, then stops (`--limit 2500`). It costs ~$0.20.

It then writes a review report (`reports/<taxonomy_version>-first-2500/`, gitignored because it contains post text), as Markdown plus CSVs:
- Post counts and share per broad topic and per subtopic, including how much lands in personal life and unclear.
- For each broad topic: ~10 random example posts, with the rendered post document, Jev's top 3 broad topics, and the chosen subtopic with probabilities.
- The 50 lowest-confidence posts, and posts where the best final path went through the 2nd or 3rd broad topic.
- The distribution of each ranking signal.
- Tokens per post, requests, cost, and 429s.

**Hailey reviews the report before labeling continues.** If the review changes the taxonomy or the questions, bump the version and relabel the 2,500 (another ~$0.20). If not, those labels stay, and labeling continues with the remaining window posts.

### 10.3 Pass 1: broad topic plus ranking questions (one request per batch of N posts)
For each post `i` in the batch:
- `p{i}_broad`: **choice**, "Which broad topic best describes the post `posts[i]` (its text, alt text, link, quoted post, and tags)?", with every broad topic from the taxonomy (id → description).
- Ranking questions (initial set; Phase 0 prunes it):
  - `p{i}_substance`: **score**, "How substantive is `posts[i]`?" Levels:
    1. "Low effort: a few words, a reaction, or filler"
    2. "Some substance: a clear thought, opinion, or share"
    3. "Substantive: informative, insightful, creative, or detailed"
  - `p{i}_news`: **noul**, "Is `posts[i]` about a current event or breaking news?"
  - `p{i}_promo`: **noul**, "Is `posts[i]` mainly self-promotion, an advertisement, or a request for follows, likes, or reposts?"
  - `p{i}_general_interest`: **noul**, "Would someone who doesn't know the author find `posts[i]` interesting?"
  - `p{i}_tone`: **choice**, with `informative`, `humorous`, `personal`, `outraged`, `supportive`, `other`.

N is the batch size chosen in Phase 0 (possibly 1). Posts in a batch are drawn at random from the pending set, not in time order.

### 10.4 Pass 2: subtopics (depends on pass 1)
- For each post, take its top **K=3** broad topics (skip any with probability < ~0.05).
- For each (post, broad topic) pair: `p{i}_{broad}_sub` is a **choice** over that broad topic's subtopics, including `other`.
- With batch size 1, a post's subtopic questions go in one request. With larger batches, **group requests by broad topic**, so the state holds only posts relevant to that topic. Pass 2 uses the same `max_batch` rule, computed from its own question tokens.
- Store `P(sub | broad)` in `sub_probs["broad/sub"]`. Compute each path score as the geometric mean `sqrt(P(broad) × P(sub | broad))` and store it in `path_scores`. The final label is the path with the highest score.

### 10.5 Throughput and rate limiting
- A token-bucket limiter on **both** requests/min (default 500) and estimated tokens/s (default 100k). Estimate tokens before sending as ~characters/4. Correct the estimate afterwards from `usage.input_tokens`.
- **Per-request size check:** before sending, the labeler checks each request's estimated tokens against a cap (default 80% of the 64k limit) and splits any request over it into smaller ones.
- Concurrency: 16–32 requests in flight (tune to keep latency and 429s low).
- On 429: the client retries with backoff. The limiter also cuts its rate by ~20% for a minute, then ramps back up.
- Checkpointing is implicit: a post counts as labeled once its row exists for the current `(taxonomy_version, label_config)`. After a crash, at most the in-flight batches get relabeled.
- Log every request to `jev_requests` to track cost, 429s, and latency.

### 10.6 Labeling-window budget (estimates; replace with Phase 0 numbers)
- ~2k tokens/post at full quality × ~175k posts ≈ **0.35B tokens ≈ $15**.
- Time depends on the batch size:
  - Batch size 5 or more: limited by tokens, **about 1 hour** at ~100k tokens/s.
  - Batch size 1: limited by requests. ~175k pass-1 requests plus ~175k pass-2 requests at 500/min is **about 12 hours**. Slower, but still fine for a one-time job.

**Done when:** every labeling-window post has a `jev_labels` row for taxonomy v1. Label counts per subtopic have been reviewed. Any subtopic with fewer than ~200 examples gets topped up: find similar posts from the rest of the backfill with embeddings and label them (source `window`, marked as a top-up in `label_config`), or merge the subtopic in taxonomy v1.1. With ~175k posts across ~250 subtopics, expect several subtopics to need this.

## 11. Phase 3: training the classifier on the 4090

### 11.1 Environment
- Same machine as everything else (§6). The NVIDIA driver (610.57, CUDA 13.3) is already installed and supports current CUDA builds of PyTorch. Python 3.12 managed with `uv`.
- Packages: `torch` (CUDA build), `transformers`, `datasets`, `pyarrow`, `scikit-learn`, `onnx`, `onnxruntime`, and `optimum` (or `torch.onnx`) for export.
- Data arrives as Parquet written locally by the `export` command (e.g. under `/data/exports/`).

### 11.2 Dataset export

The `export` command (Go) runs the query below, renders each row's student input with `internal/postdoc`, assigns each row its labeling window from `config/labeling_windows.yaml` (null for live labels), and writes Parquet with columns `uri, indexed_at, window_id, source, model_input, broad_probs, broad_confidence, sub_probs, path_scores, signals, label_config`.

The query guarantees **one training label per post**, never uses `eval` labels, and keeps every post that has an `eval` label (the Phase 0 test posts and the reference set) out of the training data entirely:

```sql
WITH
    -- Phase 0 test posts and the reference set. Never trained on.
    eval_uris AS (
        SELECT DISTINCT uri FROM jev_labels WHERE source = 'eval'
    ),
    -- Exactly one label per post: the newest row among accepted production configs.
    labels AS (
        SELECT uri, broad_probs, broad_confidence, sub_probs, path_scores, signals, source, label_config
        FROM jev_labels FINAL
        WHERE taxonomy_version = {taxonomy_version:String}
          AND label_config IN {label_configs:Array(String)}   -- production configs accepted for this training run
          AND source IN ('window', 'sample', 'uncertain')
        ORDER BY uri, labeled_at DESC
        LIMIT 1 BY uri
    )
SELECT p.uri, p.did, p.indexed_at,
       p.text, p.media_alts, p.link_domain, p.link_title, p.link_description, p.quote_text, p.tags,
       l.broad_probs, l.broad_confidence, l.sub_probs, l.path_scores, l.signals, l.source, l.label_config
FROM posts AS p FINAL
INNER JOIN labels AS l ON l.uri = p.uri
WHERE p.uri NOT IN (SELECT uri FROM eval_uris)
  AND p.uri NOT IN (SELECT uri FROM deletions WHERE collection = 'app.bsky.feed.post')
  AND p.did NOT IN (SELECT did FROM account_status FINAL WHERE active = 0)
```

`label_configs` normally holds one value, the production config chosen in Phase 0. It holds more than one only if the production config changed deliberately (for example a new batch size after a Jev upgrade) and Phase 0-style checks showed the configs agree.

The export also checks itself: it fails if any `uri` appears twice in its output, or if any reference-set URI appears.

**Split by labeling window (initial training):**
- Sort the 24 windows by start time.
- Newest 3 windows → test; the 3 before them → validation; the other 18 → train (~75 / 12.5 / 12.5).
- Because each hour of the day appears once, the test and validation windows fall at different times of day, not all in one block.

For later retrains that include live labels, split live data by time instead (§13).

**Evaluation data comes only from `window` and `sample` rows.** `uncertain` rows go only into training.

### 11.3 Model input
The `model_input` column from the export: the student rendering of the post document (§10.2). Truncate at a max sequence length of ~256 tokens (raised from 192 to leave room for quoted text). Posts are short, so this rarely cuts anything.

### 11.4 Baseline (build first; it takes minutes)
- Embed every post with a current small English sentence-embedding model (e.g. a `bge-small`/`gte`/`e5`-class model).
- Train logistic regression per level: one for broad topic, one for all subtopics flattened into `broad/sub` labels, and one per ranking signal.
- This sets the bar. It's also the embedding foundation for the later granular-topics work, and for finding top-up posts for thin subtopics (§10.6).

### 11.5 Main model
- Fine-tune **ModernBERT-base** (`answerdotai/ModernBERT-base`, ~150M parameters, Apache-2.0). Try **ModernBERT-large** (~400M) if base falls short. DeBERTa-v3-base is a fallback.
- **Outputs, all from one shared encoder:**
  - **Broad topic:** softmax over the broad topics. Loss: **KL divergence against Jev's `broad_probs`**.
  - **Subtopic:** softmax over all `broad/sub` pairs (~200–350). Target: the joint distribution `P(broad) × P(sub | broad)` over the pairs Jev scored, renormalized. Pairs Jev didn't score get ~0.
  - **Ranking signals:** one sigmoid per signal. Loss: binary cross-entropy against Jev's value normalized to 0..1 (noul probability; score divided by the top level).
- **Per-example weights:** scale by Jev's broad confidence, with a floor (e.g. 0.3) so hard examples aren't ignored.
- **Starting hyperparameters (estimate):** bf16; learning rate 5e-5 (base) or 3e-5 (large); 5% warmup, then linear decay; batch size 128–256; 2–4 epochs (more passes suit the smaller ~130k-post training split); early stopping on validation broad-topic agreement.
- **Calibration:** after training, fit a temperature on the validation set, separately for the broad and subtopic outputs.
- **Expected time on a 4090 (estimate):** a few minutes per pass over the ~130k training posts for the base model.

### 11.6 Evaluation (on the test windows, plus the reference set)
- **Top-1 agreement with Jev** on broad topic and on subtopic (the best path).
- **Top-3 agreement** (Jev's choice appears in the student's top 3).
- Per-topic precision and recall against Jev's labels, to find weak topics.
- **Calibration error** (ECE) for broad topics.
- Ranking signals: correlation and mean absolute error against Jev.
- **Reference set:** student vs Jev vs the human labels.
- Record everything in `metrics.json` and in the `models` table.

**Target:** set after Phase 0. A reasonable starting bar is broad-topic agreement at least ~90%, with no topic dramatically worse than the rest. Compare against Jev's own disagreement between batch sizes as the noise floor.

### 11.7 Model files
```
models/<version>/            # version = UTC timestamp + short git SHA, e.g. 20261001T0400Z-ab12cd3
  model.onnx                 # exported, possibly int8-quantized for CPU
  tokenizer.json
  config.json                # taxonomy_version, post document version, label maps, temperatures, max_len
  metrics.json
  data_manifest.json         # data window, row counts, label_config(s)
```

The `classify` worker refuses to use a model whose post document version differs from the one it renders.

**Done when:** the first model beats the embedding baseline, meets the agreement bar, and its files are produced.

## 12. Phase 4: serving the classifier

- **Classifier service:** Python (FastAPI or similar) + **ONNX Runtime on CPU**.
  - `POST /classify` accepts `{posts: [{uri, input}]}`, where `input` is the student rendering from `internal/postdoc`, and returns `{model_version, results: [{uri, broad_probs, sub_probs, signals, broad_confidence}]}`.
  - `GET /healthz` and `GET /model` (the latter reports the model's post document version).
  - Watches a `models/current` symlink (or polls a version file) and **hot-reloads** without dropping requests.
- **Capacity:** ~8 posts/s is trivial. A ~150M-parameter encoder on a many-core CPU handles hundreds of posts/s with batching (estimate), so the GPU isn't needed for serving.
- **`classify` worker (Go):**
  - Every few seconds, fetch posts from the last 48h that have no prediction from the current model. Render them with `internal/postdoc`, send them in batches of ~64, and write to `predictions`.
  - Flag uncertain posts for the labeler (§10.1).
  - **When a new model is promoted, re-score all posts under 24h old** (~0.7M posts, minutes of CPU).
- A Go-native inference path (onnxruntime Go bindings plus a Go tokenizer) is a possible later cleanup. The Python service is simpler to start with.

**Done when:** new posts have predictions within seconds of ingest, and model swaps work without downtime.

## 13. Phase 5: regular retraining and drift monitoring

- **Schedule:** daily at first (e.g. 04:00 UTC), via systemd timer or cron.
  1. Export labels for the current taxonomy version from a sliding window (default: the last 14 days), using the export in §11.2.
  2. Weight examples by recency (default half-life: 3 days).
  3. Train as in §11. Fine-tuning from the current model is fine for daily runs; do a full retrain from the base weights weekly.
  4. Evaluate on the **newest held-out time window** (random-sample rows only) and on the reference set.
  5. **Promote only if** it's no worse than the current model on both (e.g. broad-topic agreement at least current − 0.5 points) and no topic's agreement drops sharply.
  6. Write the model files to `models/<version>/`, then flip `models/current`.
  7. The classifier service reloads, and the last 24h are re-scored on the GPU (§6).
- Skip a run if fewer than ~20k new labels have arrived since the last run.
- **Drift check:** a daily agreement number between the student and the random Jev sample, with a threshold to alert on. A drop means retrain early, or look for missing topics.
- **Taxonomy changes:** bump the version (v1 → v2), relabel a window (e.g. the last 2–7 days of random-sample posts) with Jev under the new version, and retrain from scratch. Old labels stay in the table, tagged with their version.

**Done when:** retraining runs unattended for a week, with metrics recorded and at least one successful automatic promotion.

## 14. Phase 6 (later): the feed itself

An outline only. Detail it once classification works.

- **Endpoints (Go):**
  - `/.well-known/did.json` (`did:web` for the feed service)
  - `app.bsky.feed.describeFeedGenerator`
  - `app.bsky.feed.getFeedSkeleton`
  - `app.bsky.feed.sendInteractions`
  - Verify the service-auth JWT (issuer is the viewer's DID; the audience must match our service DID). `github.com/bluesky-social/indigo` has the relevant pieces.
- **Generator record:** publish `app.bsky.feed.generator` with `acceptsInteractions: true`.
- **Interest profile per user:**
  - A decayed, weighted sum of the subtopic distributions of posts they liked or reposted (half-life ~2 days).
  - "Show more" / "show less" interactions add direct positive or negative weight to the item's topics.
  - Compute on request from ClickHouse (likes × predictions), and cache in Redis for ~10–30 minutes.
- **Candidates:**
  - Posts **under 24h old**, with a strong preference for under 12h (freshness decay, half-life ~6h, hard cutoff at 24h).
  - Excluded: already seen or served to this user, liked or reposted by them, their own posts, deleted posts, inactive authors.
- **Scoring (first version):** `affinity(user profile, post subtopic distribution) × freshness × quality`.
  - Quality combines the ranking signals (substance, general interest, not promo) with like velocity from `like_counts_hourly`.
  - Add diversity with a per-topic and per-author cap in each page (or MMR).
- **Seen tracking:**
  - `interactionSeen` events go to a Redis set per user (expires after ~48h, since candidates are under 24h old), plus a ClickHouse log for analysis.
  - Also track what we served, even without seen events.
- **Pagination:** cache each viewer's ranked list for the session in Redis (~30 minutes). The cursor is an offset into it. Put `reqId` and a compact `feedContext` on each item so interactions map back to the request.
- **New users with few likes:** show popular recent posts across broad topics, weighted by quality.

## 15. Observability

- Each Go service exposes Prometheus metrics at `/metrics` and writes structured JSON logs (`slog`).
- **Key metrics:**
  - **Ingest:** events/s by collection, posts kept vs rejected (and why), quote resolution (found locally / fetched / missing), lag, ClickHouse insert errors.
  - **Labeler:** requests/s, tokens/s, 429s, latency percentiles, requests split for size, **estimated spend per day** (from `jev_requests`), backlog size by source.
  - **Classify:** posts/s, backlog, service latency, current model version.
  - **Training:** last run time, result, agreement numbers, whether it was promoted.
  - **Drift:** daily student-vs-Jev agreement on the random sample.
- A small Grafana dashboard is optional. Much of this can be ad hoc ClickHouse queries at first.

## 16. Cost and capacity

At $0.042 per million input tokens. **Estimates; replace with Phase 0 measurements.**

| Item | Tokens | Cost |
|---|---|---|
| Phase 0 test runs (~2,000 posts, ~6 configurations) | ~25M | ~$1 |
| Taxonomy drafting (LLM via AGW) | n/a | a few dollars |
| First 2,500 posts (review sample) | ~5M | ~$0.20 |
| **Labeling windows, full quality (~175k posts)** | ~0.35B | **~$15** |
| Labeling windows, lean (short labels, top-1 only, no ranking questions) | ~0.05B | ~$2 |
| Thin-subtopic top-ups | ~0.02–0.1B | ~$1–4 |
| **Ongoing labeling, ~20% sample plus uncertain posts** | ~0.35–0.6B/day | **~$15–25/day** |
| Ongoing labeling, every post at full quality | ~1.45B/day | ~$60/day |

**Where the tokens go:** most of each post's cost is the **option lists** (every question repeats its options), especially the top-3 subtopic pass (~60% of tokens). Batching many posts per request doesn't reduce tokens much, because posts are small. **Batching mainly reduces requests.** Tightening option descriptions is the biggest cost lever, and it also raises `max_batch`.

**Rate budget check (full quality, at our ~1/3–1/2 budget of ~400–600 requests/min):**
- **Labeling windows:** a one-time job. About 1 hour at batch size 5 or more (token-limited); about 12 hours at batch size 1 (request-limited).
- **Ongoing labeling at ~20% plus uncertain** (~1.6–2.5 posts/s, two passes):
  - Batch size 1: ~200–300 requests/min, about half the request budget. Workable, but it leaves little room.
  - Batch size 5: ~40–60 requests/min.
  - Live batches fill quickly: wait at most a few seconds to fill a batch before sending a partial one.
- **Live labeling at 100% of posts:** ~18k tokens/s (about 15–20% of our token budget). Requests: ~960/min at batch size 1, which is over budget, so this option needs batch size 2 or more (~190/min at batch size 5).

**Hardware:** power for a 4090 box training daily and idling otherwise is small. The student runs on CPU.

## 17. Risks and mitigations

| Risk | Mitigation |
|---|---|
| **Taking rate limit away from Attie** on the shared TypeSafe account | Hard client-side budget (configurable); back off on 429s; alert on sustained 429s. |
| **Jev's rate limits change without notice** | Configurable limiter; the labeler degrades gracefully (the backlog just grows). The student keeps classifying either way. |
| **Jev model changes** under an alias | Pin `jev-1.13.0`; record `jev_model` on every label; upgrade deliberately and compare. |
| **Accuracy loss from large batches** | Phase 0 batch-size test against a batch-size-1 reference, with a position check; hard cap from the context limit; fall back to batch size 1; keep batch size configurable; recheck after model upgrades. |
| **Requests over the context limit** | `max_batch` computed from real question tokens; per-request size check that splits oversize requests. |
| **Taxonomy doesn't fit Bluesky content** (e.g. too much lands in personal or unclear) | LLM-drafted from real posts, then human-reviewed; the first-2,500 review before full labeling; watch per-topic volume; iterate with versioned taxonomies. |
| **The student inherits Jev's systematic mistakes** | Human-reviewed reference set; review the student's high-confidence disagreements with Jev; tighten descriptions. |
| **Jev and the student see different content** | One post document renders both; golden-file tests; the post document version is recorded in `label_config` and model config, and checked at serving time. |
| **Test data leaking into training** | Export excludes every post with an `eval` label, picks one label per post, and fails on duplicate URIs or reference-set URIs. |
| **Topic drift** (new events, slang) | Continuous random Jev sample, a daily drift number, daily retrains on a sliding window, recency weighting. |
| **Bias from a narrow time window** in training data | Labeling windows cover every hour of the day across 2–3 days; evaluate on later windows. |
| **Too few examples for rare subtopics** (only ~175k labeled posts at first) | Top up thin subtopics with embedding-selected posts from the rest of the backfill, or merge them; the live sample adds more every day. |
| **Adversarial posts** (text written to steer the classifier) | Jev's docs list this as a known weakness. Precise option descriptions; the student is less susceptible to instruction-style text; monitor promo/bait signals. |
| **Wrong or missing language tags** | A local language detector check; log disagreements. |
| **Duplicate events** from Jetstream | Idempotent writes; `ReplacingMergeTree`. |
| **Deletion and takedown compliance** | Deletions and account status on every read path; daily physical deletes (including quoted text); sliding-window retrains age deleted content out. |
| **OpenJev license** if it's ever used in production | Only used for evaluation; CC BY-NC would need a commercial license. |

## 18. Open questions

1. ~~Project name and repo location~~: **resolved 2026-09-28.** `topic-feed`: locally at `~/bluesky/topic-feed`, on GitHub as the private repo `haileyok/topic-feed`.
2. ~~Training host access~~ and 3. ~~One machine or two~~: **resolved 2026-09-28.** Everything runs on one machine, the Threadripper 7960X + RTX 4090 box (§6).
4. ~~Jetstream endpoint~~: **resolved 2026-09-28.** Jetstream v2 replay at `jetstream.us-east.bsky.network` with an API key (§4.7).
5. **Ongoing Jev labeling rate:** the default is ~20% random plus uncertain posts. Or label everything?
6. **Taxonomy depth:** two levels (proposed), or a third level through Jev? The alternative for a finer level is embedding clusters later.
7. **Likes retention for interest profiles:** keep 14 days of likes (proposed)? For feed users, should we also fetch and classify posts they liked before the backfill window, using the AppView's `app.bsky.feed.getPosts` and the student model?
8. **Feed hosting:** where does the feed generator run, under what DID/hostname, and which account publishes the generator record?

## 19. Milestone checklist

- [ ] **M0.1** Machine and repo setup. Done 2026-09-28: second NVMe formatted and mounted at `/data`; repo at `~/bluesky/topic-feed` with worktree `~/worktrees/topic-feed/setup` (branch `hailey/setup`); ClickHouse running via docker compose with data on `/data`; schema applied; `internal/postdoc` with golden-file tests; PyTorch (CUDA) in a Python 3.12 `uv` project, sees the GPU; Jev (via AGW) and Jetstream v2 replay access verified. **Remaining:** nightly backup.
- [ ] **M0.2** Taxonomy v1 drafted by LLM from a real sample; reviewed and approved; committed as YAML.
- [ ] **M0.3** Jev test run done: `max_batch` computed; batch size, top-K, and ranking questions chosen; tokens per post measured; costs updated.
- [ ] **M0.4** ~500-post human-reviewed reference set committed, and labeled by Jev as `eval`.
- [ ] **M1.1** Ingest running: 2–3 day backfill complete; live lag < 5s; filters verified (language disagreement rate logged); quote resolution rate checked.
- [ ] **M2.0** Labeling windows chosen and committed; first 2,500 posts labeled; review report produced; **Hailey approves before labeling continues**.
- [ ] **M2.1** Labeler running under the rate budget; `jev_requests` cost tracking works; oversize requests split.
- [ ] **M2.2** All labeling-window posts labeled under taxonomy v1; per-topic counts reviewed; thin subtopics topped up or merged.
- [ ] **M3.1** Export produces one label per post with no `eval` posts; embedding baseline trained and evaluated on the test windows.
- [ ] **M3.2** ModernBERT student trained; beats the baseline; meets the agreement bar; ONNX files produced.
- [ ] **M4.1** Classifier service deployed; `classify` worker writes predictions within seconds; hot-reload verified.
- [ ] **M4.2** Ongoing Jev sampling (random + uncertain) running.
- [ ] **M5.1** Daily retrain automated with promotion rules; the drift number is tracked; a week unattended.
- [ ] **M6.x** Feed phase: detailed later.

---

## Appendix A: Jev request examples

### A.1 Pass 1: two posts, broad topic plus two ranking questions (shortened)

```json
{
  "model": "jev-1.13.0",
  "state": {
    "posts": [
      {"text": "Just watched the Eagles win in OT, what a game!!"},
      {"text": "New paper on sparse attention kernels is out",
       "link": {"domain": "arxiv.org", "title": "Faster Sparse Attention on Modern GPUs"}}
    ]
  },
  "questions": {
    "p0_broad": {
      "type": "choice",
      "instructions": "Which broad topic best describes the post `posts[0]` (its text, alt text, link, quoted post, and tags)?",
      "criteria": {
        "sports": "Games, athletes, teams, leagues, sports media. Not sports-betting promotions.",
        "science_tech": "Science, research, software, AI, gadgets, engineering.",
        "personal_life": "Personal updates, greetings, daily life, feelings, with no other clear topic.",
        "unclear": "No discernible topic, or not enough content to tell."
      }
    },
    "p0_news": {"type": "noul", "instructions": "Is `posts[0]` about a current event or breaking news?"},
    "p1_broad": {
      "type": "choice",
      "instructions": "Which broad topic best describes the post `posts[1]` (its text, alt text, link, quoted post, and tags)?",
      "criteria": {
        "sports": "Games, athletes, teams, leagues, sports media. Not sports-betting promotions.",
        "science_tech": "Science, research, software, AI, gadgets, engineering.",
        "personal_life": "Personal updates, greetings, daily life, feelings, with no other clear topic.",
        "unclear": "No discernible topic, or not enough content to tell."
      }
    },
    "p1_news": {"type": "noul", "instructions": "Is `posts[1]` about a current event or breaking news?"}
  }
}
```

Response (shape):
```json
{
  "model": "jev-1.13.0",
  "answers": {
    "p0_broad": {"type": "choice", "choice": "sports", "confidence": 0.99,
                 "probabilities": {"sports": 0.99, "science_tech": 0.0, "personal_life": 0.01, "unclear": 0.0}},
    "p0_news":  {"type": "noul", "noul": 0.71},
    "p1_broad": {"type": "choice", "choice": "science_tech", "confidence": 0.98, "probabilities": {"…": 0}},
    "p1_news":  {"type": "noul", "noul": 0.35}
  },
  "usage": {"input_tokens": 612, "output_tokens": 40}
}
```

A live check of a similar two-post, three-option request cost 372 input tokens and classified both posts correctly with confidence 1.0.

### A.2 Pass 2: subtopics for posts whose top-3 broad topics include `sports`

```json
{
  "model": "jev-1.13.0",
  "state": {"posts": [{"text": "Just watched the Eagles win in OT, what a game!!"}, {"text": "…"}]},
  "questions": {
    "p0_sports_sub": {
      "type": "choice",
      "instructions": "Which kind of sports content is the post `posts[0]` about?",
      "criteria": {
        "american_football": "NFL, college football, fantasy football.",
        "soccer": "Association football: leagues, clubs, international play.",
        "basketball": "NBA, WNBA, college basketball.",
        "other": "Sports content that fits none of the above."
      }
    }
  }
}
```

## Appendix B: Jetstream event shapes

**Jetstream v2 (what we use).** Each WebSocket message is an envelope with the event under `payload`, tagged by `$type` (from the v2 docs):

```json
{"$type": "message",
 "payload": {"$type": "network.bsky.jetstream.subscribeEvents#commit",
   "did": "did:plc:7e6kocyzb77xkncplrkoojej", "seq": 24664288881, "time": "2026-08-13T06:47:43.959305Z",
   "operation": "create", "collection": "app.bsky.feed.like", "rkey": "3msx2efqdxs27",
   "rev": "3msx2efqjtc27", "cid": "bafyrei…",
   "record": {"$type": "app.bsky.feed.like", "createdAt": "2026-08-13T06:47:44.859Z",
     "subject": {"cid": "bafyrei…", "uri": "at://did:plc:…/app.bsky.feed.post/3mjfhkjsshs2q"}}}}
```
- Commit fields sit directly on the payload (no nested `commit` object). `time` is the event time we store as `indexed_at`.
- A delete has no `record` or `cid`, only `collection` and `rkey`.
- Other payload types: `#identity`, `#account`, `#sync`, each with a field of the same name. On v2, live events also carry a `cursor` field.
- The Go SDK decodes these into typed events, so our code shouldn't parse the envelope by hand. Check the exact Go field names against the SDK version we pin.

**Jetstream v1 (for reference).** These are the older shapes, observed on the v1 public instances:

```json
{"did": "did:plc:…", "time_us": 1759030000000000, "kind": "commit",
 "commit": {"rev": "…", "operation": "create", "collection": "app.bsky.feed.post", "rkey": "3l…", "cid": "bafy…",
   "record": {"$type": "app.bsky.feed.post", "text": "…", "langs": ["en"], "createdAt": "2026-09-28T03:00:00.000Z",
     "reply": {"root": {"uri": "at://…", "cid": "…"}, "parent": {"uri": "at://…", "cid": "…"}},
     "embed": {"$type": "app.bsky.embed.images", "images": [{"alt": "…", "image": {"…": "…"}}]},
     "facets": [{"features": [{"$type": "app.bsky.richtext.facet#tag", "tag": "…"}]}],
     "tags": ["…"]}}}
```
- `reply` is present only on replies. Top-level posts don't have it.
- Other embed types:
  - `app.bsky.embed.external` → `external: {uri, title, description}`
  - `app.bsky.embed.record` → `record: {uri, cid}` (a quote)
  - `app.bsky.embed.recordWithMedia` → `{record, media}`
  - `app.bsky.embed.video` → `{alt, …}`
- Like: `collection: "app.bsky.feed.like"`, `record: {subject: {uri, cid}, createdAt}`. Repost: the same shape with `app.bsky.feed.repost`.
- Delete: `operation: "delete"`, with no `record`.
- Account: `kind: "account"`, `account: {active: bool, status?: string, did, seq, time}`.

## Appendix C: Reference links

- TypeSafe docs index: https://docs.typesafe.ai/llms.txt
  - Models, limits, pricing: https://docs.typesafe.ai/models.md
  - State: https://docs.typesafe.ai/concepts/state.md
  - Questions (primitives): https://docs.typesafe.ai/primitives.md
  - Jev 1.13 known weaknesses: https://docs.typesafe.ai/model-jaggedness/jev-1.13.md
  - Speculative fan-out: https://docs.typesafe.ai/patterns/fan-out.md
  - Parallel questions cookbook: https://docs.typesafe.ai/cookbooks/parallel_questions.md
  - Hierarchical classification cookbook: https://docs.typesafe.ai/cookbooks/hierarchical_classification.md
  - Line-by-line search cookbook (many items in one state): https://docs.typesafe.ai/cookbooks/semantic_find.md
  - Classifying RAG passages (one item per request): https://docs.typesafe.ai/cookbooks/classifying_rag_passages.md
  - Re-ranking (one candidate per request): https://docs.typesafe.ai/cookbooks/rerank_typesafe.md
  - OpenAPI spec: https://api.typesafe.ai/openapi.json
- Go client: https://github.com/haileyok/typesafe-client (module `github.com/haileyok/typesafe-client/go`; see `SPEC.md` for behavior)
- OpenJev: https://huggingface.co/openjev/openjev (also `-FP8`, `-GGUF`, `-MLX`, `-MLX-4bit`)
- Jetstream: https://github.com/bluesky-social/jetstream
- atproto lexicons: `lexicons/app/bsky/feed/{defs,getFeedSkeleton,sendInteractions,generator}.json` in https://github.com/bluesky-social/atproto
- indigo (Go atproto libraries, service auth): https://github.com/bluesky-social/indigo
- ModernBERT: https://huggingface.co/answerdotai/ModernBERT-base
- AGW routing for Jev: `apps/agentgateway/values.yaml` in the `bluesky-social/deploy` repo (route `typesafe-systemone`)
