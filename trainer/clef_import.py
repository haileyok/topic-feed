"""Load Clef-flash results into ClickHouse's clef_labels table (schema/012_clef_labels.sql).

Each row holds the model's own answer and, when --calibration is given, the same answer calibrated
to Jev (clef_calibrate.py apply). Rerunning replaces the same rows (same taxonomy, label_config and
uri). The label_config is a hash of the taxonomy version and file, the question set, the post
rendering, the model, and the top-K and minimum broad probability, like Jev's.

    python clef_import.py --results clef-v21-pictures.jsonl --taxonomy ../taxonomy/v2.1.yaml \\
        --calibration calibration-v2.1.json --source pictures
    python clef_import.py --results clef-v21-calib.jsonl --taxonomy ../taxonomy/v2.1.yaml \\
        --calibration calibration-v2.1.json --source calibration

--dry-run prints the first row and the counts without writing.
"""

import argparse
import hashlib
import json
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path

import yaml

sys.path.insert(0, str(Path(__file__).resolve().parent))
import clef_calibrate as cc  # noqa: E402

QUESTIONS_VERSION, POSTDOC_VERSION, MODEL = "q3", "pd2", "clef-flash"
TOP_K, MIN_BROAD_P = 3, 0.05
KIND = {0: "picture", 1: "link_card", 2: "text"}
INSERT = ("clickhouse-client --user topicfeed --password \"$CLICKHOUSE_PASSWORD\" --database topicfeed "
          "--date_time_input_format best_effort -q 'INSERT INTO clef_labels FORMAT JSONEachRow'")


def label_config(taxonomy_path, version):
    taxonomy_hash = hashlib.sha256(Path(taxonomy_path).read_bytes()).hexdigest()[:12]
    s = "|".join([version, taxonomy_hash, QUESTIONS_VERSION, POSTDOC_VERSION, MODEL, f"k{TOP_K}", f"m{MIN_BROAD_P:g}"])
    return hashlib.sha256(s.encode()).hexdigest()[:12]


def row(rec, version, config, source, now, cal, cal_id, has_sub):
    out = {
        "uri": rec["uri"], "taxonomy_version": version, "label_config": config, "clef_model": MODEL,
        "source": source, "post_kind": KIND[rec.get("priority", 0)], "n_images": int(rec.get("images", 0)),
        "used_pictures": int(bool(rec.get("used_pictures"))), "labeled_at": now,
        "input_tokens": int(rec.get("input_tokens", 0)), "seconds": float(rec.get("pass1_s", 0)) + float(rec.get("pass2_s", 0)),
        "broad_probs": rec["broad_probs"], "sub_probs": rec["sub_probs"], "path_scores": rec["path_scores"],
        "signals": rec["signals"], "calibration_id": "", "cal_broad_probs": {}, "cal_sub_probs": {},
        "cal_path_scores": {}, "cal_signals": {},
    }
    if cal:
        c = cc.apply_calibration(rec, cal, has_sub)
        out.update(calibration_id=cal_id, cal_broad_probs=c["broad_probs"], cal_sub_probs=c["sub_probs"],
                   cal_path_scores=c["path_scores"], cal_signals=c["signals"])
    return out


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--results", required=True, help="clef_run.py results, JSON lines")
    ap.add_argument("--taxonomy", required=True, help="the taxonomy file the results were produced under")
    ap.add_argument("--source", required=True, choices=["pictures", "calibration"])
    ap.add_argument("--calibration", help="a clef_calibrate.py fit file; adds the calibrated columns")
    ap.add_argument("--container", default="topic-feed-clickhouse")
    ap.add_argument("--batch", type=int, default=2000)
    ap.add_argument("--dry-run", action="store_true")
    a = ap.parse_args()

    tax = yaml.safe_load(open(a.taxonomy))
    version = tax["version"]
    has_sub = {b["id"]: bool(b.get("subtopics")) for b in tax["broad"]}
    config = label_config(a.taxonomy, version)
    cal, cal_id = None, ""
    if a.calibration:
        cal = json.load(open(a.calibration))
        if cal["taxonomy"] != version:
            sys.exit(f"{a.calibration} was fitted under taxonomy {cal['taxonomy']}, not {version}")
        cal_id = Path(a.calibration).stem
    now = datetime.now(timezone.utc).strftime("%Y-%m-%d %H:%M:%S.%f")[:-3]

    seen, rows, bad = set(), [], 0
    for line in open(a.results):
        try:
            rec = json.loads(line)
            rec["uri"], rec["broad_probs"]
        except (ValueError, KeyError):
            bad += 1  # a line cut off by a copy in progress
            continue
        if rec["uri"] in seen:
            continue
        seen.add(rec["uri"])
        rows.append(row(rec, version, config, a.source, now, cal, cal_id, has_sub))
    print(f"{len(rows)} results from {a.results} (taxonomy {version}, label_config {config}, "
          f"calibration {cal_id or 'none'}, {bad} unreadable lines)")
    if a.dry_run or not rows:
        if rows:
            print(json.dumps(rows[0])[:900])
        return

    cmd = ["docker", "exec", "-i", a.container, "sh", "-c", INSERT]
    for i in range(0, len(rows), a.batch):
        body = "\n".join(json.dumps(r, ensure_ascii=False) for r in rows[i:i + a.batch]) + "\n"
        res = subprocess.run(cmd, input=body, capture_output=True, text=True)
        if res.returncode != 0:
            sys.exit(f"insert failed at row {i}: {res.stderr[-500:]}")
    print(f"inserted {len(rows)} rows into clef_labels")


if __name__ == "__main__":
    main()
