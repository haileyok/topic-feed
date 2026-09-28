-- Initial schema (plan §7). Every statement is idempotent; `make schema` re-runs all files.

-- Top-level English posts. Every column the post document uses is stored here,
-- so Jev and the student always see the same content.
CREATE TABLE IF NOT EXISTS posts
(
    uri               String,                        -- at://did/app.bsky.feed.post/rkey
    did               String,
    rkey              String,
    cid               String,
    created_at        DateTime64(3, 'UTC'),          -- client-supplied createdAt, stored but not trusted
    indexed_at        DateTime64(6, 'UTC'),          -- Jetstream event time
    text              String,
    langs             Array(LowCardinality(String)),
    detected_lang     LowCardinality(String),        -- from the local language detector
    embed_type        LowCardinality(String),        -- none|images|video|external|record|recordWithMedia
    media_alts        Array(String),                 -- alt text of images and video
    link_uri          String,
    link_domain       LowCardinality(String),
    link_title        String,
    link_description  String,
    quote_uri         String,
    quote_text        String,                        -- text of the quoted post, resolved at ingest; empty if unavailable
    tags              Array(String),                 -- hashtags from facets and the tags field
    link_domains      Array(LowCardinality(String)), -- domains of links in facets
    has_labels        UInt8                          -- self-labels present (e.g. adult content)
)
ENGINE = ReplacingMergeTree(indexed_at)
PARTITION BY toDate(indexed_at)
ORDER BY uri;

-- Text of every post on the network (any language, replies included), kept briefly
-- so quoted posts can be resolved without calling the AppView.
CREATE TABLE IF NOT EXISTS post_texts
(
    uri         String,
    text        String,
    indexed_at  DateTime64(6, 'UTC')
)
ENGINE = ReplacingMergeTree(indexed_at)
PARTITION BY toDate(indexed_at)
ORDER BY uri
TTL toDateTime(indexed_at) + INTERVAL 7 DAY;

CREATE TABLE IF NOT EXISTS likes
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
CREATE TABLE IF NOT EXISTS reposts AS likes;

-- Like counts per post per hour, for engagement and velocity signals.
CREATE TABLE IF NOT EXISTS like_counts_hourly
(
    subject_uri  String,
    hour         DateTime('UTC'),
    likes        UInt64
)
ENGINE = SummingMergeTree
PARTITION BY toDate(hour)
ORDER BY (subject_uri, hour)
TTL hour + INTERVAL 14 DAY;

CREATE MATERIALIZED VIEW IF NOT EXISTS like_counts_hourly_mv TO like_counts_hourly AS
SELECT subject_uri, toStartOfHour(indexed_at) AS hour, count() AS likes
FROM likes
GROUP BY subject_uri, hour;

-- Deletes of posts, likes, and reposts. Unlikes arrive as deletes on app.bsky.feed.like.
CREATE TABLE IF NOT EXISTS deletions
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
CREATE TABLE IF NOT EXISTS account_status
(
    did         String,
    active      UInt8,
    status      LowCardinality(String),
    indexed_at  DateTime64(6, 'UTC')
)
ENGINE = ReplacingMergeTree(indexed_at)
ORDER BY did;

-- Teacher labels from hosted Jev. Kept forever; this is the training set.
-- A post can have several rows (one per taxonomy_version x label_config). The export
-- picks exactly one training label per post and never uses `eval` rows.
CREATE TABLE IF NOT EXISTS jev_labels
(
    uri               String,
    taxonomy_version  LowCardinality(String),
    label_config      LowCardinality(String),        -- hash of question set, post document version, batch size, top-K, ...
    jev_model         LowCardinality(String),        -- versioned model that answered, e.g. jev-1.13.0
    source            LowCardinality(String),        -- window|sample|uncertain|eval
    batch_size        UInt16,
    labeled_at        DateTime64(3, 'UTC'),
    broad_probs       Map(LowCardinality(String), Float32),
    broad_confidence  Float32,
    sub_probs         Map(LowCardinality(String), Float32),  -- "broad/sub": P(sub | broad)
    path_scores       Map(LowCardinality(String), Float32),  -- "broad/sub": geometric-mean path score
    signals           Map(LowCardinality(String), Float32),  -- ranking signals, normalized to 0..1
    request_ids       Array(String)
)
ENGINE = ReplacingMergeTree(labeled_at)
ORDER BY (taxonomy_version, label_config, uri);

-- One row per Jev request, for cost and throughput tracking.
CREATE TABLE IF NOT EXISTS jev_requests
(
    request_id     String,
    ts             DateTime64(3, 'UTC'),
    pass           LowCardinality(String),           -- broad|sub
    n_posts        UInt16,
    n_questions    UInt16,
    input_tokens   UInt32,
    latency_ms     UInt32,
    status         LowCardinality(String),           -- ok|rate_limited|error
    error          String
)
ENGINE = MergeTree
PARTITION BY toDate(ts)
ORDER BY ts;

-- Student predictions.
CREATE TABLE IF NOT EXISTS predictions
(
    uri               String,
    model_version     LowCardinality(String),
    taxonomy_version  LowCardinality(String),
    predicted_at      DateTime64(3, 'UTC'),
    broad_probs       Map(LowCardinality(String), Float32),
    sub_probs         Map(LowCardinality(String), Float32),  -- "broad/sub": joint probability
    signals           Map(LowCardinality(String), Float32),
    broad_confidence  Float32
)
ENGINE = ReplacingMergeTree(predicted_at)
PARTITION BY toDate(predicted_at)
ORDER BY (model_version, uri)
TTL toDateTime(predicted_at) + INTERVAL 30 DAY;

-- Model versions and their evaluation results.
CREATE TABLE IF NOT EXISTS models
(
    model_version     String,
    taxonomy_version  LowCardinality(String),
    trained_at        DateTime('UTC'),
    data_from         DateTime('UTC'),
    data_to           DateTime('UTC'),
    n_train           UInt32,
    metrics           String,                         -- JSON
    promoted          UInt8,
    promoted_at       Nullable(DateTime('UTC'))
)
ENGINE = ReplacingMergeTree(trained_at)
ORDER BY model_version;

-- Jetstream stream position, one row per consumer.
CREATE TABLE IF NOT EXISTS ingest_cursor
(
    consumer    String,
    cursor      UInt64,
    updated_at  DateTime64(3, 'UTC')
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY consumer;
