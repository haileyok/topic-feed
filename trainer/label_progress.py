"""A live page for the taxonomy v2.1 labelling runs: how far Jev and Clef-flash have got, and how
their labels look so far.

  Progress   Jev (ClickHouse jev_labels, label_config below) and Clef on the 5090 (read over ssh):
             posts done, speed, finish time, failures, Jev's estimated spend against the cap.
  Labels     for each model, how the topics are shared out so far, next to what Jev gave the same
             posts under v1; the share of 'unclear'; how often the answer changed; the biggest moves.
  Browse     the latest (or a random draw of) labelled posts with their text, pictures (Clef), and
             both answers; filter by topic, by change from v1, by 'unclear', by low confidence.

    python3 label_progress.py --port 8740
    -> http://<this machine>:8740/

Standard library plus PyYAML. It only reads (ClickHouse, the posts export, the picture archive, the
box over ssh) and writes a local copy of Clef's result file under --app-dir, which it keeps up to date
with rsync --append once a minute. Adult-flagged pictures are blurred until clicked.
"""

import argparse
import calendar
import collections
import json
import os
import random
import re
import socket
import subprocess
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

import yaml

HERE = os.path.dirname(os.path.abspath(__file__))
CH_CMD = 'clickhouse-client --user topicfeed --password "$CLICKHOUSE_PASSWORD" --database topicfeed'
USD_PER_M_TOKENS = 0.042  # Jev's v1 report: $18.434 for 438.9 million input tokens
FILTERS = ("all", "changed", "into_unclear", "out_of_unclear", "unclear", "low")


def clickhouse(query, timeout=300):
    return subprocess.run(["docker", "exec", "-i", "topic-feed-clickhouse", "sh", "-c", CH_CMD], input=query,
                          capture_output=True, text=True, check=True, timeout=timeout).stdout


class Row:
    __slots__ = ("uri", "t", "top", "top_p", "second", "second_p", "path", "text", "link", "quote", "shas", "kind",
                 "meme", "humorous", "src")

    def __init__(self, uri, t, top, top_p, second, second_p, path, post, sig=None, src=""):
        self.uri, self.t, self.top, self.top_p, self.second, self.second_p, self.path = uri, t, top, top_p, second, second_p, path
        sig = sig or {}
        self.meme, self.humorous, self.src = sig.get("meme"), sig.get("tone.humorous"), src
        self.text = " ".join((post.get("text") or "").split())[:500]
        self.link = " ".join((post.get("link_title") or "").split())[:140]
        self.quote = " ".join((post.get("quote_text") or "").split())[:200]
        self.shas = (post.get("image_shas") or [])[:4]
        self.kind = post.get("priority", 2)


def top_two(probs):
    items = sorted(probs.items(), key=lambda kv: -kv[1])
    second = items[1] if len(items) > 1 else ("", 0.0)
    return items[0][0], items[0][1], second[0], second[1]


class State:
    def __init__(self, a):
        self.a = a
        tax = yaml.safe_load(open(a.taxonomy))
        self.topic_names = {b["id"]: b["name"] for b in tax["broad"]}
        self.lock = threading.RLock()
        self.offsets = {}
        self.picture_total = 0
        self.jev_rows, self.jev_by_uri, self.jev_last_t = [], {}, 0
        self.clef_rows, self.clef_index, self.clef_offsets = [], {}, {}
        self.v1, self.adult = {}, set()
        self.jev_total = sum(1 for _ in open(a.jev_list)) if os.path.exists(a.jev_list) else 0
        self.status = {"updated": None, "jev": {}, "clef": {}}
        self.summary = {"jev": {}, "clef": {}}
        self.clef_history = collections.deque(maxlen=40)  # (time, remote picture rows)
        self.backfill_history = collections.deque(maxlen=40)  # (time, rows the 4090 has redone)
        self.remote = {"reachable": False, "last_ok": None}
        self.loading = "starting"
        os.makedirs(a.app_dir, exist_ok=True)
        # Clef's picture results come in three files. Part 1 (labelled before the tone and meme signals
        # were saved) is complete and static; part 2 is the 5090's current run, copied here once a
        # minute; the backfill is the 4090 redoing part 1's posts with the signals. A post in the
        # backfill replaces its part-1 row.
        # The redo of part 1's posts has two sources: the 1,697 the 4090 did before it was stopped, and
        # the 5090's redo, which runs after the main run and is copied here like part 2.
        self.local_part2 = os.path.join(a.app_dir, "clef-v21-pictures-part2.jsonl")
        self.local_redo = os.path.join(a.app_dir, "clef-v21-pictures-redo.jsonl")
        self.clef_files = [("part1", a.part1), ("part2", self.local_part2), ("redo", a.backfill), ("redo", self.local_redo)]
        self.part1_rows = sum(1 for _ in open(a.part1)) if os.path.exists(a.part1) else 0

    # --- loading -----------------------------------------------------------------------------

    def index_posts(self):
        pat_uri = re.compile(rb'^\{"uri":"((?:[^"\\]|\\.)*)"')
        pat_pri = re.compile(rb'"priority":(\d)\}\s*$')
        pos = 0
        with open(self.a.posts, "rb") as f:
            for line in f:
                m = pat_uri.match(line)
                if m:
                    self.offsets[json.loads(b'"' + m.group(1) + b'"')] = pos
                    p = pat_pri.search(line)
                    if p and p.group(1) == b"0":
                        self.picture_total += 1
                pos += len(line)

    def read_post(self, uri):
        off = self.offsets.get(uri)
        if off is None:
            return {}
        with open(self.a.posts, "rb") as f:
            f.seek(off)
            return json.loads(f.readline())

    def load_static(self):
        self.loading = "indexing the posts export"
        self.index_posts()
        self.loading = "reading Jev's v1 answers"
        out = clickhouse(
            "SELECT uri, argMin(arrayFirst(k -> bp[k] = arrayMax(mapValues(bp)), mapKeys(bp)), multiIf(source = 'window', 0, source = 'sample', 1, 2)) AS t, "
            "argMin(arrayMax(mapValues(bp)), multiIf(source = 'window', 0, source = 'sample', 1, 2)) AS p "
            "FROM (SELECT uri, source, broad_probs AS bp FROM jev_labels FINAL WHERE taxonomy_version = 'v1' AND jev_model = 'jev-1.13.0') "
            "GROUP BY uri FORMAT TSV")
        for line in out.splitlines():
            uri, t, p = line.split("\t")
            self.v1[uri] = (t, float(p))
        out = clickhouse("SELECT DISTINCT uri FROM post_images FINAL WHERE policy = 'adult_only' FORMAT TSV")
        self.adult = set(out.split())
        self.loading = ""

    # --- Jev ---------------------------------------------------------------------------------

    def pull_jev(self):
        cfg = f"AND label_config = '{self.a.jev_label_config}' " if self.a.jev_label_config else ""
        out = clickhouse(
            "SELECT uri, toUnixTimestamp64Milli(labeled_at) AS t, "
            "arraySort(x -> -x.2, arrayZip(mapKeys(bp), mapValues(bp)))[1].1, arraySort(x -> -x.2, arrayZip(mapKeys(bp), mapValues(bp)))[1].2, "
            "arraySort(x -> -x.2, arrayZip(mapKeys(bp), mapValues(bp)))[2].1, arraySort(x -> -x.2, arrayZip(mapKeys(bp), mapValues(bp)))[2].2, "
            "if(empty(mapKeys(ps)), '', arrayFirst(k -> ps[k] = arrayMax(mapValues(ps)), mapKeys(ps))) "
            "FROM (SELECT uri, labeled_at, broad_probs AS bp, path_scores AS ps FROM jev_labels "
            f"WHERE taxonomy_version = '{self.a.jev_version}' {cfg}AND toUnixTimestamp64Milli(labeled_at) > {self.jev_last_t - 120000}) "
            "ORDER BY t FORMAT TSV")
        new = []
        for line in out.splitlines():
            uri, t, top, tp, sec, sp, path = line.split("\t")
            if uri in self.jev_by_uri:
                continue
            row = Row(uri, int(t), top, float(tp), sec, float(sp), path, self.read_post(uri))
            new.append(row)
            self.jev_last_t = max(self.jev_last_t, row.t)
        with self.lock:
            for r in new:
                self.jev_by_uri[r.uri] = r
                self.jev_rows.append(r)

    def jev_requests(self):
        out = clickhouse("SELECT count(), sum(input_tokens), countIf(status != 'ok'), round(quantile(0.5)(latency_ms)) "
                         f"FROM jev_requests WHERE ts >= '{self.a.since}' FORMAT TSV").strip().split("\t")
        n, tok, bad, p50 = (int(float(x)) if x not in ("", "\\N", "nan") else 0 for x in out)
        return n, tok, bad, p50

    # --- Clef --------------------------------------------------------------------------------

    def sync_clef(self):
        """Poll the box (one ssh call) and keep a local copy of the picture results up to date."""
        ssh = ["ssh", "-i", self.a.remote_key, "-p", str(self.a.remote_port), "-o", "ConnectTimeout=15",
               "-o", "ServerAliveInterval=10", self.a.remote]
        script = ("cd /root/clef/out 2>/dev/null; echo \"pics=$(wc -l < clef-v21-pictures.jsonl 2>/dev/null || echo 0)\"; "
                  "echo \"calib=$(wc -l < clef-v21-calib.jsonl 2>/dev/null || echo 0)\"; "
                  "echo \"errs=$(cat clef-v21-pictures.jsonl.errors.jsonl 2>/dev/null | wc -l)\"; "
                  "echo \"redo=$(wc -l < clef-v21-pictures-redo.jsonl 2>/dev/null || echo 0)\"; "
                  "echo \"tmux=$( (tmux has-session -t clef-v21b 2>/dev/null || tmux has-session -t clef-v21c 2>/dev/null) && echo up || echo down)\"; "
                  "echo \"chain=$(tail -n 1 clef-v21-chain.log 2>/dev/null)\"; "
                  "echo \"gpu=$(nvidia-smi --query-gpu=utilization.gpu,memory.used,temperature.gpu --format=csv,noheader 2>/dev/null)\"; "
                  "echo \"log=$(tail -c 400 clef-v21-pictures.log 2>/dev/null | tr '\\r' '\\n' | grep -E '^[0-9]+/[0-9]+' | tail -n 1)\"")
        try:
            res = subprocess.run(ssh + [script], capture_output=True, text=True, timeout=60)
            kv = dict(line.split("=", 1) for line in res.stdout.splitlines() if "=" in line)
            if "pics" not in kv:
                raise RuntimeError(res.stderr[-200:] or "no answer")
            self.remote = {"reachable": True, "last_ok": time.time(), "pics": int(kv["pics"]), "calib": int(kv.get("calib", 0)),
                           "errs": int(kv.get("errs", 0)), "redo": int(kv.get("redo", 0)), "tmux": kv.get("tmux", ""),
                           "chain": kv.get("chain", ""),
                           "gpu": kv.get("gpu", ""), "log": kv.get("log", "")}
            self.clef_history.append((time.time(), self.remote["pics"]))
            subprocess.run(["rsync", "-a", "--append", "-e",
                            f"ssh -i {self.a.remote_key} -p {self.a.remote_port} -o ConnectTimeout=15",
                            f"{self.a.remote}:/root/clef/out/clef-v21-pictures.jsonl", self.local_part2],
                           capture_output=True, timeout=600)
            if int(kv.get("redo", 0)) > 0:
                subprocess.run(["rsync", "-a", "--append", "-e",
                                f"ssh -i {self.a.remote_key} -p {self.a.remote_port} -o ConnectTimeout=15",
                                f"{self.a.remote}:/root/clef/out/clef-v21-pictures-redo.jsonl", self.local_redo],
                               capture_output=True, timeout=600)
        except Exception as e:  # noqa: BLE001  (the box being unreachable is an expected state to show)
            self.remote = {**self.remote, "reachable": False, "error": str(e)[:200]}

    def read_clef(self):
        """Read what is new in the three result files. A row with the tone and meme signals replaces
        one without (a post's part-1 row is replaced when the 4090 has redone it)."""
        for name, path in self.clef_files:
            if not os.path.exists(path):
                continue
            new = []
            with open(path, "rb") as f:
                f.seek(self.clef_offsets.get(path, 0))
                while True:
                    line = f.readline()
                    if not line or not line.endswith(b"\n"):
                        break  # a line still being copied
                    self.clef_offsets[path] = self.clef_offsets.get(path, 0) + len(line)
                    try:
                        r = json.loads(line)
                        top, tp, sec, sp = top_two(r["broad_probs"])
                    except (ValueError, KeyError):
                        continue
                    have = self.clef_index.get(r["uri"])
                    if have is not None and (self.clef_rows[have].meme is not None or r["signals"].get("meme") is None):
                        continue
                    row = Row(r["uri"], 0, top, tp, sec, sp, r.get("top_path") or "", self.read_post(r["uri"]),
                              r.get("signals"), name)
                    new.append(row)
            with self.lock:
                for row in new:
                    i = self.clef_index.get(row.uri)
                    if i is None:
                        self.clef_index[row.uri] = len(self.clef_rows)
                        self.clef_rows.append(row)
                    else:
                        self.clef_rows[i] = row

    # --- summaries ---------------------------------------------------------------------------

    def summarize(self, rows):
        """Topic shares, change from Jev v1, 'unclear' and confidence for a list of rows."""
        n = len(rows)
        new_c, old_c = collections.Counter(), collections.Counter()
        moves = collections.Counter()
        cmp_n = changed = 0
        top_new = top_old = 0.0
        unclear_new = unclear_old = 0
        meme_n, meme_hi = collections.Counter(), collections.Counter()  # by topic: rows with the meme signal, and P(meme) >= 0.5
        meme_hist = collections.defaultdict(lambda: [0] * 10)  # by topic: rows by P(meme) in tenths, so the page can use any threshold
        for r in rows:
            new_c[r.top] += 1
            if r.meme is not None:
                meme_n[r.top] += 1
                meme_hi[r.top] += r.meme >= 0.5
                meme_hist[r.top][min(int(r.meme * 10), 9)] += 1
            v = self.v1.get(r.uri)
            if v:
                cmp_n += 1
                old_c[v[0]] += 1
                top_new += r.top_p
                top_old += v[1]
                unclear_new += r.top == "unclear"
                unclear_old += v[0] == "unclear"
                if v[0] != r.top:
                    changed += 1
                    moves[(v[0], r.top)] += 1
        chunk = 10000 if len(rows) > 30000 else 5000
        trend = [round(100 * sum(r.top == "unclear" for r in rows[i:i + chunk]) / len(rows[i:i + chunk]), 1)
                 for i in range(0, len(rows) - chunk // 2, chunk)]
        topics = []
        for t in self.topic_names:
            topics.append({"id": t, "name": self.topic_names[t], "n": new_c[t], "old": old_c[t],
                           "meme_n": meme_n[t], "meme_hi": meme_hi[t], "meme_hist": list(meme_hist[t]) if t in meme_hist else [0] * 10})
        return {"n": n, "compared": cmp_n, "changed": changed,
                "meme_n": sum(meme_n.values()), "meme_hi": sum(meme_hi.values()),
                "unclear_new": unclear_new, "unclear_old": unclear_old,
                "avg_top_new": top_new / cmp_n if cmp_n else None, "avg_top_old": top_old / cmp_n if cmp_n else None,
                "topics": topics, "moves": [[a, b, c] for (a, b), c in moves.most_common(10)],
                "unclear_trend": trend, "trend_chunk": chunk}

    def refresh(self):
        try:
            self.pull_jev()
        except Exception as e:  # noqa: BLE001
            self.status["jev_error"] = str(e)[:200]
        try:
            n, tok, bad, p50 = self.jev_requests()
        except Exception as e:  # noqa: BLE001
            n = tok = bad = p50 = 0
            self.status["jev_error"] = str(e)[:200]
        self.sync_clef()
        self.read_clef()
        now = time.time()
        with self.lock:
            jev_n = len(self.jev_rows)
            cut = (now - 600) * 1000
            jev_rate = sum(1 for r in self.jev_rows if r.t > cut) / 10.0
            since_ms = calendar.timegm(time.strptime(self.a.since, "%Y-%m-%d %H:%M:%S")) * 1000
            before = sum(1 for r in self.jev_rows if r.t < since_ms)  # labelled by the earlier test run
            this_run = jev_n - before
            projected = tok / 1e6 * USD_PER_M_TOKENS * (self.jev_total - before) / this_run if this_run > 0 and tok else None
            jev_left = max(self.jev_total - jev_n, 0)
            jev_eta = now + 60 * jev_left / jev_rate if jev_rate > 0 and jev_left else None
            rem = self.remote
            rate = None
            hist = [h for h in self.clef_history if h[0] >= now - 900]
            if len(hist) >= 2 and hist[-1][0] > hist[0][0]:
                rate = (hist[-1][1] - hist[0][1]) / (hist[-1][0] - hist[0][0])
            if (not rate) and rem.get("log"):
                m = re.search(r"([\d.]+) posts/s this round", rem["log"]) or re.search(r"([\d.]+) posts/s now", rem["log"])
                rate = float(m.group(1)) if m else None
            pics = rem.get("pics", 0)  # the 5090's run since the restart (part 2)
            labelled = self.part1_rows + pics
            total = self.picture_total
            clef_left = max(total - labelled, 0)
            clef_eta = now + clef_left / rate if rate and clef_left else None
            with_signals = sum(1 for r in self.clef_rows if r.meme is not None)
            backfill_done = sum(1 for r in self.clef_rows if r.src == "redo")
            bf_left = max(self.part1_rows - backfill_done, 0)
            # The redo runs on the 5090 after the main run, at the same speed: it ends about
            # bf_left / rate after the main run does (or after now, once the main run is over).
            bf_eta = (clef_eta or now) + bf_left / rate if rate and bf_left else None
            bf_rate = None
            self.status = {
                "updated": now, "loading": self.loading,
                "jev": {"done": jev_n, "total": self.jev_total, "rate_per_min": jev_rate, "eta": jev_eta,
                        "requests": n, "failed_requests": bad, "tokens_m": tok / 1e6, "est_usd": tok / 1e6 * USD_PER_M_TOKENS,
                        "cap_usd": self.a.cap_usd, "projected_usd": projected, "p50_ms": p50,
                        "finished": jev_n >= self.jev_total > 0,
                        "label_config": self.a.jev_label_config},
                "clef": {"done": labelled, "part2": pics, "loaded": len(self.clef_rows), "total": total, "rate_per_s": rate,
                         "eta": clef_eta, "with_signals": with_signals, "backfill_done": backfill_done,
                         "backfill_total": self.part1_rows, "backfill_rate_per_s": bf_rate, "backfill_eta": bf_eta,
                         "errors": rem.get("errs", 0), "tmux": rem.get("tmux", ""), "reachable": rem.get("reachable", False),
                         "last_ok": rem.get("last_ok"), "gpu": rem.get("gpu", ""), "log": rem.get("log", ""),
                         "chain": rem.get("chain", ""), "calibration_rows": rem.get("calib", 0),
                         "redo_on_box": rem.get("redo", 0),
                         "finished": "all finished" in rem.get("chain", "")},
            }
            jev_rows, clef_rows = list(self.jev_rows), list(self.clef_rows)
        self.summary = {"jev": self.summarize(jev_rows), "clef": self.summarize(clef_rows)}

    def loop(self):
        try:
            self.load_static()
        except Exception as e:  # noqa: BLE001
            self.loading = f"failed to load: {e}"
            return
        while True:
            t0 = time.time()
            try:
                self.refresh()
            except Exception as e:  # noqa: BLE001
                self.status["error"] = str(e)[:300]
            time.sleep(max(5, self.a.every - (time.time() - t0)))

    # --- browsing ----------------------------------------------------------------------------

    def items(self, q):
        run = q.get("run", "jev")
        flt = q.get("filter", "all")
        topic = q.get("topic", "")
        sort = q.get("sort", "latest")
        text = q.get("q", "").lower().strip()
        offset = max(int(q.get("offset", 0) or 0), 0)
        limit = min(max(int(q.get("limit", 30) or 30), 1), 100)
        seed = q.get("seed", "1")
        meme_min = float(q.get("meme_min", 0.5))  # a ValueError here is answered with a 400
        with self.lock:
            rows = list(self.jev_rows if run == "jev" else self.clef_rows)
        low = 0.5 if run == "jev" else 0.35

        def keep(r):
            if topic and r.top != topic:
                return False
            v = self.v1.get(r.uri)
            if flt == "changed" and not (v and v[0] != r.top):
                return False
            if flt == "into_unclear" and not (r.top == "unclear" and v and v[0] != "unclear"):
                return False
            if flt == "out_of_unclear" and not (v and v[0] == "unclear" and r.top != "unclear"):
                return False
            if flt == "unclear" and r.top != "unclear":
                return False
            if flt == "low" and r.top_p >= low:
                return False
            if flt == "meme" and not (r.meme is not None and r.meme >= meme_min):
                return False
            if flt == "not_meme" and not (r.meme is not None and r.meme < meme_min):
                return False
            if flt == "no_meme_label" and r.meme is not None:
                return False
            if text and text not in r.text.lower() and text not in r.link.lower() and text not in r.quote.lower():
                return False
            return True

        hits = [r for r in rows if keep(r)]
        total = len(hits)
        if sort == "random":
            random.Random(seed).shuffle(hits)
        elif sort == "meme":
            hits.sort(key=lambda r: -(r.meme if r.meme is not None else -1))
        else:
            hits.reverse()
        out = []
        for r in hits[offset:offset + limit]:
            did, rkey = r.uri[5:].split("/app.bsky.feed.post/")
            v = self.v1.get(r.uri)
            out.append({"uri": r.uri, "link": f"https://bsky.app/profile/{did}/post/{rkey}", "text": r.text, "card": r.link,
                        "quote": r.quote, "top": r.top, "top_p": round(r.top_p, 3), "second": r.second, "second_p": round(r.second_p, 3),
                        "path": r.path, "ref": v[0] if v else None, "ref_p": round(v[1], 3) if v else None,
                        "meme": round(r.meme, 3) if r.meme is not None else None,
                        "humorous": round(r.humorous, 3) if r.humorous is not None else None,
                        "shas": r.shas if run == "clef" else [], "adult": r.uri in self.adult})
        return {"total": total, "items": out, "offset": offset}


PAGE = r"""<!doctype html>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Label runs, taxonomy v2.1</title>
<style>
:root{--bg:#f6f7f9;--card:#fff;--ink:#1c2430;--mute:#667085;--line:#e3e7ee;--jev:#2f6fdd;--clef:#c2571a;--old:#9aa5b5;--ok:#1a8f4d;--bad:#c0392b}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--ink);font:14px/1.45 system-ui,-apple-system,Segoe UI,sans-serif}
header{padding:14px 22px;background:#fff;border-bottom:1px solid var(--line);display:flex;gap:18px;align-items:baseline;flex-wrap:wrap}
h1{font-size:18px;margin:0}h2{font-size:15px;margin:0 0 10px}.mute{color:var(--mute)}.small{font-size:12px}
main{max-width:1280px;margin:0 auto;padding:18px 22px 60px}
.grid{display:grid;grid-template-columns:1fr 1fr;gap:16px}@media(max-width:900px){.grid{grid-template-columns:1fr}}
.card{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:16px 18px}
.big{font-size:30px;font-weight:650;letter-spacing:-.5px}.bar{height:12px;background:#e9edf3;border-radius:6px;overflow:hidden;margin:8px 0}
.bar i{display:block;height:100%}.jev .bar i{background:var(--jev)}.clef .bar i{background:var(--clef)}
dl{display:grid;grid-template-columns:auto 1fr;gap:3px 14px;margin:10px 0 0}dt{color:var(--mute)}dd{margin:0}
.pill{display:inline-block;padding:1px 9px;border-radius:99px;font-size:12px;border:1px solid var(--line)}
.ok{color:var(--ok);border-color:var(--ok)}.bad{color:var(--bad);border-color:var(--bad)}
table{border-collapse:collapse;width:100%}td,th{padding:3px 6px;text-align:left;vertical-align:middle}th{color:var(--mute);font-weight:500;font-size:12px}
tr.t{cursor:pointer}tr.t:hover{background:#f1f4f9}td.n{text-align:right;font-variant-numeric:tabular-nums;white-space:nowrap}
.tb{position:relative;height:12px;background:#f0f3f8;border-radius:3px;min-width:140px}.tb i{position:absolute;left:0;top:0;height:100%;border-radius:3px}
.tb b{position:absolute;top:-2px;width:2px;height:16px;background:#222}
.jev .tb i{background:var(--jev)}.clef .tb i{background:var(--clef)}
.moves li{margin:2px 0}.tabs button,.ctl select,.ctl input,.ctl button{font:inherit;padding:5px 10px;border:1px solid var(--line);border-radius:7px;background:#fff}
.tabs button.on{background:var(--ink);color:#fff;border-color:var(--ink)}.ctl{display:flex;gap:8px;flex-wrap:wrap;margin:12px 0}
.item{display:grid;grid-template-columns:1fr 300px;gap:14px;padding:12px 0;border-top:1px solid var(--line)}@media(max-width:800px){.item{grid-template-columns:1fr}}
.text{white-space:pre-wrap;word-break:break-word}.sub{color:var(--mute);font-size:12px;margin-top:4px}
.chip{display:inline-block;padding:1px 8px;border-radius:5px;background:#eef2f8;margin-right:4px;font-size:12px}
.chip.diff{background:#fde8d9}.chip.new{background:#dbe8ff}.pics{display:flex;gap:6px;margin-top:8px;flex-wrap:wrap}
.pics img{max-height:150px;max-width:200px;border-radius:6px;border:1px solid var(--line)}.pics img.blur{filter:blur(24px);cursor:pointer}
.conf{height:6px;background:#e9edf3;border-radius:3px;margin:3px 0 6px;width:200px}.conf i{display:block;height:100%;background:#556}
a{color:#2b5fc0;text-decoration:none}a:hover{text-decoration:underline}
.mb{display:inline-block;padding:3px 11px;border-radius:6px;font-weight:650;font-size:12px;margin:8px 6px 0 0;letter-spacing:.2px}
.mb.yes{background:#c2571a;color:#fff}.mb.no{background:#e4e8ef;color:#4a5568}.mb.none{background:#fff3cd;color:#8a6d00}
</style>
<header><h1>Label runs, taxonomy v2.1</h1><span class="mute small" id="upd">loading…</span><span id="load" class="small mute"></span></header>
<main>
<div class="grid" id="progress"></div>
<h2 style="margin-top:22px">How the labels look so far <span class="mute small">(click a topic to browse its posts below)</span></h2>
<div class="grid" id="summary"></div>
<h2 style="margin-top:22px">Browse the labels</h2>
<div class="card">
 <div class="tabs"><button id="tab-jev" class="on">Jev, text and link-card posts</button> <button id="tab-clef">Clef, picture posts</button></div>
 <div class="ctl">
  <select id="topic"></select>
  <select id="filter"></select>
  <select id="sort"><option value="latest">latest first</option><option value="random">random draw</option><option value="meme">most meme-like first</option></select>
  <label class="small mute" title="A picture post is labelled a meme when Clef's P(meme) is at least this">meme if P &ge;
   <select id="mt"><option>0.3</option><option>0.4</option><option selected>0.5</option><option>0.6</option><option>0.7</option><option>0.8</option></select></label>
  <input id="q" placeholder="search text" size="22"><button id="go">Show</button><button id="shuffle">New draw</button>
  <span class="mute small" id="count"></span>
 </div>
 <div id="items"></div>
 <p><button id="more" class="ctl" style="display:none">Show more</button></p>
</div>
</main>
<script>
const $=s=>document.querySelector(s);
const esc=s=>String(s??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const fmt=n=>(n??0).toLocaleString('en-US');
const pct=(a,b)=>b?(100*a/b).toFixed(1)+'%':'–';
const hhmm=t=>t?new Date(t*1000).toISOString().slice(11,16)+' UTC':'–';
const ago=t=>{if(!t)return '–';const s=Math.round(Date.now()/1000-t);return s<90?s+' s ago':Math.round(s/60)+' min ago'};
let run='jev',offset=0,seed='1',topics=[],names={};
const FILTERS=[['all','all posts'],['changed','answer changed from v1 (Jev v1 for Clef)'],['into_unclear','moved into unclear'],['out_of_unclear','moved out of unclear'],['unclear','unclear now'],['low','low confidence'],['meme','labelled a meme (Clef)'],['not_meme','labelled not a meme (Clef)'],['no_meme_label','no meme label yet, redo pending (Clef)']];
let memeMin=0.5,lastSummary=null;
function dur(sec){if(sec==null||!isFinite(sec))return '–';const h=Math.floor(sec/3600),m=Math.round(sec%3600/60);return h?h+' h '+m+' min':m+' min'}
function card(cls,title,o,extra){return `<div class="card ${cls}"><h2>${title}</h2><div class="big">${pct(o.done,o.total)}</div>
<div class="bar"><i style="width:${o.total?Math.min(100,100*o.done/o.total):0}%"></i></div><div>${fmt(o.done)} of ${fmt(o.total)} posts</div><dl>${extra}</dl></div>`}
async function status(){
 const s=await (await fetch('/api/status')).json();
 $('#upd').textContent='updated '+(s.updated?new Date(s.updated*1000).toISOString().slice(11,19)+' UTC':'…')+' · refreshes every minute';
 $('#load').textContent=s.loading||'';
 const j=s.jev||{},c=s.clef||{};
 let jr=j.rate_per_min?fmt(Math.round(j.rate_per_min))+' posts/min':'–';
 const jstate=j.finished?'<span class="pill ok">finished</span>':(j.rate_per_min>0?'<span class="pill ok">running</span>':'<span class="pill bad">no new labels in 10 min</span>');
 const left=j.total-j.done;
 $('#progress').innerHTML=
  card('jev','Jev (hosted), text-only and link-card posts',j,
   `<dt>State</dt><dd>${jstate}</dd><dt>Speed</dt><dd>${jr}</dd><dt>Finishes</dt><dd>${j.finished?'done':hhmm(j.eta)}${j.eta?' · in '+dur(j.eta-s.updated):''}</dd>
    <dt>Failed requests</dt><dd>${fmt(j.failed_requests)} of ${fmt(j.requests)}</dd>
    <dt>Spend (estimate)</dt><dd>$${(j.est_usd||0).toFixed(2)} so far, cap $${j.cap_usd} · ${(j.tokens_m||0).toFixed(0)}M input tokens${j.projected_usd?', projected $'+j.projected_usd.toFixed(0)+' for the full run':''}</dd>
    <dt>Median latency</dt><dd>${fmt(j.p50_ms)} ms</dd>`)+
  card('clef','Clef-flash (5090), posts with a picture or video',c,
   `<dt>State</dt><dd>${c.finished?'<span class="pill ok">finished</span>':(c.reachable?(c.tmux==='up'?'<span class="pill ok">running</span>':'<span class="pill bad">session not running</span>'):'<span class="pill bad">box unreachable</span>')} <span class="mute small">box checked ${ago(c.last_ok)}</span></dd>
    <dt>Speed</dt><dd>${c.rate_per_s?c.rate_per_s.toFixed(2)+' posts/s':'–'}</dd><dt>Finishes</dt><dd>${c.finished?'done':hhmm(c.eta)}${c.eta?' · in '+dur(c.eta-s.updated):''}</dd>
    <dt>Failed posts</dt><dd>${fmt(c.errors)}</dd><dt>GPU (5090)</dt><dd>${esc(c.gpu||'–')} <span class="mute small">(% busy, MiB, °C)</span></dd>
    <dt>With tone + meme signals</dt><dd>${fmt(c.with_signals)} of ${fmt(c.total)} <span class="mute small">(the 5090's posts since the restart, plus the earlier posts once redone)</span></dd>
    <dt>Redo of the earlier posts</dt><dd>${fmt(c.backfill_done)} of ${fmt(c.backfill_total)} <span class="mute small">(1,697 done on the 4090 before it was stopped; the rest run on the 5090 right after the main run)</span>${c.backfill_eta?' · done about '+hhmm(c.backfill_eta):(c.backfill_done>=c.backfill_total?' · done':'')}</dd>
    <dt>Calibration sample</dt><dd>${fmt(c.calibration_rows)} of 5,000 text posts, done and imported</dd>`);
}
const memeHi=t=>t.meme_hist.slice(Math.round(memeMin*10)).reduce((a,b)=>a+b,0);
function sumCard(cls,title,m,refName){
 if(!m||!m.n)return `<div class="card ${cls}"><h2>${title}</h2><span class="mute">no labels yet</span></div>`;
 const memeAll=m.topics.reduce((a,t)=>a+memeHi(t),0);
 const maxn=Math.max(...m.topics.map(t=>Math.max(t.n,t.old)),1);
 const rows=m.topics.slice().sort((a,b)=>b.n-a.n).map(t=>`<tr class="t" data-t="${t.id}"><td>${esc(t.name)}</td>
  <td><div class="tb"><i style="width:${100*t.n/maxn}%"></i><b style="left:${100*t.old/maxn}%" title="${refName}: ${fmt(t.old)}"></b></div></td>
  <td class="n">${pct(t.n,m.n)}</td><td class="n mute">${m.compared?pct(t.old,m.compared):'–'}</td>
  ${cls==='clef'?`<td class="n" title="${fmt(memeHi(t))} of ${fmt(t.meme_n)} posts with the signal">${t.meme_n?pct(memeHi(t),t.meme_n):'–'}</td>`:''}</tr>`).join('');
 const dif=m.compared?pct(m.changed,m.compared):'–';
 const mv=m.moves.map(([a,b,n])=>`<li>${esc(names[a]||a)} → ${esc(names[b]||b)} <span class="mute">${fmt(n)}</span></li>`).join('');
 return `<div class="card ${cls}"><h2>${title}</h2>
 <div class="small mute">${fmt(m.n)} posts labelled. Bars: this run. Black tick and last column: what Jev gave the same posts under v1 (${fmt(m.compared)} of them have a v1 label).</div>
 <dl><dt>Unclear</dt><dd>${pct(m.unclear_new,m.compared)} <span class="mute">(${refName}: ${pct(m.unclear_old,m.compared)})</span></dd>
 <dt>${cls==='jev'?'Answer changed from v1':'Differs from Jev v1'}</dt><dd>${dif}</dd>
 <dt>Average top-topic confidence</dt><dd>${m.avg_top_new?m.avg_top_new.toFixed(2):'–'} <span class="mute">(${refName}: ${m.avg_top_old?m.avg_top_old.toFixed(2):'–'})</span>${cls==='clef'?' <span class="mute small">raw, not calibrated to Jev</span>':''}</dd>
 <dt>Unclear by ${fmt(m.trend_chunk)}-post chunk</dt><dd>${m.unclear_trend.length?m.unclear_trend.join('%, ')+'%':'–'}</dd></dl>
 ${cls==='clef'?`<dl><dt>Labelled a meme (P ≥ ${memeMin})</dt><dd>${m.meme_n?pct(memeAll,m.meme_n)+' of '+fmt(m.meme_n)+' posts that have the label':'no posts with the label yet'}<span class="mute small"> · ${fmt(m.n-m.meme_n)} posts have none yet (redo pending)</span></dd></dl>`:''}
 <table style="margin-top:10px"><tr><th>Topic</th><th></th><th class="n">share</th><th class="n">v1</th>${cls==='clef'?`<th class="n" title="share of the topic's posts labelled a meme (P at least ${memeMin})">memes</th>`:''}</tr>${rows}</table>
 <div class="small mute" style="margin-top:8px">Biggest moves (${cls==='jev'?'v1 → v2.1':'Jev v1 → Clef'}):</div><ul class="moves small" style="margin:4px 0 0 18px;padding:0">${mv||'<li class="mute">none yet</li>'}</ul></div>`}
async function summary(){lastSummary=await (await fetch('/api/summary')).json();renderSummary()}
function renderSummary(){
 const s=lastSummary;if(!s)return;
 $('#summary').innerHTML=sumCard('jev','Jev v2.1 against Jev v1',s.jev,'v1')+sumCard('clef','Clef (raw answers) against Jev v1',s.clef,'Jev v1');
 document.querySelectorAll('#summary tr.t').forEach(tr=>tr.onclick=()=>{run=tr.closest('.card').classList.contains('jev')?'jev':'clef';setTab();$('#topic').value=tr.dataset.t;load(true);$('#items').scrollIntoView({behavior:'smooth'})});
}
function setTab(){$('#tab-jev').classList.toggle('on',run==='jev');$('#tab-clef').classList.toggle('on',run==='clef')}
function memeBadge(it){
 if(run!=='clef')return '';
 if(it.meme==null)return '<span class="mb none" title="labelled before the meme signal existed; the redo will add it">meme: no label yet</span>';
 return it.meme>=memeMin?`<span class="mb yes" title="P(meme) ${it.meme}, labelled a meme at P ≥ ${memeMin}">MEME · ${it.meme}</span>`
  :`<span class="mb no" title="P(meme) ${it.meme}, below ${memeMin}">not a meme · ${it.meme}</span>`}
function itemHtml(it){
 const refChip=it.ref?`<span class="chip ${it.ref!==it.top?'diff':''}">${run==='jev'?'v1':'Jev v1'}: ${esc(names[it.ref]||it.ref)} ${it.ref_p}</span>`:'<span class="chip">no v1 label</span>';
 const pics=(it.shas||[]).map(s=>`<img src="/img/${s}" loading="lazy" class="${it.adult?'blur':''}" ${it.adult?'title="adult-flagged, click to show" onclick="this.classList.toggle(\'blur\')"':''}>`).join('');
 return `<div class="item"><div><div class="text">${esc(it.text)||'<span class="mute">(no text)</span>'}</div>
  ${it.card?`<div class="sub">link card: ${esc(it.card)}</div>`:''}${it.quote?`<div class="sub">quotes: ${esc(it.quote)}</div>`:''}
  ${memeBadge(it)}${pics?`<div class="pics">${pics}</div>`:''}<div class="sub"><a href="${it.link}" target="_blank" rel="noopener">open on Bluesky</a></div></div>
  <div><span class="chip new">${run==='jev'?'v2.1':'Clef'}: ${esc(names[it.top]||it.top)} ${it.top_p}</span>${refChip}
  ${it.meme!=null?`<span class="chip">humorous tone ${it.humorous}</span>`:''}
  <div class="conf"><i style="width:${100*it.top_p}%"></i></div>
  <div class="small mute">runner-up: ${esc(names[it.second]||it.second||'–')} ${it.second_p}<br>best path: ${esc(it.path||'–')}</div></div></div>`}
let loadSeq=0;
async function load(reset){
 const my=++loadSeq;
 if(reset){offset=0;$('#items').innerHTML=''}
 const p=new URLSearchParams({run,topic:$('#topic').value,filter:$('#filter').value,sort:$('#sort').value,q:$('#q').value,offset,limit:30,seed,meme_min:memeMin});
 const d=await (await fetch('/api/items?'+p)).json();
 if(my!==loadSeq)return; // a newer request has replaced this one
 $('#items').insertAdjacentHTML('beforeend',d.items.map(itemHtml).join('')||(reset?'<p class="mute">Nothing matches yet.</p>':''));
 offset+=d.items.length;$('#count').textContent=fmt(d.total)+' posts match';
 $('#more').style.display=offset<d.total?'':'none';
}
async function init(){
 const s=await (await fetch('/api/topics')).json();topics=s.topics;names=Object.fromEntries(topics.map(t=>[t.id,t.name]));
 $('#topic').innerHTML='<option value="">all topics</option>'+topics.map(t=>`<option value="${t.id}">${esc(t.name)}</option>`).join('');
 $('#filter').innerHTML=FILTERS.map(f=>`<option value="${f[0]}">${f[1]}</option>`).join('');
 $('#tab-jev').onclick=()=>{run='jev';setTab();load(true)};$('#tab-clef').onclick=()=>{run='clef';setTab();load(true)};
 $('#go').onclick=()=>load(true);$('#more').onclick=()=>load(false);$('#shuffle').onclick=()=>{seed=String(Math.random());$('#sort').value='random';load(true)};
 $('#q').onkeydown=e=>{if(e.key==='Enter')load(true)};
 $('#mt').onchange=()=>{memeMin=parseFloat($('#mt').value);renderSummary();load(true)};
 await status();await summary();await load(true);
 setInterval(()=>{status();summary()},60000);
}
init();
</script>
"""


class Handler(BaseHTTPRequestHandler):
    state = None

    def log_message(self, *args):
        pass

    def send(self, code, body, ctype="application/json"):
        data = body if isinstance(body, bytes) else (body if isinstance(body, str) else json.dumps(body)).encode()
        self.send_response(code)
        self.send_header("Content-Type", ctype + ("; charset=utf-8" if ctype.startswith("text") or "json" in ctype else ""))
        self.send_header("Content-Length", str(len(data)))
        self.send_header("Cache-Control", "no-store" if ctype != "image/jpeg" else "max-age=86400")
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        u = urlparse(self.path)
        st = self.state
        if u.path == "/":
            self.send(200, PAGE, "text/html")
        elif u.path == "/api/status":
            self.send(200, st.status)
        elif u.path == "/api/summary":
            self.send(200, st.summary)
        elif u.path == "/api/topics":
            self.send(200, {"topics": [{"id": k, "name": v} for k, v in st.topic_names.items()]})
        elif u.path == "/api/items":
            q = {k: v[0] for k, v in parse_qs(u.query).items()}
            try:
                self.send(200, st.items(q))
            except ValueError:
                self.send(400, {"error": "bad request"})
        elif u.path.startswith("/img/"):
            sha = u.path[5:]
            if not re.fullmatch(r"[0-9a-f]{64}", sha):
                return self.send(404, {"error": "not found"})
            path = os.path.join(st.a.images, "1000", sha[:2], sha + ".jpg")
            if not os.path.exists(path):
                return self.send(404, {"error": "no such picture here"})
            self.send(200, open(path, "rb").read(), "image/jpeg")
        else:
            self.send(404, {"error": "not found"})


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--port", type=int, default=8740)
    ap.add_argument("--host", default="::", help="address to listen on (default: every interface)")
    ap.add_argument("--taxonomy", default=os.path.join(HERE, "..", "taxonomy", "v2.1.yaml"))
    ap.add_argument("--posts", default="/data/clef/full_posts.jsonl", help="clef_export.py's file")
    ap.add_argument("--images", default="/data/images")
    ap.add_argument("--part1", default="/data/clef/v2/remote/clef-v21-pictures-part1.jsonl",
                    help="Clef's picture results from before the tone and meme signals were saved (complete)")
    ap.add_argument("--backfill", default="/data/clef/v2/backfill/clef-v21-backfill.jsonl",
                    help="the local run that redoes part 1 with the signals")
    ap.add_argument("--jev-version", default="v2.1")
    ap.add_argument("--jev-label-config", default="30cf2c546019")
    ap.add_argument("--jev-list", default="/data/clef/v2/jev-main-uris.txt", help="the posts the Jev run labels (its length is the total)")
    ap.add_argument("--since", default="2026-10-02 14:54:00", help="count Jev requests from this time (UTC)")
    ap.add_argument("--cap-usd", type=float, default=30.0)
    ap.add_argument("--every", type=int, default=60, help="seconds between refreshes")
    ap.add_argument("--app-dir", default="/data/clef/v2/app")
    ap.add_argument("--remote", default=os.environ.get("CLEF_BOX", ""))
    ap.add_argument("--remote-port", type=int, default=9305)
    ap.add_argument("--remote-key", default=os.path.expanduser("~/.ssh/id_ed25519_clef"))
    a = ap.parse_args()

    st = State(a)
    Handler.state = st
    threading.Thread(target=st.loop, daemon=True).start()

    class Server(ThreadingHTTPServer):
        address_family = socket.AF_INET6
        daemon_threads = True

    srv = Server((a.host, a.port), Handler)
    print(f"label progress page: http://localhost:{a.port}/", flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    main()
