"""Does taxonomy v2 beat v1? The automatic checks that decide whether the big v2 runs start.

Compares four sets of answers for the same test posts: Jev under v1 (ClickHouse, taxonomy_version
v1), Clef-flash under v1 (the pulled result file), and both again under v2. The test posts are
listed in test-sets.json (2,000 text-only posts, of which 108 were judged by hand and 1,892 are a
random draw, plus the 500 picture posts).

Checks (fixed before the v2 answers were seen):

  A  text agreement   On the 1,892 random text posts, Jev and Clef disagree on the broad topic
                      for fewer posts under v2 than under v1, and the paired McNemar test
                      (one-sided) gives p < 0.05.
  B  picture posts    On the 500 picture posts, v2 agreement is not more than 3 points below v1.
  C  unclear          'unclear' is not more than 1.5 points more of Jev's, or of Clef's, v2
                      answers than of its v1 answers (random text posts).
  D  no collapse      Every topic Jev put at least 20 of the random text posts in under v1 keeps
                      between half and double that many under v2, no topic gets more than 25% of
                      the posts, and no topic present under v1 disappears from the 2,500 posts.
  E  judged posts     Listed for reading (the 139 posts judged by hand): who each model now
                      agrees with. Reported with scores, not a number to pass.

    trainer/.venv/bin/python v2_gates.py --clef-v2 /data/clef/v2/clef-v2-test.jsonl --out /data/clef/v2/test-report.md
"""

import argparse
import collections
import json
import math
import subprocess
import sys
from pathlib import Path

import yaml
from scipy.stats import binomtest

sys.path.insert(0, str(Path(__file__).resolve().parent))
import clef_calibrate as cc  # noqa: E402  (read_clef, read_jev)

TAX = Path(__file__).resolve().parent.parent / "taxonomy"
EDITED = ["us_politics", "world_news", "personal_life", "humor", "society", "online_culture", "unclear"]
PAIRS = [("humor", "personal_life"), ("us_politics", "world_news"), ("personal_life", "unclear"),
         ("online_culture", "personal_life"), ("humor", "unclear"), ("personal_life", "society"),
         ("society", "us_politics"), ("health", "personal_life"), ("online_culture", "technology"),
         ("entertainment", "humor")]


def read_jev_v2(uris, label_config):
    out = {}
    uris = list(uris)
    for i in range(0, len(uris), 800):
        lst = ",".join("'%s'" % u for u in uris[i:i + 800])
        q = ("SELECT uri, broad_probs, sub_probs, path_scores, signals FROM jev_labels FINAL WHERE taxonomy_version = 'v2' "
             f"AND label_config = '{label_config}' AND uri IN ({lst}) FORMAT JSONEachRow")
        res = subprocess.run(["docker", "exec", "-i", "topic-feed-clickhouse", "sh", "-c",
                              'clickhouse-client --user topicfeed --password "$CLICKHOUSE_PASSWORD" --database topicfeed'],
                             input=q, capture_output=True, text=True, check=True).stdout
        for line in res.splitlines():
            r = json.loads(line)
            out[r["uri"]] = r
    return out


class Answers:
    def __init__(self, rows, has_sub):
        self.rows, self.has_sub = rows, has_sub

    def __contains__(self, u):
        return u in self.rows

    def broad(self, u):
        d = self.rows[u]["broad_probs"]
        return max(d, key=d.get)

    def top_prob(self, u):
        return max(self.rows[u]["broad_probs"].values())

    def path(self, u):
        r = self.rows[u]
        cand = dict(r["path_scores"])
        for b, sub in self.has_sub.items():
            if not sub:
                cand[b] = math.sqrt(r["broad_probs"].get(b, 0.0))
        return max(cand, key=cand.get) if cand else None


def pct(a, n):
    return 100.0 * a / n if n else float("nan")


def agreement(j, c, uris):
    n = len(uris)
    return (sum(j.broad(u) == c.broad(u) for u in uris), sum(j.path(u) == c.path(u) for u in uris), n)


def mcnemar(j1, c1, j2, c2, uris):
    """Posts that agree only under v1 (b), only under v2 (c); one-sided p that v2 agrees more."""
    b = sum(j1.broad(u) == c1.broad(u) and j2.broad(u) != c2.broad(u) for u in uris)
    c = sum(j1.broad(u) != c1.broad(u) and j2.broad(u) == c2.broad(u) for u in uris)
    p = binomtest(c, b + c, 0.5, alternative="greater").pvalue if b + c else 1.0
    return b, c, p


def share(a, uris, topic):
    return pct(sum(a.broad(u) == topic for u in uris), len(uris))


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--sets", default="/data/clef/v2/test-sets.json")
    ap.add_argument("--clef-v1", default="/data/clef/clef-flash-remote.jsonl")
    ap.add_argument("--clef-v2", required=True)
    ap.add_argument("--verdicts", default="/data/clef/verdicts.jsonl")
    ap.add_argument("--posts", default="/data/clef/v2/test-posts.jsonl")
    ap.add_argument("--label-config", default="c8c15812c7ab", help="Jev's label_config for v2 (labeler prints it)")
    ap.add_argument("--out", required=True)
    ap.add_argument("--json", help="write the numbers as JSON here")
    a = ap.parse_args()

    sets = json.load(open(a.sets))
    text, pics, rand_text = sets["text"], sets["pictures"], sets["random_text"]
    judged_text, judged_pics = sets["judged_text"], sets["judged_pictures"]
    all_uris = text + pics
    hs1 = {b["id"]: bool(b.get("subtopics")) for b in yaml.safe_load(open(TAX / "v1.yaml"))["broad"]}
    hs2 = {b["id"]: bool(b.get("subtopics")) for b in yaml.safe_load(open(TAX / "v2.yaml"))["broad"]}

    j1 = Answers(cc.read_jev(all_uris), hs1)
    c1_rows = {}
    for pr in (0, 2):
        c1_rows.update(cc.read_clef(a.clef_v1, priority=pr))
    c1 = Answers({u: c1_rows[u] for u in all_uris if u in c1_rows}, hs1)
    j2 = Answers(read_jev_v2(all_uris, a.label_config), hs2)
    c2_rows = {}
    for pr in (0, 2):
        c2_rows.update(cc.read_clef(a.clef_v2, priority=pr))
    c2 = Answers({u: c2_rows[u] for u in all_uris if u in c2_rows}, hs2)

    have = [u for u in all_uris if u in j1 and u in c1 and u in j2 and u in c2]
    missing = {"jev_v1": sum(u not in j1 for u in all_uris), "clef_v1": sum(u not in c1 for u in all_uris),
               "jev_v2": sum(u not in j2 for u in all_uris), "clef_v2": sum(u not in c2 for u in all_uris)}
    hv = set(have)
    rand_text = [u for u in rand_text if u in hv]
    text = [u for u in text if u in hv]
    pics = [u for u in pics if u in hv]
    posts = {}
    for line in open(a.posts):
        r = json.loads(line)
        posts[r["uri"]] = r

    lines = []
    out = lambda s="": lines.append(s)  # noqa: E731
    res = {"missing": missing, "n": {"random_text": len(rand_text), "text": len(text), "pictures": len(pics)}}
    out("# Taxonomy v2 test: v1 against v2, Jev against Clef-flash")
    out()
    out(f"Posts with all four answers: {len(have)} of {len(all_uris)}; missing {missing}.")
    out()

    # --- agreement tables ---------------------------------------------------------------------
    out("## Do Jev and Clef agree more?")
    out()
    out("| Posts | n | broad v1 | broad v2 | path v1 | path v2 | disagree v1 -> v2 | only agree v1 / only v2 | one-sided p |")
    out("|---|---|---|---|---|---|---|---|---|")
    groups = [("random text", rand_text), ("judged text", [u for u in judged_text if u in hv]), ("all text", text),
              ("pictures", pics), ("judged pictures", [u for u in judged_pics if u in hv])]
    for name, us in groups:
        b1, p1, n = agreement(j1, c1, us)
        b2, p2, _ = agreement(j2, c2, us)
        b, c, p = mcnemar(j1, c1, j2, c2, us)
        out(f"| {name} | {n} | {pct(b1, n):.1f}% | {pct(b2, n):.1f}% | {pct(p1, n):.1f}% | {pct(p2, n):.1f}% | "
            f"{n - b1} -> {n - b2} | {b} / {c} | {p:.3f} |")
        res.setdefault("agreement", {})[name] = {"n": n, "broad_v1": pct(b1, n), "broad_v2": pct(b2, n),
                                                 "path_v1": pct(p1, n), "path_v2": pct(p2, n), "b": b, "c": c, "p": p}
    out()

    # --- unclear and top-prob -----------------------------------------------------------------
    out("## Share of each model's answers that are 'unclear', and its average top-topic confidence")
    out()
    out("| Posts | Jev v1 | Jev v2 | Clef v1 | Clef v2 | conf Jev v1/v2 | conf Clef v1/v2 |")
    out("|---|---|---|---|---|---|---|")
    for name, us in (("random text", rand_text), ("pictures", pics)):
        sh = [share(x, us, "unclear") for x in (j1, j2, c1, c2)]
        cf = [sum(x.top_prob(u) for u in us) / len(us) for x in (j1, j2, c1, c2)]
        out(f"| {name} | {sh[0]:.1f}% | {sh[1]:.1f}% | {sh[2]:.1f}% | {sh[3]:.1f}% | {cf[0]:.2f} / {cf[1]:.2f} | {cf[2]:.2f} / {cf[3]:.2f} |")
        res.setdefault("unclear", {})[name] = sh
    out()

    # --- topic shares -------------------------------------------------------------------------
    out("## Topic shares (random text posts, % of posts; * = edited in v2)")
    out()
    out("| Topic | Jev v1 | Jev v2 | Clef v1 | Clef v2 | Jev count v1 -> v2 |")
    out("|---|---|---|---|---|---|")
    topics = sorted(hs2, key=lambda t: -sum(j1.broad(u) == t for u in rand_text))
    collapse = []
    for t in topics:
        n1 = sum(j1.broad(u) == t for u in rand_text)
        n2 = sum(j2.broad(u) == t for u in rand_text)
        s = [share(x, rand_text, t) for x in (j1, j2, c1, c2)]
        flag = ""
        if n1 >= 20 and not (0.5 * n1 <= n2 <= 2 * n1):
            flag = " **OUT OF RANGE**"
            collapse.append(t)
        out(f"| {t}{'*' if t in EDITED else ''} | {s[0]:.1f}% | {s[1]:.1f}% | {s[2]:.1f}% | {s[3]:.1f}% | {n1} -> {n2}{flag} |")
    out()
    gone = [t for t in hs2 if any(j1.broad(u) == t for u in all_uris) and not any(j2.broad(u) == t for u in all_uris)]
    big = [(t, share(j2, rand_text, t)) for t in hs2 if share(j2, rand_text, t) > 25]
    res["collapse"] = {"out_of_range": collapse, "disappeared": gone, "over_25_percent": big}

    # --- pairs --------------------------------------------------------------------------------
    out("## The ten biggest v1 disagreements, before and after (random text posts)")
    out()
    out("| Pair | Jev and Clef split v1 | split v2 | Jev changed answer v1 -> v2 | Clef changed |")
    out("|---|---|---|---|---|")
    for x, y in PAIRS:
        def split(J, C):
            return sum({J.broad(u), C.broad(u)} == {x, y} for u in rand_text)
        jch = sum(j1.broad(u) != j2.broad(u) and {j1.broad(u), j2.broad(u)} == {x, y} for u in rand_text)
        cch = sum(c1.broad(u) != c2.broad(u) and {c1.broad(u), c2.broad(u)} == {x, y} for u in rand_text)
        out(f"| {x} / {y} | {split(j1, c1)} | {split(j2, c2)} | {jch} | {cch} |")
    out()
    jchanged = sum(j1.broad(u) != j2.broad(u) for u in rand_text)
    cchanged = sum(c1.broad(u) != c2.broad(u) for u in rand_text)
    out(f"Broad answer changed v1 -> v2: Jev {jchanged} of {len(rand_text)} ({pct(jchanged, len(rand_text)):.1f}%), "
        f"Clef {cchanged} ({pct(cchanged, len(rand_text)):.1f}%).")
    moves = collections.Counter((j1.broad(u), j2.broad(u)) for u in rand_text if j1.broad(u) != j2.broad(u))
    out()
    out("Jev's biggest moves v1 -> v2: " + ", ".join(f"{a_}->{b_} {n}" for (a_, b_), n in moves.most_common(10)))
    out()
    moves = collections.Counter((c1.broad(u), c2.broad(u)) for u in rand_text if c1.broad(u) != c2.broad(u))
    out("Clef's biggest moves v1 -> v2: " + ", ".join(f"{a_}->{b_} {n}" for (a_, b_), n in moves.most_common(10)))
    out()

    # --- judged posts -------------------------------------------------------------------------
    verd = {}
    for line in open(a.verdicts):
        v = json.loads(line)
        verd[v["uri"]] = v["verdict"]
    out("## The posts judged by hand")
    out()
    out("An answer counts as accepted when it is the v1 answer of a model the owner marked as closer "
        "('jev', 'clef', or 'both' marks both). 'neither' and skipped posts are left out of the scores.")
    out()
    out("| Set | posts scored | Jev v1 | Clef v1 | Jev v2 | Clef v2 |")
    out("|---|---|---|---|---|---|")
    for name, us in (("text", [u for u in judged_text if u in hv]), ("pictures", [u for u in judged_pics if u in hv])):
        acc = {}
        for u in us:
            v = verd.get(u)
            ok = set()
            if v in ("jev", "both"):
                ok.add(j1.broad(u))
            if v in ("clef", "both"):
                ok.add(c1.broad(u))
            if ok:
                acc[u] = ok
        scores = [pct(sum(x.broad(u) in acc[u] for u in acc), len(acc)) for x in (j1, c1, j2, c2)]
        out(f"| {name} | {len(acc)} | {scores[0]:.0f}% | {scores[1]:.0f}% | {scores[2]:.0f}% | {scores[3]:.0f}% |")
        res.setdefault("judged_scores", {})[name] = scores
    out()
    out("### Every judged post whose answer from either model changed")
    out()
    out("Format: verdict; text; Jev v1 > v2; Clef v1 > v2")
    out()
    n_changed = 0
    for u in judged_text + judged_pics:
        if u not in hv:
            continue
        if j1.broad(u) != j2.broad(u) or c1.broad(u) != c2.broad(u):
            n_changed += 1
            p = posts[u]
            t = " ".join((p.get("text") or "").split())[:160]
            pic = " [picture post]" if p.get("priority") == 0 else ""
            out(f"- **{verd.get(u, '?')}**{pic} {t!r}  \n  Jev {j1.broad(u)} > {j2.broad(u)}; Clef {c1.broad(u)} > {c2.broad(u)}")
    out()
    out(f"{n_changed} judged posts had an answer change.")
    out()

    # --- verdicts on the checks ---------------------------------------------------------------
    A = res["agreement"]["random text"]
    ok_a = (A["n"] - A["broad_v2"] * A["n"] / 100) < (A["n"] - A["broad_v1"] * A["n"] / 100) and A["p"] < 0.05
    Bp = res["agreement"]["pictures"]
    ok_b = Bp["broad_v2"] >= Bp["broad_v1"] - 3
    u1, u2 = res["unclear"]["random text"][0], res["unclear"]["random text"][1]
    uc1, uc2 = res["unclear"]["random text"][2], res["unclear"]["random text"][3]
    ok_c = (u2 - u1 <= 1.5) and (uc2 - uc1 <= 1.5)
    ok_d = not collapse and not gone and not big
    out("## Checks")
    out()
    out(f"- A text agreement: broad {A['broad_v1']:.1f}% -> {A['broad_v2']:.1f}%, p = {A['p']:.3f}: **{'PASS' if ok_a else 'FAIL'}**")
    out(f"- B picture posts: {Bp['broad_v1']:.1f}% -> {Bp['broad_v2']:.1f}%: **{'PASS' if ok_b else 'FAIL'}**")
    out(f"- C unclear: Jev {u1:.1f}% -> {u2:.1f}%, Clef {uc1:.1f}% -> {uc2:.1f}%: **{'PASS' if ok_c else 'FAIL'}**")
    out(f"- D no collapse: out of range {collapse or 'none'}, disappeared {gone or 'none'}, over 25% {big or 'none'}: "
        f"**{'PASS' if ok_d else 'FAIL'}**")
    out("- E judged posts: read the list above.")
    res["checks"] = {"A": ok_a, "B": ok_b, "C": ok_c, "D": ok_d}
    Path(a.out).write_text("\n".join(lines) + "\n")
    if a.json:
        json.dump(res, open(a.json, "w"), indent=1)
    print("\n".join(lines[-8:]))
    print(f"report -> {a.out}")


if __name__ == "__main__":
    main()
