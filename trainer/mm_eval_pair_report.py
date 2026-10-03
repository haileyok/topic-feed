"""Results of a blind side-by-side review (mm_eval_pair.py): how often the model's answer beat its teacher's.

Maps each post's A/B verdict back to the model and the teacher using the order stored in sample.json, then counts,
for text posts and picture posts: the model better, the teacher better, both fine, neither, can't tell; the share of
decided posts where each answer was acceptable (better or both fine), with 95% intervals; and an exact sign test on
the posts where you preferred one answer. Lists the posts in each group.

    python3 mm_eval_pair_report.py --dir /data/models/mm2/eval-disagree
"""

import argparse
import collections
import json
import math
import os


def wilson(k, n, z=1.96):
    if not n:
        return (0, 0)
    p = k / n
    d = 1 + z * z / n
    c = (p + z * z / (2 * n)) / d
    h = z * math.sqrt(p * (1 - p) / n + z * z / (4 * n * n)) / d
    return (round(100 * (c - h)), round(100 * (c + h)))


def sign_test(k, n):
    """two-sided exact binomial p for k of n at probability 1/2"""
    if not n:
        return 1.0
    tail = sum(math.comb(n, i) for i in range(0, min(k, n - k) + 1)) / 2 ** n
    return min(1.0, 2 * tail)


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--dir", default="/data/models/mm2/eval-disagree")
    ap.add_argument("--posts", default="/data/clef/full_posts.jsonl")
    ap.add_argument("--list", type=int, default=25)
    a = ap.parse_args()

    sample = json.load(open(os.path.join(a.dir, "sample.json")))["posts"]
    verdicts = {}
    lines = 0
    for line in open(os.path.join(a.dir, "verdicts.jsonl")):
        v = json.loads(line)
        verdicts[v["uri"]] = v
        lines += 1
    rows = []
    for p in sample:
        v = verdicts.get(p["uri"])
        if not v:
            continue
        shown = dict(zip(("a", "b"), p["order"]))  # which answer sits under A and B
        if v["pick"] in ("a", "b"):
            result = shown[v["pick"]] + "_better"
        else:
            result = v["pick"]  # both / neither / unsure
        rows.append({"p": p, "v": v, "result": result, "kind": "pictures" if p["source"] == "clef" else "text"})
    out = [f"# Model against its teacher, blind\n", f"{len(rows)} of {len(sample)} posts judged ({lines} verdict lines; the last one for a post counts).\n",
           "| posts | model better | teacher better | both fine | neither | can't tell |", "|---|---|---|---|---|---|"]
    groups = (("text", [r for r in rows if r["kind"] == "text"]), ("pictures", [r for r in rows if r["kind"] == "pictures"]), ("all", rows))
    for name, sel in groups:
        c = collections.Counter(r["result"] for r in sel)
        out.append(f"| {name}: {len(sel)} | {c['model_better']} | {c['teacher_better']} | {c['both']} | {c['neither']} | {c['unsure']} |")
    out.append("")
    out.append("## How often each answer was acceptable (better or both fine), of the posts you could decide\n")
    out.append("| | model | teacher | where you preferred one: model won | sign test p |\n|---|---|---|---|---|")
    for name, sel in groups:
        c = collections.Counter(r["result"] for r in sel)
        dec = len(sel) - c["unsure"]
        m_ok, t_ok = c["model_better"] + c["both"], c["teacher_better"] + c["both"]
        pref = c["model_better"] + c["teacher_better"]
        out.append(f"| {name} ({dec} decided) | {m_ok}/{dec} = {100 * m_ok / max(dec, 1):.0f}% (CI {wilson(m_ok, dec)}) | {t_ok}/{dec} = {100 * t_ok / max(dec, 1):.0f}% (CI {wilson(t_ok, dec)}) | "
                   f"{c['model_better']} of {pref} | {sign_test(c['model_better'], pref):.2f} |")
    out.append("")

    # what the named topics said
    fix = [r for r in rows if r["v"]["should_be"]]
    if fix:
        mt = sum(1 for r in fix if r["v"]["should_be"].split("/")[0] == r["p"]["model_top"]["broad"])
        tt = sum(1 for r in fix if r["v"]["should_be"].split("/")[0] == list(r["p"]["teacher"]["broad"])[0])
        out.append(f"Where you named the topic it should be ({len(fix)} posts): it was the model's top topic on {mt} and the teacher's top topic on {tt}.\n")

    want = {r["p"]["uri"] for r in rows}
    texts = {}
    with open(a.posts) as f:
        for line in f:
            if line.startswith('{"uri":"'):
                u = json.loads('"' + line[8:line.index('"', 8)] + '"')
                if u in want:
                    texts[u] = json.loads(line).get("text", "")

    def show(title, key):
        sel = [r for r in rows if r["result"] == key]
        if not sel:
            return
        out.append(f"## {title} ({len(sel)})\n")
        for r in sel[: a.list]:
            p = r["p"]
            did, rkey = p["uri"][5:].split("/app.bsky.feed.post/")
            t = p["teacher"]
            tb = list(t["broad"])[0]
            out.append(f"- [{r['kind']}] https://bsky.app/profile/{did}/post/{rkey}")
            out.append(f"  - post: {texts.get(p['uri'], '')[:140]!r}")
            out.append(f"  - model: {p['model_top']['broad']} / {p['model_top']['path']} · teacher: {tb} {t['broad'][tb]:.0%} / {list(t['paths'])[0]}"
                       + (f" · should be: {r['v']['should_be']}" if r["v"]["should_be"] else "") + (f" · note: {r['v']['note']}" if r["v"]["note"] else ""))
        out.append("")

    show("The model's answer was better", "model_better")
    show("The teacher's answer was better", "teacher_better")
    show("Neither was right", "neither")
    show("Both fine", "both")
    show("Can't tell", "unsure")
    text = "\n".join(out)
    open(os.path.join(a.dir, "report.md"), "w").write(text)
    print(text)


if __name__ == "__main__":
    main()
