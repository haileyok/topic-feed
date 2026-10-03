"""Trains the fusion student: a text encoder fine-tuned together with frozen, cached image-encoder embeddings.

Same data, targets, losses, split and scoring as train_mm.py (it imports them), but the pictures are not run through
a vision-language model: extract_image_features.py has already turned each picture into one embedding from a strong
frozen image encoder. A post's text goes through the text encoder (mean pooled); its pictures' embeddings (at most
--max-images) are projected and averaged, or replaced by one learned "no picture" vector for posts without pictures;
the text and picture vectors and their elementwise product go through a small network to the same heads as before
(broad topic, subtopic path, signals including meme, tone).

    python train_fusion.py --export /root/mm/v21 --feats /root/mm/feats/siglip2-so400m-512 --out /root/models/fusion1 \\
        --taxonomy /root/mm/taxonomy/v2.1.yaml --epochs 8 --tb /root/mm/tb/fusion1
"""

import argparse
import json
import math
import os
import sys
import time
from pathlib import Path

import numpy as np
import torch
import torch.nn as nn
import torch.nn.functional as F

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import common  # noqa: E402
import mm_score as M  # noqa: E402
import train_mm as T  # noqa: E402


class FusionStudent(nn.Module):
    def __init__(self, text_model: str, img_dim: int, n_broad: int, n_paths: int, n_signals: int):
        super().__init__()
        from transformers import AutoModel

        self.text = AutoModel.from_pretrained(text_model)
        h = self.text.config.hidden_size
        self.img = nn.Sequential(nn.LayerNorm(img_dim), nn.Linear(img_dim, h), nn.GELU())
        self.no_img = nn.Parameter(torch.zeros(h))
        self.fuse = nn.Sequential(nn.LayerNorm(3 * h), nn.Linear(3 * h, h), nn.GELU(), nn.Dropout(0.1))
        self.broad, self.path = nn.Linear(h, n_broad), nn.Linear(h, n_paths)
        self.signals, self.tone = nn.Linear(h, n_signals), nn.Linear(h, len(T.TONES))

    def forward(self, input_ids, attention_mask, img, img_mask):
        hs = self.text(input_ids=input_ids, attention_mask=attention_mask).last_hidden_state
        m = attention_mask.unsqueeze(-1).to(hs.dtype)
        t = (hs * m).sum(1) / m.sum(1).clamp(min=1)
        v = self.img(img.to(t.dtype))  # [batch, pictures, h]
        w = img_mask.unsqueeze(-1).to(v.dtype)
        have = w.sum(1)
        pooled = (v * w).sum(1) / have.clamp(min=1)
        v = torch.where(have > 0, pooled, self.no_img.to(pooled.dtype).expand_as(pooled))
        z = self.fuse(torch.cat([t, v, t * v], -1))
        return self.broad(z), self.path(z), self.signals(z), self.tone(z)


class Data:
    """The posts' token ids and the rows of the cached picture embeddings each post uses."""

    def __init__(self, rows, tok, sha_index, max_images, max_len):
        self.ids = tok(list(rows.texts), truncation=True, max_length=max_len)["input_ids"]
        self.img_idx = [[sha_index[s] for s in shas[:max_images] if s in sha_index] for shas in rows.shas]
        self.max_images = max_images


def make_batch(data, feats, idx, pad_id, dev):
    L = max(len(data.ids[i]) for i in idx)
    ids = torch.full((len(idx), L), pad_id, dtype=torch.long)
    att = torch.zeros((len(idx), L), dtype=torch.long)
    img = np.zeros((len(idx), data.max_images, feats.shape[1]), np.float16)
    have = np.zeros((len(idx), data.max_images), bool)
    for b, i in enumerate(idx):
        t = data.ids[i]
        ids[b, : len(t)] = torch.tensor(t)
        att[b, : len(t)] = 1
        for k, j in enumerate(data.img_idx[i]):
            img[b, k] = feats[j]
            have[b, k] = True
    return ids.to(dev), att.to(dev), torch.from_numpy(img).to(dev), torch.from_numpy(have).to(dev)


@torch.no_grad()
def predict(model, data, feats, idx, bs, pad_id, dev):
    model.eval()
    order = np.argsort([len(data.ids[i]) for i in idx], kind="stable")  # similar lengths together: less padding
    outs = [[], [], [], []]
    for s in range(0, len(idx), bs):
        with torch.autocast(dev.type, dtype=torch.bfloat16, enabled=dev.type == "cuda"):
            o = model(*make_batch(data, feats, idx[order[s:s + bs]], pad_id, dev))
        for k in range(4):
            outs[k].append(o[k].float().cpu())
    inv = np.empty(len(order), int)
    inv[order] = np.arange(len(order))
    return [torch.cat(x)[inv] for x in outs]


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--export", required=True)
    ap.add_argument("--feats", required=True, help="path prefix of extract_image_features.py's output")
    ap.add_argument("--out", required=True)
    ap.add_argument("--taxonomy", default=str(HERE.parent / "taxonomy" / "v2.1.yaml"))
    ap.add_argument("--text-model", default="jhu-clsp/ettin-encoder-150m")
    ap.add_argument("--epochs", type=int, default=8)
    ap.add_argument("--bs", type=int, default=32)
    ap.add_argument("--eval-bs", type=int, default=128)
    ap.add_argument("--lr", type=float, default=5e-5)
    ap.add_argument("--head-lr", type=float, default=1e-3)
    ap.add_argument("--max-images", type=int, default=2)
    ap.add_argument("--max-len", type=int, default=512)
    ap.add_argument("--conf-floor", type=float, default=0.3)
    ap.add_argument("--test-frac", type=float, default=0.08)
    ap.add_argument("--val-frac", type=float, default=0.04)
    ap.add_argument("--seed", type=int, default=0)
    ap.add_argument("--smoke", type=int, default=0, help="a tiny run on this many rows per source, for checking the code")
    ap.add_argument("--tb", help="TensorBoard log folder (train and val_full runs under it)")
    ap.add_argument("--log-every", type=int, default=50)
    a = ap.parse_args()

    torch.manual_seed(a.seed)
    rng = np.random.default_rng(a.seed)
    dev = torch.device("cuda" if torch.cuda.is_available() else "cpu")
    space = common.Space.from_taxonomy(a.taxonomy)
    rows = T.Rows(a.export, space)
    tr, va, te = T.split(rows, a.test_frac, a.val_frac)
    if a.smoke:
        keep = lambda ix: np.concatenate([ix[rows.source[ix] == s][: a.smoke] for s in sorted(set(rows.source))])  # noqa: E731
        tr, va, te = keep(tr), keep(va), keep(te)
        a.epochs = 1
    info = {"rows": len(rows), "train": len(tr), "val": len(va), "test": len(te),
            "by_source": {s: int((rows.source == s).sum()) for s in sorted(set(rows.source))}, "device": str(dev)}
    print(json.dumps(info), flush=True)

    from transformers import AutoTokenizer

    meta = json.load(open(f"{a.feats}.json"))
    feats = np.load(f"{a.feats}.npy")
    sha_index = {s: i for i, s in enumerate(meta["shas"])}
    tok = AutoTokenizer.from_pretrained(a.text_model)
    pad_id = tok.pad_token_id
    data = Data(rows, tok, sha_index, a.max_images, a.max_len)
    with_pics = sum(1 for x in data.img_idx if x)
    print(f"image features {feats.shape} from {meta['model']}; {with_pics} posts use at least one; {len(meta['missing'])} pictures were unreadable", flush=True)

    model = FusionStudent(a.text_model, feats.shape[1], len(space.broad), len(space.paths), len(T.SIGNALS)).to(dev)
    enc = [p for n, p in model.named_parameters() if n.startswith("text.")]
    rest = [p for n, p in model.named_parameters() if not n.startswith("text.")]
    opt = torch.optim.AdamW([{"params": enc, "lr": a.lr}, {"params": rest, "lr": a.head_lr}], weight_decay=0.01)
    steps = a.epochs * math.ceil(len(tr) / a.bs) + 1
    warm = max(int(0.05 * steps), 1)
    sched = torch.optim.lr_scheduler.LambdaLR(opt, lambda s: min((s + 1) / warm, max((steps - s) / max(steps - warm, 1), 0.0)))

    tb_train = tb_full = None
    if a.tb:
        from torch.utils.tensorboard import SummaryWriter

        tb_train, tb_full = (SummaryWriter(f"{a.tb}/{n}", flush_secs=10) for n in ("train", "val_full"))
        tb_train.add_text("run", "```\n" + json.dumps({"args": vars(a), "data": info}, indent=1) + "\n```", 0)
    os.makedirs(a.out, exist_ok=True)

    best, best_state, best_epoch, history, t0, gstep = -1.0, None, -1, [], time.time(), 0
    for epoch in range(a.epochs):
        model.train()
        order = rng.permutation(tr)
        run, n, seen, t_log = 0.0, 0, 0, time.time()
        iv = {"n": 0, "loss": 0.0, "seen": 0, "t": time.time(), **{k: 0.0 for k in T.WEIGHTS}}
        for s in range(0, len(order), a.bs):
            j = order[s:s + a.bs]
            with torch.autocast(dev.type, dtype=torch.bfloat16, enabled=dev.type == "cuda"):
                out = model(*make_batch(data, feats, j, pad_id, dev))
            parts = T.losses_for(out, rows, j, dev)
            w = torch.tensor(np.maximum(rows.conf[j], a.conf_floor)).to(dev)
            loss = (sum(T.WEIGHTS[k] * v for k, v in parts.items()) * w).sum() / w.sum()
            opt.zero_grad(set_to_none=True)
            loss.backward()
            torch.nn.utils.clip_grad_norm_(model.parameters(), 1.0)
            opt.step()
            sched.step()
            li = loss.item()
            run += li; n += 1; seen += len(j); gstep += 1
            iv["n"] += 1; iv["loss"] += li; iv["seen"] += len(j)
            for k, v in parts.items():
                iv[k] += float((v * w).sum() / w.sum())
            if gstep % a.log_every == 0:
                if tb_train is not None:
                    tb_train.add_scalar("loss/total", iv["loss"] / iv["n"], gstep)
                    for k in T.WEIGHTS:
                        tb_train.add_scalar(f"loss/{k}", iv[k] / iv["n"], gstep)
                    tb_train.add_scalar("lr/encoder", opt.param_groups[0]["lr"], gstep)
                    tb_train.add_scalar("train/posts_per_s", iv["seen"] / max(time.time() - iv["t"], 1e-9), gstep)
                    tb_train.add_scalar("train/epoch", epoch + (s + a.bs) / len(order), gstep)
                iv = {"n": 0, "loss": 0.0, "seen": 0, "t": time.time(), **{k: 0.0 for k in T.WEIGHTS}}
            if gstep % 200 == 0:
                print(f"epoch {epoch + 1} step {gstep} loss {run / n:.3f} {seen / max(time.time() - t_log, 1e-9):.0f} posts/s", flush=True)
        preds = predict(model, data, feats, va, a.eval_bs, pad_id, dev)
        sc, vl = T.score(rows, va, preds), T.val_losses(rows, va, preds, a.conf_floor)
        if tb_full is not None:
            T.log_eval(tb_full, sc, vl, gstep)
        key = float(np.mean([v["broad_top1"] for v in sc.values()]))
        history.append({"epoch": epoch + 1, "train_loss": run / n, "val": sc, "val_loss": vl})
        print(f"epoch {epoch + 1}: train loss {run / n:.3f} val loss {vl['all']['total']:.3f} - validation " +
              " | ".join(f"{s} broad {v['broad_top1']:.1%} path {v['path_top1']:.1%}" for s, v in sc.items()) + f" - {time.time() - t0:.0f}s", flush=True)
        if key > best:
            best, best_epoch = key, epoch
            best_state = {k: v.detach().cpu().clone() for k, v in model.state_dict().items()}
    model.load_state_dict(best_state)

    vb, vp, _, _ = predict(model, data, feats, va, a.eval_bs, pad_id, dev)[:4]
    t_broad, t_path = T.fit_temperature(vb, rows.broad[va]), T.fit_temperature(vp, rows.path[va])
    lb, lp, ls, lt = predict(model, data, feats, te, a.eval_bs, pad_id, dev)
    qb, qp, qt = F.softmax(lb / t_broad, -1).numpy(), F.softmax(lp / t_path, -1).numpy(), F.softmax(lt, -1).numpy()
    src = rows.source[te]
    closeness = {"broad": M.score_head(rows.broad[te], qb, src, 0.8), "path": M.score_head(rows.path[te], qp, src, 0.8)}
    known = ~np.isnan(rows.tone[te]).any(-1)
    closeness["tone"] = M.score_head(rows.tone[te][known], qt[known], src[known], 0.8)
    metrics = {"split": info, "test": T.score(rows, te, [lb, lp, ls, lt], t_broad, t_path), "closeness": closeness, "history": history, "best_epoch": best_epoch,
               "temperature_broad": t_broad, "temperature_path": t_path, "train_seconds": time.time() - t0, "args": vars(a)}
    torch.save(model.state_dict(), f"{a.out}/model.pt")
    np.savez_compressed(f"{a.out}/test-probs.npz", idx=te, broad=qb.astype(np.float32), path=qp.astype(np.float32))
    json.dump({"type": "fusion", "text_model": a.text_model, "image_features": {"model": meta["model"], "dim": int(feats.shape[1])},
               "taxonomy_version": space.version, "broad": space.broad, "paths": space.paths, "signals": T.SIGNALS, "tones": T.TONES,
               "max_images": a.max_images, "max_len": a.max_len, "temperature_broad": t_broad, "temperature_path": t_path},
              open(f"{a.out}/config.json", "w"), indent=2)
    json.dump(metrics, open(f"{a.out}/metrics.json", "w"), indent=2)
    for w_ in (tb_train, tb_full):
        if w_ is not None:
            w_.close()
    for s, v in metrics["test"].items():
        print(f"TEST {s} ({v['n']} posts, newest): broad {v['broad_top1']:.1%} (top-3 {v['broad_top3']:.1%}) - path {v['path_top1']:.1%} "
              f"(top-3 {v['path_top3']:.1%}) - tone {v['tone_top1']}", flush=True)
    for s, d in closeness["broad"].items():
        print(f"CLOSENESS broad {s}: overlap {d['overlap_mean']:.3f} KL {d['kl_mean']:.2f} top pick inside target area {d['top_pick_in_area']:.0%} "
              f"student mass inside {d['student_mass_in_area']:.0%}", flush=True)


if __name__ == "__main__":
    main()
