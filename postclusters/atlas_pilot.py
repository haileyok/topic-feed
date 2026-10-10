"""A trial of the delve atlas's way of making topics, on the embeddings we already have.

The delve atlas (~/bluesky/delve-atlas/pipeline/build_atlas.py) does: UMAP to 2D for the layout, UMAP to 12D for
clustering, HDBSCAN on the 12D points, noise points attached to a nearby topic, topics grouped into regions by
agglomerative clustering of their centres, an LLM to title each. This does the same on the posts of some shards,
with the nearest-neighbour search (the slow part of UMAP) done exactly on the GPU (gpu_knn.py) and handed to UMAP.

Writes data/<name>/<mcs>/posts.parquet and segments.parquet in the shape cluster.py writes (segment = topic,
area = region), so `label.py --dir pilot/<mcs>` can title them. Prints how many topics there are, how much was left
unclustered, and how well the topics agree with the existing classifier's subtopics (not used to make them).

Usage: atlas_pilot.py [--shards 0] [--mcs 60,150,300] [--name pilot]
"""
import argparse
import subprocess
import time
from pathlib import Path

import numpy as np
import pandas as pd

import cluster
from common import DATA, shard_list

TRAINER_PY = Path.home() / 'bluesky/topic-feed/trainer/.venv/bin/python'


def log(*a):
    print(time.strftime('%H:%M:%S'), *a, flush=True)


def gpu_knn(x, prefix, k):
    np.save(f'{prefix}.in.npy', x.astype(np.float16))
    here = Path(__file__).parent
    subprocess.run(['sg', 'render', '-c', f'{TRAINER_PY} {here / "gpu_knn.py"} {prefix}.in.npy {prefix} {k}'], check=True)
    return np.load(f'{prefix}.idx.npy'), np.load(f'{prefix}.dist.npy')


def umap_from_knn(idx, dist, n_components, n_neighbors, min_dist, seed=42):
    import umap
    return umap.UMAP(n_components=n_components, n_neighbors=n_neighbors, min_dist=min_dist, metric='cosine', random_state=seed,
                     precomputed_knn=(idx[:, :n_neighbors], dist[:, :n_neighbors], None)).fit_transform(np.zeros((len(idx), 1), dtype=np.float32))


def hdbscan_topics(z, mcs, attach):
    """As build_atlas.cluster(): HDBSCAN (excess of mass), then noise within `attach` 90th-percentile radii of a
    topic's centre joins that topic."""
    from sklearn.cluster import HDBSCAN
    labels = HDBSCAN(min_cluster_size=mcs, min_samples=max(5, mcs // 8), cluster_selection_method='eom', copy=True).fit_predict(z)
    raw_noise = int((labels == -1).sum())
    ids = sorted(set(labels) - {-1})
    cent = np.stack([z[labels == i].mean(0) for i in ids])
    limit = np.array([np.percentile(np.linalg.norm(z[labels == i] - cent[k], axis=1), 90) * attach for k, i in enumerate(ids)])
    out = labels.copy()
    noise = np.where(labels == -1)[0]
    for lo in range(0, len(noise), 4000):
        part = noise[lo:lo + 4000]
        dd = np.linalg.norm(z[part][:, None, :] - cent[None], axis=2)
        best = dd.argmin(1)
        ok = dd[np.arange(len(part)), best] <= limit[best]
        out[part[ok]] = np.array(ids)[best[ok]]
    return out, raw_noise


def regions(z, topic_of, target):
    from sklearn.cluster import AgglomerativeClustering
    tids = sorted(set(topic_of) - {-1})
    cent = np.stack([z[topic_of == t].mean(0) for t in tids])
    lab = AgglomerativeClustering(n_clusters=min(target, len(tids)), linkage='ward').fit_predict(cent)
    return dict(zip(tids, lab.tolist()))


def purity(topic_of, subtopic):
    """Share of posts whose topic's most common classifier subtopic is their own (over the clustered posts)."""
    df = pd.DataFrame({'t': topic_of, 's': subtopic})
    df = df[df.t >= 0]
    return float(df.groupby(['t', 's']).size().groupby('t').max().sum() / len(df))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--shards', default='0')
    ap.add_argument('--mcs', default='60,150,300')
    ap.add_argument('--subsample', type=int, default=0, help='run HDBSCAN on this many random posts only (it is slow on many); the others are left unclustered, for atlas_extend.py to place')
    ap.add_argument('--name', default='pilot', help='the folder under data/ to write to')
    ap.add_argument('--attach', type=float, default=4.0)
    ap.add_argument('--fresh', action='store_true', help='redo the neighbour search and the UMAPs even if saved')
    a = ap.parse_args()
    out = DATA / a.name
    out.mkdir(exist_ok=True)
    t0 = time.time()
    posts, x = cluster.load(shard_list(a.shards))
    n = len(posts)
    log(f'{n:,} posts')

    if a.fresh or not (out / 'z12.npy').exists():
        idx, dist = gpu_knn(x, str(out / 'knn'), 25)
        log(f'exact neighbours on the GPU ({time.time() - t0:.0f}s)')
        np.save(out / 'xy2d.npy', umap_from_knn(idx, dist, 2, 25, 0.08))
        log(f'umap 2d ({time.time() - t0:.0f}s)')
        np.save(out / 'z12.npy', umap_from_knn(idx, dist, 12, 20, 0.0))
        log(f'umap 12d ({time.time() - t0:.0f}s)')
    xy, z = np.load(out / 'xy2d.npy'), np.load(out / 'z12.npy')
    sub = posts['top_path'].fillna('?').to_numpy()

    for mcs in [int(m) for m in a.mcs.split(',')]:
        if a.subsample and a.subsample < n:
            rows = np.sort(np.random.default_rng(0).choice(n, a.subsample, replace=False))
            topic_of = np.full(n, -1)
            topic_of[rows], raw_noise = hdbscan_topics(z[rows], mcs, a.attach)
        else:
            topic_of, raw_noise = hdbscan_topics(z, mcs, a.attach)
        ids = sorted(set(topic_of) - {-1})
        remap = {t: i for i, t in enumerate(ids)}
        seg = np.array([remap.get(int(t), -1) for t in topic_of])
        # the posts left unclustered form one more topic, so that nothing is dropped from the map
        seg[seg == -1] = len(ids)
        k = len(ids) + 1
        sizes = np.bincount(seg[seg < len(ids)], minlength=len(ids))
        target = int(np.clip(round(np.sqrt(len(ids)) * 1.4), 3, 60))
        reg = regions(z, np.where(seg < len(ids), seg, -1), target)
        cent = np.zeros((k, x.shape[1]), np.float32)
        np.add.at(cent, seg, x)
        cent /= np.maximum(np.linalg.norm(cent, axis=1, keepdims=True), 1e-9)
        cos = (x * cent[seg]).sum(1)
        d = out / (f'sub{a.subsample}-mcs{mcs}' if a.subsample else f'mcs{mcs}')
        d.mkdir(exist_ok=True)
        posts_out = posts[['shard', 'row', 'uri']].copy()
        posts_out['segment'], posts_out['cos'], posts_out['edge'] = seg, cos, 0.0
        posts_out.to_parquet(d / 'posts.parquet')
        g = posts.assign(segment=seg, x=xy[:, 0], y=xy[:, 1]).groupby('segment')
        seg_df = pd.DataFrame({'segment': np.arange(k), 'n_texts': g.size().reindex(range(k)).to_numpy(), 'n_posts': g['n_posts'].sum().reindex(range(k)).to_numpy(),
                               'x': g['x'].mean().reindex(range(k)).to_numpy(), 'y': g['y'].mean().reindex(range(k)).to_numpy()})
        seg_df['area'] = [reg.get(s, len(set(reg.values()))) for s in range(k)]
        seg_df.to_parquet(d / 'segments.parquet')
        log(f'mcs={mcs}: {len(ids)} topics (+1 for the {int((seg == len(ids)).sum()):,} unclustered), {raw_noise:,} noise before attaching, '
            f'sizes median {int(np.median(sizes))} p10 {int(np.percentile(sizes, 10))} p90 {int(np.percentile(sizes, 90))} max {sizes.max()}, '
            f'{target} regions, purity vs classifier subtopics {purity(seg, sub):.3f} ({time.time() - t0:.0f}s)')


if __name__ == '__main__':
    main()
