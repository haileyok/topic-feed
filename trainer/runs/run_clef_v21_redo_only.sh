#!/bin/bash
# The owner stopped the main picture run early (2026-10-02) to get to training sooner. This redoes the
# part-1 posts (labelled before the tone and meme signals were saved), except the ones the local 4090
# already did. The posts the main run did not reach, and Jev's remaining posts, can be labelled later
# by resuming with the same commands (both runs skip what is already done).
cd /root/clef || exit 1
export PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True
echo "chain finished (main run stopped early on purpose) $(date -u +%FT%TZ)" >> out/clef-v21-chain.log
echo "redo start $(date -u +%FT%TZ)" >> out/clef-v21-chain.log
/root/clef-venv/bin/python clef_run.py --posts redo-posts.jsonl --model /root/clef-flash --images /root/images \
  --taxonomy taxonomy/v2.1.yaml --gpu-fraction 0.92 --skip out/clef-v21-backfill-4090.jsonl \
  --out out/clef-v21-pictures-redo.jsonl 2>&1 | tee -a out/clef-v21-redo.log
echo "redo ended $(date -u +%FT%TZ)" >> out/clef-v21-chain.log
echo "all finished $(date -u +%FT%TZ)" >> out/clef-v21-chain.log
