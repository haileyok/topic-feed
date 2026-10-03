#!/bin/bash
# Runs on the 5090 after part 2 of the picture run ends, without a gap. First it makes sure part 2 is
# complete (running the same command again does nothing if it is, and finishes the job if it stopped
# early). Then it redoes the part-1 posts, which were labelled before the tone and meme signals were
# saved, except the ones the local 4090 already redid (out/clef-v21-backfill-4090.jsonl). Both steps
# resume if this script is run again.
cd /root/clef || exit 1
export PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True
PY=/root/clef-venv/bin/python
COMMON="--model /root/clef-flash --images /root/images --taxonomy taxonomy/v2.1.yaml --gpu-fraction 0.92"

while ! grep -q "chain finished" out/clef-v21-chain.log; do sleep 30; done
echo "redo script: part 2 ended, completeness check $(date -u +%FT%TZ)" >> out/clef-v21-chain.log
$PY clef_run.py --posts full_posts.jsonl $COMMON --priorities 0 --skip out/clef-v21-pictures-part1.jsonl \
  --out out/clef-v21-pictures.jsonl 2>&1 | tee -a out/clef-v21-pictures.log

echo "redo start $(date -u +%FT%TZ)" >> out/clef-v21-chain.log
$PY clef_run.py --posts redo-posts.jsonl $COMMON --skip out/clef-v21-backfill-4090.jsonl \
  --out out/clef-v21-pictures-redo.jsonl 2>&1 | tee -a out/clef-v21-redo.log
echo "redo ended $(date -u +%FT%TZ)" >> out/clef-v21-chain.log
echo "all finished $(date -u +%FT%TZ)" >> out/clef-v21-chain.log
