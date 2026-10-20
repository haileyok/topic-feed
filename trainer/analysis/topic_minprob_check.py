"""Check per-topic match strictness through a feedgen's preview API (scratch check).

A feed of cats and US politics at 50%: then with cats loosened to 20% and politics tightened to
80%, and the whole feed at 10%. Counts candidates per topic, with the least confident match.
"""

import json
import sys
import urllib.request
from collections import defaultdict

URL = sys.argv[1] if len(sys.argv) > 1 else "http://localhost:8799/api/preview"
CATS, POLITICS = "animals_nature/cats", "us_politics"


def ask(min_prob, topic_rules=None):
    body = {"paths": [CATS, POLITICS], "min_prob": min_prob, "tone": {}, "signals": {}, "limit": 100}
    if topic_rules:
        body["topic_rules"] = topic_rules
    posts, offset, total = [], 0, 0
    while True:
        req = urllib.request.Request(URL, json.dumps({**body, "offset": offset}).encode(), {"content-type": "application/json"})
        try:
            d = json.load(urllib.request.urlopen(req, timeout=60))
        except urllib.error.HTTPError as e:
            return f"HTTP {e.code}: {e.read().decode()[:200]}"
        posts += d["posts"]
        total = d["total"]
        offset += 100
        if not d["posts"] or offset >= total:
            break
    by = defaultdict(list)
    for p in posts:
        cat = "cats" if p["topic"].startswith("animals_nature") else "politics" if p["topic"].startswith("us_politics") else "other"
        by[cat].append(p["match"])
    return f"{total:5d} posts | " + " | ".join(f"{k}: {len(v)} (least match {min(v):.2f})" for k, v in sorted(by.items()))


print("both at 50%:                  ", ask(0.5))
print("cats 20%, politics 80%:       ", ask(0.5, {CATS: {"min_prob": 0.2}, POLITICS: {"min_prob": 0.8}}))
print("whole feed at 10%:            ", ask(0.1))
print("a topic at 1.5 (refused):     ", ask(0.5, {CATS: {"min_prob": 1.5}}))
