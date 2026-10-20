"""Re-rank live builder candidates under different freshness formulas (scratch analysis).

Fetches every candidate of a few feeds from the local builder (/api/preview), then ranks them
offline and prints the age and likes of the top 20 for each formula.
"""

import json
import math
import urllib.request

URL = "http://localhost:8710/api/preview"
W = {"like": 1, "repost": 2, "reply": 2, "quote": 3}


def fetch(paths):
    out, offset = [], 0
    while True:
        body = {"paths": paths, "min_prob": 0.5, "tone": {}, "signals": {}, "limit": 100, "offset": offset,
                "ranking": {"weights": W, "gravity": 1.8, "fresh_every": 0, "author_gap": 0, "promo_penalty": 1}}
        req = urllib.request.Request(URL, json.dumps(body).encode(), {"content-type": "application/json"})
        d = json.load(urllib.request.urlopen(req, timeout=60))
        out += d["posts"]
        offset += 100
        if not d["posts"] or offset >= d["total"]:
            return out, d["total"]


def parts(p):
    s = p.get("signals") or {}
    prior = max(0.1, 1 + s.get("substance", 0) + s.get("general_interest", 0) - s.get("promo", 0))
    eng = p["likes"] * W["like"] + p["reposts"] * W["repost"] + p["replies"] * W["reply"] + p["quotes"] * W["quote"]
    return prior, eng, p["age_minutes"] / 60


def current(g):
    return lambda p: (lambda pr, e, a: (pr + e) / (a + 2) ** g)(*parts(p))


def damped(g, x):
    # engagement raised to x (1 = today, 0.5 = square root)
    return lambda p: (lambda pr, e, a: (pr + e ** x) / (a + 2) ** g)(*parts(p))


def halflife(h, x=1.0):
    return lambda p: (lambda pr, e, a: (pr + e ** x) * 0.5 ** (a / h))(*parts(p))


def show(name, posts, fn, max_age_h=None):
    ps = [p for p in posts if max_age_h is None or p["age_minutes"] <= max_age_h * 60]
    top = sorted(ps, key=fn, reverse=True)[:20]
    ages = sorted(p["age_minutes"] for p in top)
    print(f"  {name:28s} median age {ages[len(ages)//2]:4d}m  oldest {ages[-1]:4d}m  | "
          + " ".join(f"{p['age_minutes']}m/{p['likes']}" for p in top[:10]))


import sys

for paths in ([a.split(",") for a in sys.argv[1:]] or (["sports"], ["us_politics"], ["*"])):
    posts, total = fetch(paths)
    span = max(p["age_minutes"] for p in posts) if posts else 0
    print(f"== {paths}: {len(posts)} candidates reaching back {span}m; top 10 shown as age/likes")
    for g in (1.8, 3, 4):
        show(f"today, gravity {g}", posts, current(g))
    show("today, gravity 3, max 2h", posts, current(3), 2)
    for x in (0.5, 0.3):
        show(f"gravity 3, engagement^{x}", posts, damped(3, x))
    for h in (2, 1, 0.5):
        show(f"half-life {h}h", posts, halflife(h))
        show(f"half-life {h}h, engagement^0.5", posts, halflife(h, 0.5))
