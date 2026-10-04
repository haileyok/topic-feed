-- Posts our personal feeds (cmd/feedgen, `personal:` in config/feeds.yaml) have shown to
-- each viewer, one row per time shown. A post that was shown but never reported as seen
-- (see feed_interactions) counts as seen once it has been shown `max_serves` times, so the
-- feed doesn't repeat itself. Idempotent.
CREATE TABLE IF NOT EXISTS viewer_served
(
    viewer_did  String,
    uri         String,                   -- the post
    feed        LowCardinality(String),   -- feed rkey
    served_at   DateTime64(3, 'UTC')
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(served_at)
ORDER BY (viewer_did, uri, served_at)
TTL toDateTime(served_at) + INTERVAL 30 DAY;
