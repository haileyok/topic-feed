#!/bin/bash
# Clef-flash under taxonomy v2.1 on the 5090: first the 5,000 text-only posts that calibrate Clef to
# Jev, then every post with an attached picture or video. Both runs append to their result file
# and skip posts already in it, so rerunning this script after a crash resumes.
cd /root/clef || exit 1
export PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True
PY=/root/clef-venv/bin/python
COMMON="--model /root/clef-flash --images /root/images --taxonomy taxonomy/v2.1.yaml --gpu-fraction 0.92"

echo "calibration run start $(date -u +%FT%TZ)" >> out/clef-v21-chain.log
$PY clef_run.py --posts calib-posts-v21.jsonl $COMMON --out out/clef-v21-calib.jsonl 2>&1 | tee -a out/clef-v21-calib.log
echo "calibration run exit $? $(date -u +%FT%TZ)" >> out/clef-v21-chain.log

echo "picture run start $(date -u +%FT%TZ)" >> out/clef-v21-chain.log
$PY clef_run.py --posts full_posts.jsonl $COMMON --priorities 0 --out out/clef-v21-pictures.jsonl 2>&1 | tee -a out/clef-v21-pictures.log
echo "picture run exit $? $(date -u +%FT%TZ)" >> out/clef-v21-chain.log
echo "chain finished $(date -u +%FT%TZ)" >> out/clef-v21-chain.log
