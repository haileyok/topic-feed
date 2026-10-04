---
license: mit
library_name: pytorch
pipeline_tag: text-classification
language:
  - en
tags:
  - topic-classification
  - multimodal
  - bluesky
base_model: jhu-clsp/ettin-encoder-150m
base_model_relation: finetune
---

# microblog-topic-classifier-v5

Classifies a short social-media post, using its text and up to two pictures.

| output | contents |
|---|---|
| `broad` | probabilities over 25 broad topics (`sports`, `us_politics`, `art`, ..., `unclear`) |
| `paths` | probabilities over 118 subtopics (`sports/baseball`, `art/fan_art`, ...) |
| `signals` | 11 scores from 0 to 1: substance, news, promo, general_interest, sentiment, critical, ad, engagement_bait, spam, self_promo, meme |
| `tone` | probabilities over 6 tones: informative, humorous, personal, outraged, supportive, other |

`topics.json` lists every topic (taxonomy v2.1) with its name and description.

## Architecture

- **Text:** the post text, with alt text, link card, quoted post and hashtags (see `render_post`), goes through a fine-tuned
  [Ettin-150M](https://huggingface.co/jhu-clsp/ettin-encoder-150m) encoder, mean pooled.
- **Pictures:** up to two go through a frozen [SigLIP 2 so400m (512 px)](https://huggingface.co/google/siglip2-so400m-patch16-512) image encoder,
  then are projected and averaged. A learned vector stands in for posts without pictures.
- **Heads:** the text vector, picture vector and their product feed a small network with four output heads.

`model.safetensors` (151.8M parameters) holds the text encoder, picture projection and heads. SigLIP 2 is downloaded from its own repository
on first use. A post with pictures runs about 580M parameters; a text-only post runs only the text encoder.

## Usage

```python
import sys
from huggingface_hub import snapshot_download

folder = snapshot_download("haileyok/microblog-topic-classifier-v5")
sys.path.insert(0, folder)
from topic_classifier import TopicClassifier, render_post

clf = TopicClassifier.from_pretrained(folder)  # uses a GPU if available
post = {"text": "Great win for the Braves tonight", "media_kinds": ["image"]}
out = clf.predict([{"text": render_post(post, with_pictures=True), "pictures": ["braves.jpg"]}], top_k=3)
print(out[0]["top_broad"], out[0]["broad"], out[0]["signals"]["meme"])
```

- `pictures` accepts file paths, PIL images or encoded bytes. Unreadable pictures are skipped.
- For posts with pictures, pass `with_pictures=True` to `render_post`, which drops text extracted from the pictures, as in training.
  For text-only posts, omit `pictures`.
- `predict_arrays(items)` returns NumPy arrays, plus `pictures_used` per post.
- `render_post` documents the fields it reads (`text`, `tags`, `media_kinds`, `labels`, `media_alts`, `link_*`, `quote_text`, ...).
  Any other renderer must match `postdoc_version` in `config.json` (`pd2`).
- Text is cut to 1,800 characters and 512 tokens. Broad and subtopic probabilities are temperature scaled (1.146 and 1.22, fitted on validation posts).
- Requires `torch`, `transformers` (tested with 5.17.0), `safetensors`, `pillow` and `huggingface_hub`.

## Training

Public Bluesky posts from 2026-09-25 to 2026-09-29, labelled by two LLM teachers, not humans. Each teacher gave a full probability list per post,
and the model was trained to reproduce those lists.

| teacher | posts | rows |
|---|---|---|
| Jev | text only, or with a link card | 104,800 |
| Clef-flash 9B | with pictures or video (shown to the teacher) | 37,896 |

- Clef-flash's probabilities were calibrated to Jev's on about 5,000 posts both had labelled.
- Loss: soft cross-entropy on broad topics and subtopics (weight 1 each), signals 0.5, tone 0.3. Each post is weighted by the teacher's confidence (minimum 0.3).
- Split by time within each teacher: 125,674 train, 5,674 validation, 11,348 test (the newest 8%).
- 8 epochs, batch size 32, learning rate 5e-5 (encoder) and 1e-3 (other layers), bfloat16, one RTX 5090, 37 minutes, one seed.
- Kept epoch 7, which had the best validation broad-topic top pick averaged over both teachers (78.0%, vs 77.7% for epoch 8).
- `meme` was only trained on picture posts; ignore it for text-only posts.

![Loss and validation scores per epoch](images/training.png)

![Training labels by broad topic, per teacher](images/training-data.png)

## Results

On held-out test posts, compared with each post's teacher: 8,384 text posts (Jev) and 2,964 picture posts (Clef-flash).
**This measures agreement with the teacher, not correctness.**

| | text | pictures |
|---|---|---|
| broad topic: probability shared with the teacher (1 = identical) | 0.758 | 0.781 |
| broad topic: top pick among the teacher's main topics¹ | 90% | 96% |
| broad topic: same top pick | 75.5% | 78.3% |
| subtopic: probability shared with the teacher | 0.658 | 0.664 |
| subtopic: same top pick | 65.7% | 69.0% |
| subtopic: top pick among the teacher's main subtopics¹ | 84% | 90% |
| tone: probability shared | 0.791 | 0.811 |
| meme: ROC AUC | n/a | 0.970 |

¹ The fewest topics holding 80% of the teacher's probability.

![Agreement with the teachers](images/results.png)

The teachers agree with each other less than the model agrees with them. On the same 5,000 text posts, calibrated Clef-flash shares 0.683 of
Jev's broad-topic probability, and its top pick is among Jev's main topics 85% of the time.

The model's top broad topic falls outside the teacher's main topics on 849 text posts (10%) and 120 picture posts (4%). In a blind comparison of
25 of those picture posts, a reviewer preferred the model 6 times, Clef-flash 8 times, both 2, neither 1, and couldn't decide 8. The sample is too
small to separate them. Text-post disagreements were not reviewed.

Disagreement concentrates where the teacher was unsure. When the teacher gave its top topic at least 80%, the model matched it on 91% of text posts
and 98% of picture posts; below 50%, on 40% and 49%.

![Agreement by teacher confidence](images/teacher-confidence.png)

The model's confidence tracks agreement: the higher its top probability, the more often it matches the teacher.

![Agreement by model confidence](images/confidence.png)

![Agreement per broad topic](images/per-topic.png)

The most common swaps are `humor`/`personal_life` on text posts and `art`/`gaming` on picture posts.

![Topic pairs swapped most often](images/swaps.png)

Signal scores differ from the teacher's by 0.01 to 0.12 on average: least for `engagement_bait` and `spam`, most for `sentiment` and `critical`.

![Signal score differences](images/signals.png)

Full scores and per-epoch validation history are in `metrics.json`.

## Limitations

- Agreement with a teacher is not accuracy. Where both teachers are wrong, the model is too.
- English only. The text encoder was not trained on other languages.
- Posts with pictures need a GPU for practical speed. The picture encoder alone runs at about 155 pictures/s on an RTX 5090.
- Trained on five days of posts from one platform. The test set is the newest 8% of those days, so it doesn't show how the model ages.
- `unclear` is a real topic (bare links, emoji-only replies, fragments), not an error.
- One training run: differences of a point or two from other models are noise.

## Files

| file | contents |
|---|---|
| `model.safetensors` | weights |
| `config.json` | labels, temperatures, limits |
| `topics.json` | taxonomy v2.1: ids, names, descriptions |
| `topic_classifier.py` | inference code |
| `text_encoder/`, `tokenizer/` | from Ettin-150M |
| `metrics.json` | training and test scores |
| `images/` | the charts above, made with matplotlib from `metrics.json` and the test predictions |
| `LICENSE`, `NOTICE.md` | licence and credits |

No posts or training labels are included, and the charts contain no post text.

## License

MIT. The text encoder was initialised from Ettin-150M (MIT), and `text_encoder/` and `tokenizer/` are copied from it. SigLIP 2 (Apache-2.0) is not
included. Credits are in `NOTICE.md`.
