"""Summarises the hand check of the picture-capable student (mm_eval_app.py's verdicts).

Per kind of post (text and link cards / pictures): how many topics were judged right, acceptable,
wrong, can't tell; how that splits by the student's confidence; and, now that the teachers' answers
may be looked at, how the verdicts relate to them: where the student agreed with its teacher and was
right or wrong, and where it differed from the teacher and who the judge sided with. Also lists the
wrong posts and the meme verdicts.

    python3 mm_eval_report.py --dir /data/models/mm1/eval
"""

import argparse
import collections
import json
import os


def pct(a, b):
    return f"{100 * a / b:.0f}%" if b else "n/a"


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--dir", default="/data/models/mm1/eval")
    ap.add_argument("--posts", default="/data/clef/full_posts.jsonl")
    ap.add_argument("--list-wrong", type=int, default=40)
    a = ap.parse_args()

    sample = json.load(open(os.path.join(a.dir, "sample.json")))["posts"]
    teacher = {p["uri"]: p["teacher"] for p in sample}
    order = {p["uri"]: i + 1 for i, p in enumerate(sample)}
    pred = {}
    for line in open(os.path.join(a.dir, "predictions.jsonl")):
        r = json.loads(line)
        pred[r["uri"]] = r
    verdicts = {}
    for line in open(os.path.join(a.dir, "verdicts.jsonl")):
        v = json.loads(line)
        verdicts[v["uri"]] = v

    def student(uri):
        r = pred[uri]
        broad = max(r["broad"].items(), key=lambda kv: kv[1])
        path = max(r["paths"].items(), key=lambda kv: kv[1])
        return broad, path

    kinds = {"text": [], "pictures": []}
    for u in sample:
        kinds["pictures" if pred[u["uri"]]["images_shown"] > 0 else "text"].append(u["uri"])

    out = [f"# Hand check of the new model\n", f"{len(verdicts)} of {len(sample)} posts judged.\n"]
    out.append("## Topic verdicts\n\n| posts | judged | right | acceptable | wrong | can't tell | right or acceptable (of decided) | right only (of decided) |\n|---|---|---|---|---|---|---|---|")
    tot = collections.Counter()
    for kind, uris in list(kinds.items()) + [("all", [u for k in kinds.values() for u in k])]:
        c = collections.Counter(verdicts[u]["topic"] for u in uris if u in verdicts)
        dec = c["right"] + c["acceptable"] + c["wrong"]
        out.append(f"| {kind}: {len(uris)} | {sum(c.values())} | {c['right']} | {c['acceptable']} | {c['wrong']} | {c['unsure']} | "
                   f"{c['right'] + c['acceptable']} of {dec} ({pct(c['right'] + c['acceptable'], dec)}) | {c['right']} of {dec} ({pct(c['right'], dec)}) |")
    out.append("")

    out.append("## By the student's confidence in its broad topic (decided posts only)\n\n| confidence | posts | right | acceptable | wrong |\n|---|---|---|---|---|")
    bins = [("90% or more", .9, 1.01), ("60-90%", .6, .9), ("under 60%", 0, .6)]
    for name, lo, hi in bins:
        c = collections.Counter(verdicts[u]["topic"] for u in verdicts if lo <= student(u)[0][1] < hi and verdicts[u]["topic"] != "unsure")
        out.append(f"| {name} | {sum(c.values())} | {c['right']} | {c['acceptable']} | {c['wrong']} |")
    out.append("")

    out.append("## Against the teachers' answers (not shown while judging)\n")
    agree_n = sum(1 for u in sample if student(u["uri"])[0][0] == u["teacher"]["broad"])
    out.append(f"On these {len(sample)} posts the student's broad topic equals the teacher's on {agree_n} ({pct(agree_n, len(sample))}); "
               f"for the full test set it was 74.6% (text) and 59.7% (pictures).\n")
    out.append("| | student = teacher (broad) | student differs from teacher |\n|---|---|---|")
    grid = collections.defaultdict(collections.Counter)
    for u, v in verdicts.items():
        if v["topic"] == "unsure":
            continue
        same = student(u)[0][0] == teacher[u]["broad"]
        good = "right or acceptable" if v["topic"] in ("right", "acceptable") else "wrong"
        grid[good][same] += 1
    for good in ("right or acceptable", "wrong"):
        out.append(f"| judged {good} | {grid[good][True]} | {grid[good][False]} |")
    out.append("")
    out.append("Reading the table: 'judged wrong' with 'student = teacher' is a mistake both share; 'judged right or acceptable' with "
               "'differs from teacher' is a place where the student was right and its teacher was not.\n")

    # where the judge gave a topic to use instead, did it match the teacher?
    fix = [(u, v) for u, v in verdicts.items() if v["topic"] in ("wrong", "acceptable") and v.get("should_be")]
    if fix:
        def top_of(s):
            return s.split("/")[0]
        t_ok = sum(1 for u, v in fix if top_of(v["should_be"]) == teacher[u]["broad"])
        s_ok = sum(1 for u, v in fix if top_of(v["should_be"]) == student(u)[0][0])
        out.append(f"Where you named a topic to use instead ({len(fix)} posts, broad topic compared): it matched the teacher's on {t_ok} and the student's on {s_ok}.\n")

    memes = [(u, v) for u, v in verdicts.items() if v.get("meme")]
    if memes:
        c = collections.Counter(v["meme"] for _, v in memes)
        called = sum(1 for u, _ in memes if pred[u]["signals"]["meme"] >= .5)
        out.append(f"## Meme calls\n\n{len(memes)} picture posts judged: {c['right']} right, {c['wrong']} wrong. The student called {called} of them a meme.\n")
        for u, v in memes:
            if v["meme"] == "wrong":
                out.append(f"- #{order[u]}: student said {'meme' if pred[u]['signals']['meme'] >= .5 else 'not a meme'} ({pred[u]['signals']['meme']:.0%}); judged wrong")
        out.append("")

    # the posts' text, for the lists
    want = set(u for u, v in verdicts.items() if v["topic"] in ("wrong", "acceptable", "unsure"))
    texts = {}
    with open(a.posts) as f:
        for line in f:
            if '"uri"' in line[:20]:
                r = json.loads(line)
                if r["uri"] in want:
                    texts[r["uri"]] = r.get("text", "")
    for label, topic in (("Wrong", "wrong"), ("Acceptable but not right", "acceptable"), ("Can't tell", "unsure")):
        rows = [(u, v) for u, v in sorted(verdicts.items(), key=lambda kv: order[kv[0]]) if v["topic"] == topic]
        if not rows:
            continue
        out.append(f"## {label} ({len(rows)})\n")
        for u, v in rows[: a.list_wrong]:
            (sb, sbp), (sp, spp) = student(u)
            t = teacher[u]
            did, rkey = u[5:].split("/app.bsky.feed.post/")
            out.append(f"- #{order[u]} [{'pictures' if pred[u]['images_shown'] else 'text'}] https://bsky.app/profile/{did}/post/{rkey}")
            out.append(f"  - post: {texts.get(u, '')[:160]!r}")
            out.append(f"  - student: {sb} {sbp:.0%} / {sp} {spp:.0%} · teacher ({'Clef' if teacher[u] and pred[u]['source'] == 'clef' else 'Jev'}): {t['broad']} {t['broad_p']:.0%} / {t['path']}"
                       f" · you said it should be: {v.get('should_be') or '(not given)'}" + (f" · note: {v['note']}" if v.get("note") else ""))
        out.append("")

    text = "\n".join(out)
    open(os.path.join(a.dir, "report.md"), "w").write(text)
    print(text)


if __name__ == "__main__":
    main()
