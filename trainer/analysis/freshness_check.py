"""Ask a feedgen's preview API for one topic under a few freshness settings (scratch check)."""

import json
import sys
import urllib.request

URL = sys.argv[1] if len(sys.argv) > 1 else "http://localhost:8799/api/preview"
PATHS = sys.argv[2:] or ["us_politics/immigration"]
W = {"like": 1, "repost": 2, "reply": 2, "quote": 3}


def ask(extra_ranking=None, max_age=None):
    ranking = {"weights": W, "gravity": 3, "fresh_every": 0, "author_gap": 10, "promo_penalty": 1, **(extra_ranking or {})}
    body = {"paths": PATHS, "min_prob": 0.5, "tone": {}, "signals": {}, "limit": 20, "ranking": ranking}
    if max_age:
        body["max_age_minutes"] = max_age
    req = urllib.request.Request(URL, json.dumps(body).encode(), {"content-type": "application/json"})
    try:
        d = json.load(urllib.request.urlopen(req, timeout=60))
    except urllib.error.HTTPError as e:
        return f"HTTP {e.code}: {e.read().decode()[:200]}"
    ages = sorted(p["age_minutes"] for p in d["posts"])
    top = " ".join(f"{p['age_minutes']}m/{p['likes']}" for p in d["posts"][:10])
    return f"{d['total']:5d} candidates, top 20 median age {ages[len(ages)//2]}m, oldest {ages[-1]}m | {top}"


print("freshness 3 (today's max):        ", ask())
print("+ popularity half (0.5):          ", ask({"engagement_power": 0.5}))
print("+ max age 2 h:                    ", ask(max_age=120))
print("+ both:                           ", ask({"engagement_power": 0.5}, 120))
print("max age 10 min (should be refused):", ask(max_age=10))
