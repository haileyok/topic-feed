"""Compare student models on held-out live posts and on the labeling-window test set.

Each model sees the input it gets in production, chosen by the post document version
in its config.json:
  - pd2 models: the export's model_input (attachments, image text, and labels on their
    own lines, as Jev saw them)
  - pd1 models: post_pipeline.model_input as the pipeline served it (text found in
    images folded into the alt text), from --production-inputs

The live test set is the same hash-picked hold-out train.py uses (common.live_holdout),
so no model trained with these settings has seen it.

    uv run python compare_live.py --export /data/exports/v4 --live-configs f101541647c7 \
        --production-inputs /data/exports/v4/production_inputs.jsonl.gz --models v3-blend v4 \
        --out /data/models/v4/compare_live.json
"""

import argparse
import gzip
import json

import numpy as np

import common

TAX = "../taxonomy/v1.yaml"
NEW_LINES = ("\n[media] ", "\n[labels] ", "\n[image text] ", "\n[image description] ")


def model_probs(model_dir: str, space, *sets: common.Data):
    """Calibrated probabilities (broad, path, signals, tone) for each data set."""
    import torch
    from transformers import AutoTokenizer

    import tbreport
    from train import Student

    cfg = json.load(open(f"{model_dir}/config.json"))
    tok = AutoTokenizer.from_pretrained(model_dir)
    m = Student(cfg["base"], len(space.broad), len(space.paths), n_signals=len(cfg["signals"])).cuda()
    m.load_state_dict(torch.load(f"{model_dir}/model.pt", map_location="cuda"))
    out = [tbreport.probs(tbreport.predict(m, d, tok, cfg["max_len"]), cfg["temperature_broad"], cfg["temperature_path"])
           for d in sets]
    del m
    torch.cuda.empty_cache()
    return out


def with_texts(d: common.Data, texts: list[str]) -> common.Data:
    return common.Data(d.uris, texts, d.windows, d.broad, d.path, d.signals, d.tone, d.conf, d.sources, d.configs)


def headline(m: dict) -> dict:
    keys = ("n", "broad_top1", "broad_top3", "path_top1", "path_top3", "path_plausible_equiv", "broad_ece", "tone_top1")
    out = {k: m[k] for k in keys if k in m}
    if "signals" in m:
        out["signals_corr"] = {k: round(v["corr"], 3) for k, v in m["signals"].items()}
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--export", required=True)
    ap.add_argument("--live-configs", required=True)
    ap.add_argument("--live-test", type=int, default=10000)
    ap.add_argument("--live-val", type=int, default=3000)
    ap.add_argument("--production-inputs", required=True, help="jsonl.gz of {uri, model_input} from post_pipeline")
    ap.add_argument("--models", nargs="+", required=True)
    ap.add_argument("--out", default="")
    a = ap.parse_args()

    space = common.Space.from_taxonomy(TAX)
    eq = common.Equivalence.for_taxonomy(TAX, space)
    live_configs = set(a.live_configs.split(","))
    rest, live, _ = common.live_holdout(common.load(a.export, space), live_configs, a.live_test, a.live_val)
    _, _, win_te, info = common.split(rest)

    prod = {}
    with gzip.open(a.production_inputs, "rt") as f:
        for line in f:
            r = json.loads(line)
            prod[r["uri"]] = r["model_input"]
    missing = [u for u in live.uris if u not in prod]
    if missing:
        raise SystemExit(f"{len(missing)} live test posts have no production input (e.g. {missing[0]})")

    has_new = np.array([any(s in "\n" + t for s in NEW_LINES) for t in live.texts])
    groups = {"all": np.arange(len(live)), "with_new_lines": np.flatnonzero(has_new), "without_new_lines": np.flatnonzero(~has_new)}
    jev_top = live.broad.argmax(1)
    res = {"live_test_posts": len(live), "window_test": info["test_windows"], "models": {}, "topic_mix": {}}
    res["topic_mix"]["Jev"] = {b: float((jev_top == i).mean()) for i, b in enumerate(space.broad)}
    hits = {}

    for name in a.models:
        mdir = f"/data/models/{name}"
        pd = json.load(open(f"{mdir}/config.json"))["postdoc_version"]
        d = live if pd != "pd1" else with_texts(live, [prod[u] for u in live.uris])
        (pb, pp, ps, pt), (wb, wp, ws, wt) = model_probs(mdir, space, d, win_te)
        r = {"postdoc_version": pd, "live": {}}
        for g, idx in groups.items():
            r["live"][g] = headline(common.evaluate(space, pb[idx], pp[idx], d.subset(idx), ps[idx], pt[idx], eq))
        full = common.evaluate(space, pb, pp, d, ps, pt, eq)
        r["live_per_class"] = full["broad_per_class"]
        r["window_test"] = headline(common.evaluate(space, wb, wp, win_te, ws, wt, eq))
        res["models"][name] = r
        res["topic_mix"][name] = {b: float((pb.argmax(1) == i).mean()) for i, b in enumerate(space.broad)}
        hits[name] = pb.argmax(1) == jev_top

    names = list(hits)
    res["paired_broad_top1"] = {f"{x} vs {y}": [int((hits[x] & ~hits[y]).sum()), int((~hits[x] & hits[y]).sum())]
                                for i, x in enumerate(names) for y in names[i + 1:]}

    print(f"live test: {len(live)} held-out posts ({has_new.sum()} with attachment, image-text, or label lines); "
          f"window test: {', '.join(info['test_windows'])} ({len(win_te)} posts)\n")
    print(f"{'model':10s} {'set':22s} {'n':>6s} {'broad':>7s} {'top-3':>7s} {'path':>7s} {'plaus.':>7s} {'ECE':>6s}")
    for name, r in res["models"].items():
        rows = [(f"live/{g}", m) for g, m in r["live"].items()] + [("window test", r["window_test"])]
        for label, m in rows:
            print(f"{name:10s} {label:22s} {m['n']:6d} {m['broad_top1']:7.1%} {m['broad_top3']:7.1%} {m['path_top1']:7.1%} "
                  f"{m['path_plausible_equiv']:7.1%} {m['broad_ece']:6.3f}")
    print("\npaired, live broad top-1 (A right & B wrong / A wrong & B right):")
    for k, (b, c) in res["paired_broad_top1"].items():
        print(f"  {k}: {b} / {c}")
    print("\ntopic mix on live test (share of posts by top broad topic):")
    cols = ["Jev"] + names
    print(f"  {'topic':18s} " + " ".join(f"{c:>9s}" for c in cols))
    for b in sorted(space.broad, key=lambda b: -res["topic_mix"]["Jev"][b]):
        print(f"  {b:18s} " + " ".join(f"{res['topic_mix'][c][b]:9.1%}" for c in cols))
    print("\nlive signals, correlation with Jev (- : the model has no such output):")
    print(f"  {'signal':18s} " + " ".join(f"{c:>9s}" for c in names))
    for k in common.SIGNALS:
        vals = [res["models"][c]["live"]["all"].get("signals_corr", {}).get(k) for c in names]
        print(f"  {k:18s} " + " ".join(f"{v:9.3f}" if v is not None else f"{'-':>9s}" for v in vals))
    print("\nlive recall by broad topic (Jev's top topic):")
    print(f"  {'topic':18s} {'n':>5s} " + " ".join(f"{c:>9s}" for c in names))
    for b in sorted(space.broad, key=lambda b: -res["topic_mix"]["Jev"][b]):
        pcs = [res["models"][c]["live_per_class"][b] for c in names]
        print(f"  {b:18s} {pcs[0]['support']:5d} " + " ".join(f"{(p['recall'] or 0):9.1%}" for p in pcs))
    if a.out:
        json.dump(res, open(a.out, "w"), indent=2)
        print(f"\nwrote {a.out}")


if __name__ == "__main__":
    main()
