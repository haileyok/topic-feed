"""Make the k-means segments (cluster.py's way) for the same posts as atlas_pilot.py, about as many and as big as its
topics, and compare the two on what the existing classifier says (not used by either): the share of posts that have
their cluster's most common subtopic. Writes data/pilot/kmeans<k>/ in the same shape, so label.py can rate both.

Usage: pilot_compare.py [--shards 0] [--like mcs60]
"""
import argparse

import numpy as np
import pandas as pd

import cluster
from atlas_pilot import log, purity
from common import DATA, shard_list


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--shards', default='0')
    ap.add_argument('--like', default='mcs60')
    a = ap.parse_args()
    posts, x = cluster.load(shard_list(a.shards))
    ref = pd.read_parquet(DATA / 'pilot' / a.like / 'posts.parquet')
    k = int(ref['segment'].nunique()) - 1  # the unclustered posts are one extra topic
    log(f'{len(posts):,} posts; k-means with k={k} to match {a.like}')
    cent, seg, cos = cluster.segment(x, k)
    sub = posts['top_path'].fillna('?').to_numpy()
    d = DATA / 'pilot' / f'kmeans{k}'
    d.mkdir(exist_ok=True)
    out = posts[['shard', 'row', 'uri']].copy()
    out['segment'], out['cos'], out['edge'] = seg, cos, 0.0
    out.to_parquet(d / 'posts.parquet')
    g = posts.assign(segment=seg).groupby('segment')
    pd.DataFrame({'segment': np.arange(k), 'n_texts': g.size().reindex(range(k)).fillna(0).astype(int).to_numpy(), 'area': 0}).to_parquet(d / 'segments.parquet')
    sizes = np.bincount(seg, minlength=k)
    print(f'kmeans k={k}: sizes median {int(np.median(sizes))} p10 {int(np.percentile(sizes, 10))} p90 {int(np.percentile(sizes, 90))} max {sizes.max()}')
    print(f'purity vs classifier subtopics: kmeans {purity(seg, sub):.3f}, {a.like} {purity(ref["segment"].to_numpy(), sub):.3f} '
          f'(the unclustered posts counted as a topic of their own in both)')
    rc = ref['cos'].to_numpy()
    print(f'cosine of a post to its cluster centre: kmeans median {np.median(cos):.3f} p10 {np.percentile(cos, 10):.3f}; {a.like} median {np.median(rc):.3f} p10 {np.percentile(rc, 10):.3f}')


if __name__ == '__main__':
    main()
