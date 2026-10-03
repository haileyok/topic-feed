"""Consolidates every hand-judged set into one file with one row per post per round.

Rounds (oldest first), each from its own folder of verdicts, the answers shown, and the hidden teacher answers:

  gold-v1-100                    the owner's own labels for 88 text posts under taxonomy v1 (primary topic, acceptable
                                 topics, can't-judge flag, note); no model answer was shown
  jev-v1-vs-clef-flash           which of two answers was closer to right (Jev under v1, the first Clef-flash run)
  single-answer-3-epochs         one answer from the first ModernVBERT student (3 epochs): topic right / acceptable /
                                 wrong / can't tell, the topic it should have been, a meme verdict for picture posts
  single-answer-3-epochs-pictures  the same for 50 more picture posts
  changed-answers-8-epochs       only the posts where the 8-epoch ModernVBERT student's answer differed from the first one
  blind-ab-8-epochs-vs-teacher   blind side by side of that student against its teacher where they disagree
  blind-ab-fusion-vs-clef        blind side by side of the fusion model against Clef-flash, picture posts

For every round the last verdict line for a post counts. Rows carry the post, the judgment, the answers the judge saw, and
what was hidden from the judge (the teacher's answer, which side was the model). Writes <out>/judged-sets.jsonl,
<out>/README.md and copies the raw files to <out>/raw/.

    python3 consolidate_judged.py --out /data/judged-sets
"""

import argparse
import collections
import json
import os
import re
import shutil
import subprocess
import time

ROOT = "/data"
CH = ["docker", "exec", "-i", "topic-feed-clickhouse", "sh", "-c",
      'clickhouse-client --user topicfeed --password "$CLICKHOUSE_PASSWORD" --database topicfeed']
URI_AT_START = re.compile(rb'^\{"uri":"((?:[^"\\]|\\.)*)"')


def last_per_post(path):
    out = {}
    for line in open(path):
        try:
            v = json.loads(line)
            out[v["uri"]] = v
        except (ValueError, KeyError):
            pass
    return out


def jsonl(path, key="uri"):
    out = {}
    for line in open(path):
        try:
            r = json.loads(line)
            out[r[key]] = r
        except (ValueError, KeyError):
            pass
    return out


def top(d, k=1):
    return sorted((d or {}).items(), key=lambda kv: -kv[1])[:k]


def first(d):
    """Top (name, probability) of a map, or (None, None) when the map is empty or missing."""
    t = top(d)
    return t[0] if t else (None, None)


def iso(v):
    if isinstance(v, (int, float)):
        return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(v))
    return v


def load_posts(uris):
    found = {}
    with open(f"{ROOT}/clef/full_posts.jsonl", "rb") as f:
        for line in f:
            m = URI_AT_START.match(line)
            if m:
                u = json.loads(b'"' + m.group(1) + b'"')
                if u in uris:
                    found[u] = json.loads(line)
    return found


def post_view(uri, posts, fallback_text=None):
    p = posts.get(uri)
    if not p:
        return {"uri": uri, "text": fallback_text or "", "in_export": False}
    return {"uri": uri, "text": p.get("text", ""), "alt_text": p.get("media_alts") or [], "quote": p.get("quote_text") or "",
            "link_card": {k: p.get(f"link_{k}") for k in ("domain", "title", "description") if p.get(f"link_{k}")},
            "picture_hashes": p.get("image_shas") or [], "in_export": True}


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--out", default="/data/judged-sets")
    a = ap.parse_args()
    os.makedirs(f"{a.out}/raw", exist_ok=True)
    rows, rounds = [], collections.OrderedDict()

    def add(rnd, uri, judgment, shown=None, hidden=None, at=None, meta=None):
        rows.append({"round": rnd, "uri": uri, "judged_at": iso(at), "judgment": judgment, "answers_shown": shown, "hidden_from_judge": hidden, **(meta or {})})
        rounds[rnd] = rounds.get(rnd, 0) + 1

    # ---- gold-v1-100
    gold = json.load(open("/home/penguin/bluesky/topic-feed/reference/gold/v1-gold-100.labels-1.json"))
    gold_text = {}
    for l in gold["labels"]:
        gold_text[l["uri"]] = l.get("text", "")
        add("gold-v1-100", l["uri"], {"primary": l["primary"], "acceptable": l["acceptable"], "cant_judge": l["cant_judge"], "note": l["note"]},
            meta={"taxonomy": gold["taxonomy"]})
    shutil.copy("/home/penguin/bluesky/topic-feed/reference/gold/v1-gold-100.labels-1.json", f"{a.out}/raw/gold-v1-100.labels-1.json")

    # ---- jev-v1-vs-clef-flash
    early = last_per_post(f"{ROOT}/clef/verdicts.jsonl")
    clef_first = jsonl(f"{ROOT}/clef/clef-flash-remote.jsonl")
    jev_v1 = {}
    uris = list(early)
    for i in range(0, len(uris), 500):
        lst = ",".join("'" + u.replace("'", "\\'") + "'" for u in uris[i:i + 500])
        out = subprocess.run(CH, input=("SELECT uri, source, broad_probs, path_scores FROM jev_labels FINAL WHERE taxonomy_version = 'v1' "
                                        f"AND jev_model = 'jev-1.13.0' AND uri IN ({lst}) FORMAT JSONEachRow"), capture_output=True, text=True, check=True).stdout
        rank = {"window": 0, "sample": 1, "uncertain": 2}
        for line in out.splitlines():
            r = json.loads(line)
            if r["uri"] not in jev_v1 or rank.get(r["source"], 9) < rank.get(jev_v1[r["uri"]]["source"], 9):
                jev_v1[r["uri"]] = r
    for u, v in early.items():
        j, c = jev_v1.get(u), clef_first.get(u)
        def brief(m, path):
            b, bp = first(m["broad_probs"])
            return {"broad": b, "broad_p": round(bp, 3) if bp is not None else None, "path": path}
        shown = {"jev_v1": (brief(j, first(j["path_scores"])[0]) if j else None),
                 "clef_flash_first_run": (brief(c, c.get("top_path")) if c else None)}
        add("jev-v1-vs-clef-flash", u, {"closer_to_right": v["verdict"]}, shown, at=v.get("at"), meta={"taxonomy": "v1"})
    shutil.copy(f"{ROOT}/clef/verdicts.jsonl", f"{a.out}/raw/jev-v1-vs-clef-flash.verdicts.jsonl")

    # ---- single-answer rounds and changed answers
    def single(rnd, d, model, preds_name="predictions.jsonl", previous=None):
        sample = {p["uri"]: p for p in json.load(open(f"{d}/sample.json"))["posts"]}
        preds = jsonl(f"{d}/{preds_name}")
        for u, v in last_per_post(f"{d}/verdicts.jsonl").items():
            if u not in sample:
                continue
            r, t = preds[u], sample[u]["teacher"]
            shown = {"model": model, "broad_top3": [[k, round(p, 3)] for k, p in top(r["broad"], 3)], "path_top3": [[k, round(p, 3)] for k, p in top(r["paths"], 3)],
                     "meme_probability": r["signals"].get("meme") if r["images_shown"] > 0 and r.get("signals") else None, "pictures_shown_to_model": r["images_shown"]}
            hidden = {"teacher": "Jev" if sample[u]["source"] == "jev" else "Clef-flash", "teacher_broad": top(t["broad"])[0][0] if "broad" in t and isinstance(t["broad"], dict) else t.get("broad"),
                      "teacher_broad_p": round(t.get("broad_p", 0) or (top(t["broad"])[0][1] if isinstance(t.get("broad"), dict) else 0), 3), "teacher_path": t.get("path") or (top(t["paths"])[0][0] if "paths" in t else None)}
            if previous:
                hidden["previous_model_answer"] = previous.get(u)
            add(rnd, u, {"topic": v["topic"], "should_be": v["should_be"], "meme_call": v["meme"] or None, "note": v["note"]}, shown, hidden, at=v.get("at"), meta={"taxonomy": "v2.1"})
        shutil.copytree(d, f"{a.out}/raw/{rnd}", dirs_exist_ok=True, ignore=shutil.ignore_patterns("sample-export"))

    single("single-answer-3-epochs", f"{ROOT}/models/mm1/eval", "first ModernVBERT student, 3 epochs")
    single("single-answer-3-epochs-pictures", f"{ROOT}/models/mm1/eval-pictures", "first ModernVBERT student, 3 epochs")
    d = f"{ROOT}/models/mm2/eval-changed"
    old = json.load(open(f"{d}/old-answers.json"))
    # the changed-answers folder holds no teacher answers of its own: take them from the two first-round samples
    teach = {}
    for src in (f"{ROOT}/models/mm1/eval", f"{ROOT}/models/mm1/eval-pictures"):
        for p in json.load(open(f"{src}/sample.json"))["posts"]:
            teach[p["uri"]] = p
    sample = json.load(open(f"{d}/sample.json"))
    for p in sample["posts"]:
        p["teacher"], p["source"] = teach[p["uri"]]["teacher"], teach[p["uri"]]["source"]
    json.dump(sample, open(f"{a.out}/raw/_changed-sample-with-teacher.json", "w"))
    tmp = f"{a.out}/raw/_changed_tmp"
    os.makedirs(tmp, exist_ok=True)
    json.dump(sample, open(f"{tmp}/sample.json", "w"))
    shutil.copy(f"{d}/predictions.jsonl", f"{tmp}/predictions.jsonl")
    shutil.copy(f"{d}/verdicts.jsonl", f"{tmp}/verdicts.jsonl")
    single("changed-answers-8-epochs", tmp, "ModernVBERT student, 8 epochs", previous=old)
    shutil.rmtree(tmp)
    os.remove(f"{a.out}/raw/_changed-sample-with-teacher.json")
    shutil.copytree(d, f"{a.out}/raw/changed-answers-8-epochs-original", dirs_exist_ok=True)
    # the single() call above wrote the changed folder under its round name too; keep the original files only once
    shutil.rmtree(f"{a.out}/raw/changed-answers-8-epochs", ignore_errors=True)

    # ---- blind side-by-side rounds
    def blind(rnd, d, model):
        sample = {p["uri"]: p for p in json.load(open(f"{d}/sample.json"))["posts"]}
        preds = jsonl(f"{d}/predictions.jsonl")
        for u, v in last_per_post(f"{d}/verdicts.jsonl").items():
            p, r = sample[u], preds[u]
            names = lambda dist, k: [n for n, _ in top(dist, k)]  # noqa: E731
            mine = {"broad_top3": names(r["broad"], 3), "path_top2": names(r["paths"], 2)}
            theirs = {"broad_top3": names(p["teacher"]["broad"], 3), "path_top2": names(p["teacher"]["paths"], 2)}
            sides = dict(zip(("a", "b"), p["order"]))
            better = (sides[v["pick"]] if v["pick"] in ("a", "b") else v["pick"])
            shown = {"a": mine if sides["a"] == "model" else theirs, "b": mine if sides["b"] == "model" else theirs}
            hidden = {"a_was": sides["a"], "b_was": sides["b"], "model": model, "teacher": "Jev" if p["source"] == "jev" else "Clef-flash",
                      "teacher_broad_probabilities": p["teacher"]["broad"], "model_top_broad": p["model_top"]["broad"]}
            add(rnd, u, {"pick": v["pick"], "better": better, "should_be": v["should_be"] or None, "note": v["note"]}, shown, hidden, at=v.get("at"), meta={"taxonomy": "v2.1"})
        shutil.copytree(d, f"{a.out}/raw/{rnd}", dirs_exist_ok=True)

    blind("blind-ab-8-epochs-vs-teacher", f"{ROOT}/models/mm2/eval-disagree", "ModernVBERT student, 8 epochs")
    blind("blind-ab-fusion-vs-clef", f"{ROOT}/models/fusion1/eval-disagree", "fusion model (frozen SigLIP 2 + Ettin-150M)")

    # ---- attach the posts
    uris = {r["uri"] for r in rows}
    posts = load_posts(uris)
    with open(f"{a.out}/judged-sets.jsonl", "w") as f:
        for r in rows:
            fb = gold_text.get(r["uri"]) if r["round"] == "gold-v1-100" else None
            f.write(json.dumps({"post": post_view(r["uri"], posts, fb), **r}, ensure_ascii=False) + "\n")
    summary = {"rows": len(rows), "distinct_posts": len(uris), "by_round": rounds, "posts_found_in_export": sum(1 for u in uris if u in posts)}
    json.dump(summary, open(f"{a.out}/summary.json", "w"), indent=1)
    print(json.dumps(summary, indent=1))


if __name__ == "__main__":
    main()
