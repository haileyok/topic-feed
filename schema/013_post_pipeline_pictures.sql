-- The topic model looks at each post's first pictures instead of text found in them.
-- pictures_wanted: pictures the model should have seen (the post's first attachments, up to the model's limit)
-- pictures_used:   how many it did see; fewer means a download failed, and the retry worker tries again.
-- The older image_texts, image_text_sources and luna_cost_usd columns stay for the rows that have them
-- (the pipeline no longer writes them).
ALTER TABLE post_pipeline ADD COLUMN IF NOT EXISTS pictures_wanted UInt8 DEFAULT 0;
ALTER TABLE post_pipeline ADD COLUMN IF NOT EXISTS pictures_used UInt8 DEFAULT 0;
