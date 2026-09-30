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
import losses
import tb_report
import tbreport
from losses import soft_ce

POSTDOC_VERSION = "pd2"  # must match internal/postdoc.Version used by the export


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
    ap.add_argument("--live-configs", default="", help="comma-separated label_configs of live-post labels (outside the windows)")
    ap.add_argument("--live-test", type=int, default=10000, help="live random-sample posts held out for testing")
    ap.add_argument("--live-val", type=int, default=3000, help="live random-sample posts added to validation")
    ap.add_argument("--live-weight", type=float, default=1.0, help="multiply the training weight of live-post labels by this")
    a = ap.parse_args()

    torch.manual_seed(a.seed)
    rng = np.random.default_rng(a.seed)
    space = common.Space.from_taxonomy(a.taxonomy)
    eq = common.Equivalence.for_taxonomy(a.taxonomy, space)
    data = common.load(a.export, space)
    live_configs = {c for c in a.live_configs.split(",") if c}
    te_live = None
    if live_configs:
        data, te_live, va_live = common.live_holdout(data, live_configs, a.live_test, a.live_val)
    tr, va, te, info = common.split(data)
    if live_configs:
        va = va.concat(va_live)
        info.update({"live_configs": sorted(live_configs), "n_live_test": len(te_live), "n_live_val": len(va_live),
                     "n_live_train": int(tr.is_live(live_configs).sum()), "live_weight": a.live_weight, "n_val": len(va)})
    print(json.dumps(info), flush=True)
    live_w = np.where(tr.is_live(live_configs), a.live_weight, 1.0).astype(np.float32)

    tok = AutoTokenizer.from_pretrained(a.base)
    model = Student(a.base, len(space.broad), len(space.paths)).cuda()
    steps = a.epochs * math.ceil(len(tr) / a.bs)
    opt = torch.optim.AdamW(model.parameters(), lr=a.lr, weight_decay=0.01)
    sched = get_linear_schedule_with_warmup(opt, int(0.05 * steps), steps)

    # Live progress: a progress bar per epoch in the terminal, and TensorBoard scalars in
    # <out>/tb (served by the tensorboard compose service on port 6006).
    os.makedirs(a.out, exist_ok=True)
    tb = SummaryWriter(f"{a.out}/tb")
    n_live = int(tr.is_live(live_configs).sum())
    n_relabeled = sum(s == "uncertain" for s in tr.sources) - sum(s == "uncertain" for s in np.array(tr.sources)[tr.is_live(live_configs)])
    tb.add_text("run", f"base `{a.base}` · export `{a.export}` · train {len(tr)} / val {len(va)} / test {len(te)} · "
                       f"{n_relabeled} training posts relabeled · {n_live} live training posts (weight x{a.live_weight}) · "
                       f"up to {a.epochs} epochs, batch {a.bs}, lr {a.lr}")
    steps_per_epoch = math.ceil(len(tr) / a.bs)
    # Posts people judged (all in the test windows): their counts are logged every epoch
    # to watch, never used to pick the epoch.
    human = tbreport.Human()
    human_data = human.subset(te)

    best, best_epoch, best_state, history = -1.0, -1, None, []
    t_start = time.time()
    global_step = 0
    t_log, posts_since_log = time.time(), 0
    for epoch in range(a.epochs):
        model.train()
        running, n = 0.0, 0
        bar = tqdm(batches(tr, tok, a.bs, a.max_len, True, rng), total=steps_per_epoch, desc=f"epoch {epoch + 1}/{a.epochs}",
                   dynamic_ncols=True, mininterval=2, unit="batch")
        for step, (j, enc) in enumerate(bar):
            ids, mask = enc["input_ids"].cuda(), enc["attention_mask"].cuda()
            yb, yp = torch.tensor(tr.broad[j]).cuda(), torch.tensor(tr.path[j]).cuda()
            ys, yt = torch.tensor(tr.signals[j]).cuda(), torch.tensor(tr.tone[j]).cuda()
            w = torch.tensor(np.maximum(tr.conf[j], a.conf_floor) * live_w[j]).cuda()
            with torch.autocast("cuda", dtype=torch.bfloat16):
                lb, lp, ls, lt = model(ids, mask)
            parts = losses.parts(lb, lp, ls, lt, yb, yp, ys, yt)
            loss = losses.total(parts, w)
            opt.zero_grad(set_to_none=True)
            loss.backward()
            grad_norm = torch.nn.utils.clip_grad_norm_(model.parameters(), 1.0)
            opt.step()
            sched.step()
            running += loss.item(); n += 1; global_step += 1; posts_since_log += len(j)
            if step % 25 == 0:
                bar.set_postfix(loss=f"{running / n:.3f}", lr=f"{sched.get_last_lr()[0]:.1e}")
            if global_step % 50 == 0:
                tb.add_scalar("train/loss", loss.item(), global_step)
                tb.add_scalar("train/loss_epoch_avg", running / n, global_step)
                tb.add_scalar("train/lr", sched.get_last_lr()[0], global_step)
                tb.add_scalar("train/grad_norm", float(grad_norm), global_step)
                for k, v in parts.items():  # weighted like the objective, before the head weights
                    tb.add_scalar(f"train_loss/{k}", float((v.detach() * w).sum() / w.sum()), global_step)
                now = time.time()
                tb.add_scalar("system/posts_per_second", posts_since_log / max(now - t_log, 1e-9), global_step)
                tb.add_scalar("system/gpu_memory_gb", torch.cuda.max_memory_allocated() / 2**30, global_step)
                t_log, posts_since_log = now, 0
        bar.close()
        val_out = predict(model, va, tok, a.max_len)
        # Everything for validation this epoch: scores (val/...), loss parts (val_loss/...),
        # confidence and label-source splits, confusion matrix and calibration chart.
        vl = tbreport.log(tb, epoch + 1, "val", space, eq, va, val_out, conf_floor=a.conf_floor)
        vm = vl["scores"]
        if human_data is not None:
            human.log(tb, epoch + 1, space, human_data, predict(model, human_data, tok, a.max_len))
        history.append({"epoch": epoch + 1, "train_loss": running / n, "val_loss": vl["loss"]["total"],
                        "val_broad_top1": vm["broad_top1"], "val_path_top1": vm["path_top1"],
                        "val_broad_top3": vm["broad_top3"], "val_path_top3": vm["path_top3"],
                        "val_path_plausible_equiv": vm["path_plausible_equiv"]})
        tb.flush()
        print(f"epoch {epoch + 1}: train loss {running / n:.3f} · validation loss {vl['loss']['total']:.3f} · "
              f"broad {vm['broad_top1']:.1%} (top-3 {vm['broad_top3']:.1%}) · path {vm['path_top1']:.1%} "
              f"(top-3 {vm['path_top3']:.1%}, plausible or look-alike {vm['path_plausible_equiv']:.1%}) · "
              f"{time.time() - t_start:.0f}s elapsed", flush=True)
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
                              torch.sigmoid(test_s).numpy(), F.softmax(test_t, -1).numpy(), eq)
    if te_live is not None:
        lb, lp, ls, lt = predict(model, te_live, tok, a.max_len)
        metrics["live_test"] = common.evaluate(space, F.softmax(lb / t_broad, -1).numpy(), F.softmax(lp / t_path, -1).numpy(), te_live,
                                               torch.sigmoid(ls).numpy(), F.softmax(lt, -1).numpy(), eq)
        lm = metrics["live_test"]
        print(f"LIVE TEST ({len(te_live)} held-out live posts): broad {lm['broad_top1']:.1%} (top-3 {lm['broad_top3']:.1%}) · "
              f"path {lm['path_top1']:.1%} (plausible or look-alike {lm['path_plausible_equiv']:.1%})", flush=True)
        for k in ("broad_top1", "broad_top3", "path_top1", "path_top3", "path_plausible_equiv", "broad_ece"):
            tb.add_scalar(f"live_test/{k}", lm[k], best_epoch + 1)
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
    for k in ("broad_top1", "broad_top3", "path_top1", "path_top3", "path_top1_equiv", "path_plausible",
              "path_plausible_equiv", "broad_ece", "tone_top1"):
        tb.add_scalar(f"test/{k}", metrics[k], best_epoch + 1)
    tb.close()

    # The full final report (run <out>/tb/final): val and test with every chart and table,
    # the common Jev-only test yardstick, people's judgments, and the HParams row.
    name = os.path.basename(a.out.rstrip("/"))
    print("\nwriting the final TensorBoard report ...", flush=True)
    tb_report.final_report(a.out, model, tok, space=space, eq=eq, export=a.export, va=va, te=te,
                           temps=(t_broad, t_path), best_epoch=best_epoch, max_len=a.max_len,
                           hparams=tb_report.hparams_for(name, config, metrics, a.export), conf_floor=a.conf_floor)
    print(f"\nTEST (windows {', '.join(info['test_windows'])}, {len(te)} posts, best epoch {best_epoch + 1}): "
          f"broad {metrics['broad_top1']:.1%} (top-3 {metrics['broad_top3']:.1%}) · path {metrics['path_top1']:.1%} "
          f"(top-3 {metrics['path_top3']:.1%}, one of Jev's plausible answers {metrics['path_plausible_equiv']:.1%}) · "
          f"GPU {metrics['gpu_posts_per_second']:.0f} posts/s · saved to {a.out}", flush=True)


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
