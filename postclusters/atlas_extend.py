"""Give every post a topic and a place, from topics made on a sample of the posts (atlas_pilot.py).

atlas_pilot.py clusters a sample (shards 0-3) with UMAP and HDBSCAN. UMAP and HDBSCAN cannot take all 3.4 million
posts, so the rest are placed by what they are most like. For each post that is not in the sample, the GPU finds its
nearest sampled posts (gpu_knn.py search): its topic is the one most of them (by similarity) are in, and its place on
the map is where they are, spread a little. Posts the sample left unclustered get a topic the same way, from their
own neighbours. Then the topics are grouped into regions (agglomerative clustering of the topic centres, as the
delve atlas does).

Writes data/<out>/ in the shape cluster.py writes (posts.parquet, segments.parquet, seg_edges.parquet,
centroids.npy; segment = topic, area = region) and xy.npy, a place for every post in the order of posts.parquet.

Usage: atlas_extend.py --sample atlas-sample/mcs60 [--shards 0-15] [--out atlas]
"""
import argparse
import subprocess
import time
from pathlib import Path

import numpy as np
import pandas as pd
import scipy.sparse as sp

import cluster
from atlas_pilot import TRAINER_PY, log, regions
from common import DATA, shard_list


def vote(nbr_topic, nbr_sim, ntopics):
    """For each row, the topic with the most total similarity among its neighbours' topics (-1 are ignored), or -1
    where every neighbour is unclustered."""
    n, k = nbr_topic.shape
    out = np.full(n, -1, dtype=np.int64)
    for lo in range(0, n, 400_000):
        t, w = nbr_topic[lo:lo + 400_000], nbr_sim[lo:lo + 400_000]
        rows = np.repeat(np.arange(len(t)), k)
        ok = t.ravel() >= 0
        key = rows[ok].astype(np.int64) * ntopics + t.ravel()[ok]
        uk, inv = np.unique(key, return_inverse=True)
        tot = np.bincount(inv, weights=np.maximum(w.ravel()[ok], 0) + 1e-6)
        r, tp = uk // ntopics, uk % ntopics
        order = np.lexsort((-tot, r))  # by row, then by total, largest first
        first = np.ones(len(order), bool)
        first[1:] = r[order][1:] != r[order][:-1]
        pick = order[first]
        out[lo + r[pick]] = tp[pick]
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--sample', required=True, help='the sample run to extend, under data/ (e.g. atlas-sample/mcs60)')
    ap.add_argument('--shards', default='0-15')
    ap.add_argument('--out', default='atlas')
    ap.add_argument('--vote', type=int, default=15, help='nearest sampled posts that vote for a topic')
    ap.add_argument('--place', type=int, default=5, help='nearest sampled posts that give a post its place')
    ap.add_argument('--regions', type=int, default=0, help='number of regions (0: about 1.4 times the square root of the topics)')
    a = ap.parse_args()
    t0 = time.time()
    sdir = DATA / a.sample
    root = sdir.parent
    out = DATA / a.out
    out.mkdir(parents=True, exist_ok=True)

    sp_posts = pd.read_parquet(sdir / 'posts.parquet')
    sseg = pd.read_parquet(sdir / 'segments.parquet')
    ns, T = len(sp_posts), len(sseg) - 1
    seg_s = sp_posts['segment'].to_numpy().astype(np.int64)
    seg_s[seg_s == T] = -1  # atlas_pilot.py gives the unclustered posts one topic number after the real ones
    xy_s = np.load(root / 'xy2d.npy').astype(np.float32)
    idx_s = np.load(root / 'knn.idx.npy').astype(np.int64)
    sim_s = 1 - np.load(root / 'knn.dist.npy')

    posts, x = cluster.load(shard_list(a.shards))
    n = len(posts)
    if n < ns or not (posts['uri'].to_numpy()[:ns] == sp_posts['uri'].to_numpy()).all():
        raise SystemExit('the sample was made from different posts than these shards start with')
    log(f'{n:,} posts, {ns:,} in the sample, {T} topics ({time.time() - t0:.0f}s)')

    # Every post without a topic (the sample's unclustered ones, and every post outside the sample) gets one from
    # its nearest labelled posts, found on the GPU. A post with a place of its own (a sampled one) keeps it; the others
    # are put where their nearest posts are.
    seg = np.full(n, -1, dtype=np.int64)
    seg[:ns] = seg_s
    xy = np.zeros((n, 2), dtype=np.float32)
    xy[:ns] = xy_s
    labelled = np.flatnonzero(seg >= 0)
    unl = np.flatnonzero(seg < 0)
    tmp = out / 'tmp'
    tmp.mkdir(exist_ok=True)
    np.save(tmp / 'q.npy', x[unl].astype(np.float16))
    np.save(tmp / 'base.npy', x[labelled].astype(np.float16))
    here = Path(__file__).parent
    subprocess.run(['sg', 'render', '-c', f'{TRAINER_PY} {here / "gpu_knn.py"} search {tmp / "q.npy"} {tmp / "base.npy"} {tmp / "nn"} {a.vote}'], check=True)
    idx, sim = labelled[np.load(tmp / 'nn.idx.npy').astype(np.int64)], np.load(tmp / 'nn.sim.npy')
    log(f'nearest labelled posts for {len(unl):,} posts ({time.time() - t0:.0f}s)')
    seg_c = seg.copy()
    seg[unl] = vote(seg[idx], sim, T)
    away = unl[unl >= ns]
    near = xy[idx[unl >= ns][:, :a.place]]
    mid = near.mean(axis=1)
    spread = np.sqrt(((near - mid[:, None, :]) ** 2).sum(-1).mean(axis=1))
    xy[away] = mid + np.random.default_rng(0).normal(size=mid.shape).astype(np.float32) * (0.5 * spread)[:, None]
    del near, mid, spread

    # Anything still without a topic (every neighbour unclustered): the nearest topic centre.
    cent = _centroids(x, seg, T)
    left = np.flatnonzero(seg == -1)
    if len(left):
        seg[left] = (x[left] @ cent.T).argmax(1)
        log(f'{len(left):,} posts had no clustered neighbour and took the nearest topic centre')
    cent = _centroids(x, seg, T)

    # How many of a post's nearest posts are in another topic, and how the topics link.
    edge = np.empty(n, dtype=np.float32)
    nb = idx_s[:, 1:11]
    edges, _ = cluster.segment_links(nb, seg[:ns], T)
    edge[unl] = (seg[idx[:, :10]] != seg[unl, None]).mean(axis=1)
    edge[:ns][seg_c[:ns] >= 0] = np.asarray(seg[idx_s[:, 1:11]][seg_c[:ns] >= 0] != seg[:ns][seg_c[:ns] >= 0, None]).mean(axis=1)
    cos = np.empty(n, dtype=np.float32)
    for lo in range(0, n, 300_000):
        cos[lo:lo + 300_000] = (x[lo:lo + 300_000] * cent[seg[lo:lo + 300_000]]).sum(1)
    posts['segment'], posts['cos'], posts['edge'] = seg, cos, edge
    seg_df = cluster.segment_table(posts, x, cent, seg)
    g = pd.DataFrame({'s': seg, 'x': xy[:, 0], 'y': xy[:, 1]}).groupby('s')
    med = g[['x', 'y']].median().reindex(range(T))
    seg_df['x'], seg_df['y'] = med['x'].to_numpy(), med['y'].to_numpy()
    log(f'topic table ({time.time() - t0:.0f}s)')

    target = a.regions or int(np.clip(round(np.sqrt(T) * 1.4), 3, 80))
    reg = regions(cent, np.arange(T), target)
    area = np.array([reg[s] for s in range(T)])
    order = np.argsort(-np.bincount(area, weights=seg_df['n_texts'].to_numpy(), minlength=area.max() + 1))
    renumber = np.empty(len(order), dtype=int)
    renumber[order] = np.arange(len(order))
    seg_df['area'] = renumber[area]

    posts[['shard', 'row', 'uri', 'segment', 'cos', 'edge']].to_parquet(out / 'posts.parquet')
    seg_df.to_parquet(out / 'segments.parquet')
    edges.to_parquet(out / 'seg_edges.parquet')
    np.save(out / 'centroids.npy', cent)
    np.save(out / 'xy.npy', xy)
    sizes = seg_df['n_texts'].to_numpy()
    log(f'{T} topics (sizes median {int(np.median(sizes))}, p10 {int(np.percentile(sizes, 10))}, p90 {int(np.percentile(sizes, 90))}, max {sizes.max()}), '
        f'{target} regions; {len(unl):,} posts got their topic from neighbours ({time.time() - t0:.0f}s)')


def _centroids(x, seg, T):
    ok = seg >= 0
    m = sp.csr_matrix((np.ones(int(ok.sum()), np.float32), (seg[ok], np.flatnonzero(ok))), shape=(T, len(x)))
    c = np.asarray(m @ x, dtype=np.float32)
    return c / np.maximum(np.linalg.norm(c, axis=1, keepdims=True), 1e-9)


if __name__ == '__main__':
    main()
