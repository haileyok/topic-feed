# Training the topic classifier

The live model reads a post's text and up to two of its pictures. It is a fine-tuned
Ettin-150M text encoder plus frozen SigLIP 2 so400m (512 px) picture embeddings, fused by a small
network into four heads (broad topic, subtopic, signals including meme, tone). It is trained to
reproduce two teachers' probability lists under taxonomy v2.1:

- **Jev** labels posts with text or a link card (no pictures).
- **Clef-flash** labels posts with attached pictures or a video poster frame (it sees the
  pictures), plus the meme and tone questions; its probabilities are calibrated to Jev's sharpness.

Run these from a checkout of the repo. The Python environment lives in `trainer/`. Training
itself needs a GPU (about 37 minutes for 8 epochs of 142k posts on an RTX 5090).

## 1. Label

- **Jev:** the Go labeler writes to ClickHouse `jev_labels`. It is resumable: rerun the same
  command and it skips posts already labelled.

  ```sh
  set -a; . ~/.config/topic-feed/env; set +a
  go run ./cmd/labeler -taxonomy taxonomy/v2.1.yaml -uris <file of post URIs> -source relabel \
    -batch 1 -topk 3 -max-rpm 500 -rounds 3
  ```

  The labeler prints a `label_config` hash when it starts (taxonomy, questions, post rendering,
  model); the training set is built from one such configuration.
- **Clef-flash:** runs on a GPU with 24 GB or more (it is a 9B vision-language model). The scripts in
  `trainer/runs/run_clef_v21*.sh` show the commands (`trainer/clef_run.py`, `clef_fast.py`,
  `clef_labels.py`). The pictures come from the image archive (below). Results are JSONL files.
- **Calibrate and import (optional but recommended):** `trainer/clef_calibrate.py fit` fits
  Clef-flash's sharpness against Jev's on posts both labelled; `trainer/clef_import.py` loads the
  results, raw and calibrated, into `clef_labels`.

The image archive (`make images-resolve images-fetch`, then `trainer/prepare_images.py`) keeps the
pictures of the labelled posts on disk (`/data/images/1000/...`), so labelling and training read the
same pixels (see the README's "Image archive").

## 2. Build the training set

```sh
trainer/.venv/bin/python trainer/build_mm_dataset.py --out /data/mm/v21 \
    --clef <Clef-flash result files, calibrated while building> ...
```

One row per labelled post: the rendered post text (the same document the pipeline sends), the
picture hashes (up to two), when the post was indexed, and the targets. Only Clef-flash rows that
carry the meme and tone answers are taken. Writes `labels.jsonl.gz` and `manifest.json`.

## 3. Embed the pictures

```sh
python trainer/extract_image_features.py --export /data/mm/v21 --images /data/images \
    --out /data/mm/feats/siglip2-so400m-512
```

Runs the frozen SigLIP 2 picture encoder once over every distinct picture (about 155 pictures a
second on an RTX 5090) and saves the embeddings, so training does not run it again.

## 4. Train

```sh
python trainer/train_fusion.py --export /data/mm/v21 --feats /data/mm/feats/siglip2-so400m-512 \
    --out /data/models/<RUN> --taxonomy taxonomy/v2.1.yaml --epochs 8 --bs 32 --lr 5e-5
```

Split by time within each teacher: the newest 8% of posts are the test set and the 4% before them
the validation set. The best epoch by validation top pick is kept, and temperatures for the broad
and subtopic heads are fitted on the validation set. `trainer/runs/run_fusion1.sh` is the run that
produced the live model. Outputs in `/data/models/<RUN>/`:

| File | What |
|---|---|
| `model.pt` | All trained weights |
| `config.json` | Text and picture encoders used, labels, signals, tones, temperatures, limits |
| `metrics.json` | Test scores, closeness to the teachers, per-epoch history, arguments |
| `test-probs.npz` | The model's probabilities on the test posts, for later analysis |

## 5. Score against the teachers

```sh
python trainer/mm_score.py --model /data/models/<RUN> --export /data/mm/v21 --images /data/images \
    --taxonomy taxonomy/v2.1.yaml --out score.json
```

Reports how much of the teacher's probability the model shares (1 = identical), the extra cost
of using the model's list instead of the teacher's, and how often the model's top pick falls inside
the few topics that hold 80% of the teacher's probability, split by teacher and by how sure the
teacher was. Agreement with a teacher is not human accuracy: check by hand (below).
`trainer/analysis/fusion_report_plots.py` draws charts and a summary from these files.

## 6. Package, verify, deploy

```sh
python trainer/package_fusion.py --model /data/models/<RUN> --out /data/models/<NAME>
python trainer/verify_fusion_package.py --package /data/models/<NAME> --export /data/mm/v21 \
    --render --posts /data/clef/full_posts.jsonl
python trainer/verify_fusion_package.py --package /data/models/<NAME> --export /data/mm/v21 \
    --images /data/images --predict --probs /data/models/<RUN>/test-probs.npz --n 20
```

`package_fusion.py` converts the weights to safetensors and writes the model folder the classifier
service reads: `model.safetensors`, `config.json` (with the post document version), the text
encoder's config and tokenizer, `topics.json`, and a copy of `topic_classifier.py`. The first
verification checks that the post document the Go pipeline renders matches the training text; the second
that the packaged model reproduces the test probabilities. Then point `MODEL_DIR` in
`deploy/systemd/topic-feed-classifier.service` at the folder and follow "To switch models" in the
README. The Hugging Face card's source is `trainer/hf/microblog-topic-classifier-v5/`.

## Checking a model by hand

The judging pages (`trainer/mm_eval_sample.py` and `mm_eval_app.py` for one answer at a time,
`mm_disagree_sample.py` and `mm_eval_pair.py` for blind side-by-sides against the teacher) take
random or disagreeing test posts and record verdicts, and `mm_eval_report.py` /
`mm_eval_pair_report.py` summarise them. `trainer/consolidate_judged.py` collects every round of
verdicts into one file.
