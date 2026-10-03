"""Call a deployed Clef Flumina app (see README.md). Standard library only.

    FIREWORKS_API_KEY=... python call.py --account ACCOUNT --app clef-27b \\
        --deployment accounts/ACCOUNT/deployments/ID info
    ... call.py ... ask --state "Checkout is down for everyone." [--image photo.jpg]

`ask` sends one request with a team question and an urgency question, as a smoke test.
"""

import argparse
import base64
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request


def call(account: str, app: str, deployment: str, route: str, body: dict, key: str, timeout: int = 600) -> dict:
    url = (f"https://api.fireworks.ai/inference/v1/workflows/accounts/{account}/models/{app}/{route}"
           f"?{urllib.parse.urlencode({'deployment': deployment})}")
    req = urllib.request.Request(url, data=json.dumps(body).encode(), method="POST", headers={
        "Authorization": f"Bearer {key}", "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return json.load(r)
    except urllib.error.HTTPError as e:
        sys.exit(f"HTTP {e.code}: {e.read().decode()[:500]}")


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--account", required=True)
    ap.add_argument("--app", required=True, help="the name given to `flumina deploy`")
    ap.add_argument("--deployment", required=True, help="accounts/ACCOUNT/deployments/ID, printed by `flumina deploy`")
    ap.add_argument("command", choices=["info", "ask"])
    ap.add_argument("--state", default="Checkout has been failing for every customer for the last hour.")
    ap.add_argument("--image", action="append", default=[], help="a picture to attach (up to 4)")
    a = ap.parse_args()
    key = os.environ.get("FIREWORKS_API_KEY")
    if not key:
        sys.exit("set FIREWORKS_API_KEY")

    if a.command == "info":
        print(json.dumps(call(a.account, a.app, a.deployment, "info", {}, key), indent=1))
        return
    body = {
        "model": "clef", "state": a.state,
        "questions": {
            "team": {"type": "choice", "instructions": "Which team should handle this request?",
                     "criteria": {"billing": "Payments, invoices, and refunds",
                                  "technical": "Outages, errors, and configuration", "sales": "Plans and upgrades"}},
            "urgent": {"type": "noul", "instructions": "Is this support request urgent?"},
        },
    }
    if a.image:
        body["images"] = [base64.b64encode(open(p, "rb").read()).decode() for p in a.image]
    t = time.time()
    out = call(a.account, a.app, a.deployment, "systemone", body, key)
    print(json.dumps(out, indent=1))
    print(f"{time.time() - t:.2f}s round trip", file=sys.stderr)


if __name__ == "__main__":
    main()
