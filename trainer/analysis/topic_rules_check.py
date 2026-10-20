"""Check rules for particular topics through a feedgen's preview API (scratch check).

An all-topics feed that leaves out critical posts, then the same with critical let back in for
US politics, with elections held to a stricter cutoff. Reports, per broad topic of the posts'
most likely subtopic, how many candidates there are and the most critical one's score.
"""

import json
import sys
import urllib.request
from collections import defaultdict

URL = sys.argv[1] if len(sys.argv) > 1 else "http://localhost:8799/api/preview"


def ask(topic_rules=None):
    body = {"paths": ["*"], "min_prob": 0.5, "tone": {}, "signals": {"max": {"critical": 0.3}}, "limit": 100}
    if topic_rules:
        body["topic_rules"] = topic_rules
    posts, offset = [], 0
    while True:
        req = urllib.request.Request(URL, json.dumps({**body, "offset": offset}).encode(), {"content-type": "application/json"})
        d = json.load(urllib.request.urlopen(req, timeout=60))
        posts += d["posts"]
        offset += 100
        if not d["posts"] or offset >= d["total"] or offset >= 1500:
            return posts, d["total"]


def summary(posts):
    by = defaultdict(list)
    for p in posts:
        by[p["topic"]].append(p["signals"].get("critical", 0))
    return by


plain, n_plain = ask()
ruled, n_ruled = ask({"us_politics": {"signals": {"max": {"critical": 1}}},
                      "us_politics/elections": {"signals": {"max": {"critical": 0.1}}}})
print(f"without topic rules: {n_plain} candidates; with: {n_ruled}")
for name, posts in (("without", plain), ("with", ruled)):
    by = summary(posts)
    pol = [c for t, cs in by.items() if t.startswith("us_politics/") and t != "us_politics/elections" for c in cs]
    ele = by.get("us_politics/elections", [])
    other = [c for t, cs in by.items() if not t.startswith("us_politics/") for c in cs]
    f = lambda cs: f"{len(cs):4d} posts, most critical {max(cs):.2f}" if cs else "   0 posts"
    print(f"  {name:8s} politics (not elections): {f(pol)} | elections: {f(ele)} | other topics: {f(other)}")
