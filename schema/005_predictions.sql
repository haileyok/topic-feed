-- Topic predictions on post_pipeline rows (the classifier service, trainer/serve.py).
-- Dropped posts are not classified: their model is ''. Idempotent.

ALTER TABLE post_pipeline ADD COLUMN IF NOT EXISTS model        LowCardinality(String);               -- model directory name, e.g. v3-blend
ALTER TABLE post_pipeline ADD COLUMN IF NOT EXISTS model_input  String;                               -- the post document the model saw (with image text as [alt] lines)
ALTER TABLE post_pipeline ADD COLUMN IF NOT EXISTS broad_probs  Map(LowCardinality(String), Float32); -- top 5 broad topics
ALTER TABLE post_pipeline ADD COLUMN IF NOT EXISTS path_probs   Map(LowCardinality(String), Float32); -- top 8 subtopic paths
ALTER TABLE post_pipeline ADD COLUMN IF NOT EXISTS signals      Map(LowCardinality(String), Float32); -- substance, news, promo, general_interest
ALTER TABLE post_pipeline ADD COLUMN IF NOT EXISTS tone         Map(LowCardinality(String), Float32);
ALTER TABLE post_pipeline ADD COLUMN IF NOT EXISTS top_broad    LowCardinality(String);
ALTER TABLE post_pipeline ADD COLUMN IF NOT EXISTS top_path     LowCardinality(String);
ALTER TABLE post_pipeline ADD COLUMN IF NOT EXISTS top_path_p   Float32;
