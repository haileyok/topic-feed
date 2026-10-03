#!/bin/bash
# Retrains the picture-capable student for 8 epochs on the 5090 box, logging to TensorBoard (tmux session mm2).
# Same settings as run_mm1.sh except: --epochs 8, --patience 8 (no early stop; the best epoch is still the one kept), --tb.
cd /root/mm/trainer
export PYTHONPATH=/root/tb-overlay HF_HUB_OFFLINE=1 PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True
LOG=/root/mm/train-mm2.log
echo "train start $(date -u +%FT%TZ)" > $LOG
/root/clef-venv/bin/python train_mm.py \
  --export /root/mm/v21 --out /root/models/mm2 --images /root/images \
  --taxonomy /root/mm/taxonomy/v2.1.yaml \
  --grad-ckpt --bs 32 --workers 16 --epochs 8 --patience 8 \
  --tb /root/mm/tb/mm2 >> $LOG 2>&1
echo "train ended exit $? $(date -u +%FT%TZ)" >> $LOG
