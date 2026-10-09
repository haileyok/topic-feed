"""Pull the posts to embed, deduplicated, into hash shards.

For the last --days days of English top-level posts (the `posts` table), writes
data/posts/shard-XX.parquet, one file per shard (a post's shard is a hash of its text, so identical texts
land together and a sample is a set of whole shards). Each row is one distinct text:

  doc          what gets embedded: the post's text, with the title of its link card on a new line when it
               has one (a news post is often only a link), cut to 600 characters
  uri, did, indexed_at   the earliest post with this text
  n_posts      how many posts have exactly this text (bots and templates repeat themselves)
  n_authors    how many distinct accounts posted it
  top_path, top_path_p, top_broad, tone_outraged, sentiment, critical, spam, promo, news
               the existing classifier's reading of that earliest post, kept only to check the clusters
               against (they play no part in making them)

Posts whose document is shorter than 15 characters are left out ("lol", a lone emoji): there is
nothing to embed. Read-only on the database.

Usage: pull.py [--days 5] [--shards 0-3]
"""
import argparse
import time

import pandas as pd

from common import DATA, NSHARDS, clickhouse, shard_list


def query(days: int, shard: int) -> str:
    # Every column of the inner query is prefixed d_, so that the names of the outputs (uri, did,
    # indexed_at ...) can't be taken by ClickHouse for the columns used inside the aggregates.
    return f"""
        WITH d AS (
            SELECT p.uri AS d_uri, p.did AS d_did, p.indexed_at AS d_at,
                   substringUTF8(if(p.link_title != '' AND position(p.text, p.link_title) = 0,
                                    concat(p.text, '\\n', p.link_title), p.text), 1, 600) AS d_doc,
                   pp.top_path AS d_top_path, pp.top_path_p AS d_top_path_p, pp.top_broad AS d_top_broad,
                   pp.tone['outraged'] AS d_outraged, pp.signals['sentiment'] AS d_sentiment,
                   pp.signals['critical'] AS d_critical, pp.signals['spam'] AS d_spam,
                   pp.signals['promo'] AS d_promo, pp.signals['news'] AS d_news
            FROM posts AS p FINAL
            LEFT JOIN (SELECT uri, top_path, top_path_p, top_broad, tone, signals
                       FROM post_pipeline FINAL WHERE indexed_at > now() - INTERVAL {days} DAY AND model != '') AS pp
                   ON pp.uri = p.uri
            WHERE p.indexed_at > now() - INTERVAL {days} DAY
        )
        SELECT argMin(d_uri, d_at) AS uri, argMin(d_did, d_at) AS did, min(d_at) AS indexed_at,
               any(d_doc) AS doc, count() AS n_posts, uniqExact(d_did) AS n_authors,
               argMin(d_top_path, d_at) AS top_path, argMin(d_top_path_p, d_at) AS top_path_p,
               argMin(d_top_broad, d_at) AS top_broad, argMin(d_outraged, d_at) AS tone_outraged,
               argMin(d_sentiment, d_at) AS sentiment, argMin(d_critical, d_at) AS critical,
               argMin(d_spam, d_at) AS spam, argMin(d_promo, d_at) AS promo, argMin(d_news, d_at) AS news
        FROM d
        WHERE lengthUTF8(d_doc) >= 15 AND cityHash64(d_doc) % {NSHARDS} = {shard}
        GROUP BY cityHash64(d_doc)
    """


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--days', type=int, default=5)
    ap.add_argument('--shards', default='0-3', help="shards to pull: '0-3', '0,5' or 'all' (a quarter of the posts is 0-3)")
    a = ap.parse_args()
    out = DATA / 'posts'
    out.mkdir(parents=True, exist_ok=True)
    c = clickhouse()
    for s in shard_list(a.shards):
        t = time.time()
        df = c.query_df(query(a.days, s))
        df['indexed_at'] = pd.to_datetime(df['indexed_at'], utc=True)
        df.to_parquet(out / f'shard-{s:02d}.parquet')
        print(f'shard {s:02d}: {len(df):,} distinct texts, {int(df["n_posts"].sum()):,} posts ({time.time() - t:.0f}s)', flush=True)


if __name__ == '__main__':
    main()
