-- Interactions Bluesky sends back for posts our feeds served (app.bsky.feed.sendInteractions,
-- cmd/feedgen): seen, like, repost, reply, quote, share, clickthroughs, and "show more /
-- show less". One row per interaction, as received. Idempotent.
CREATE TABLE IF NOT EXISTS feed_interactions
(
    received_at   DateTime64(3, 'UTC'),
    viewer_did    String,                   -- who interacted (from their service credential)
    feed          LowCardinality(String),   -- feed rkey, e.g. nfl; '' if unknown
    item          String,                   -- post URI
    event         LowCardinality(String),   -- e.g. interactionSeen, requestLess (app.bsky.feed.defs# stripped)
    feed_context  String,                   -- the feedContext we served: {"id", "topic", "p"}
    req_id        String                    -- the reqId of the feed page the post was on
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(received_at)
ORDER BY (feed, event, received_at);
