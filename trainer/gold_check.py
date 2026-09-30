"""Score models against a human gold set (labels from the blind page, gold_page.py).

For each labeled post the person picked every acceptable topic path and the best one.
A model's top path is scored four ways:
  best         it is the person's best (starred) path
  acceptable   it is one of the acceptable paths
  +look-alike  it matches an acceptable path through taxonomy/<v>-equivalences.yaml
  broad        its broad topic is the broad topic of an acceptable path

Scores Jev (its label in --export), each student model, and optionally an LLM asked
fresh with relabel.py's prompt (answers cached in /data/relabel/gold-<name>.llm.jsonl).

    uv run python gold_check.py --labels ../reference/gold/v1-gold-100.labels-1.json \
        --models v1 v2 v3-blend --llm gpt-6-luna:api
"""

import argparse
import concurrent.futures as cf
import gzip
import json
import math
import os

import numpy as np

import common

TAX = "../taxonomy/v1.yaml"


def wilson(k: int, n: int) -> tuple[float, float]:
    if n == 0:
        return (0.0, 0.0)
    p, z = k / n, 1.96
    d = 1 + z * z / n
    c = p + z * z / (2 * n)
    r = z * math.sqrt(p * (1 - p) / n + z * z / (4 * n * n))
    return ((c - r) / d, (c + r) / d)


def score(answers: dict[str, str], gold: dict[str, dict], eq, pi, texts) -> dict:
    """answers: uri -> path. Returns per-rule hit lists (aligned with gold order)."""
    out = {"best": [], "acceptable": [], "look_alike": [], "broad": []}
    for u, g in gold.items():
        a = answers.get(u)
        acc = set(g["acceptable"])
        if a is None or a not in pi:
            for k in out:
                out[k].append(False)
            continue
        acc_idx = np.zeros((1, len(pi)), dtype=bool)
        for p in acc:
            if p in pi:
                acc_idx[0, pi[p]] = True
        out["best"].append(a == g["primary"])
        out["acceptable"].append(a in acc)
        out["look_alike"].append(bool(common.matches(np.array([pi[a]]), acc_idx, eq,
                                                     np.array([common.quotes_a_post(texts[u])]))[0]))
        out["broad"].append(a.split("/", 1)[0] in {p.split("/", 1)[0] for p in acc})
    return out


def student_answers(model_dir: str, space, uris: list[str], texts: dict[str, str]) -> dict[str, str]:
    import torch
    from transformers import AutoTokenizer

    import tbreport
    from train import Student

    cfg = json.load(open(f"{model_dir}/config.json"))
    tok = AutoTokenizer.from_pretrained(model_dir)
    m = Student(cfg["base"], len(space.broad), len(space.paths), n_signals=len(cfg["signals"])).cuda()
    m.load_state_dict(torch.load(f"{model_dir}/model.pt", map_location="cuda"))
    d = common.Data(uris, [texts[u] for u in uris], [""] * len(uris), np.zeros((len(uris), len(space.broad))),
                    np.zeros((len(uris), len(space.paths))), np.zeros((len(uris), 4)), np.zeros((len(uris), 6)),
                    np.zeros(len(uris)), ["window"] * len(uris))
    _, sp, _, _ = tbreport.probs(tbreport.predict(m, d, tok, cfg["max_len"]), cfg["temperature_broad"], cfg["temperature_path"])
    del m
    torch.cuda.empty_cache()
    return {u: space.paths[int(sp[i].argmax())] for i, u in enumerate(uris)}


def llm_answers(model: str, name: str, space, uris, texts) -> tuple[dict[str, str], float]:
    import relabel

    has_subs = {p.split("/", 1)[0] for p in space.paths if "/" in p}
    head = relabel.PROMPT_HEAD.format(taxonomy=relabel.taxonomy_text(TAX))
    cache_path = f"/data/relabel/gold-{name}.llm.jsonl"
    done = {}
    if os.path.exists(cache_path):
        for line in open(cache_path):
            x = json.loads(line)
            if "error" not in x["llm"]:
                done[x["uri"]] = x["llm"]
    todo = [u for u in uris if u not in done]
    with open(cache_path, "a") as f, cf.ThreadPoolExecutor(16) as pool:
        futs = {pool.submit(relabel.label_one, model, head, texts[u], space, has_subs, f"topic-feed-gold-{name}"): u for u in todo}
        for fut in cf.as_completed(futs):
            ans = fut.result()
            f.write(json.dumps({"uri": futs[fut], "llm": ans}) + "\n")
            if "error" not in ans:
                done[futs[fut]] = ans
    cost = sum(relabel.cost(a.get("usage", {})) for a in done.values())
    out = {}
    for u, a in done.items():
        joint = {k: a["broad"].get(k.split("/", 1)[0], 0) * v for k, v in a["sub"].items()}
        for b, v in a["broad"].items():
            if b not in has_subs:
                joint[b] = v
        if joint:
            out[u] = max(joint.items(), key=lambda kv: kv[1])[0]
    return out, cost


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--labels", required=True, help="labels downloaded from the gold page")
    ap.add_argument("--export", default="/data/exports/v1-final", help="export holding Jev's labels")
    ap.add_argument("--models", nargs="*", default=["v1", "v2", "v3-blend"])
    ap.add_argument("--llm", default="", help="also ask this LLM fresh (e.g. gpt-6-luna:api)")
    ap.add_argument("--fix", default="", help="JSON file of {uri: {acceptable, primary}} corrections to apply")
    ap.add_argument("--out", default="")
    a = ap.parse_args()

    space = common.Space.from_taxonomy(TAX)
    eq = common.Equivalence.for_taxonomy(TAX, space)
    pi = {p: i for i, p in enumerate(space.paths)}
    doc = json.load(open(a.labels))
    gold = {l["uri"]: l for l in doc["labels"] if not l["cant_judge"] and l["acceptable"]}
    if a.fix:
        for u, g in json.load(open(a.fix)).items():
            gold[u] = {**gold[u], **g}
    uris = list(gold)
    texts = {l["uri"]: l["text"] for l in doc["labels"]}
    bad = sorted({p for g in gold.values() for p in g["acceptable"] if p not in pi})
    if bad:
        raise SystemExit(f"unknown paths in labels: {bad}")

    answers = {}
    jev = {}
    with gzip.open(f"{a.export}/labels.jsonl.gz", "rt") as f:
        for line in f:
            r = json.loads(line)
            if r["uri"] in gold:
                texts[r["uri"]] = r["model_input"]
                ps = r["path_scores"] or {}
                bp = r["broad_probs"]
                cand = dict(ps)
                for b, v in bp.items():
                    if f"{b}/" not in " ".join(space.paths) and b in pi:
                        cand[b] = math.sqrt(v)  # broad topics without subtopics: comparable to path scores
                jev[r["uri"]] = max(cand.items(), key=lambda kv: kv[1])[0] if cand else None
    answers["Jev"] = jev
    for m in a.models:
        answers[m] = student_answers(f"/data/models/{m}", space, uris, texts)
    cost = None
    if a.llm:
        answers[a.llm], cost = llm_answers(a.llm, os.path.basename(a.labels).split(".")[0], space, uris, texts)

    n = len(gold)
    res = {"labels": os.path.basename(a.labels), "posts": n, "models": {}}
    hits = {}
    print(f"{n} gold posts ({len(doc['labels']) - n} skipped as can't-judge or empty)"
          + (f"; {a.llm} cost ${cost:.3f} at list price" if cost is not None else "") + "\n")
    print(f"{'model':16s} {'best':>14s} {'acceptable':>16s} {'+look-alike':>16s} {'broad':>14s}")
    for name, ans in answers.items():
        h = score(ans, gold, eq, pi, texts)
        hits[name] = h
        row = {}
        cells = []
        for k in ("best", "acceptable", "look_alike", "broad"):
            kk = sum(h[k])
            lo, hi = wilson(kk, n)
            row[k] = {"right": kk, "share": kk / n, "ci95": [round(lo, 3), round(hi, 3)]}
            cells.append(f"{kk:3d} ({kk / n:4.0%})" + (f" {lo:.0%}–{hi:.0%}" if k == "acceptable" else ""))
        res["models"][name] = {**row, "missing": sum(1 for u in uris if ans.get(u) is None)}
        print(f"{name:16s} {cells[0]:>14s} {cells[1]:>16s} {cells[2]:>16s} {cells[3]:>14s}")

    # Paired comparisons on "acceptable": posts where one model is right and the other isn't.
    names = list(answers)
    print("\npaired, acceptable (A right & B wrong / A wrong & B right):")
    res["paired"] = {}
    for i, x in enumerate(names):
        for y in names[i + 1:]:
            ax, ay = np.array(hits[x]["acceptable"]), np.array(hits[y]["acceptable"])
            b, c = int((ax & ~ay).sum()), int((~ax & ay).sum())
            res["paired"][f"{x} vs {y}"] = [b, c]
            print(f"  {x:>14s} vs {y:<14s} {b:2d} / {c:2d}")

    # Misses for the best student model, to see what goes wrong.
    res["answers"] = {u: {"gold": gold[u]["acceptable"], **{k: v.get(u) for k, v in answers.items()},
                          "text": texts[u][:140]} for u in uris}
    if a.out:
        json.dump(res, open(a.out, "w"), indent=2, ensure_ascii=False)
        print(f"\nwrote {a.out}")


if __name__ == "__main__":
    main()
