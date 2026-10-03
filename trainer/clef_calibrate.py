"""Make Clef-flash's probabilities as sharp as Jev's, without changing which answer it picks.

Clef spreads its probability over more options than Jev does (its average confidence in its top
topic is about 0.5, Jev's about 0.8). Mixing the two as teachers would let a student learn "has a
picture, so use Clef's softer style". This fits a temperature T for each kind of answer so Clef's
distributions have Jev's sharpness on posts both labelled:

    p'(x) is proportional to p(x) ** (1 / T)      (T < 1 sharpens; the top option never changes)

  broad topic   one T, fitted by minimising cross-entropy against Jev's distribution
  subtopic      one T, the same, within each broad topic both models answered
  signals       s' = sigmoid(a * logit(s) + b), fitted by least squares against Jev's value

It fits on 80% of the posts, reports on the other 20% (split by a hash of the post), and writes the
fitted numbers to --out. Calibrating to Jev is a choice: it makes the two teachers comparable, it
does not make either one right. The numbers belong to one taxonomy and question wording; refit after
either changes.

    python clef_calibrate.py fit   --results clef-flash-remote.jsonl --out calibration-v1.json
    python clef_calibrate.py apply --calibration calibration-v1.json --results IN.jsonl --out OUT.jsonl

Under another taxonomy, name it and Jev's labels for it:

    python clef_calibrate.py fit   --taxonomy taxonomy/v2.1.yaml --jev-version v2.1 --jev-label-config 30cf2c546019 \
        --results clef-v21-calib.jsonl --pictures clef-v21-pictures.jsonl --out calibration-v2.1.json
    python clef_calibrate.py apply --taxonomy taxonomy/v2.1.yaml --calibration calibration-v2.1.json --results IN --out OUT
"""

import argparse
import collections
import json
import math
import subprocess
import sys
import zlib
from pathlib import Path

import numpy as np
import yaml
from scipy.optimize import least_squares, minimize_scalar

TAXONOMY = Path(__file__).resolve().parent.parent / "taxonomy" / "v1.yaml"
RANK = {"window": 0, "sample": 1, "uncertain": 2}
EPS = 1e-9


def read_clef(path, priority=None):
    out = {}
    for line in open(path):
        try:
            r = json.loads(line)
        except ValueError:
            continue
        if priority is None or r.get("priority") == priority:
            out[r["uri"]] = r
    return out


def read_jev(uris, version="v1", label_config=None):
    """Jev's answers under one taxonomy version for the given posts. Under v1 that is the main run,
    then sample, then uncertain; give label_config to take one label_config only."""
    jev = {}
    uris = list(uris)
    only = f"AND label_config = '{label_config}' " if label_config else ""
    for i in range(0, len(uris), 800):
        lst = ",".join("'%s'" % u for u in uris[i:i + 800])
        q = ("SELECT uri, source, broad_probs, sub_probs, path_scores, signals FROM jev_labels FINAL "
             f"WHERE taxonomy_version = '{version}' {only}"
             f"AND jev_model = 'jev-1.13.0' AND uri IN ({lst}) FORMAT JSONEachRow")
        out = subprocess.run(["docker", "exec", "-i", "topic-feed-clickhouse", "sh", "-c",
                              'clickhouse-client --user topicfeed --password "$CLICKHOUSE_PASSWORD" --database topicfeed'],
                             input=q, capture_output=True, text=True, check=True).stdout
        for line in out.splitlines():
            r = json.loads(line)
            if r["uri"] not in jev or RANK.get(r["source"], 9) < RANK.get(jev[r["uri"]]["source"], 9):
                jev[r["uri"]] = r
    return jev


def sharpen(p, T):
    """p ** (1/T), renormalised. p is a 1-D array."""
    q = np.power(np.clip(p, EPS, 1.0), 1.0 / T)
    return q / q.sum()


def sharpen_dict(d, T):
    keys = list(d)
    return dict(zip(keys, sharpen(np.array([d[k] for k in keys], dtype=float), T).tolist()))


def logit(x):
    x = np.clip(x, 1e-4, 1 - 1e-4)
    return np.log(x / (1 - x))


def sigmoid(z):
    return 1 / (1 + np.exp(-z))


def by_broad(subs):
    """{'sports/soccer': 0.7, ...} -> {'sports': {'soccer': 0.7, ...}}"""
    out = collections.defaultdict(dict)
    for k, v in subs.items():
        b, s = k.split("/", 1)
        out[b][s] = v
    return out


def cross_entropy(pairs, T):
    return float(np.mean([-(j * np.log(np.clip(sharpen(c, T), EPS, 1))).sum() for j, c in pairs]))


def fit_temperature(pairs):
    """pairs: list of (jev_vector, clef_vector). Returns T minimising mean cross-entropy."""
    return float(minimize_scalar(lambda T: cross_entropy(pairs, T), bounds=(0.1, 3.0), method="bounded").x)


def make_pairs(clef, jev, uris):
    broad, sub = [], []
    for u in uris:
        c, j = clef[u], jev[u]
        keys = sorted(set(c["broad_probs"]) | set(j["broad_probs"]))
        jv = np.array([j["broad_probs"].get(k, 0.0) for k in keys])
        cv = np.array([c["broad_probs"].get(k, 0.0) for k in keys])
        broad.append((jv / jv.sum(), cv / cv.sum()))
        cs, js = by_broad(c["sub_probs"]), by_broad(j["sub_probs"])
        for b in set(cs) & set(js):
            ks = sorted(set(cs[b]) | set(js[b]))
            jv = np.array([js[b].get(k, 0.0) for k in ks])
            cv = np.array([cs[b].get(k, 0.0) for k in ks])
            if jv.sum() > 0 and cv.sum() > 0:
                sub.append((jv / jv.sum(), cv / cv.sum()))
    return broad, sub


def fit_signal(pairs):
    x = logit(np.array([c for c, _ in pairs]))
    y = np.array([j for _, j in pairs])
    res = least_squares(lambda ab: sigmoid(ab[0] * x + ab[1]) - y, x0=[1.0, 0.0])
    return [float(res.x[0]), float(res.x[1])]


def apply_calibration(rec, cal, has_sub):
    """A copy of one Clef result with broad, subtopic and signal numbers calibrated."""
    out = dict(rec)
    broad = sharpen_dict(rec["broad_probs"], cal["broad_T"])
    subs = {}
    for b, d in by_broad(rec["sub_probs"]).items():
        for s, p in sharpen_dict(d, cal["sub_T"]).items():
            subs[f"{b}/{s}"] = p
    paths = {k: math.sqrt(broad.get(k.split("/")[0], 0.0) * p) for k, p in subs.items()}
    cand = dict(paths)
    for b, sub in has_sub.items():
        if not sub:
            cand[b] = math.sqrt(broad.get(b, 0.0))
    sig = dict(rec["signals"])
    for name, (a, b) in cal["signals"].items():
        if name in sig:
            sig[name] = float(sigmoid(a * logit(np.array(sig[name])) + b))
    out.update(broad_probs=broad, sub_probs=subs, path_scores=paths, signals=sig,
               top_path=max(cand.items(), key=lambda kv: kv[1])[0] if cand else None, calibrated=True)
    return out


def top_prob(d):
    return max(d.values())


def argmax(d):
    return max(d, key=d.get)


def fit(a):
    tax = yaml.safe_load(open(a.taxonomy))
    clef = read_clef(a.results, priority=2)  # text-only posts: Jev answered these from the same input
    jev = read_jev(clef, a.jev_version, a.jev_label_config)
    uris = sorted(u for u in clef if u in jev)
    train = [u for u in uris if zlib.crc32(u.encode()) % 5 != 0]
    test = [u for u in uris if zlib.crc32(u.encode()) % 5 == 0]
    print(f"{len(uris)} text-only posts answered by both: fit on {len(train)}, check on {len(test)}")

    bt, st = make_pairs(clef, jev, train)
    cal = {"taxonomy": tax["version"], "fit_posts": len(train), "broad_T": fit_temperature(bt), "sub_T": fit_temperature(st), "signals": {}}
    # A signal is fitted on the training posts where both models gave it. v1's answers come from
    # two question sets (the older one lacks six signals), so look at every row, not the first.
    names = set().union(*(r["signals"] for r in jev.values())) & set().union(*(r["signals"] for r in clef.values()))
    for name in sorted(names):
        pairs = [(clef[u]["signals"][name], jev[u]["signals"][name]) for u in train
                 if name in clef[u]["signals"] and name in jev[u]["signals"]]
        if len(pairs) >= 200:
            cal["signals"][name] = fit_signal(pairs)
        else:
            print(f"signal {name}: only {len(pairs)} posts answered by both, not calibrated")
    json.dump(cal, open(a.out, "w"), indent=1)
    sigs = {k: [round(x, 2) for x in v] for k, v in cal["signals"].items()}
    print(f"broad T = {cal['broad_T']:.2f}   subtopic T = {cal['sub_T']:.2f}   signals {sigs}")

    has_sub = {b["id"]: bool(b.get("subtopics")) for b in tax["broad"]}
    cc = {u: apply_calibration(clef[u], cal, has_sub) for u in test}
    jb, _ = make_pairs(clef, jev, test)
    st2 = make_pairs(clef, jev, test)[1]
    print("\nON THE HELD-OUT 20%")
    print(f"  broad topic cross-entropy vs Jev:  raw {cross_entropy(jb, 1.0):.3f}  ->  calibrated {cross_entropy(jb, cal['broad_T']):.3f}")
    print(f"  subtopic cross-entropy vs Jev:     raw {cross_entropy(st2, 1.0):.3f}  ->  calibrated {cross_entropy(st2, cal['sub_T']):.3f}")
    tj = np.mean([top_prob(jev[u]["broad_probs"]) for u in test])
    tr = np.mean([top_prob(clef[u]["broad_probs"]) for u in test])
    tc = np.mean([top_prob(cc[u]["broad_probs"]) for u in test])
    print(f"  average confidence in top topic:   Jev {tj:.2f}   Clef raw {tr:.2f}   Clef calibrated {tc:.2f}")
    same = sum(argmax(clef[u]["broad_probs"]) == argmax(cc[u]["broad_probs"]) for u in test)
    print(f"  Clef's top topic unchanged by calibration: {same}/{len(test)}")
    for name in cal["signals"]:
        us = [u for u in test if name in jev[u]["signals"] and name in clef[u]["signals"]]
        pj = np.array([jev[u]["signals"][name] for u in us])
        pr = np.array([clef[u]["signals"][name] for u in us])
        pc = np.array([cc[u]["signals"][name] for u in us])
        print(f"  signal {name:<16} mean |diff| to Jev: raw {np.mean(np.abs(pj - pr)):.3f} -> calibrated {np.mean(np.abs(pj - pc)):.3f}"
              f"   (means: Jev {pj.mean():.2f}, raw {pr.mean():.2f}, calibrated {pc.mean():.2f})")

    print("\nDoes Clef's calibrated confidence match how often Jev agrees with it? (held-out posts)")
    print(f"  {'calibrated conf':<17}{'posts':>6}{'Jev agrees':>12}{'raw mean conf':>15}{'calibrated mean':>17}")
    for lo, hi in ((0, .3), (.3, .5), (.5, .7), (.7, .9), (.9, 1.01)):
        row = [u for u in test if lo <= top_prob(cc[u]["broad_probs"]) < hi]
        if row:
            ag = np.mean([argmax(cc[u]["broad_probs"]) == argmax(jev[u]["broad_probs"]) for u in row])
            print(f"  {lo:.1f}-{min(hi, 1):.1f}           {len(row):>6}{100 * ag:>11.0f}%"
                  f"{np.mean([top_prob(clef[u]['broad_probs']) for u in row]):>15.2f}{np.mean([top_prob(cc[u]['broad_probs']) for u in row]):>17.2f}")

    pics = read_clef(a.pictures or a.results, priority=0)
    if pics:
        pc2 = [apply_calibration(r, cal, has_sub) for r in pics.values()]
        print(f"\nPicture posts ({len(pics)}): average confidence in top topic raw "
              f"{np.mean([top_prob(r['broad_probs']) for r in pics.values()]):.2f} -> calibrated "
              f"{np.mean([top_prob(r['broad_probs']) for r in pc2]):.2f}   (text-only: raw {tr:.2f} -> {tc:.2f})")


def apply(a):
    cal = json.load(open(a.calibration))
    tax = yaml.safe_load(open(a.taxonomy))
    if cal["taxonomy"] != tax["version"]:
        sys.exit(f"{a.calibration} was fitted under taxonomy {cal['taxonomy']}, not {tax['version']}: refit it")
    has_sub = {b["id"]: bool(b.get("subtopics")) for b in tax["broad"]}
    n = 0
    with open(a.out, "w") as out:
        for line in open(a.results):
            try:
                rec = json.loads(line)
            except ValueError:
                continue
            out.write(json.dumps(apply_calibration(rec, cal, has_sub), ensure_ascii=False) + "\n")
            n += 1
    print(f"calibrated {n} results -> {a.out}")


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    sub = ap.add_subparsers(dest="cmd", required=True)
    default_tax = str(TAXONOMY)
    f = sub.add_parser("fit")
    f.add_argument("--results", required=True, help="Clef results holding the text-only calibration posts (priority 2)")
    f.add_argument("--out", required=True)
    f.add_argument("--taxonomy", default=default_tax, help="the taxonomy file Clef and Jev answered under")
    f.add_argument("--jev-version", default="v1", help="jev_labels.taxonomy_version to compare with")
    f.add_argument("--jev-label-config", help="only Jev's labels with this label_config")
    f.add_argument("--pictures", help="Clef results with picture posts (priority 0), for the confidence comparison")
    p = sub.add_parser("apply")
    p.add_argument("--calibration", required=True)
    p.add_argument("--results", required=True)
    p.add_argument("--out", required=True)
    p.add_argument("--taxonomy", default=default_tax, help="must be the taxonomy the calibration was fitted under")
    a = ap.parse_args()
    {"fit": fit, "apply": apply}[a.cmd](a)


if __name__ == "__main__":
    sys.exit(main())
