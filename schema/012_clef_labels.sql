-- Teacher labels from Clef-flash, the 9B vision-language model we run ourselves (trainer/clef_run.py).
-- It answers the same questions as Jev (internal/labeler/questions.go), but it can look at a post's
-- pictures, so it labels the posts that have an attached image or a video poster. One row per post
-- per run, replaced row by row (read with FINAL). Loaded by trainer/clef_import.py. Idempotent.
--
-- Clef spreads its probability over more options than Jev (average confidence in its top topic about
-- 0.5 against Jev's 0.8), so each row holds two answers: the model's own, and the same answer
-- calibrated to Jev's sharpness (trainer/clef_calibrate.py). The calibrated top topic is always the
-- raw top topic. Train on the calibrated columns when mixing the two teachers.
CREATE TABLE IF NOT EXISTS clef_labels
(
    uri               String,
    taxonomy_version  LowCardinality(String),
    label_config      LowCardinality(String),   -- hash of version, taxonomy file, question set, post rendering, model, top-K, min broad p (trainer/clef_import.py)
    clef_model        LowCardinality(String),   -- clef-flash
    source            LowCardinality(String),   -- pictures (the relabel run) | calibration (text-only sample)
    post_kind         LowCardinality(String),   -- picture (image or video poster attached) | link_card | text
    n_images          UInt8,                    -- pictures the post has in the archive (up to 4 are shown)
    used_pictures     UInt8,                    -- 1 if the model was shown them
    labeled_at        DateTime64(3, 'UTC'),     -- when the row was imported; the results carry no timestamp
    input_tokens      UInt32,
    seconds           Float32,                  -- both passes together
    -- the model's own answer
    broad_probs       Map(LowCardinality(String), Float32),
    sub_probs         Map(LowCardinality(String), Float32),   -- "broad/sub": P(sub | broad)
    path_scores       Map(LowCardinality(String), Float32),   -- "broad/sub": sqrt(P(broad) * P(sub | broad))
    signals           Map(LowCardinality(String), Float32),
    -- the same answer calibrated to Jev; empty maps and an empty id when the row was not calibrated
    calibration_id    LowCardinality(String),   -- the calibration file's name, e.g. calibration-v2.1
    cal_broad_probs   Map(LowCardinality(String), Float32),
    cal_sub_probs     Map(LowCardinality(String), Float32),
    cal_path_scores   Map(LowCardinality(String), Float32),
    cal_signals       Map(LowCardinality(String), Float32)
)
ENGINE = ReplacingMergeTree(labeled_at)
ORDER BY (taxonomy_version, label_config, uri);
