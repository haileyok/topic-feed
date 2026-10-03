#!/bin/bash
# Trains the picture-capable student (ModernVBERT) on the 5090 box. Started in tmux session mm1.
cd /root/mm/trainer
export HF_HUB_OFFLINE=1 PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True
LOG=/root/mm/train-mm1.log
echo "train start $(date -u +%FT%TZ)" > $LOG
/root/clef-venv/bin/python train_mm.py \
  --export /root/mm/v21 --out /root/models/mm1 --images /root/images \
  --taxonomy /root/mm/taxonomy/v2.1.yaml \
  --grad-ckpt --bs 32 --workers 16 --epochs 3 >> $LOG 2>&1
echo "train ended exit $? $(date -u +%FT%TZ)" >> $LOG
