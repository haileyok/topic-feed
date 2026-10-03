"""Export every Jev-labelled post, with its pictures, for clef_run.py.

One JSON line per post: the columns of a `posts` row (uri, text, media_alts, link_domain,
link_title, link_description, quote_text, tags, media_kinds), what post_pipeline has (image_texts,
image_text_sources, labels), and the pictures the image archive holds for it:

    image_shas    sha256 of each downloaded picture, in the post's order
    image_kinds   image | video_thumb | link_card, matching image_shas
    priority      0 the post has an attached picture or video poster
                  1 its only picture is a link card's preview
                  2 no picture at all

Rows come out by priority, then in a fixed pseudo-random order, so a run that stops early has
labelled a fair sample of every kind of post. Needs the topic-feed ClickHouse container (docker).

    python3 trainer/clef_export.py --out /data/clef/full_posts.jsonl
"""

import argparse
import json
import subprocess
import sys

QUERY = """
SELECT
    p.uri AS uri, p.text AS text, p.media_alts AS media_alts, p.link_domain AS link_domain,
    p.link_title AS link_title, p.link_description AS link_description, p.quote_text AS quote_text,
    p.tags AS tags, p.media_kinds AS media_kinds,
    pp.image_texts AS image_texts, pp.image_text_sources AS image_text_sources, pp.labels AS labels,
    im.shas AS image_shas, im.kinds AS image_kinds,
    multiIf(has(im.kinds, 'image') OR has(im.kinds, 'video_thumb'), 0, length(im.kinds) > 0, 1, 2) AS priority
FROM posts AS p FINAL
LEFT JOIN (SELECT * FROM post_pipeline FINAL) AS pp ON pp.uri = p.uri
LEFT JOIN
(
    SELECT uri, groupArray(sha256) AS shas, groupArray(kind) AS kinds
    FROM (SELECT uri, idx, sha256, kind FROM post_images FINAL WHERE status = 'ok' ORDER BY uri, idx)
    GROUP BY uri
) AS im ON im.uri = p.uri
WHERE p.uri IN (SELECT uri FROM jev_labels)
ORDER BY priority, cityHash64(p.uri)
FORMAT JSONEachRow
"""


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--out", required=True)
    ap.add_argument("--container", default="topic-feed-clickhouse")
    a = ap.parse_args()

    cmd = ["docker", "exec", "-i", a.container, "sh", "-c",
           'clickhouse-client --user topicfeed --password "$CLICKHOUSE_PASSWORD" --database topicfeed '
           "--max_memory_usage=40000000000 --max_threads=8"]
    n, by_priority = 0, {0: 0, 1: 0, 2: 0}
    with subprocess.Popen(cmd, stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True) as proc, open(a.out, "w") as out:
        proc.stdin.write(QUERY)
        proc.stdin.close()
        for line in proc.stdout:
            row = json.loads(line)
            by_priority[row["priority"]] += 1
            n += 1
            out.write(line)
        if proc.wait() != 0:
            sys.exit(f"clickhouse-client failed (exit {proc.returncode})")
    print(f"{n} posts -> {a.out}: with pictures {by_priority[0]}, link card only {by_priority[1]}, text only {by_priority[2]}")


if __name__ == "__main__":
    main()
