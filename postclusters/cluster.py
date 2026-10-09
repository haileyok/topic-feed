"""Group the embedded posts into small segments of the embedding space, link them, and group the segments.

Reads data/posts/shard-XX.parquet and data/emb/shard-XX/ for the shards asked for and writes, in data/clusters/:

  posts.parquet      one row per distinct text: shard, row, uri, segment, cos (cosine to its segment's
                     centre), edge (the share of its nearest neighbours that are in another segment)
  segments.parquet   one row per segment: size, how many posts and accounts, cohesion, the existing
                     classifier's subtopics for its posts (a check, not an input), the area it is in, x and y
  seg_edges.parquet  links between segments: how many nearest-neighbour links of posts cross between them
  centroids.npy      the segment centres, unit length

Method:
 1. Segments: spherical k-means on the 768-dimensional embeddings into about N/350 segments, the "little
    segments of the embedding space".
 2. The graph of posts: every post linked to its 10 nearest posts (HNSW, inner product on unit vectors, so
    cosine). Counting the links that cross from one segment to another gives a graph of the segments, and
    the share of a post's links that leave its segment says how clean the segment's edge is.
 3. Areas: Leiden (modularity) on the segment graph, with edge weights the crossing links divided by the
    square root of the two segments' sizes, so large segments don't link to everything.
 4. A 2D layout of the segment centres (UMAP), for the review map.

The existing classifier's subtopics are kept next to the segments only so that the clusters can be checked
against them; nothing about them (or about authors, tone or time) is used to make the clusters.

Usage: cluster.py [--shards 0-3] [--per-segment 350] [--resolution 3.0]
"""
import argparse
import time

import faiss
import igraph as ig
import numpy as np
import pandas as pd

from common import DATA, NON_LATIN, doc_fingerprint, non_latin_share, shard_list


def load(shards):
    posts, embs = [], []
    for s in shards:
        df = pd.read_parquet(DATA / 'posts' / f'shard-{s:02d}.parquet')
        df.insert(0, 'row', np.arange(len(df)))
        df.insert(0, 'shard', s)
        fp_file = DATA / 'emb' / f'shard-{s:02d}' / 'docs.sha1'
        if not fp_file.exists() or fp_file.read_text().strip() != doc_fingerprint(df['doc']):
            raise SystemExit(f'shard {s}: the posts do not match what was embedded (no fingerprint, or the shard was pulled again)')
        files = sorted((DATA / 'emb' / f'shard-{s:02d}').glob('chunk-*.npy'))
        e = np.concatenate([np.load(f) for f in files]) if files else np.zeros((0, 768), np.float16)
        if len(e) != len(df):
            raise SystemExit(f'shard {s}: {len(df):,} posts but {len(e):,} embeddings; finish embed.py for it first')
        posts.append(df)
        embs.append(e)
    posts = pd.concat(posts, ignore_index=True)
    x = np.concatenate(embs).astype(np.float32)
    x /= np.maximum(np.linalg.norm(x, axis=1, keepdims=True), 1e-9)
    # Posts mostly in a non-Latin script are not English; they would form segments of their own by language.
    keep = (posts['doc'].map(non_latin_share) < 0.3).to_numpy()
    if not keep.all():
        print(f'leaving out {int((~keep).sum()):,} posts written mostly in a non-Latin script', flush=True)
    return posts[keep].reset_index(drop=True), np.ascontiguousarray(x[keep])


def segment(x, k, seed=1, niter=20):
    """Spherical k-means; returns (centroids, assignment, cosine of each point to its centroid)."""
    km = faiss.Kmeans(x.shape[1], k, niter=niter, spherical=True, seed=seed, verbose=False, max_points_per_centroid=400)
    km.train(x)
    c = km.centroids / np.maximum(np.linalg.norm(km.centroids, axis=1, keepdims=True), 1e-9)
    index = faiss.IndexFlatIP(x.shape[1])
    index.add(c)
    sim, lab = index.search(x, 1)
    return c.astype(np.float32), lab[:, 0], sim[:, 0]


def neighbour_graph(x, k=10, m=32, ef_construction=80, ef_search=96):
    """Each point's k nearest other points (indices) by cosine, with HNSW."""
    index = faiss.IndexHNSWFlat(x.shape[1], m, faiss.METRIC_INNER_PRODUCT)
    index.hnsw.efConstruction = ef_construction
    index.add(x)
    index.hnsw.efSearch = ef_search
    _, nn = index.search(x, k + 1)
    # The point itself is normally first; where duplicates tie, drop it wherever it is.
    out = np.empty((len(x), k), dtype=np.int64)
    for i in range(len(x)):
        row = nn[i][nn[i] != i]
        out[i] = row[:k] if len(row) >= k else np.pad(row, (0, k - len(row)), constant_values=row[-1] if len(row) else i)
    return out


def segment_links(nn, seg, nseg):
    """Count the nearest-neighbour links between segments, and the share of each point's links that leave
    its segment. Returns (edges: DataFrame a, b, n with a < b; edge_share per point)."""
    a = np.repeat(seg, nn.shape[1])
    b = seg[nn.ravel()]
    cross = a != b
    edge_share = cross.reshape(nn.shape).mean(axis=1)
    lo, hi = np.minimum(a[cross], b[cross]), np.maximum(a[cross], b[cross])
    key = lo.astype(np.int64) * nseg + hi
    uk, cnt = np.unique(key, return_counts=True)
    return pd.DataFrame({'a': uk // nseg, 'b': uk % nseg, 'n': cnt}), edge_share


def areas(edges, sizes, resolution=1.0, seed=1):
    """Leiden on the segment graph; weights are crossing links over sqrt(size_a * size_b). Returns the area of
    each segment, numbered from the one with the most posts."""
    w = edges['n'].to_numpy() / np.sqrt(sizes[edges['a'].to_numpy()] * sizes[edges['b'].to_numpy()])
    g = ig.Graph(n=len(sizes), edges=list(zip(edges['a'].tolist(), edges['b'].tolist())))
    g.es['weight'] = w.tolist()
    import random
    random.seed(seed)
    part = g.community_leiden(objective_function='modularity', weights='weight', resolution=resolution, n_iterations=-1)
    mem = np.array(part.membership)
    order = np.argsort(-np.bincount(mem, weights=sizes, minlength=mem.max() + 1))
    renumber = np.empty(len(order), dtype=int)
    renumber[order] = np.arange(len(order))
    return renumber[mem]


def segment_table(posts, x, centroids, seg):
    """Per-segment numbers, including how the existing classifier's subtopics fall in it (a check)."""
    rows = []
    for s in range(len(centroids)):
        m = np.flatnonzero(seg == s)
        p = posts.iloc[m]
        tp = p['top_path'].value_counts(normalize=True)
        weight = p['n_posts'].to_numpy()
        rows.append({
            'segment': s, 'n_texts': len(m), 'n_posts': int(weight.sum()),
            'n_authors': int(p['did'].nunique()), 'top_author_share': float(p['did'].value_counts(normalize=True).iloc[0]) if len(m) else 0.0,
            'repeat_share': float(1 - len(m) / max(weight.sum(), 1)),
            'cohesion': float(p['cos'].mean()),
            'classifier_top': tp.index[0] if len(tp) else '', 'classifier_top_share': float(tp.iloc[0]) if len(tp) else 0.0,
            'classifier_top3': '; '.join(f'{k} {v:.0%}' for k, v in tp.head(3).items()),
            'outraged': float(p['tone_outraged'].mean()), 'sentiment': float(p['sentiment'].mean()),
            'critical': float(p['critical'].mean()), 'spam': float(p['spam'].mean()), 'promo': float(p['promo'].mean()),
            'news': float(p['news'].mean()),
            'edge_share': float(p['edge'].mean()),
        })
    return pd.DataFrame(rows)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--shards', default='0-3')
    ap.add_argument('--per-segment', type=int, default=350, help='about this many distinct texts in a segment')
    ap.add_argument('--resolution', type=float, default=3.0, help='Leiden resolution for grouping segments into areas')
    ap.add_argument('--neighbours', type=int, default=10)
    a = ap.parse_args()
    t0 = time.time()
    out = DATA / 'clusters'
    out.mkdir(parents=True, exist_ok=True)

    posts, x = load(shard_list(a.shards))
    k = max(8, round(len(x) / a.per_segment))
    print(f'{len(x):,} distinct texts, {int(posts.n_posts.sum()):,} posts, {k} segments wanted ({time.time() - t0:.0f}s)', flush=True)

    centroids, seg, cos = segment(x, k)
    print(f'segments ({time.time() - t0:.0f}s)', flush=True)
    nn = neighbour_graph(x, a.neighbours)
    print(f'neighbour graph ({time.time() - t0:.0f}s)', flush=True)
    edges, edge_share = segment_links(nn, seg, k)
    posts['segment'], posts['cos'], posts['edge'] = seg, cos, edge_share
    seg_df = segment_table(posts, x, centroids, seg)
    seg_df['area'] = areas(edges, seg_df['n_texts'].to_numpy(dtype=float), a.resolution)
    print(f'{seg_df.area.nunique()} areas ({time.time() - t0:.0f}s)', flush=True)

    import umap
    xy = umap.UMAP(n_components=2, n_neighbors=15, min_dist=0.1, metric='cosine', random_state=1).fit_transform(centroids)
    seg_df['x'], seg_df['y'] = xy[:, 0], xy[:, 1]

    posts[['shard', 'row', 'uri', 'segment', 'cos', 'edge']].to_parquet(out / 'posts.parquet')
    seg_df.to_parquet(out / 'segments.parquet')
    edges.to_parquet(out / 'seg_edges.parquet')
    np.save(out / 'centroids.npy', centroids)
    print(f'segment sizes: median {int(seg_df.n_texts.median())}, min {seg_df.n_texts.min()}, max {seg_df.n_texts.max()}; '
          f'cohesion median {seg_df.cohesion.median():.3f}; done ({time.time() - t0:.0f}s)', flush=True)


if __name__ == '__main__':
    main()
