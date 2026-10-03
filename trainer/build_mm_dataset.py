"""Builds the training export for the picture-capable student (train_mm.py).

One row per labelled post, from two teachers:

  jev   text and link-card posts, from ClickHouse jev_labels (taxonomy version, label_config below).
        Jev saw no pictures, and these posts have none in the archive.
  clef  posts with an attached picture or video poster, from Clef-flash's result files, calibrated to
        Jev's sharpness (clef_calibrate.apply_calibration). The tone and meme answers are Clef's own.
        Only rows that carry the meme signal are taken, so the part-1 rows (labelled before it was
        saved) never come in; give the redo and the 4090 rows instead.

Every row has the student's text input (the same rendering the deployed model reads; for a picture
post the pipeline's description of the picture is left out, because the model sees the picture),
the picture file hashes (up to --max-images), the time the post was indexed (for a time-based
hold-out), and the targets exactly as common.load builds them. Writes <out>/labels.jsonl.gz and
manifest.json.

    trainer/.venv/bin/python build_mm_dataset.py --out /data/mm/v21 \\
        --clef /data/clef/v2/remote/clef-v21-pictures-part2.jsonl \\
        --clef /data/clef/v2/remote/clef-v21-pictures-redo.jsonl \\
        --clef /data/clef/v2/backfill/clef-v21-backfill.clean.jsonl
"""

import argparse
import collections
import gzip
import json
import subprocess
import sys
import time
from pathlib import Path

import yaml

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import clef_calibrate as cc  # noqa: E402
import clef_labels as cl  # noqa: E402
import clef_run as cr  # noqa: E402  (prepare: a picture replaces the pipeline's description of it)

CH = ["docker", "exec", "-i", "topic-feed-clickhouse", "sh", "-c",
      'clickhouse-client --user topicfeed --password "$CLICKHOUSE_PASSWORD" --database topicfeed']


def ch(query: str) -> str:
    return subprocess.run(CH, input=query, capture_output=True, text=True, check=True).stdout


def read_clef(paths):
    rows, skipped = {}, collections.Counter()
    for p in paths:
        for line in open(p):
            try:
                r = json.loads(line)
            except ValueError:
                skipped["unreadable"] += 1
                continue
            if "meme" not in (r.get("signals") or {}):
                skipped["no meme signal"] += 1
                continue
            rows[r["uri"]] = r
    return rows, skipped


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--out", required=True)
    ap.add_argument("--taxonomy", default=str(HERE.parent / "taxonomy" / "v2.1.yaml"))
    ap.add_argument("--calibration", default="/data/clef/calibration-v2.1.json")
    ap.add_argument("--clef", action="append", required=True, help="Clef result files (JSON lines); repeat")
    ap.add_argument("--jev-version", default="v2.1")
    ap.add_argument("--jev-label-config", default="30cf2c546019")
    ap.add_argument("--posts", default="/data/clef/full_posts.jsonl")
    ap.add_argument("--max-images", type=int, default=4)
    ap.add_argument("--limit-jev", type=int, default=0, help="for a quick test: only this many Jev rows")
    a = ap.parse_args()

    tax = yaml.safe_load(open(a.taxonomy))
    has_sub = {b["id"]: bool(b.get("subtopics")) for b in tax["broad"]}
    cal = json.load(open(a.calibration))
    if cal["taxonomy"] != tax["version"]:
        sys.exit(f"{a.calibration} was fitted under taxonomy {cal['taxonomy']}, not {tax['version']}")

    clef, skipped = read_clef(a.clef)
    print(f"Clef rows with the meme signal: {len(clef)} (skipped {dict(skipped)})", flush=True)

    out = ch("SELECT uri, broad_probs, sub_probs, signals, broad_confidence, label_config FROM jev_labels FINAL "
             f"WHERE taxonomy_version = '{a.jev_version}' AND label_config = '{a.jev_label_config}' FORMAT JSONEachRow")
    jev = {}
    for line in out.splitlines():
        r = json.loads(line)
        if r["uri"] not in clef:
            jev[r["uri"]] = r
    if a.limit_jev:
        jev = dict(list(jev.items())[: a.limit_jev])
    print(f"Jev rows: {len(jev)}", flush=True)

    wanted = set(jev) | set(clef)
    times = {}
    for line in ch("SELECT uri, toUnixTimestamp(indexed_at) FROM posts FINAL WHERE uri IN "
                   "(SELECT uri FROM jev_labels WHERE taxonomy_version IN ('v1', 'v2.1')) FORMAT TSV").splitlines():
        u, t = line.split("\t")
        if u in wanted:
            times[u] = int(t)

    rows = {}
    with open(a.posts) as f:
        for line in f:
            if not line.startswith('{"uri":"'):
                continue
            u = json.loads('"' + line[8:line.index('"', 8)] + '"')
            if u in wanted:
                rows[u] = json.loads(line)
    print(f"posts found in the export: {len(rows)} of {len(wanted)}", flush=True)

    n = collections.Counter()
    Path(a.out).mkdir(parents=True, exist_ok=True)
    with gzip.open(f"{a.out}/labels.jsonl.gz", "wt") as f:
        for source, labelled in (("jev", jev), ("clef", clef)):
            for uri, r in labelled.items():
                row = rows.get(uri)
                if row is None:
                    n[f"{source}: post missing from the export"] += 1
                    continue
                if source == "jev":
                    text = cl.student_string(cl.make_doc(row))
                    shas, rec, conf = [], r, r["broad_confidence"]
                else:
                    used = bool(r.get("used_pictures"))
                    text = cl.student_string(cl.make_doc(cr.prepare(row, [1] if used else [])))
                    shas = (row.get("image_shas") or [])[: a.max_images] if used else []
                    rec = cc.apply_calibration(r, cal, has_sub)
                    conf = max(rec["broad_probs"].values())
                f.write(json.dumps({
                    "uri": uri, "source": source, "model_input": text, "image_shas": shas, "t": times.get(uri, 0),
                    "broad_probs": rec["broad_probs"], "sub_probs": rec["sub_probs"], "signals": rec["signals"],
                    "broad_confidence": conf,
                }, ensure_ascii=False) + "\n")
                n[source] += 1
    manifest = {"created_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "rows": dict(n),
                "taxonomy_version": tax["version"], "calibration": Path(a.calibration).stem,
                "jev_label_config": a.jev_label_config, "clef_files": a.clef, "max_images": a.max_images,
                "postdoc_version": "pd2", "clef_rows_skipped": dict(skipped)}
    json.dump(manifest, open(f"{a.out}/manifest.json", "w"), indent=2)
    print(json.dumps(manifest))


if __name__ == "__main__":
    main()
