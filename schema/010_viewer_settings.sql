-- How each viewer has tuned their personal feed (the page at /me on the feed service): topic
-- weights, freshness, filters. One row per save; the latest row per viewer wins. Idempotent.
CREATE TABLE IF NOT EXISTS viewer_settings
(
    viewer_did  String,
    settings    String,                   -- JSON: feedgen.Tuning
    updated_at  DateTime64(3, 'UTC')
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY viewer_did;
