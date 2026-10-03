#!/bin/bash
# TensorBoard on the 5090 box, for the training runs under /root/mm/tb. Listens on the box's localhost only;
# view it through an ssh port forward: ssh -p 9305 -L 6006:localhost:6006 root@<box>
mkdir -p /root/mm/tb
export PYTHONPATH=/root/tb-overlay
exec /root/clef-venv/bin/python -m tensorboard.main --logdir /root/mm/tb --host 127.0.0.1 --port 6006 --reload_interval 10
