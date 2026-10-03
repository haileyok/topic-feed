# Plan: a Qwen3.5-2B student fine-tuned with LoRA

Status: plan only, nothing run. Written 2026-10-03. Owner decisions needed are in section 13.

## 1. What this is for

The fusion model (fine-tuned Ettin-150M text encoder + frozen SigLIP 2 picture embeddings) is the current choice. This plan describes one bounded
trial of a bigger student: **Qwen3.5-2B** (a 2B-parameter language model with a built-in picture encoder), adapted with LoRA, trained on exactly the same
labels, split and scoring as the fusion model, so the numbers can be compared directly. The trial answers one question: *does a larger, general-purpose
language model read these posts measurably better than the fusion model, by enough to pay for what it costs to serve?*

Expectation, stated up front so the trial is not read as a promise: I expect little or no gain on text and a small gain on pictures (section 2). The plan is built so
that the cheap part runs first and the expensive part only runs if the cheap part shows a gain.

## 2. Where we are and why gain is doubtful

Held-out test: the newest 8% of each teacher's posts (8,384 text and link-card posts against Jev; 2,964 picture posts against Clef-flash).
"Shared" = probability two lists have in common (1 = identical).

| broad topic | text (vs Jev) | pictures (vs Clef-flash) |
|---|---|---|
| ModernVBERT, 3 epochs | 0.742 | 0.624 |
| ModernVBERT, 8 epochs | 0.753 | 0.694 |
| fusion model (current) | 0.758 | 0.781 |
| Clef-flash (calibrated) vs Jev, 5,000 text posts | 0.683 | n/a |

- Text: three students with different text encoders and training lengths land within 0.016 of each other, and the two teachers share only 0.683 with each other.
  That pattern says the limit on text is the labels, not the student's size. A bigger student would mostly copy the same noise more closely.
- Pictures: the fusion model already disagrees with Clef-flash (top pick outside Clef-flash's main topics) on 4% of test posts, down from 12% for ModernVBERT.
  Judged blind on 25 of those, it was about even with Clef-flash (model better 6, Clef-flash better 8, both fine 2, neither 1, can't tell 8). Little room is left to win.
- Labels cover 142,696 posts (labelling was stopped at 105,962 of 175,304 text posts, 104,800 of them used in training, and 38,546 of 67,711 picture posts, 37,896 used), so a larger model may also be more data-limited than the small one.

What would make me wrong: a decoder model that has seen far more text than the 150M encoder may resolve the humor / personal_life / unclear confusions
that dominated the text misses in the earlier blind check of the 8-epoch ModernVBERT student (humor vs personal_life 4, sports vs humor 3 of its 25 judged text disagreements; not repeated for the fusion model).
That is the thing the first run tests.

## 3. Success criteria (decided before running; owner may change the thresholds)

Compared on the same test posts with the same scorer (`trainer/mm_score.py`), each score as a paired difference per post with a bootstrap 95% interval:

| gate | threshold to count as better than the fusion model |
|---|---|
| text, broad shared with Jev | at least +0.015 (0.758 to 0.773), interval above 0 |
| pictures, broad shared with Clef-flash | at least +0.020 (0.781 to 0.801), interval above 0 |
| no regressions | subtopic, tone and meme scores each no worse than 0.01 below the fusion model |
| human check | on 40 posts where the two models' top broad topics differ, the owner judges blind: Qwen better is not fewer than fusion better |
| cost | measured serving cost per post is within the owner's budget (section 11) |

All five must hold to adopt it. The thresholds are my proposal: roughly the size where a gain stops being noise (single seed, 11,348 test posts) and starts being visible
in a feed. If the text gate fails in step 5, the picture run is not started.

## 4. The model

Qwen3.5-2B (`Qwen/Qwen3.5-2B`; Apache-2.0 per the Hub API; 2,274,069,824 parameters in the safetensors files, text and picture parts together).
From its model card: 24 layers, hidden size 2048, layer pattern 6 x (3 x (Gated DeltaNet, then feed-forward) + 1 x (full attention, then feed-forward)),
vocabulary 248,320 with the output layer tied to the token embedding, picture encoder taken from Qwen3-VL, early-fusion multimodal training.
About 0.5B of the 2.27B parameters are token-embedding rows that cost no compute; the transformer body is roughly 1.5B and the picture encoder roughly 0.3B
(the 0.3B is my inference from the size of its picture-encoder file in a third-party conversion, not a published figure).

Why this one: it is a small, current, natively multimodal model in the same family as Clef-flash (the 9B teacher for picture posts; same tokenizer and
Qwen3.5 code in transformers 5.17, which we already run). That family tie is a hope, not a measured benefit.

Alternatives if it fails to load or train well:
- `Qwen/Qwen3-VL-2B-Instruct` (Apache-2.0): ordinary attention layers, no DeltaNet kernels needed, a mature LoRA path. Slightly older.
- `Qwen/Qwen3.5-0.8B`: the cheap size, same recipe, to see how the result scales.
Not considered: models with a different tokenizer family, or larger than 4B (they do not fit the serving story).

## 5. Student design

Use the backbone **without the language-model output layer**. The output layer has 248,320 outputs per token and would dominate memory and time for no use.
The class that holds only the backbone is `Qwen3_5Model` (or `Qwen3_5ForConditionalGeneration(...).model`; the weight names `model.language_model.*` and `model.visual.*`
show it exists as a part). Confirm which in the smoke test.

```
post text + up to 2 pictures
   -> Qwen3.5-2B backbone (LoRA on the language layers, picture encoder frozen)
   -> mean of the last hidden states over all real tokens (text and picture tokens)
   -> dropout 0.1
   -> four linear heads: broad (25), subtopic path (118), signals (11 incl. meme), tone (6)      [same heads, losses and weights as the fusion model]
```

- Same targets as before: the teachers' full probability lists (soft cross-entropy), loss weights broad 1, path 1, signals 0.5, tone 0.3, each post weighted by teacher
  confidence with a floor of 0.3, meme/tone masked where a post has no label. All of this is already in `trainer/train_mm.py` (`Rows`, `losses_for`, `score`, `fit_temperature`) and is reused unchanged.
- Pooling: mean over tokens, as the ModernVBERT student did. A causal decoder only sees earlier tokens, so a variant that appends a fixed closing phrase and reads the last token is the one
  planned ablation (section 8).
- Input text: the same rendered post string as before (`student_string`, cut at 1,800 characters). No chat template or instruction: this is a classifier, not a chat model.
  Pictures are inserted with the model's own picture markers.
- LoRA on the language layers only: rank 32, alpha 64, dropout 0.05, on every linear layer of the language model. Names from the 9B weight index (the 2B uses the same code; confirm on the 2B):
  DeltaNet layers `linear_attn.in_proj_qkv`, `in_proj_z`, `out_proj`; full-attention layers `self_attn.q_proj`, `k_proj`, `v_proj`, `o_proj`; all layers `mlp.gate_proj`, `up_proj`, `down_proj`.
  The small `in_proj_a`, `in_proj_b`, the convolution, norms and `A_log` stay frozen. Match with a name pattern that requires `language_model` so the picture encoder (`model.visual.*`) is not touched.
- Picture encoder and its merger frozen at first (unfreezing the merger is an ablation).
- Picture size: cap each picture at 256 tokens (the model turns every 32x32 pixels into one token, so about 512x512 pixels, the same size the SigLIP 2 model sees). Sizes 128 and 512 are ablations.
- Optimiser: AdamW, weight decay 0.01; LoRA learning rate 2e-4, heads 1e-3; 5% warm-up then linear decay (the fusion model's schedule); gradient clipping 1.0; effective batch 32; bfloat16 base weights with the
  adapter in float32; gradient checkpointing on; 3 epochs with validation twice per epoch, keeping the best checkpoint by the same rule as before (mean broad top pick over the two sources).
  The learning rate is my starting point from common LoRA practice, not tuned for this task; the first benchmark run also looks at the early loss curve to catch a rate that is plainly too high or low.
- Batching: batches are all-picture or all-text, as in the ModernVBERT run. Text posts are about 53 tokens and picture posts about 390, so mixing them would pad text posts about 7 times over.
  Within each kind, sort by length in buckets. (Whether the Qwen processor accepts mixed batches does not matter then, but check it in the smoke test in case it is wanted.)

## 6. Data and split (nothing new to build)

Identical to the fusion model: `/data/mm/v21/labels.jsonl.gz`, 142,696 rows (Jev 104,800 text and link-card posts; Clef-flash 37,896 picture posts), taxonomy v2.1, Clef-flash calibration 2.1,
split per teacher by time (train 125,674, validation 5,674, test 11,348), pictures from `/data/images/1000` (already at `/root/images/1000` on the box), at most 2 pictures per post.
Measured on a sample of the real rendered inputs with the Qwen3.5 tokenizer: text and link-card posts average 53 tokens (median 45, 95th percentile 127, longest 301, n = 5,240);
picture posts average 61 text tokens (median 45, 95th percentile 159, longest 551, n = 1,895) and 1.27 pictures each.

## 7. Compute estimate

All of this is an estimate; step 3 replaces it with a measurement.

- Tokens per epoch: text and link-card training posts about 92k x 53 = 4.9M; picture training posts about 33k x (61 + 1.27 x 256) = 12.9M. Total about 18M, of which 73% are picture tokens.
- Work: about 4 x 1.5e9 operations per token for the backbone (forward plus backward through the frozen weights; LoRA adds little), 33% more with gradient checkpointing, plus the picture encoder forward
  on about 42k pictures per epoch. Total about 1.7e17 operations per epoch.
- Speed: short sequences and the DeltaNet layers keep a GPU well below its peak. At 20 to 50 trillion useful operations per second on the RTX 5090 that is about 1 to 2.5 hours per epoch; 3 epochs is
  **about 3 to 7 hours**, against 37 minutes for the whole fusion run. The text-only first run (step 5) is about a quarter of that: roughly 0.3 to 0.7 hours per epoch.
- Memory: 4.5 GB of base weights in bfloat16, adapters and optimiser state in the hundreds of MB, activations small with checkpointing at batch 32 and 256 tokens per picture. It should fit in the 32 GB with room to spare.
- On the local RTX 4090 instead (24 GB; about 21.5 GB free, another process already holds about 3 GB): it fits, because the base weights are 4.5 GB and the activations are small (my estimate; the smoke test
  confirms, and a smaller micro-batch with gradient accumulation is the fallback). It is slower: I earlier estimated a 5090 at 1.5 to 2 times a 4090 for this kind of work (third-party benchmarks, not measured here),
  which would make run A about 1.5 to 4 hours and the full run about 5 to 14 hours. The 4090 did run the 9B Clef-flash at about 7.8k tokens per second on long prompts with the `fla` kernels (measured earlier),
  so this model family runs well on it. Advantages: the export and the 51 GB picture archive are already on this machine, nothing to rent, no box to lose; `fla` is available as an overlay at
  `/data/clef/fla-overlay`. Needs: `peft` installed the same way (an overlay, leaving the existing environments untouched) and Qwen3.5-2B downloaded. Cost: the GPU is tied up for the whole run, and the model cache
  suggests it is also used for video and other work. It needs the owner's explicit OK before any use.
- Rental cost (5090 box): the hourly rate was never told to me, so I cannot give dollars. Hours x rate is the whole cost (the Jev labelling API is not involved, except the small check in step 1).

## 8. Experiments (kept small on purpose)

| run | what | when |
|---|---|---|
| A: text-only | LoRA on text posts only (all rows with no pictures); compared on the 8,384 text test posts | first |
| B: full | text and picture posts together, the design in section 5 | only if A passes the text gate |
| ablation 1 | last-token pooling with a closing phrase instead of mean pooling | only if B passes |
| ablation 2 | 128 or 512 tokens per picture (whichever way B suggests) | only if B passes |

Run A is deliberately conservative: it has seen fewer examples than the fusion model (no picture posts), so if even it beats the fusion model on text, a gain is real. If it only ties or loses, the text
side is label-limited and the rest of the plan should not be spent. Run B then tests pictures. At most two ablations; there is no hyper-parameter search (single seed, no budget for it).

## 9. Steps, with gates

1. **Box health (blocker as of this writing).** The 5090 box's GPU stopped working during this session: `nvidia-smi` reports a driver/library version mismatch and PyTorch fails with CUDA error 804.
   The box's package log shows the NVIDIA driver packages were upgraded from 580.95.05 to 580.178.04 (the log was last written at 06:51 UTC on 2026-10-03) while the running kernel module is still 580.95.05, which would produce
   exactly this error. The card was working earlier today (the fusion run finished at 05:54 and `nvidia-smi` still listed the card when I queried it at about 06:40). I did not cause it; I only ran read-only commands on the box. Options for the owner: reboot or reload the driver module on the box (a reboot of a rented box has a small risk
   it does not come back), rent a fresh box, or use the local RTX 4090 (24 GB; it fits this job but is slower, and I will not use it without explicit OK).
2. **Ceiling check (no GPU, about 25 cents).** Relabel 2,000 text test posts a second time with Jev (the labelling run cost about $0.12 per 1,000 posts) and measure how closely Jev agrees with itself. If Jev agrees
   with itself at about 0.76 shared, the fusion model is already at the ceiling and the trial cannot show a text gain: stop here. If it is clearly higher (say 0.80 or more), there is room.
3. **Environment and benchmark (about 1 hour).** On the box: install `peft` (not installed; `fla` and `einops` are, `causal_conv1d` is not, so the DeltaNet convolution step runs a slower fallback, which the benchmark includes), download Qwen3.5-2B
   (about 4.5 GB), confirm the backbone class and LoRA target names, run a smoke test on 64 posts (loss falls, no NaN, picture and text batches both work, adapter saves and reloads), then time 200 training steps on each kind of batch.
   **Gate:** projected time for run A under 2 hours and for run B under 8 hours; otherwise reduce picture tokens, drop to Qwen3.5-0.8B, or stop.
4. **Training scripts.** New `trainer/train_qwen_lora.py` (model, batching, loop, checkpointing every half epoch with resume, same output files as `train_fusion.py`: `metrics.json`, `config.json`, `test-probs.npz`, adapter and head weights)
   and `trainer/runs/qwen_*.sh`. It imports `train_mm.py` for rows, split, losses and scoring so nothing is re-implemented. Checkpoints are copied off the box after every epoch, since the box can be lost.
5. **Run A (text-only).** About 1 to 2 hours. **Gate:** the text gate in section 3. If it fails, write up the result and stop.
6. **Run B (full).** About 3 to 7 hours. Evaluate with the same scorer and temperature fitting as the fusion model, plus paired bootstrap intervals against the fusion model's saved test probabilities (`/data/models/fusion1/test-probs.npz`).
7. **Blind check.** The owner judges 40 posts where the two models' top broad topics differ (a small page like the earlier ones; the model order hidden and shuffled). Takes the owner about 20 to 30 minutes.
8. **Serving measurement.** On the same box, per post: milliseconds, posts per second at batch 32, and peak memory for Qwen and for the fusion model, text and picture posts separately.
9. **Report and decision**, using the gates in section 3. If it passes, a follow-up plan covers merging the adapter, the standalone inference script and a Hugging Face package like the fusion model's; none of that is part of this trial.

Active work for me is small; most of the wall-clock time is steps 5 and 6 running. Roughly one working day end to end if the box is healthy.

## 10. Risks

| risk | what happens | what I do about it |
|---|---|---|
| box GPU broken (now) | nothing can train | step 1; other hardware options |
| box lost mid-run | hours lost | checkpoint and copy off after every epoch; the run resumes |
| DeltaNet layers slow without the convolution kernel | run takes 2x longer than estimated | measured in step 3 before committing; gate on projected hours |
| LoRA target names differ on the 2B, or the backbone class differs | smoke test fails | fix in step 3 (cheap); fall back to Qwen3-VL-2B-Instruct |
| PEFT does not support the DeltaNet linear layers | cannot train those layers | adapt only the attention and feed-forward layers, or fall back to Qwen3-VL-2B-Instruct |
| learning rate wrong for LoRA | bad or unstable training | benchmark shows the first 200 steps; one retry at half or double |
| bigger student overfits 142k posts | validation turns up early | validate twice per epoch and keep the best checkpoint; 3 epochs, not 8 |
| result within noise | cannot call it | the gates require an interval above zero and a minimum size; single seed is stated in the report |
| the labels, not the model, are the limit | no gain | step 2 and run A are designed to show this early and cheaply |

## 11. What serving would look like

The fusion model has about 150M parameters on text posts and about 580M on picture posts. Qwen3.5-2B is about 1.5B of body compute on every post plus about 0.3B for each picture, so roughly 10 times the
compute on text posts and 3 to 4 times on picture posts, and 4.5 GB or more of weights (plus the picture encoder) resident. Those ratios come from parameter counts; the real numbers come from step 8.
Short posts (53 tokens on average) keep the absolute cost per text post low; picture posts at 256 tokens per picture cost about 7 times a text post.
It still needs a GPU. Unknown and needed before adopting: how many posts per second the feed must classify and what hardware is available. The adapter can be merged into the base weights, so serving does not
need the LoRA code at inference time.

## 12. Out of scope

Training on more labels (labelling was stopped by the owner), a multilingual text model, a new hand-judged evaluation set, changes to the live feeds or the deployed classifier, distilling the result further,
and anything about publishing.

## 13. Decisions needed from the owner

1. How to fix the box: reboot or reload the NVIDIA driver on the current box, rent a fresh one, or OK for the local RTX 4090 (slower, free).
2. The hourly rental price of the 5090 box (to turn the hours into a budget), and the largest total time or money you are willing to spend on this trial.
3. The serving budget (posts per second, hardware, whether a GPU is available at all). Without it the cost gate in section 3 cannot be applied.
4. Whether the thresholds in section 3 (+0.015 text, +0.020 pictures) are the right bar for you.
5. Whether to spend the 25 cents and the few minutes on the ceiling check first (recommended: it can end the trial before any GPU time is used).

## 14. What was checked and what is an estimate

Checked today: the Qwen3.5-2B facts above (model card and Hub API: Apache-2.0, 2,274,069,824 parameters, layer layout); the token counts and picture counts (measured on samples of the real export with the Qwen3.5 tokenizer);
the LoRA target module names (read from the 9B Clef-flash weight index; the 2B is assumed to match); what is installed on the box (`fla`, `einops`, `accelerate`, `triton` present; `peft`, `flash_attn`, `causal_conv1d`, `liger_kernel`, `kernels` absent);
the box GPU fault and the driver upgrade in its package log; Jev's cost per 1,000 posts ($13.01 for 105,962 posts).
Estimated, not measured: all training times and memory (section 7), the serving ratios (section 11), the thresholds (section 3), the LoRA settings (section 5).
Not checked: whether PEFT handles the DeltaNet layers, whether the Qwen processor accepts mixed text and picture batches, the 5090's actual speed on this model, and the exact number of tokens per picture
(the 32x32-pixels-per-token figure assumes the Qwen3-VL picture encoder's 16-pixel patches merged 2x2, which the Qwen3.5 model card says it reuses; confirm on the 2B in the smoke test).
