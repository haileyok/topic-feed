-- The image archive (internal/imagearchive, cmd/images): every picture of the posts we
-- labeled, downloaded once so relabeling and training can read the same pixels as often as
-- they like. Two tables, both replaced row by row (read them with FINAL). Idempotent.

-- One row per post that was looked at: what the AppView showed for it, and the feed policy its
-- labels (the post's and its author's) give. The archive never fetches the pictures of a post
-- whose policy is "drop".
CREATE TABLE IF NOT EXISTS post_image_resolve
(
    uri          String,
    did          String,
    outcome      LowCardinality(String),   -- found | no_images | gone (the AppView has no view of it)
    n_images     UInt8,                    -- pictures listed in post_images
    policy       LowCardinality(String),   -- ok | adult_only | drop; empty when the post has no media
    labels       Array(String),            -- label values behind the policy
    resolved_at  DateTime64(3, 'UTC'),
    updated_at   DateTime64(3, 'UTC')
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY uri;

-- One row per picture of a post: an attached image or gallery item at full size, a video's
-- poster frame, or a link card's preview picture. Files live under the archive root, named by
-- the SHA-256 of their bytes, so a picture posted twice is stored once.
CREATE TABLE IF NOT EXISTS post_images
(
    uri          String,
    idx          UInt8,                    -- position among the post's pictures
    kind         LowCardinality(String),   -- image | video_thumb | link_card
    cid          String,                   -- blob CID the CDN link carries (a video's CID for video_thumb)
    url          String,                   -- the CDN link to fetch
    policy       LowCardinality(String),   -- the post's feed policy when it was resolved
    status       LowCardinality(String),   -- pending | ok | gone | error | bad_image | skipped_policy | purged
    attempts     UInt8,                    -- fetches tried
    sha256       String,                   -- of the file's bytes; empty until fetched
    width        UInt16,
    height       UInt16,
    bytes        UInt32,
    error        String,                   -- why the last fetch failed
    fetched_at   DateTime64(3, 'UTC'),
    updated_at   DateTime64(3, 'UTC')
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY (uri, idx);
