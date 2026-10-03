#!/bin/bash
# Part 3 of the Clef picture run under taxonomy v2.1: the picture posts still unlabelled after part 2 was stopped on
# purpose and the earlier posts were redone (29,815 of the 67,711). Resumable: rerun to continue. Posts already in
# out/clef-v21-pictures.jsonl (part 2) or in any --skip file (part 1, its redo, the 4090's rows) are done.
cd /root/clef || exit 1
export PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True
echo "part 3 start $(date -u +%FT%TZ)" >> out/clef-v21-chain.log
/root/clef-venv/bin/python clef_run.py --posts full_posts.jsonl --model /root/clef-flash --images /root/images \
  --taxonomy taxonomy/v2.1.yaml --priorities 0 --gpu-fraction 0.92 --prefetch 3 \
  --skip out/clef-v21-pictures-part1.jsonl --skip out/clef-v21-pictures-redo.jsonl --skip out/clef-v21-backfill-4090.jsonl \
  --out out/clef-v21-pictures.jsonl 2>&1 | tee -a out/clef-v21-pictures.log
echo "part 3 exit $? $(date -u +%FT%TZ)" >> out/clef-v21-chain.log
echo "all finished $(date -u +%FT%TZ)" >> out/clef-v21-chain.log
