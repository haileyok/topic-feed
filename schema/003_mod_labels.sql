-- Labels and label removals from labeler streams (cmd/modlabels), as received.
-- A label's current state is its newest row per (src, uri, val): removed when neg = 1,
-- or expired when exp has passed. Labels on whole accounts have uri = the DID and
-- is_account = 1; they apply to every post by that account. Idempotent.

CREATE TABLE IF NOT EXISTS mod_labels
(
    src          LowCardinality(String),         -- labeler DID
    uri          String,                         -- at:// record URI, or a DID for an account
    subject_did  String,                         -- record author, or the account itself
    is_account   UInt8,
    cid          String,                         -- optional record version the label applies to
    val          LowCardinality(String),         -- e.g. porn, sexual, nudity, graphic-media, spam, !hide
    neg          UInt8,                          -- 1: removes an earlier label with the same src, uri, val
    cts          DateTime64(3, 'UTC'),           -- label creation time (from the labeler)
    exp          Nullable(DateTime64(3, 'UTC')), -- optional expiry
    seq          UInt64,                         -- stream sequence number
    received_at  DateTime64(3, 'UTC')
)
ENGINE = ReplacingMergeTree(received_at)
PARTITION BY toYYYYMM(received_at)
ORDER BY (uri, val, src, cts, neg);
