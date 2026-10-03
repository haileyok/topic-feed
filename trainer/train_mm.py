"""Trains the picture-capable student: ModernVBERT (ModernBERT text encoder + SigLIP vision encoder)
with the same kinds of heads as train.py, trained to match the teachers' distributions.

Data: build_mm_dataset.py's export. Jev's text posts and Clef's picture posts train one model; a post
with pictures is shown to the model with them (at most --max-images, each as one 512 px tile), a text
post is not. Heads: broad topic and path (soft cross-entropy against the teacher's distribution),
signals (binary cross-entropy; Clef's meme is one of them and is simply missing for Jev's posts, so
those rows do not count for it), tone (soft cross-entropy). Examples are weighted by the teacher's
broad confidence, floored. The split is by time within each teacher: the newest posts are the test set,
the ones before them validate, and the rest train (posts with no timestamp only train).

    python train_mm.py --export /data/mm/v21 --out /data/models/mm1 --images /root/images
    python train_mm.py --export /data/mm/smoke --out /tmp/mm-smoke --images /data/images --smoke 24
"""

import argparse
import gzip
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
from PIL import Image

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import common  # noqa: E402
from losses import soft_ce  # noqa: E402

SIGNALS = common.SIGNALS + ["meme"]  # new signals go at the end, as in common.py
TONES = common.TONES
WEIGHTS = {"broad": 1.0, "path": 1.0, "signals": 0.5, "tone": 0.3}
MAX_CHARS = 1800  # the student text is cut here, before any image tokens are added


class Rows:
    """The export as arrays, with the targets built exactly as common.load builds them."""

    def __init__(self, export: str, space: common.Space):
        bi = {b: i for i, b in enumerate(space.broad)}
        pi = {p: i for i, p in enumerate(space.paths)}
        has_subs = {space.broad[space.path_broad[i]] for i, p in enumerate(space.paths) if "/" in p}
        self.uris, self.texts, self.shas, self.source, self.t = [], [], [], [], []
        B, P, S, T, C = [], [], [], [], []
        with gzip.open(f"{export}/labels.jsonl.gz", "rt") as f:
            for line in f:
                r = json.loads(line)
                b = np.zeros(len(space.broad), np.float32)
                for k, v in r["broad_probs"].items():
                    if k in bi:
                        b[bi[k]] = v
                if b.sum() <= 0:
                    continue
                b /= b.sum()
                p = np.zeros(len(space.paths), np.float32)
                for k, v in (r["sub_probs"] or {}).items():
                    if k in pi:
                        p[pi[k]] = b[bi[k.split("/", 1)[0]]] * v
                for name, i in bi.items():
                    if name not in has_subs:
                        p[pi[name]] = b[i]
                if p.sum() <= 0:
                    top = space.broad[int(b.argmax())]
                    p[pi[f"{top}/other" if f"{top}/other" in pi else top]] = 1.0
                p /= p.sum()
                sig = r["signals"] or {}
                S.append(np.array([sig.get(k, np.nan) for k in SIGNALS], np.float32).clip(0, 1))
                tone = np.array([sig.get("tone." + k, 0.0) for k in TONES], np.float32)
                T.append(tone / tone.sum() if tone.sum() > 0 else np.full(len(TONES), np.nan, np.float32))
                B.append(b); P.append(p); C.append(r["broad_confidence"])
                self.uris.append(r["uri"]); self.texts.append(r["model_input"][:MAX_CHARS]); self.shas.append(r["image_shas"])
                self.source.append(r["source"]); self.t.append(r["t"])
        self.broad, self.path, self.signals, self.tone = np.stack(B), np.stack(P), np.stack(S), np.stack(T)
        self.conf = np.array(C, np.float32)
        self.source, self.t = np.array(self.source), np.array(self.t)

    def __len__(self):
        return len(self.uris)


def split(rows: Rows, test_frac: float, val_frac: float):
    """Per source, newest test_frac of the timestamped posts test, the val_frac before them validate."""
    tr, va, te = [], [], []
    for src in sorted(set(rows.source)):
        idx = np.where(rows.source == src)[0]
        stamped = idx[rows.t[idx] > 0]
        stamped = stamped[np.argsort(rows.t[stamped], kind="stable")]
        n_te, n_va = int(len(stamped) * test_frac), int(len(stamped) * val_frac)
        te += list(stamped[len(stamped) - n_te:]) if n_te else []
        va += list(stamped[len(stamped) - n_te - n_va: len(stamped) - n_te]) if n_va else []
        tr += list(idx[rows.t[idx] <= 0]) + list(stamped[: len(stamped) - n_te - n_va])
    return np.array(tr), np.array(va), np.array(te)


class Student(nn.Module):
    def __init__(self, base: str, n_broad: int, n_paths: int, n_signals: int):
        super().__init__()
        from transformers import ModernVBertModel

        self.encoder = ModernVBertModel.from_pretrained(base)
        h = self.encoder.config.text_config.hidden_size
        self.drop = nn.Dropout(0.1)
        self.broad, self.path = nn.Linear(h, n_broad), nn.Linear(h, n_paths)
        self.signals, self.tone = nn.Linear(h, n_signals), nn.Linear(h, len(TONES))

    def forward(self, **inputs):
        hs = self.encoder(**inputs).last_hidden_state
        m = inputs["attention_mask"].unsqueeze(-1).to(hs.dtype)
        pooled = self.drop((hs * m).sum(1) / m.sum(1).clamp(min=1))  # mean over the text and picture tokens
        return self.broad(pooled), self.path(pooled), self.signals(pooled), self.tone(pooled)


class BatchSet(torch.utils.data.Dataset):
    """Each item is a whole, ready batch (so DataLoader workers load and process the pictures)."""

    def __init__(self, rows: Rows, batches, processor, images_root: str, max_images: int):
        self.rows, self.batches, self.processor, self.root, self.max_images = rows, batches, processor, images_root, max_images

    def __len__(self):
        return len(self.batches)

    def __getitem__(self, i):
        idx = self.batches[i]
        pics = [self._load(self.rows.shas[j]) for j in idx]
        prompts = []
        for j, p in zip(idx, pics):
            content = [{"type": "image"}] * len(p) + [{"type": "text", "text": self.rows.texts[j] or " "}]
            prompts.append(self.processor.apply_chat_template([{"role": "user", "content": content}]))
        if any(pics):  # a batch is all pictures or all text, by construction
            enc = self.processor(text=prompts, images=pics, padding=True, return_tensors="pt", do_image_splitting=False)
        else:
            enc = self.processor(text=prompts, padding=True, return_tensors="pt")
        enc = dict(enc)
        enc["index"] = torch.tensor(idx)
        return enc

    def _load(self, shas):
        out = []
        for sha in shas[: self.max_images]:
            try:
                with Image.open(Path(self.root) / "1000" / sha[:2] / f"{sha}.jpg") as im:
                    out.append(im.convert("RGB"))
            except OSError:
                continue
        return out


def make_batches(rows: Rows, idx: np.ndarray, bs: int, max_images: int, rng=None):
    """Batches that are all-picture or all-text (the processor wants one kind per batch)."""
    pic = np.array([bool(rows.shas[j]) and max_images > 0 for j in idx])
    out = []
    for group in (idx[pic], idx[~pic]):
        if rng is not None:
            group = group[rng.permutation(len(group))]
        out += [group[i:i + bs].tolist() for i in range(0, len(group), bs)]
    if rng is not None:
        out = [out[i] for i in rng.permutation(len(out))]
    return out


def loader(rows, idx, bs, processor, a, rng=None):
    ds = BatchSet(rows, make_batches(rows, idx, bs, a.max_images, rng), processor, a.images, a.max_images)
    return torch.utils.data.DataLoader(ds, batch_size=None, shuffle=False, num_workers=a.workers,
                                       prefetch_factor=4 if a.workers else None, persistent_workers=False)


def to_device(batch, dev):
    return {k: v.to(dev) for k, v in batch.items() if k != "index"}


def losses_for(out, rows, j, dev):
    lb, lp, ls, lt = out
    yb, yp = torch.tensor(rows.broad[j]).to(dev), torch.tensor(rows.path[j]).to(dev)
    ys, yt = torch.tensor(rows.signals[j]).to(dev), torch.tensor(rows.tone[j]).to(dev)
    known_s, known_t = ~torch.isnan(ys), ~torch.isnan(yt).any(-1)
    bce = F.binary_cross_entropy_with_logits(ls.float(), torch.nan_to_num(ys), reduction="none")
    parts = {"broad": soft_ce(lb, yb), "path": soft_ce(lp, yp),
             "signals": (bce * known_s).sum(-1) / known_s.sum(-1).clamp(min=1),
             "tone": soft_ce(lt, torch.nan_to_num(yt)) * known_t}
    return parts


@torch.no_grad()
def predict(model, rows, idx, processor, a, dev):
    model.eval()
    outs, order = [[], [], [], []], []
    for batch in loader(rows, idx, a.eval_bs, processor, a):
        j = batch["index"].tolist()
        with torch.autocast(dev.type, dtype=torch.bfloat16, enabled=dev.type == "cuda"):
            o = model(**to_device(batch, dev))
        for k in range(4):
            outs[k].append(o[k].float().cpu())
        order += j
    pos = {j: i for i, j in enumerate(order)}
    sel = [pos[j] for j in idx]
    return [torch.cat(x)[sel] for x in outs]


def fit_temperature(logits: torch.Tensor, target: np.ndarray) -> float:
    t = torch.tensor(target)
    log_t = torch.zeros(1, requires_grad=True)
    opt = torch.optim.LBFGS([log_t], lr=0.1, max_iter=100)

    def closure():
        opt.zero_grad()
        loss = soft_ce(logits / log_t.exp(), t).mean()
        loss.backward()
        return loss

    opt.step(closure)
    return float(log_t.detach().exp())


def topk(pred: np.ndarray, target: np.ndarray, k: int) -> float:
    want = target.argmax(-1)
    return float((np.argsort(-pred, -1)[:, :k] == want[:, None]).any(-1).mean())


def auc(score: np.ndarray, label: np.ndarray) -> float:
    pos, neg = score[label], score[~label]
    if len(pos) == 0 or len(neg) == 0:
        return float("nan")
    ranks = np.argsort(np.argsort(np.concatenate([pos, neg]))) + 1
    return float((ranks[: len(pos)].sum() - len(pos) * (len(pos) + 1) / 2) / (len(pos) * len(neg)))


def score(rows, idx, preds, t_broad=1.0, t_path=1.0) -> dict:
    lb, lp, ls, lt = preds
    pb, pp = F.softmax(lb / t_broad, -1).numpy(), F.softmax(lp / t_path, -1).numpy()
    ps, pt = torch.sigmoid(ls).numpy(), F.softmax(lt, -1).numpy()
    out = {}
    for src in sorted(set(rows.source[idx])):
        m = rows.source[idx] == src
        j = idx[m]
        d = {"n": int(m.sum()), "broad_top1": topk(pb[m], rows.broad[j], 1), "broad_top3": topk(pb[m], rows.broad[j], 3),
             "path_top1": topk(pp[m], rows.path[j], 1), "path_top3": topk(pp[m], rows.path[j], 3)}
        tk = ~np.isnan(rows.tone[j]).any(-1)
        d["tone_top1"] = float((pt[m][tk].argmax(-1) == rows.tone[j][tk].argmax(-1)).mean()) if tk.any() else None
        sig_mae = {}
        for si, name in enumerate(SIGNALS):
            k = ~np.isnan(rows.signals[j][:, si])
            if k.sum() >= 30:
                sig_mae[name] = float(np.abs(ps[m][k][:, si] - rows.signals[j][k][:, si]).mean())
        d["signal_mae"] = sig_mae
        mi = SIGNALS.index("meme")
        k = ~np.isnan(rows.signals[j][:, mi])
        if k.sum() >= 30:
            lab = rows.signals[j][k][:, mi] >= 0.5
            d["meme_auc_vs_teacher_at_0.5"] = auc(ps[m][k][:, mi], lab)
            d["meme_teacher_rate_at_0.5"] = float(lab.mean())
        out[src] = d
    return out


def val_losses(rows, idx, preds, conf_floor) -> dict:
    """The training objective (same terms, weights and confidence weighting) on held-out posts: overall and per teacher."""
    parts = losses_for(preds, rows, idx, torch.device("cpu"))
    w = torch.tensor(np.maximum(rows.conf[idx], conf_floor))
    total = sum(WEIGHTS[k] * v for k, v in parts.items())
    src = rows.source[idx]
    out = {}
    for name, m in [(s, src == s) for s in sorted(set(src))] + [("all", np.ones(len(idx), bool))]:
        m = torch.tensor(m)
        out[name] = {"total": float((total * w)[m].sum() / w[m].sum()), **{k: float((v * w)[m].sum() / w[m].sum()) for k, v in parts.items()}}
    return out


def log_eval(writer, sc, vl, step):
    """Validation numbers as TensorBoard scalars. The train, val_subset and val_full runs use the same loss/ tags, so they overlay."""
    for src, d in sc.items():
        for k in ("broad_top1", "broad_top3", "path_top1", "path_top3", "tone_top1", "meme_auc_vs_teacher_at_0.5"):
            if d.get(k) is not None:
                writer.add_scalar(f"acc_{src}/{k}", d[k], step)
    for src, d in vl.items():
        for k, v in d.items():
            writer.add_scalar(f"loss/{k}" if src == "all" else f"loss_{src}/{k}", v, step)
    writer.flush()


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--export", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--taxonomy", default=str(HERE.parent / "taxonomy" / "v2.1.yaml"))
    ap.add_argument("--images", default="/root/images", help="picture archive root (holds 1000/)")
    ap.add_argument("--base", default="ModernVBERT/modernvbert")
    ap.add_argument("--epochs", type=int, default=3)
    ap.add_argument("--bs", type=int, default=32)
    ap.add_argument("--eval-bs", type=int, default=64)
    ap.add_argument("--accum", type=int, default=1, help="batches whose gradients are added before each optimizer step (bs x accum = effective batch)")
    ap.add_argument("--grad-ckpt", action="store_true", help="recompute activations in the backward pass: slower, much less memory")
    ap.add_argument("--lr", type=float, default=5e-5)
    ap.add_argument("--head-lr", type=float, default=1e-3)
    ap.add_argument("--max-images", type=int, default=2, help="pictures per post (0: text only)")
    ap.add_argument("--workers", type=int, default=12)
    ap.add_argument("--conf-floor", type=float, default=0.3)
    ap.add_argument("--test-frac", type=float, default=0.08)
    ap.add_argument("--val-frac", type=float, default=0.04)
    ap.add_argument("--patience", type=int, default=2)
    ap.add_argument("--seed", type=int, default=0)
    ap.add_argument("--smoke", type=int, default=0, help="a tiny run on this many rows per source, for checking the code")
    ap.add_argument("--tb", help="TensorBoard log folder; adds train, val_subset and val_full runs under it")
    ap.add_argument("--log-every", type=int, default=25, help="batches between training points in TensorBoard")
    ap.add_argument("--eval-every", type=int, default=750, help="batches between quick validation checks on a fixed sample (0: only at the end of each epoch)")
    ap.add_argument("--eval-subset", type=int, default=1500, help="size of that fixed random sample of the validation posts")
    a = ap.parse_args()

    torch.manual_seed(a.seed)
    rng = np.random.default_rng(a.seed)
    dev = torch.device("cuda" if torch.cuda.is_available() else "cpu")
    space = common.Space.from_taxonomy(a.taxonomy)
    rows = Rows(a.export, space)
    tr, va, te = split(rows, a.test_frac, a.val_frac)
    if a.smoke:
        keep = lambda ix: np.concatenate([ix[rows.source[ix] == s][: a.smoke] for s in sorted(set(rows.source))])  # noqa: E731
        tr, va, te = keep(tr), keep(va) if len(va) else keep(tr), keep(te) if len(te) else keep(tr)
        a.epochs = 1
        if dev.type == "cpu":  # on a GPU keep the batch size and workers asked for, to measure speed and memory
            a.workers, a.bs, a.eval_bs = 0, 4, 4
    info = {"rows": len(rows), "train": len(tr), "val": len(va), "test": len(te),
            "by_source": {s: int((rows.source == s).sum()) for s in sorted(set(rows.source))}, "device": str(dev)}
    print(json.dumps(info), flush=True)

    from transformers import AutoProcessor

    processor = AutoProcessor.from_pretrained(a.base)
    model = Student(a.base, len(space.broad), len(space.paths), len(SIGNALS)).to(dev)
    heads = [p for n, p in model.named_parameters() if not n.startswith("encoder.")]
    opt = torch.optim.AdamW([{"params": model.encoder.parameters(), "lr": a.lr}, {"params": heads, "lr": a.head_lr}], weight_decay=0.01)
    if a.grad_ckpt:
        model.encoder.gradient_checkpointing_enable()
    steps = a.epochs * math.ceil(math.ceil(len(tr) / a.bs) / a.accum) + 1  # optimizer steps
    warm = max(int(0.05 * steps), 1)
    sched = torch.optim.lr_scheduler.LambdaLR(opt, lambda s: min((s + 1) / warm, max((steps - s) / max(steps - warm, 1), 0.0)))

    os.makedirs(a.out, exist_ok=True)
    tb_train = tb_sub = tb_full = None
    if a.tb:
        from torch.utils.tensorboard import SummaryWriter  # needs the tensorboard package; fails here, loudly, if it is missing

        tb_train, tb_sub, tb_full = (SummaryWriter(f"{a.tb}/{n}", flush_secs=10) for n in ("train", "val_subset", "val_full"))
        tb_train.add_text("run", "```\n" + json.dumps({"args": vars(a), "data": info}, indent=1) + "\n```", 0)
    sub = np.sort(np.random.default_rng(1234).permutation(va)[: a.eval_subset]) if a.eval_every else None  # fixed, keeps the data mix

    def evaluate(idx, writer, step, workers):
        ea = argparse.Namespace(**{**vars(a), "workers": workers})
        preds = predict(model, rows, idx, processor, ea, dev)
        sc, vl = score(rows, idx, preds), val_losses(rows, idx, preds, a.conf_floor)
        if writer is not None:
            log_eval(writer, sc, vl, step)
        model.train()
        return sc, vl

    def fresh_interval():  # sums over the batches since the last training point written to TensorBoard
        return {"n": 0, "loss": 0.0, "gn": 0.0, "gn_n": 0, "seen": 0, "t": time.time(), **{k: 0.0 for k in WEIGHTS}}

    best, best_state, best_epoch, history, t0 = -1.0, None, -1, [], time.time()
    gstep, iv = 0, fresh_interval()  # gstep: batches over the whole run, the TensorBoard x axis
    for epoch in range(a.epochs):
        model.train()
        run, n, seen, t_log, pending = 0.0, 0, 0, time.time(), 0
        opt.zero_grad(set_to_none=True)
        train_loader = loader(rows, tr, a.bs, processor, a, rng)
        for step, batch in enumerate(train_loader):
            j = batch["index"].numpy()
            with torch.autocast(dev.type, dtype=torch.bfloat16, enabled=dev.type == "cuda"):
                out = model(**to_device(batch, dev))
            parts = losses_for(out, rows, j, dev)
            w = torch.tensor(np.maximum(rows.conf[j], a.conf_floor)).to(dev)
            loss = (sum(WEIGHTS[k] * v for k, v in parts.items()) * w).sum() / w.sum()
            (loss / a.accum).backward()  # the weights are normalised within each small batch, not across the accumulated ones
            pending += 1
            if pending == a.accum:
                gn = torch.nn.utils.clip_grad_norm_(model.parameters(), 1.0)
                opt.step()
                sched.step()
                opt.zero_grad(set_to_none=True)
                pending = 0
                iv["gn"] += gn
                iv["gn_n"] += 1
            li = loss.item()
            run += li; n += 1; seen += len(j); gstep += 1
            iv["n"] += 1; iv["loss"] += li; iv["seen"] += len(j)
            for k, v in parts.items():
                iv[k] += (v * w).sum() / w.sum()
            if tb_train is not None and gstep % a.log_every == 0:
                tb_train.add_scalar("loss/total", iv["loss"] / iv["n"], gstep)
                for k in WEIGHTS:
                    tb_train.add_scalar(f"loss/{k}", float(iv[k]) / iv["n"], gstep)
                tb_train.add_scalar("lr/encoder", opt.param_groups[0]["lr"], gstep)
                tb_train.add_scalar("lr/heads", opt.param_groups[1]["lr"], gstep)
                if iv["gn_n"]:
                    tb_train.add_scalar("train/grad_norm", float(iv["gn"]) / iv["gn_n"], gstep)
                tb_train.add_scalar("train/posts_per_s", iv["seen"] / max(time.time() - iv["t"], 1e-9), gstep)
                tb_train.add_scalar("train/epoch", epoch + (step + 1) / len(train_loader), gstep)
                iv = fresh_interval()
            if a.eval_every and gstep % a.eval_every == 0:
                t_eval = time.time()
                sc, vl = evaluate(sub, tb_sub, gstep, min(a.workers, 8))
                print(f"epoch {epoch + 1} step {step}: quick validation on {len(sub)} posts - loss {vl['all']['total']:.3f} - " +
                      " | ".join(f"{s} broad {v['broad_top1']:.1%} path {v['path_top1']:.1%}" for s, v in sc.items()), flush=True)
                t_log += time.time() - t_eval  # keep the speed readings free of the time spent validating
                iv["t"] += time.time() - t_eval
            if step % 50 == 0:
                peak = f" peak {torch.cuda.max_memory_allocated() / 2**30:.1f} GiB" if dev.type == "cuda" else ""
                print(f"epoch {epoch + 1} step {step} loss {run / n:.3f} {seen / max(time.time() - t_log, 1e-9):.0f} posts/s{peak}", flush=True)
        if pending:  # the last, shorter group of batches of the epoch
            torch.nn.utils.clip_grad_norm_(model.parameters(), 1.0)
            opt.step()
            opt.zero_grad(set_to_none=True)
        sc, vl = evaluate(va, tb_full, gstep, a.workers)
        key = float(np.mean([v["broad_top1"] for v in sc.values()]))
        history.append({"epoch": epoch + 1, "train_loss": run / n, "val": sc, "val_loss": vl})
        print(f"epoch {epoch + 1}: train loss {run / n:.3f} val loss {vl['all']['total']:.3f} - validation " +
              " | ".join(f"{s} broad {v['broad_top1']:.1%} path {v['path_top1']:.1%}" for s, v in sc.items()) +
              f" - {time.time() - t0:.0f}s", flush=True)
        if key > best:
            best, best_epoch = key, epoch
            best_state = {k: v.detach().cpu().clone() for k, v in model.state_dict().items()}
            torch.save(best_state, f"{a.out}/model.best-so-far.pt")  # in case the run is lost before the end
        elif epoch - best_epoch >= a.patience:
            print("stopping: no validation gain", flush=True)
            break
    model.load_state_dict(best_state)

    vb, vp, _, _ = predict(model, rows, va, processor, a, dev)
    t_broad, t_path = fit_temperature(vb, rows.broad[va]), fit_temperature(vp, rows.path[va])
    test_preds = predict(model, rows, te, processor, a, dev)
    metrics = {"split": info, "test": score(rows, te, test_preds, t_broad, t_path), "history": history, "best_epoch": best_epoch,
               "temperature_broad": t_broad, "temperature_path": t_path, "train_seconds": time.time() - t0, "args": vars(a)}
    torch.save(model.state_dict(), f"{a.out}/model.pt")
    processor.save_pretrained(a.out)
    json.dump({"base": a.base, "taxonomy_version": space.version, "broad": space.broad, "paths": space.paths, "signals": SIGNALS,
               "tones": TONES, "max_images": a.max_images, "do_image_splitting": False, "max_chars": MAX_CHARS,
               "temperature_broad": t_broad, "temperature_path": t_path}, open(f"{a.out}/config.json", "w"), indent=2)
    json.dump(json.load(open(f"{a.export}/manifest.json")), open(f"{a.out}/data_manifest.json", "w"), indent=2)
    json.dump(metrics, open(f"{a.out}/metrics.json", "w"), indent=2)
    if os.path.exists(f"{a.out}/model.best-so-far.pt"):
        os.remove(f"{a.out}/model.best-so-far.pt")
    for w in (tb_train, tb_sub, tb_full):
        if w is not None:
            w.close()
    for s, v in metrics["test"].items():
        print(f"TEST {s} ({v['n']} posts, newest): broad {v['broad_top1']:.1%} (top-3 {v['broad_top3']:.1%}) - path {v['path_top1']:.1%} "
              f"(top-3 {v['path_top3']:.1%}) - tone {v['tone_top1']}", flush=True)


if __name__ == "__main__":
    main()
