"""Fine-tune the student classifier (plan §11.5).

One shared encoder (ModernBERT-base by default) with four heads, trained to match
Jev's distributions:
  - broad topic: soft cross-entropy (= KL up to a constant) against Jev's broad probs
  - path: soft cross-entropy against the joint P(broad) x P(sub | broad)
  - signals: binary cross-entropy against substance/news/promo/general_interest in 0..1
  - tone: soft cross-entropy against Jev's tone distribution
Examples are weighted by Jev's broad confidence, floored at 0.3. After training, a
temperature is fit on the validation set for the broad and path heads. The model is
exported to ONNX for CPU serving and checked against PyTorch.

    uv run python train.py --export /data/exports/v1-v0 --taxonomy ../taxonomy/v1.yaml --out /data/models/v0
"""

import argparse
import json
import math
import os
import time

import numpy as np
import torch
import torch.nn as nn
import torch.nn.functional as F
from torch.utils.tensorboard import SummaryWriter
from tqdm import tqdm
from transformers import AutoModel, AutoTokenizer, get_linear_schedule_with_warmup

import common

POSTDOC_VERSION = "pd1"  # must match internal/postdoc.Version used by the export


class Student(nn.Module):
    def __init__(self, base: str, n_broad: int, n_paths: int, attn: str = "sdpa"):
        super().__init__()
        self.encoder = AutoModel.from_pretrained(base, attn_implementation=attn)
        h = self.encoder.config.hidden_size
        self.drop = nn.Dropout(0.1)
        self.broad = nn.Linear(h, n_broad)
        self.path = nn.Linear(h, n_paths)
        self.signals = nn.Linear(h, len(common.SIGNALS))
        self.tone = nn.Linear(h, len(common.TONES))

    def forward(self, input_ids, attention_mask):
        hs = self.encoder(input_ids=input_ids, attention_mask=attention_mask).last_hidden_state
        m = attention_mask.unsqueeze(-1).to(hs.dtype)
        pooled = self.drop((hs * m).sum(1) / m.sum(1).clamp(min=1))  # mean pooling
        return self.broad(pooled), self.path(pooled), self.signals(pooled), self.tone(pooled)


def soft_ce(logits, target):
    return -(target * F.log_softmax(logits.float(), -1)).sum(-1)


def batches(d: common.Data, tok, bs: int, max_len: int, shuffle: bool, rng=None):
    idx = np.arange(len(d))
    if shuffle:
        rng.shuffle(idx)
    for i in range(0, len(idx), bs):
        j = idx[i:i + bs]
        enc = tok([d.texts[k] for k in j], padding=True, truncation=True, max_length=max_len, return_tensors="pt")
        yield j, enc


@torch.no_grad()
def predict(model, d, tok, max_len, bs=256):
    model.eval()
    outs = [[], [], [], []]
    for _, enc in batches(d, tok, bs, max_len, False):
        with torch.autocast("cuda", dtype=torch.bfloat16):
            o = model(enc["input_ids"].cuda(), enc["attention_mask"].cuda())
        for k in range(4):
            outs[k].append(o[k].float().cpu())
    return [torch.cat(x) for x in outs]


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


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--export", required=True)
    ap.add_argument("--taxonomy", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--base", default="answerdotai/ModernBERT-base")
    ap.add_argument("--epochs", type=int, default=3)
    ap.add_argument("--bs", type=int, default=64)
    ap.add_argument("--lr", type=float, default=5e-5)
    ap.add_argument("--max-len", type=int, default=256)
    ap.add_argument("--conf-floor", type=float, default=0.3)
    ap.add_argument("--seed", type=int, default=0)
    ap.add_argument("--patience", type=int, default=2, help="stop after this many epochs without a validation gain")
    ap.add_argument("--onnx", action="store_true", help="also export ONNX (not used: we serve on the GPU)")
    a = ap.parse_args()

    torch.manual_seed(a.seed)
    rng = np.random.default_rng(a.seed)
    space = common.Space.from_taxonomy(a.taxonomy)
    tr, va, te, info = common.split(common.load(a.export, space))
    print(json.dumps(info), flush=True)

    tok = AutoTokenizer.from_pretrained(a.base)
    model = Student(a.base, len(space.broad), len(space.paths)).cuda()
    steps = a.epochs * math.ceil(len(tr) / a.bs)
    opt = torch.optim.AdamW(model.parameters(), lr=a.lr, weight_decay=0.01)
    sched = get_linear_schedule_with_warmup(opt, int(0.05 * steps), steps)

    # Live progress: a progress bar per epoch in the terminal, and TensorBoard scalars in
    # <out>/tb (served by the tensorboard compose service on port 6006).
    os.makedirs(a.out, exist_ok=True)
    tb = SummaryWriter(f"{a.out}/tb")
    tb.add_text("run", f"base `{a.base}` · export `{a.export}` · train {len(tr)} / val {len(va)} / test {len(te)} · "
                       f"up to {a.epochs} epochs, batch {a.bs}, lr {a.lr}")
    steps_per_epoch = math.ceil(len(tr) / a.bs)

    best, best_epoch, best_state, history = -1.0, -1, None, []
    t_start = time.time()
    global_step = 0
    for epoch in range(a.epochs):
        model.train()
        running, n = 0.0, 0
        bar = tqdm(batches(tr, tok, a.bs, a.max_len, True, rng), total=steps_per_epoch, desc=f"epoch {epoch + 1}/{a.epochs}",
                   dynamic_ncols=True, mininterval=2, unit="batch")
        for step, (j, enc) in enumerate(bar):
            ids, mask = enc["input_ids"].cuda(), enc["attention_mask"].cuda()
            yb, yp = torch.tensor(tr.broad[j]).cuda(), torch.tensor(tr.path[j]).cuda()
            ys, yt = torch.tensor(tr.signals[j]).cuda(), torch.tensor(tr.tone[j]).cuda()
            w = torch.tensor(np.maximum(tr.conf[j], a.conf_floor)).cuda()
            with torch.autocast("cuda", dtype=torch.bfloat16):
                lb, lp, ls, lt = model(ids, mask)
            loss = (soft_ce(lb, yb) + soft_ce(lp, yp)
                    + 0.5 * F.binary_cross_entropy_with_logits(ls.float(), ys, reduction="none").mean(-1)
                    + 0.3 * soft_ce(lt, yt))
            loss = (loss * w).sum() / w.sum()
            opt.zero_grad(set_to_none=True)
            loss.backward()
            torch.nn.utils.clip_grad_norm_(model.parameters(), 1.0)
            opt.step()
            sched.step()
            running += loss.item(); n += 1; global_step += 1
            if step % 25 == 0:
                bar.set_postfix(loss=f"{running / n:.3f}", lr=f"{sched.get_last_lr()[0]:.1e}")
            if global_step % 50 == 0:
                tb.add_scalar("train/loss", loss.item(), global_step)
                tb.add_scalar("train/loss_epoch_avg", running / n, global_step)
                tb.add_scalar("train/lr", sched.get_last_lr()[0], global_step)
        bar.close()
        lb, lp, _, _ = predict(model, va, tok, a.max_len)
        vm = common.evaluate(space, F.softmax(lb, -1).numpy(), F.softmax(lp, -1).numpy(), va)
        history.append({"epoch": epoch + 1, "train_loss": running / n, "val_broad_top1": vm["broad_top1"], "val_path_top1": vm["path_top1"],
                        "val_broad_top3": vm["broad_top3"], "val_path_top3": vm["path_top3"]})
        for k in ("val_broad_top1", "val_path_top1", "val_broad_top3", "val_path_top3"):
            tb.add_scalar(k.replace("val_", "val/"), history[-1][k], epoch + 1)
        tb.flush()
        print(f"epoch {epoch + 1}: train loss {running / n:.3f} · validation broad {vm['broad_top1']:.1%} (top-3 {vm['broad_top3']:.1%})"
              f" · path {vm['path_top1']:.1%} (top-3 {vm['path_top3']:.1%}) · {time.time() - t_start:.0f}s elapsed", flush=True)
        if vm["broad_top1"] > best:
            best, best_epoch = vm["broad_top1"], epoch
            best_state = {k: v.detach().cpu().clone() for k, v in model.state_dict().items()}
        elif epoch - best_epoch >= a.patience:
            print(f"stopping: no validation gain for {a.patience} epochs (best epoch {best_epoch})", flush=True)
            break
    model.load_state_dict(best_state)
    train_seconds = time.time() - t_start

    # Temperatures on validation, then test metrics with calibrated probabilities.
    vb, vp, _, _ = predict(model, va, tok, a.max_len)
    t_broad, t_path = fit_temperature(vb, va.broad), fit_temperature(vp, va.path)
    test_b, test_p, test_s, test_t = predict(model, te, tok, a.max_len)
    metrics = common.evaluate(space, F.softmax(test_b / t_broad, -1).numpy(), F.softmax(test_p / t_path, -1).numpy(), te,
                              torch.sigmoid(test_s).numpy(), F.softmax(test_t, -1).numpy())
    metrics.update({"split": info, "history": history, "best_epoch": best_epoch, "temperature_broad": t_broad,
                    "temperature_path": t_path, "train_seconds": train_seconds, "base": a.base, "args": vars(a)})
    # Serving runs on the GPU (Hailey, 2026-09-28): measure its throughput.
    t0 = time.time()
    predict(model, te, tok, a.max_len)
    metrics["gpu_posts_per_second"] = len(te) / (time.time() - t0)

    os.makedirs(a.out, exist_ok=True)
    torch.save(model.state_dict(), f"{a.out}/model.pt")
    tok.save_pretrained(a.out)
    manifest = json.load(open(f"{a.export}/manifest.json"))
    config = {"base": a.base, "taxonomy_version": space.version, "postdoc_version": POSTDOC_VERSION,
              "max_len": a.max_len, "broad": space.broad, "paths": space.paths, "signals": common.SIGNALS,
              "tones": common.TONES, "temperature_broad": t_broad, "temperature_path": t_path}
    if manifest.get("postdoc_version") != POSTDOC_VERSION:
        raise SystemExit(f"export was rendered with post document {manifest.get('postdoc_version')}, trainer expects {POSTDOC_VERSION}")
    json.dump(config, open(f"{a.out}/config.json", "w"), indent=2)
    json.dump(manifest, open(f"{a.out}/data_manifest.json", "w"), indent=2)

    if a.onnx:
        metrics["onnx"] = export_onnx(model, tok, te, a, f"{a.out}/model.onnx")
    json.dump(metrics, open(f"{a.out}/metrics.json", "w"), indent=2)
    for k in ("broad_top1", "broad_top3", "path_top1", "path_top3", "broad_ece", "tone_top1"):
        tb.add_scalar(f"test/{k}", metrics[k], best_epoch + 1)
    tb.close()
    print(f"\nTEST (windows {', '.join(info['test_windows'])}, {len(te)} posts, best epoch {best_epoch + 1}): "
          f"broad {metrics['broad_top1']:.1%} (top-3 {metrics['broad_top3']:.1%}) · path {metrics['path_top1']:.1%} "
          f"(top-3 {metrics['path_top3']:.1%}) · GPU {metrics['gpu_posts_per_second']:.0f} posts/s · saved to {a.out}", flush=True)


def export_onnx(model: Student, tok, te: common.Data, a, path: str) -> dict:
    """Export to ONNX (fp32), check it matches PyTorch, and time CPU inference."""
    import onnxruntime as ort

    class Wrapped(nn.Module):
        def __init__(self, m):
            super().__init__()
            self.m = m

        def forward(self, input_ids, attention_mask):
            b, p, s, t = self.m(input_ids, attention_mask)
            return F.softmax(b, -1), F.softmax(p, -1), torch.sigmoid(s), F.softmax(t, -1)

    # Export from an eager-attention copy on CPU: the simplest graph to trace.
    cpu = Student(a.base, model.broad.out_features, model.path.out_features, attn="eager")
    cpu.load_state_dict({k: v.float() for k, v in model.state_dict().items()})
    cpu.eval()
    enc = tok(te.texts[:8], padding=True, truncation=True, max_length=a.max_len, return_tensors="pt")
    try:
        torch.onnx.export(Wrapped(cpu), (enc["input_ids"], enc["attention_mask"]), path,
                          input_names=["input_ids", "attention_mask"], output_names=["broad", "path", "signals", "tone"],
                          dynamic_axes={"input_ids": {0: "batch", 1: "seq"}, "attention_mask": {0: "batch", 1: "seq"},
                                        "broad": {0: "batch"}, "path": {0: "batch"}, "signals": {0: "batch"}, "tone": {0: "batch"}},
                          opset_version=17, dynamo=False)
    except Exception as e:  # report, don't lose the trained model
        return {"ok": False, "error": repr(e)[:500]}

    sess = ort.InferenceSession(path, providers=["CPUExecutionProvider"])
    n = 256
    enc = tok(te.texts[:n], padding=True, truncation=True, max_length=a.max_len, return_tensors="np")
    t0 = time.time()
    got = sess.run(None, {"input_ids": enc["input_ids"].astype(np.int64), "attention_mask": enc["attention_mask"].astype(np.int64)})
    cpu_seconds = time.time() - t0
    with torch.no_grad():
        ref = Wrapped(cpu)(torch.tensor(enc["input_ids"]), torch.tensor(enc["attention_mask"]))
    diff = max(float(np.abs(g - r.numpy()).max()) for g, r in zip(got, ref))
    agree = float((got[0].argmax(1) == ref[0].numpy().argmax(1)).mean())
    return {"ok": True, "max_abs_diff_vs_torch": diff, "broad_argmax_agreement_vs_torch": agree,
            "cpu_posts_per_second_batch256": n / cpu_seconds, "size_mb": os.path.getsize(path) / 1e6}


if __name__ == "__main__":
    main()
