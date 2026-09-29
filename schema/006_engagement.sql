-- Engagement counts for feed ranking. Idempotent; `make schema` re-runs every file.
--
-- post_refs: every post on the network (any language, replies included) that replies to
-- or quotes another post (cmd/ingest). Replies count for their direct parent. Replies and
-- quotes of the author's own posts are not recorded.
CREATE TABLE IF NOT EXISTS post_refs
(
    subject_uri  String,                        -- the post replied to or quoted
    kind         LowCardinality(String),        -- reply | quote
    uri          String,                        -- the replying or quoting post
    actor_did    String,
    indexed_at   DateTime64(6, 'UTC')
)
ENGINE = ReplacingMergeTree(indexed_at)
PARTITION BY toDate(indexed_at)
ORDER BY (subject_uri, kind, uri)
TTL toDateTime(indexed_at) + INTERVAL 14 DAY;

-- Reposts, replies, and quotes per post per hour (likes are in like_counts_hourly). Like
-- like_counts_hourly, counts come from inserts: deletions aren't subtracted.
CREATE TABLE IF NOT EXISTS engagement_hourly
(
    subject_uri  String,
    kind         LowCardinality(String),        -- repost | reply | quote
    hour         DateTime('UTC'),
    n            UInt64
)
ENGINE = SummingMergeTree
PARTITION BY toDate(hour)
ORDER BY (subject_uri, kind, hour)
TTL hour + INTERVAL 14 DAY;

CREATE MATERIALIZED VIEW IF NOT EXISTS engagement_hourly_refs_mv TO engagement_hourly AS
SELECT subject_uri, kind, toStartOfHour(indexed_at) AS hour, count() AS n
FROM post_refs
GROUP BY subject_uri, kind, hour;

CREATE MATERIALIZED VIEW IF NOT EXISTS engagement_hourly_reposts_mv TO engagement_hourly AS
SELECT subject_uri, 'repost' AS kind, toStartOfHour(indexed_at) AS hour, count() AS n
FROM reposts
GROUP BY subject_uri, hour;

-- Reposts stored before this file was applied were back-filled once by hand (2026-09-29):
--   INSERT INTO engagement_hourly SELECT subject_uri, 'repost', toStartOfHour(indexed_at), count()
--   FROM reposts WHERE indexed_at < <time the view was created> GROUP BY subject_uri, toStartOfHour(indexed_at);
