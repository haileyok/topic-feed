"""The newer model's hand-checked accuracy on every post you judged for the older one.

Posts whose answer (best broad topic and best subtopic path) did not change keep the verdict you gave the older
model. Posts whose answer changed use the verdicts you gave the newer model (mm_eval_changed.py's page). Also
reports how the changed posts moved (right before and wrong now, wrong before and right now, ...), the named
topics, and the meme calls, using every meme verdict from both rounds as ground truth.

    python3 mm_eval_combine.py --old /data/models/mm1/eval /data/models/mm1/eval-pictures --new-name mm2 \\
        --changed /data/models/mm2/eval-changed
"""

import argparse
import collections
import json
import math
import os

import numpy as np


def wilson(k, n, z=1.96):
    if not n:
        return (0, 0)
    p = k / n
    d = 1 + z * z / n
    c = (p + z * z / (2 * n)) / d
    h = z * math.sqrt(p * (1 - p) / n + z * z / (4 * n * n)) / d
    return (round(100 * (c - h)), round(100 * (c + h)))


def last(path):
    out = {}
    for line in open(path):
        v = json.loads(line)
        out[v["uri"]] = v
    return out


def top(p, key):
    return max(p[key].items(), key=lambda kv: kv[1])


def auc(score, lab):
    pos, neg = score[lab], score[~lab]
    if not len(pos) or not len(neg):
        return float("nan")
    return float(np.mean([(p > n) + 0.5 * (p == n) for p in pos for n in neg]))


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--old", nargs="+", required=True)
    ap.add_argument("--new-name", required=True)
    ap.add_argument("--changed", required=True)
    ap.add_argument("--posts", default="/data/clef/full_posts.jsonl")
    ap.add_argument("--list", type=int, default=12)
    ap.add_argument("--allow-missing", action="store_true", help="leave out changed posts that have no verdict yet instead of stopping")
    a = ap.parse_args()

    rows = {}
    for d in a.old:
        teacher = {p["uri"]: p["teacher"] for p in json.load(open(os.path.join(d, "sample.json")))["posts"]}
        p1 = {json.loads(l)["uri"]: json.loads(l) for l in open(os.path.join(d, "predictions.jsonl"))}
        p2 = {json.loads(l)["uri"]: json.loads(l) for l in open(os.path.join(d, f"predictions-{a.new_name}.jsonl"))}
        v1 = last(os.path.join(d, "verdicts.jsonl"))
        for u in teacher:
            rows[u] = {"uri": u, "teacher": teacher[u], "m1": p1[u], "m2": p2[u], "v1": v1[u], "pics": p1[u]["images_shown"] > 0}
    v2 = last(os.path.join(a.changed, "verdicts.jsonl"))
    for u, r in rows.items():
        r["changed"] = top(r["m1"], "broad")[0] != top(r["m2"], "broad")[0] or top(r["m1"], "paths")[0] != top(r["m2"], "paths")[0]
        r["v2"] = v2.get(u)
        r["now"] = (r["v2"] if r["changed"] else r["v1"])
    missing = [u for u, r in rows.items() if r["changed"] and not r["v2"]]
    if missing and not a.allow_missing:
        raise SystemExit(f"{len(missing)} changed posts have no verdict yet (use --allow-missing to leave them out)")
    for u in missing:
        del rows[u]
    n_changed = sum(r["changed"] for r in rows.values())

    out = [f"# The new model on the {len(rows)} hand-checked posts\n",
           f"{len(rows) - n_changed} posts kept the first model's answer, so your earlier verdict stands; {n_changed} changed and were judged again."
           + (f" {len(missing)} changed posts have no verdict yet and are left out." if missing else "") + "\n",
           "## Topic verdicts on the new model's answers\n",
           "| posts | right | acceptable | wrong | can't tell | right, of the decided | right or acceptable, of the decided | first model: right / right or acceptable |",
           "|---|---|---|---|---|---|---|---|"]
    for label, sel in (("text", [r for r in rows.values() if not r["pics"]]), ("pictures", [r for r in rows.values() if r["pics"]]), ("all", list(rows.values()))):
        c = collections.Counter(r["now"]["topic"] for r in sel)
        o = collections.Counter(r["v1"]["topic"] for r in sel)
        dec, odec = c["right"] + c["acceptable"] + c["wrong"], o["right"] + o["acceptable"] + o["wrong"]
        out.append(f"| {label}: {len(sel)} | {c['right']} | {c['acceptable']} | {c['wrong']} | {c['unsure']} | {c['right']}/{dec} = {100 * c['right'] / dec:.0f}% (CI {wilson(c['right'], dec)}) | "
                   f"{c['right'] + c['acceptable']}/{dec} = {100 * (c['right'] + c['acceptable']) / dec:.0f}% (CI {wilson(c['right'] + c['acceptable'], dec)}) | "
                   f"{100 * o['right'] / odec:.0f}% / {100 * (o['right'] + o['acceptable']) / odec:.0f}% |")
    out.append("")

    out.append("## How the changed answers moved (your verdict on the first model's answer -> your verdict on the new model's)\n")
    trans = collections.Counter((r["v1"]["topic"], r["v2"]["topic"]) for r in rows.values() if r["changed"])
    out.append("| before \\ now | right | acceptable | wrong | can't tell |\n|---|---|---|---|---|")
    for b in ("right", "acceptable", "wrong", "unsure"):
        tot = sum(trans[(b, n)] for n in ("right", "acceptable", "wrong", "unsure"))
        if tot:
            out.append(f"| {b} ({tot}) | " + " | ".join(str(trans[(b, n)]) for n in ("right", "acceptable", "wrong", "unsure")) + " |")
    out.append("")

    # the named topics
    fix = [r for r in rows.values() if r["v1"]["should_be"]]
    hit1 = sum(1 for r in fix if top(r["m1"], "broad")[0] == r["v1"]["should_be"].split("/")[0])
    hit2 = sum(1 for r in fix if top(r["m2"], "broad")[0] == r["v1"]["should_be"].split("/")[0])
    out.append(f"## The {len(fix)} posts where you named the right topic\n\nBroad topic matches your answer: first model {hit1}, new model {hit2}.\n")

    # memes: ground truth from every meme verdict in both rounds (the verdict is about the call that was shown, at 50%)
    truth = {}
    conflicts = []
    for r in rows.values():
        for m, v in ((r["m1"], r["v1"]), (r["m2"], r["v2"])):
            if v and v.get("meme"):
                called = m["signals"]["meme"] >= 0.5
                t = called if v["meme"] == "right" else (not called)
                if r["uri"] in truth and truth[r["uri"]] != t:
                    conflicts.append(r["uri"])
                truth[r["uri"]] = t
    labelled = [r for r in rows.values() if r["uri"] in truth]
    lab = np.array([truth[r["uri"]] for r in labelled])
    out.append(f"## Meme calls\n\n{len(labelled)} picture posts have a meme verdict from you in one round or the other: {int(lab.sum())} are memes, {int((~lab).sum())} are not"
               f" ({len(conflicts)} posts where your two rounds disagree).\n")
    out.append("| model | ranking score | memes found at 50% / false alarms | at 40% | at 30% |\n|---|---|---|---|---|")
    for name, key in (("first (3 epochs)", "m1"), ("new (8 epochs)", "m2")):
        s = np.array([r[key]["signals"]["meme"] for r in labelled])
        cells = [f"{int((s[lab] >= t).sum())} of {int(lab.sum())} / {int((s[~lab] >= t).sum())}" for t in (0.5, 0.4, 0.3)]
        out.append(f"| {name} | {auc(s, lab):.2f} | " + " | ".join(cells) + " |")
    out.append("")

    # the lists
    want = {r["uri"] for r in rows.values() if r["changed"]}
    texts = {}
    with open(a.posts) as f:
        for line in f:
            if line.startswith('{"uri":"'):
                u = json.loads('"' + line[8:line.index('"', 8)] + '"')
                if u in want:
                    texts[u] = json.loads(line).get("text", "")
    order = {"right": 0, "acceptable": 1, "wrong": 2, "unsure": 3}

    def show(title, cond):
        sel = [r for r in rows.values() if r["changed"] and cond(r)]
        if not sel:
            return
        out.append(f"## {title} ({len(sel)})\n")
        for r in sel[: a.list]:
            did, rkey = r["uri"][5:].split("/app.bsky.feed.post/")
            b1, b2 = top(r["m1"], "broad"), top(r["m2"], "broad")
            p1, p2 = top(r["m1"], "paths"), top(r["m2"], "paths")
            out.append(f"- [{'pictures' if r['pics'] else 'text'}] https://bsky.app/profile/{did}/post/{rkey}")
            out.append(f"  - post: {texts.get(r['uri'], '')[:140]!r}")
            out.append(f"  - first model: {b1[0]} {b1[1]:.0%} / {p1[0]}  ->  new model: {b2[0]} {b2[1]:.0%} / {p2[0]} · teacher: {r['teacher']['broad']} {r['teacher']['broad_p']:.0%}"
                       f" · you: {r['v1']['topic']} -> {r['v2']['topic']}" + (f" · should be: {r['v2']['should_be']}" if r["v2"]["should_be"] else "") + (f" · note: {r['v2']['note']}" if r["v2"]["note"] else ""))
        out.append("")

    show("Judged right before, wrong now (steps backwards)", lambda r: r["v1"]["topic"] == "right" and r["v2"]["topic"] == "wrong")
    show("Judged wrong before, right or acceptable now (fixed)", lambda r: r["v1"]["topic"] == "wrong" and r["v2"]["topic"] in ("right", "acceptable"))
    show("Still wrong", lambda r: r["v1"]["topic"] == "wrong" and r["v2"]["topic"] == "wrong")
    text = "\n".join(out)
    open(os.path.join(a.changed, "report.md"), "w").write(text)
    print(text)


if __name__ == "__main__":
    main()
