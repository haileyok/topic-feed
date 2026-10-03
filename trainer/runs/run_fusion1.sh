#!/bin/bash
# Trains the fusion student (frozen SigLIP 2 picture embeddings + fine-tuned Ettin-150M text encoder) on the 5090 box
# (tmux session fusion1). Same data, split and settings as the 8-epoch run_mm2.sh: 8 epochs, batch 32, lr 5e-5.
cd /root/mm/trainer
export PYTHONPATH=/root/tb-overlay
LOG=/root/mm/train-fusion1.log
echo "train start $(date -u +%FT%TZ)" > $LOG
/root/clef-venv/bin/python train_fusion.py \
  --export /root/mm/v21 --feats /root/mm/feats/siglip2-so400m-512 --out /root/models/fusion1 \
  --taxonomy /root/mm/taxonomy/v2.1.yaml --epochs 8 --bs 32 --lr 5e-5 \
  --tb /root/mm/tb/fusion1 >> $LOG 2>&1
echo "train ended exit $? $(date -u +%FT%TZ)" >> $LOG
