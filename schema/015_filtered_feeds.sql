-- Filtered feeds (`filtered:` in config/feeds.yaml): another feed's posts, less the ones the
-- viewer's filters leave out. Idempotent.

-- Viewers' sign-ins for the filtered feeds, which the service uses to ask the viewer's own server
-- for the tokens it sends to the source feeds on their behalf. sealed is the sign-in (its tokens
-- and key) encrypted with a key made from FEEDGEN_FILTER_SECRET; nobody without that secret can
-- read it. One row per save (a sign-in's tokens change every time they are refreshed); the newest
-- row per viewer wins, and deleted = 1 is the viewer signed out. Old rows go when parts merge,
-- and every row goes after 180 days without a save (a sign-in unused that long has run out anyway).
CREATE TABLE IF NOT EXISTS filter_logins
(
    viewer_did  String,
    sealed      String,
    deleted     UInt8 DEFAULT 0,
    updated_at  DateTime64(3, 'UTC')
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY viewer_did
TTL toDateTime(updated_at) + INTERVAL 180 DAY;

-- The posts a filtered feed left out of a viewer's feed, and which filter did (feedgen.LeftOutReason),
-- for the viewer to look back at on /filtered. A post read again is a row again; the page shows each
-- once. Kept for a week.
CREATE TABLE IF NOT EXISTS filtered_left_out
(
    viewer_did   String,
    feed         LowCardinality(String),    -- the filtered feed's rkey
    uri          String,                    -- the post
    reason       LowCardinality(String),    -- label | unscored | topic | tone | signal
    name         String,                    -- the label, topic or score
    value        Float32,                   -- the post's probability or score
    cutoff       Float32,                   -- the filter's
    bound        LowCardinality(String),    -- max | min
    rule         String,                    -- the topic whose own rules decided, or ''
    left_out_at  DateTime64(3, 'UTC')
)
ENGINE = MergeTree
PARTITION BY toYYYYMMDD(left_out_at)
ORDER BY (viewer_did, feed, left_out_at)
TTL toDateTime(left_out_at) + INTERVAL 7 DAY;

-- The filters each viewer chose for a filtered feed (the page at /filtered). feed is the feed's
-- rkey; filters is JSON, feedgen.Filters. One row per save; the newest per (viewer, feed) wins.
CREATE TABLE IF NOT EXISTS viewer_filters
(
    viewer_did  String,
    feed        String,
    filters     String,
    updated_at  DateTime64(3, 'UTC')
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY (viewer_did, feed);
