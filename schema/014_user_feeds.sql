-- The feeds the feed service serves, kept here so they can be made and changed on the web
-- (the page at /feeds) rather than in config/feeds.yaml. One row per version of a feed; the
-- newest row per (owner_did, rkey) wins, and a row with deleted = 1 is the feed being removed
-- (its key stays reserved, and saving it again brings it back). Idempotent.
--
-- owner_did is the account whose repo holds the feed's app.bsky.feed.generator record. spec is
-- JSON, feedgen.FeedSpec: what the feed takes, how it ranks, and its name and description.
-- The feeds of the config file are copied in the first time the service starts with this table
-- (see feedgen.Store.SeedFeeds); after that the table, not the file, is what is served.
CREATE TABLE IF NOT EXISTS user_feeds
(
    owner_did   String,
    rkey        String,
    spec        String,
    deleted     UInt8 DEFAULT 0,
    created_at  DateTime64(3, 'UTC'),
    updated_at  DateTime64(3, 'UTC')
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY (owner_did, rkey);
