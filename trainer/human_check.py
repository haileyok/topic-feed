"""Check automatic scores against human judgments.

Each human judgment says whether one model's answer (a topic path) for one post is right.
For every such answer this asks four automatic rules whether it matches Jev, and reports
how often each rule agrees with the human:

  exact           the answer is Jev's top path (what path_top1 measures)
  exact+equiv     it matches Jev's top path through taxonomy/<v>-equivalences.yaml
  plausible       it is one of Jev's plausible paths (common.PLAUSIBLE_RATIO)
  plausible+equiv it matches a plausible path through the equivalences

Answers only Jev gave are left out of the agreement numbers (every rule counts them
right). The human accuracy of Jev's top path is reported separately: a score measured
against Jev can't tell when Jev itself is wrong.

Reads both files the review pages download: the student-vs-Jev spot check (verdicts) and
the three-way reference review (marks).

    uv run python human_check.py
"""

import argparse
import glob
import json
import os

import numpy as np

import common

RULES = ["exact", "exact+equiv", "plausible", "plausible+equiv"]


def judgments(files: list[str]) -> dict[tuple[str, str], dict]:
    """(uri, path) -> {"right": bool, "models": set, "file": str}. Later files win on conflicts."""
    out: dict[tuple[str, str], dict] = {}

    def put(uri, path, right, model, f):
        k = (uri, path)
        prev = out.get(k)
        models = (prev["models"] if prev else set()) | {model}
        out[k] = {"right": right, "models": models, "file": os.path.basename(f)}

    for f in files:
        doc = json.load(open(f))
        if "verdicts" in doc:  # student-vs-Jev spot check: jev / student / both / neither
            for v in doc["verdicts"]:
                for model in ("jev", "student"):
                    put(v["uri"], v[f"{model}_path"], v["verdict"] in (model, "both"), model, f)
        elif "marks" in doc:  # three-way review: list of answers marked right
            for m in doc["marks"]:
                right = set(m["right"])
                # The same answer from two models gets the same verdict: right if either was marked.
                ok_paths = {m[j] for j in ("jev", "student", "llm") if j in right}
                for model in ("jev", "student", "llm"):
                    put(m["uri"], m[model], m[model] in ok_paths, model, f)
        else:
            raise SystemExit(f"{f}: neither verdicts nor marks")
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--human", nargs="*", default=sorted(glob.glob("../reference/human/*.json")))
    ap.add_argument("--export", default="/data/exports/v1-full", help="export holding Jev's distributions")
    ap.add_argument("--taxonomy", default="../taxonomy/v1.yaml")
    ap.add_argument("--out", default=None, help="write the result as JSON here")
    a = ap.parse_args()

    space = common.Space.from_taxonomy(a.taxonomy)
    eq = common.Equivalence.for_taxonomy(a.taxonomy, space)
    ident = common.Equivalence.identity(space)
    js = judgments(a.human)
    uris = {u for u, _ in js}
    d = common.load(a.export, space)
    row = {u: i for i, u in enumerate(d.uris) if u in uris}
    pi = {p: i for i, p in enumerate(space.paths)}
    missing = sorted(uris - row.keys())

    units = []  # judged (post, answer) pairs given by the student or the reference LLM
    jev_units = []  # judged answers equal to Jev's top path, from any model
    jev_mismatch = 0
    for (uri, path), j in js.items():
        if uri not in row or path not in pi:
            continue
        i = row[uri]
        target = d.path[i:i + 1]
        exact = np.zeros_like(target, dtype=bool)
        exact[0, target.argmax()] = True
        plaus = common.plausible_paths(target)
        quoting = np.array([common.quotes_a_post(d.texts[i])])
        ans = np.array([pi[path]])
        auto = {
            "exact": bool(exact[0, ans[0]]),
            "exact+equiv": bool(common.matches(ans, exact, eq, quoting)[0]),
            "plausible": bool(plaus[0, ans[0]]),
            "plausible+equiv": bool(common.matches(ans, plaus, eq, quoting)[0]),
        }
        assert auto["exact"] == bool(common.matches(ans, exact, ident, quoting)[0])
        u = {"uri": uri, "path": path, "models": sorted(j["models"]), "human": j["right"], **auto,
             "jev_top": space.paths[int(target.argmax())], "text": d.texts[i][:160]}
        if "jev" in j["models"] and path != u["jev_top"]:
            jev_mismatch += 1  # the review page showed a different Jev answer than this export has
        if auto["exact"]:
            jev_units.append(u)
        if j["models"] - {"jev"}:
            units.append(u)

    h = np.array([u["human"] for u in units])
    res = {"human_files": [os.path.basename(f) for f in a.human], "posts": len(row), "posts_missing_from_export": len(missing),
           "answers_judged": len(units), "human_says_right": float(h.mean()),
           "jev_answers_human_right": float(np.mean([u["human"] for u in jev_units])) if jev_units else None,
           "n_jev_answers": len(jev_units), "jev_answer_mismatches": jev_mismatch, "rules": {}}
    for r in RULES:
        s = np.array([u[r] for u in units])
        res["rules"][r] = {
            "says_right": float(s.mean()),
            "agrees_with_human": float((s == h).mean()),
            "right_by_rule_and_human": int((s & h).sum()),
            "right_by_rule_only": int((s & ~h).sum()),
            "right_by_human_only": int((~s & h).sum()),
        }
    # Per model: its accuracy by human judgment and by each rule, on the same answers.
    res["by_model"] = {}
    for model in ("student", "llm"):
        us = [u for u in units if model in u["models"]]
        if us:
            res["by_model"][model] = {"n": len(us), "human": float(np.mean([u["human"] for u in us])),
                                      **{r: float(np.mean([u[r] for u in us])) for r in RULES}}
    res["disagreements"] = {r: [{k: u[k] for k in ("path", "jev_top", "human", "models", "text")}
                                for u in units if u[r] != u["human"]] for r in ("exact", "plausible+equiv")}

    print(f"{res['posts']} judged posts ({len(missing)} not in {a.export}), {len(units)} non-Jev answers judged; "
          f"human says {res['human_says_right']:.0%} right. Jev's own answers: human says "
          f"{res['jev_answers_human_right']:.0%} right (n={len(jev_units)}).")
    if jev_mismatch:
        print(f"WARNING: {jev_mismatch} judged Jev answers differ from Jev's top path in {a.export}")
    print()
    print(f"{'rule':17s} {'says right':>10s} {'agrees w/ human':>16s} {'both right':>11s} {'rule only':>10s} {'human only':>11s}")
    for r, v in res["rules"].items():
        print(f"{r:17s} {v['says_right']:10.0%} {v['agrees_with_human']:16.0%} {v['right_by_rule_and_human']:11d} "
              f"{v['right_by_rule_only']:10d} {v['right_by_human_only']:11d}")
    print("\nper model (share right):")
    for model, v in res["by_model"].items():
        print(f"  {model:8s} n={v['n']:3d}  human {v['human']:.0%}  " + "  ".join(f"{r} {v[r]:.0%}" for r in RULES))
    if a.out:
        json.dump(res, open(a.out, "w"), indent=2)
        print(f"\nwrote {a.out}")


if __name__ == "__main__":
    main()
