"""Write a finished model's full report into TensorBoard, as the run <model>/tb/final:

  - final_val, final_test: every score, loss part, confidence and label-source split,
    confusion matrix, calibration chart and per-topic table (tbreport.log)
  - final_test_common: the same scores on the test windows of the Jev-only export, the
    one yardstick every run is measured on (v1, v2, and relabeled runs alike)
  - human: the model's answers on posts people judged, and an example table
  - HParams: settings and headline scores, so runs compare in one table

train.py calls final_report() when training ends. For models trained before that:

    uv run python tb_report.py --model /data/models/v2
    uv run python tb_report.py --model /data/models/v1 --device cpu --limit 300   # quick check

Re-running replaces <model>/tb/final. Also writes <model>/report.json.
"""

import argparse
import json
import os
import shutil

import numpy as np
import torch
from torch.utils.tensorboard import SummaryWriter
from transformers import AutoTokenizer

import common
import tbreport

COMMON_EXPORT = "/data/exports/v1-final"
HEADLINE = ["broad_top1", "path_top1", "path_plausible_equiv", "broad_ece"]


def _sample(d: common.Data, n: int, seed: int = 0) -> common.Data:
    if not n or len(d) <= n:
        return d
    return d.subset(np.sort(np.random.default_rng(seed).choice(len(d), n, replace=False)))


def summary_markdown(name: str, res: dict, hparams: dict) -> str:
    lines = [f"**{name}** · base `{hparams.get('base')}` · export `{hparams.get('export')}` · "
             f"{hparams.get('n_train')} training posts · best epoch {hparams.get('best_epoch')}", "",
             "| split | posts | broad = label | path = label | path plausible or look-alike | broad ECE |",
             "|---|---|---|---|---|---|"]
    for k in ("final_val", "final_test", "final_test_common"):
        if k in res:
            s = res[k]["scores"]
            lines.append(f"| {k} | {res[k]['n']} | {s['broad_top1']:.1%} | {s['path_top1']:.1%} | "
                         f"{s['path_plausible_equiv']:.1%} | {s['broad_ece']:.3f} |")
    h = res.get("human")
    if h:
        lines += ["", f"People's judgments, {h['posts']} posts: accepted {h['accepted']}, rejected {h['rejected']}, "
                      f"never judged {h['unjudged']} ({h['accepted_share_of_judged']:.0%} accepted of those judged)"]
    return "\n".join(lines)


def final_report(out_dir: str, model, tok, *, space, eq, export: str, va: common.Data, te: common.Data,
                 temps: tuple[float, float], best_epoch: int, max_len: int, hparams: dict, conf_floor: float = 0.3,
                 common_export: str = COMMON_EXPORT, device: str = "cuda", bs: int = 256, limit: int = 0) -> dict:
    step = best_epoch + 1
    tb_dir = os.path.join(out_dir, "tb", "final")
    shutil.rmtree(tb_dir, ignore_errors=True)
    tb = SummaryWriter(tb_dir)
    res = {}
    outs = {}
    for prefix, d in (("final_val", va), ("final_test", te)):
        outs[prefix] = tbreport.predict(model, d, tok, max_len, bs, device)
        res[prefix] = {"n": len(d), **tbreport.log(tb, step, prefix, space, eq, d, outs[prefix], temps=temps,
                                                   conf_floor=conf_floor)}
        print(f"  {prefix}: {len(d)} posts, path plausible or look-alike {res[prefix]['scores']['path_plausible_equiv']:.1%}", flush=True)

    # The common yardstick: test windows of the Jev-only export.
    if os.path.abspath(common_export) == os.path.abspath(export):
        ce_full = te
    else:
        _, _, ce_full, _ = common.split(common.load(common_export, space))
    human = tbreport.Human()
    hd = human.subset(ce_full)
    ce = _sample(ce_full, limit)
    ce_outs = outs["final_test"] if ce is te else tbreport.predict(model, ce, tok, max_len, bs, device)
    res["final_test_common"] = {"n": len(ce), **tbreport.log(tb, step, "final_test_common", space, eq, ce, ce_outs,
                                                             temps=temps, conf_floor=conf_floor, charts=False)}
    print(f"  final_test_common: {len(ce)} posts, path plausible or look-alike "
          f"{res['final_test_common']['scores']['path_plausible_equiv']:.1%}", flush=True)
    if hd is not None:
        res["human"] = human.log(tb, step, space, hd, tbreport.predict(model, hd, tok, max_len, bs, device), temps=temps)
        print(f"  human: {res['human']}", flush=True)

    metrics = {f"hparam/common_test_{k}": res["final_test_common"]["scores"][k] for k in HEADLINE}
    if "human" in res:
        metrics["hparam/human_accepted_of_judged"] = res["human"]["accepted_share_of_judged"]
        metrics["hparam/human_unjudged"] = float(res["human"]["unjudged"])
    tb.add_hparams(hparams, metrics, run_name=".")
    tb.add_text("final/summary", summary_markdown(os.path.basename(out_dir.rstrip("/")), res, hparams), step)
    tb.close()
    json.dump({"hparams": hparams, **res}, open(os.path.join(out_dir, "report.json"), "w"), indent=2)
    return res


def hparams_for(name: str, cfg: dict, metrics: dict, export: str) -> dict:
    args = metrics.get("args", {})
    return {"run": name, "base": cfg["base"], "export": os.path.basename(export.rstrip("/")),
            "epochs": int(args.get("epochs", 0)), "lr": float(args.get("lr", 0)), "bs": int(args.get("bs", 0)),
            "max_len": int(cfg.get("max_len", 0)), "conf_floor": float(args.get("conf_floor", 0.3)),
            "n_train": int(metrics["split"]["n_train"]), "best_epoch": int(metrics["best_epoch"]) + 1}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--model", required=True)
    ap.add_argument("--export", default=None, help="default: the export the model was trained on")
    ap.add_argument("--common-export", default=COMMON_EXPORT)
    ap.add_argument("--taxonomy", default="../taxonomy/v1.yaml")
    ap.add_argument("--device", default="cuda")
    ap.add_argument("--bs", type=int, default=256)
    ap.add_argument("--limit", type=int, default=0, help="score only this many val/test posts (quick check)")
    a = ap.parse_args()

    from train import Student  # here, not at the top: train.py imports this module

    cfg = json.load(open(f"{a.model}/config.json"))
    metrics = json.load(open(f"{a.model}/metrics.json"))
    export = a.export or metrics["args"]["export"]
    space = common.Space.from_taxonomy(a.taxonomy)
    eq = common.Equivalence.for_taxonomy(a.taxonomy, space)
    _, va, te, _ = common.split(common.load(export, space))
    va, te = _sample(va, a.limit), _sample(te, a.limit)

    tok = AutoTokenizer.from_pretrained(a.model)
    model = Student(cfg["base"], len(space.broad), len(space.paths)).to(a.device)
    model.load_state_dict(torch.load(f"{a.model}/model.pt", map_location=a.device))
    name = os.path.basename(a.model.rstrip("/"))
    print(f"{name}: export {export}, common yardstick {a.common_export}", flush=True)
    final_report(a.model, model, tok, space=space, eq=eq, export=export, va=va, te=te,
                 temps=(cfg["temperature_broad"], cfg["temperature_path"]), best_epoch=metrics["best_epoch"],
                 max_len=cfg["max_len"], hparams=hparams_for(name, cfg, metrics, export),
                 conf_floor=float(metrics.get("args", {}).get("conf_floor", 0.3)),
                 common_export=a.common_export, device=a.device, bs=a.bs, limit=a.limit)
    print(f"wrote {a.model}/tb/final and {a.model}/report.json")


if __name__ == "__main__":
    main()
