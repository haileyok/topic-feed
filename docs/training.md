# Training the topic classifier

Run these from a checkout of the repo (the Python environment lives in `trainer/`).
Training takes 5-40 minutes, so run it inside `tmux` or `screen` if your SSH session
might drop.

## 1. Export the labels

```sh
make export LABEL_CONFIG=5697660f73fc EXPORT=/data/exports/v1-full
```

Writes one Jev label per post (never `eval` posts), with each post rendered exactly as
the model will see it, to `/data/exports/v1-full/labels.jsonl.gz`. `manifest.json`
next to it lists row counts per labeling window. `LABEL_CONFIG` is printed by the
labeler when it starts; `5697660f73fc` is the v1 full run.

## 2. (Optional) Embedding baseline

```sh
make baseline EXPORT=/data/exports/v1-full RUN=v1
```

A few minutes. Results in `/data/models/baseline-v1/metrics.json`. This is the bar the
fine-tuned model has to beat.

## 3. Train

```sh
make train EXPORT=/data/exports/v1-full RUN=v1 EPOCHS=8
```

- Trains ModernBERT-base on the GPU into `/data/models/v1`.
- Stops early once validation agreement hasn't improved for `PATIENCE` (default 2)
  epochs, and keeps the best epoch.
- Split by labeling window: newest 3 windows test, the 3 before them validation, the
  rest train.

## Watching progress

- **Terminal:** a progress bar per epoch (loss, learning rate, batches/s, time left),
  then one line per epoch with validation agreement, and a final `TEST` line.
  Everything also goes to `/data/models/<RUN>/train.log`
  (`tail -f /data/models/v1/train.log` from another terminal).
- **Browser:** TensorBoard at `http://<machine>:6006/` (the `tensorboard` compose
  service). Charts for training loss and learning rate (every 50 steps) and
  validation agreement (every epoch), with every run under `/data/models` side by
  side. It refreshes every 15 seconds.

## Outputs (`/data/models/<RUN>/`)

| File | What |
|---|---|
| `model.pt` | Weights (encoder and heads) |
| `config.json` | Base model, taxonomy version, label maps, temperatures, max length, post document version |
| `metrics.json` | Test agreement (top-1/top-3 broad and path), per-topic precision and recall, calibration, signal correlation, GPU throughput, per-epoch history |
| `data_manifest.json` | The export's manifest |
| `train.log`, `tb/` | Logs and TensorBoard events |

Agreement is measured against Jev on posts from windows the model never trained on.

## Checking a model against Jev by hand

```sh
cd trainer && uv run python compare.py --model /data/models/v1 --page
```

Writes `compare_jev.json` into the model's directory (agreement by Jev's confidence,
distribution distances, the most common confident disagreements) and a side-by-side
review page at `http://<machine>:8090/<RUN>-vs-jev/`. The page filters by agreement,
Jev's confidence, topic, and text, and has verdict buttons on each post (Jev right,
student right, both fine, neither). Verdicts are saved in your browser; "Download
verdicts" saves them as JSON, the start of the human-reviewed reference set.
