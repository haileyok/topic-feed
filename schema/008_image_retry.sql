-- Posts whose image step failed (a description or thumbnail error, or skipped while
-- descriptions were paused), queued for another try by the pipeline's retry worker
-- (internal/pipeline/retry.go). One row per post; each update replaces it. Posts keep
-- appearing in feeds meanwhile with their text-only classification. Idempotent.

CREATE TABLE IF NOT EXISTS image_retry_queue
(
    uri              String,
    did              String,
    indexed_at       DateTime64(6, 'UTC'),
    status           LowCardinality(String),   -- pending | fixed | gave_up
    attempts         UInt8,                    -- retries that ran (paused tries don't count)
    next_attempt_at  DateTime64(3, 'UTC'),
    last_error       String,                   -- why the last try failed
    queued_at        DateTime64(3, 'UTC'),
    updated_at       DateTime64(3, 'UTC')
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY uri
TTL toDateTime(updated_at) + INTERVAL 14 DAY;
