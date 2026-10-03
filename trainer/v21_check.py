"""Jev under taxonomy v2.1 against v1 and v2 on the 2,000 text-only test posts.

The checks that decide whether the full v2.1 runs go on (agreed 2026-10-02; text agreement
between Jev and Clef is not one of them, Jev is the text teacher):

  C  unclear   'unclear' is at most 1.5 points more of Jev's answers than under v1 (random posts)
  D  collapse  every topic with at least 20 of the random posts under v1 keeps between half and
               double that many, none gets more than 25%, none disappears
  E  judged    Jev's accepted-answer score on the judged text posts is at most 5 points below v1,
               and the sports posts that left sports under v2 are listed with their v2.1 answer

    trainer/.venv/bin/python v21_check.py --label-config <Jev's v2.1 label_config>
"""

import argparse
import json
import sys
from pathlib import Path

import yaml

sys.path.insert(0, str(Path(__file__).resolve().parent))
import clef_calibrate as cc  # noqa: E402
import v2_gates as g  # noqa: E402

TAX = Path(__file__).resolve().parent.parent / "taxonomy"


def read_jev_version(uris, version, label_config):
    out = {}
    uris = list(uris)
    for i in range(0, len(uris), 800):
        lst = ",".join("'%s'" % u for u in uris[i:i + 800])
        q = ("SELECT uri, broad_probs, sub_probs, path_scores, signals FROM jev_labels FINAL "
             f"WHERE taxonomy_version = '{version}' AND label_config = '{label_config}' AND uri IN ({lst}) FORMAT JSONEachRow")
        import subprocess
        res = subprocess.run(["docker", "exec", "-i", "topic-feed-clickhouse", "sh", "-c",
                              'clickhouse-client --user topicfeed --password "$CLICKHOUSE_PASSWORD" --database topicfeed'],
                             input=q, capture_output=True, text=True, check=True).stdout
        for line in res.splitlines():
            r = json.loads(line)
            out[r["uri"]] = r
    return out


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--label-config", required=True, help="Jev's label_config under v2.1")
    ap.add_argument("--v2-label-config", default="c8c15812c7ab")
    ap.add_argument("--sets", default="/data/clef/v2/test-sets.json")
    ap.add_argument("--verdicts", default="/data/clef/verdicts.jsonl")
    ap.add_argument("--posts", default="/data/clef/v2/test-posts.jsonl")
    a = ap.parse_args()

    sets = json.load(open(a.sets))
    rand, text, judged = sets["random_text"], sets["text"], sets["judged_text"]
    hs1 = {b["id"]: bool(b.get("subtopics")) for b in yaml.safe_load(open(TAX / "v1.yaml"))["broad"]}
    hs21 = {b["id"]: bool(b.get("subtopics")) for b in yaml.safe_load(open(TAX / "v2.1.yaml"))["broad"]}
    j1 = g.Answers(cc.read_jev(text), hs1)
    j2 = g.Answers(read_jev_version(text, "v2", a.v2_label_config), hs1)
    j21 = g.Answers(read_jev_version(text, "v2.1", a.label_config), hs21)
    have = [u for u in text if u in j1 and u in j2 and u in j21]
    print(f"posts with v1, v2 and v2.1 answers: {len(have)} of {len(text)}")
    hv = set(have)
    rand = [u for u in rand if u in hv]
    judged = [u for u in judged if u in hv]
    posts = {json.loads(l)["uri"]: json.loads(l) for l in open(a.posts)}

    print(f"\n'unclear' share of Jev's answers on {len(rand)} random text posts: "
          f"v1 {g.share(j1, rand, 'unclear'):.1f}%   v2 {g.share(j2, rand, 'unclear'):.1f}%   v2.1 {g.share(j21, rand, 'unclear'):.1f}%")
    ok_c = g.share(j21, rand, "unclear") - g.share(j1, rand, "unclear") <= 1.5

    print("\ntopic counts on the random posts (v1 / v2 / v2.1):")
    bad = []
    big = []
    for t in sorted(hs21, key=lambda t: -sum(j1.broad(u) == t for u in rand)):
        n = [sum(x.broad(u) == t for u in rand) for x in (j1, j2, j21)]
        flag = ""
        if n[0] >= 20 and not (0.5 * n[0] <= n[2] <= 2 * n[0]):
            flag = "  <-- OUT OF RANGE"
            bad.append(t)
        if g.share(j21, rand, t) > 25:
            big.append(t)
        print(f"  {t:<18}{n[0]:>5}{n[1]:>5}{n[2]:>5}{flag}")
    gone = [t for t in hs21 if any(j1.broad(u) == t for u in have) and not any(j21.broad(u) == t for u in have)]
    ok_d = not bad and not gone and not big

    verd = {}
    for line in open(a.verdicts):
        v = json.loads(line)
        verd[v["uri"]] = v["verdict"]
    acc = {}
    cj = cc.read_clef("/data/clef/clef-flash-remote.jsonl", priority=2)
    c1 = g.Answers({u: cj[u] for u in judged if u in cj}, hs1)
    for u in judged:
        v, ok = verd.get(u), set()
        if v in ("jev", "both"):
            ok.add(j1.broad(u))
        if v in ("clef", "both") and u in c1:
            ok.add(c1.broad(u))
        if ok:
            acc[u] = ok
    sc = [g.pct(sum(x.broad(u) in acc[u] for u in acc), len(acc)) for x in (j1, j2, j21)]
    print(f"\njudged text posts scored ({len(acc)}): Jev accepted-answer score v1 {sc[0]:.0f}%  v2 {sc[1]:.0f}%  v2.1 {sc[2]:.0f}%")
    ok_e = sc[2] >= sc[0] - 5

    sports_out = [u for u in text if j1.broad(u) == "sports" and j2.broad(u) != "sports"]
    back = [u for u in sports_out if j21.broad(u) == "sports"]
    print(f"\nsports posts that left sports under v2: {len(sports_out)}; back in sports under v2.1: {len(back)}")
    for u in sports_out:
        t = " ".join((posts[u].get("text") or "").split())[:70]
        print(f"  v2 {j2.broad(u):<14} v2.1 {j21.broad(u):<14} {t!r}")
    moved_in = [u for u in text if j1.broad(u) != "sports" and j21.broad(u) == "sports"]
    print(f"posts that joined sports under v2.1: {len(moved_in)}; sports posts overall v1 {sum(j1.broad(u) == 'sports' for u in text)}"
          f" v2 {sum(j2.broad(u) == 'sports' for u in text)} v2.1 {sum(j21.broad(u) == 'sports' for u in text)}")

    print("\nCHECKS")
    print(f"  C unclear <= v1 + 1.5 points: {'PASS' if ok_c else 'FAIL'}")
    print(f"  D no collapse: {'PASS' if ok_d else 'FAIL'} (out of range {bad or 'none'}, gone {gone or 'none'}, over 25% {big or 'none'})")
    print(f"  E judged score not more than 5 points below v1: {'PASS' if ok_e else 'FAIL'}")
    print("ALL PASS" if ok_c and ok_d and ok_e else "NOT ALL PASS")


if __name__ == "__main__":
    main()
