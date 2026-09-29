-- Self-label values and per-media blob references on posts (image text step and the
-- label policy). Idempotent; `make schema` re-runs every file. Rows written before this
-- change get empty arrays.

ALTER TABLE posts ADD COLUMN IF NOT EXISTS self_labels     Array(LowCardinality(String)) AFTER has_labels;  -- e.g. porn, sexual, nudity, graphic-media
ALTER TABLE posts ADD COLUMN IF NOT EXISTS media_kinds     Array(LowCardinality(String)) AFTER self_labels; -- image|video, one per attachment
ALTER TABLE posts ADD COLUMN IF NOT EXISTS media_cids      Array(String)                 AFTER media_kinds; -- blob CID, aligned with media_kinds
ALTER TABLE posts ADD COLUMN IF NOT EXISTS media_alt_texts Array(String)                 AFTER media_cids;  -- alt text or "", aligned with media_kinds
