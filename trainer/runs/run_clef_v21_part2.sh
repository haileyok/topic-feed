#!/bin/bash
# Part 2 of the Clef picture run under taxonomy v2.1, with the labeler that also saves the tone
# answer and asks the new meme question. Part 1 (out/clef-v21-pictures-part1.jsonl) was labelled
# without them; those posts are redone on the local RTX 4090. Resumable: rerun to continue.
cd /root/clef || exit 1
export PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True
echo "part 2 start $(date -u +%FT%TZ)" >> out/clef-v21-chain.log
/root/clef-venv/bin/python clef_run.py --posts full_posts.jsonl --model /root/clef-flash --images /root/images \
  --taxonomy taxonomy/v2.1.yaml --priorities 0 --skip out/clef-v21-pictures-part1.jsonl --gpu-fraction 0.92 \
  --out out/clef-v21-pictures.jsonl 2>&1 | tee -a out/clef-v21-pictures.log
echo "part 2 exit $? $(date -u +%FT%TZ)" >> out/clef-v21-chain.log
echo "chain finished $(date -u +%FT%TZ)" >> out/clef-v21-chain.log
