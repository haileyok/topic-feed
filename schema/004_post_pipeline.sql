-- One row per post processed by the live pipeline (cmd/pipeline), about 10 seconds after
-- ingest: the label policy decision and the text found in its images. Re-processing a
-- post replaces its row. Idempotent.

CREATE TABLE IF NOT EXISTS post_pipeline
(
    uri                 String,
    did                 String,
    indexed_at          DateTime64(6, 'UTC'),            -- from posts
    processed_at        DateTime64(3, 'UTC'),
    feed_policy         LowCardinality(String),          -- ok | adult_only | drop (config/label_policy.yaml)
    labels              Array(LowCardinality(String)),   -- self-labels and labeler labels that applied when processed
    image_texts         Array(String),                   -- one per image/video without alt text, in attachment order
    image_text_sources  Array(LowCardinality(String)),   -- ocr | luna | none (nothing usable) | budget | error, aligned
    luna_cost_usd       Float64                          -- list-price cost of this post's image descriptions
)
ENGINE = ReplacingMergeTree(processed_at)
PARTITION BY toDate(indexed_at)
ORDER BY uri;
