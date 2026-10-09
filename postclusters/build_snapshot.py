"""Package the clusters as a snapshot for the postmap service: a map with every post on it.

Reads what pull.py, embed.py, cluster.py and label.py wrote, and writes data/snapshot/<UTC time>/ (and repoints
data/snapshot/latest):

  map.json        the areas and segments: titles, descriptions, kinds, coherence, tone, flags, positions,
                  example posts, the segments each is linked to, and the headline numbers
  xy.f32          2 float32 per post (x, y); post i is at offset 8*i
  seg.u16         the segment of each post
  outraged.u8, sentiment.u8, critical.u8
                  0-255: the existing classifier's reading of each post (a segment-wide colouring, not an input)
  posts.jsonl     one JSON object per post, in the same order: i, u (at:// URI), t (text), n (posts with this
                  exact text), a (accounts that posted it), at (when, epoch seconds), p (classifier subtopic), s (segment)
  posts.off       uint64 byte offset of each line of posts.jsonl, little endian, plus a final end offset
  texts.txt       each post's text, lowercased with whitespace flattened, one per line, for text search

Where posts go. cluster.py's UMAP of the segment centres says roughly where each segment belongs, but it leaves tight
clumps and wide gaps. So each segment gets a disc whose area follows its number of posts, and the discs are pushed
apart until they hardly overlap, each held near its UMAP position by a weak spring (pack()). Inside its disc a post
is placed by the first two principal components of the embeddings of the segment's posts. Distances between
segments are as meaningful as that UMAP, and no more; distances inside a segment mean that the posts are alike
along the directions that vary most among them.

Usage: build_snapshot.py [--shards 0-3] [--models haiku]
"""
import argparse
import json
import time

import numpy as np
import pandas as pd
from scipy.spatial import ConvexHull, cKDTree

import cluster
import report
from common import DATA, shard_list
from label import pick_representatives


def unit_scale(xy, pct=99.5):
    """Centre on the median and scale so the `pct`-th percentile of |coordinate| is 1."""
    xy = xy - np.median(xy, axis=0)
    return xy / max(float(np.percentile(np.abs(xy), pct)), 1e-9)


def pack(home, weight, cover=0.45, iters=6000, seed=0):
    """Give each segment a disc whose area follows its size and push the discs apart until they no longer overlap,
    while a weak spring keeps each near `home`, the position the UMAP gave it. Returns (centres, radii).

    The UMAP of segment centres leaves tight clumps and wide gaps, which would draw as many small islands in a lot
    of empty space. Here the discs together cover `cover` of the convex hull of the UMAP positions, so the map
    reads as one body of posts with the segments in it sized by how many posts they hold."""
    rng = np.random.default_rng(seed)
    home = np.asarray(home, dtype=np.float64)
    w = np.asarray(weight, dtype=np.float64)
    r = np.sqrt(w)
    r *= np.sqrt(cover * ConvexHull(home).volume / (np.pi * np.sum(r * r)))
    p = home + rng.normal(scale=1e-4, size=home.shape)
    rmax = float(r.max())
    for it in range(iters):
        pairs = cKDTree(p).query_pairs(2 * rmax, output_type='ndarray')
        if len(pairs):
            i, j = pairs[:, 0], pairs[:, 1]
            d = p[j] - p[i]
            dist = np.hypot(d[:, 0], d[:, 1])
            over = r[i] + r[j] - dist
            hit = over > 0
            i, j, d, dist, over = i[hit], j[hit], d[hit], dist[hit], over[hit]
            u = d / np.maximum(dist, 1e-12)[:, None]
            step = 0.5 * 0.8 * over[:, None] * u
            move = np.zeros_like(p)
            np.add.at(move, i, -step)
            np.add.at(move, j, step)
            p += move
        if it < iters - 500:
            p += 0.002 * (home - p)  # the last iterations only separate
    return p, r


def within_layout(emb, rng=None):
    """2D positions for the posts of one segment, centred on 0 with the 90th percentile of the distance from the
    centre equal to 1. The first two principal components of the embeddings; a segment of fewer than 3 posts is
    spread on a small circle."""
    n = len(emb)
    rng = rng or np.random.default_rng(0)
    if n < 3:
        ang = np.arange(n) * 2 * np.pi / max(n, 1)
        return 0.3 * np.stack([np.cos(ang), np.sin(ang)], axis=1)
    c = emb - emb.mean(axis=0)
    _, _, vt = np.linalg.svd(c, full_matrices=False)
    xy = c @ vt[:2].T
    r = float(np.percentile(np.hypot(xy[:, 0], xy[:, 1]), 90))
    xy = xy / max(r, 1e-9)
    return xy + rng.normal(scale=0.02, size=xy.shape)  # break exact ties between identical vectors


def to_u8(values, default=0):
    v = np.nan_to_num(np.asarray(values, dtype=np.float64), nan=default / 255.0)
    return np.clip(v * 255, 0, 255).astype(np.uint8)


def text_line(t):
    """One line of texts.txt: lowercased, every run of whitespace a single space."""
    return ' '.join(str(t).split()).lower()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--shards', default='0-3')
    ap.add_argument('--models', default='haiku')
    ap.add_argument('--examples', type=int, default=12)
    a = ap.parse_args()
    t0 = time.time()
    out = DATA / 'clusters'

    posts, x = cluster.load(shard_list(a.shards))
    cl = pd.read_parquet(out / 'posts.parquet')
    if len(cl) != len(posts) or not (cl['uri'].to_numpy() == posts['uri'].to_numpy()).all():
        raise SystemExit('the clusters were made from different posts than these shards give: run cluster.py again')
    seg = cl['segment'].to_numpy()
    posts['segment'], posts['cos'] = seg, cl['cos'].to_numpy()
    segs = pd.read_parquet(out / 'segments.parquet').set_index('segment')
    edges = pd.read_parquet(out / 'seg_edges.parquet')
    nseg = len(segs)
    labels = {m: report.read_labels(m) for m in a.models.split(',')}
    area_labels = {}
    for m in labels:
        p = out / f'areas-{m}.json'
        if p.exists():
            area_labels.update({int(k): v for k, v in json.loads(p.read_text()).items()})
    lab = next(iter(labels.values()))
    print(f'{len(posts):,} posts, {nseg} segments, {len(lab)} labels ({time.time() - t0:.0f}s)', flush=True)

    # Positions.
    cxy, radius = pack(unit_scale(segs[['x', 'y']].to_numpy()), segs['n_texts'].to_numpy())
    radius = 0.95 * radius  # a segment's 90th-percentile post lands just inside its disc; the tails touch the neighbours
    order = np.argsort(seg, kind='stable')
    bounds = np.searchsorted(seg[order], np.arange(nseg + 1))
    xy = np.zeros((len(posts), 2), dtype=np.float32)
    rng = np.random.default_rng(2)
    for s in range(nseg):
        m = order[bounds[s]:bounds[s + 1]]
        xy[m] = cxy[s] + within_layout(x[m], rng) * radius[s]
    print(f'positions ({time.time() - t0:.0f}s)', flush=True)

    # Segments and areas.
    tmpl = report.template_shares(posts)
    nb = {}
    w = edges['n'].to_numpy() / np.sqrt(segs['n_texts'].to_numpy()[edges['a'].to_numpy()] * segs['n_texts'].to_numpy()[edges['b'].to_numpy()])
    for ai, bi, wi in zip(edges['a'], edges['b'], w):
        nb.setdefault(int(ai), []).append((int(bi), float(wi)))
        nb.setdefault(int(bi), []).append((int(ai), float(wi)))
    segments = []
    for s in range(nseg):
        row = segs.loc[s]
        m = order[bounds[s]:bounds[s + 1]]
        rep = pick_representatives(posts['cos'].to_numpy()[m], posts['n_authors'].to_numpy()[m], posts['doc'].iloc[m].tolist(), a.examples - 6, 3, 3, seed=s)
        seg_row = row.copy()
        seg_row['segment'] = s
        seg_row['template_share'] = float(tmpl.get(s, 0.0))
        labs = {mname: d[s] for mname, d in labels.items() if s in d}
        first = next(iter(labs.values()), None)
        top = sorted(nb.get(s, []), key=lambda t: -t[1])[:8]
        segments.append({
            'id': s, 'area': int(row['area']),
            'title': first['title'] if first else f'segment {s}', 'description': first['description'] if first else '',
            'kind': first['kind'] if first else 'other', 'coherence': first['coherence'] if first else 0,
            'common_thread': first['common_thread'] if first else '', 'disagreement': first['disagreement'] if first else [],
            'n_texts': int(row['n_texts']), 'n_posts': int(row['n_posts']), 'n_authors': int(row['n_authors']),
            'top_author_share': round(float(row['top_author_share']), 3), 'repeat_share': round(float(row['repeat_share']), 3),
            'template_share': round(float(seg_row['template_share']), 3), 'classifier_top3': row['classifier_top3'],
            'outraged': round(float(row['outraged']), 3), 'sentiment': round(float(row['sentiment']), 3), 'critical': round(float(row['critical']), 3),
            'spam': round(float(row['spam']), 3), 'promo': round(float(row['promo']), 3), 'news': round(float(row['news']), 3),
            'x': round(float(cxy[s, 0]), 4), 'y': round(float(cxy[s, 1]), 4), 'r': round(float(radius[s]), 4),
            'flags': report.flags(seg_row, list(labs.values())),
            'examples': [[int(m[i]), g] for i, g in rep],
            'neighbours': [[int(b), round(wt, 3)] for b, wt in top],
        })
    areas = []
    for ar, g in segs.groupby('area'):
        ids = g.index.to_numpy()
        wts = g['n_texts'].to_numpy(dtype=float)
        c = (cxy[ids] * wts[:, None]).sum(axis=0) / wts.sum()
        al = area_labels.get(int(ar), {})
        areas.append({'id': int(ar), 'title': al.get('title', f'area {ar}'), 'description': al.get('description', ''), 'coherence': al.get('coherence', 0),
                      'x': round(float(c[0]), 4), 'y': round(float(c[1]), 4), 'r': round(float(np.percentile(np.hypot(*(cxy[ids] - c).T), 80)) + 0.02, 4),
                      'segments': int(len(ids)), 'texts': int(wts.sum()), 'posts': int(g['n_posts'].sum())})
    print(f'segments and areas ({time.time() - t0:.0f}s)', flush=True)

    # Files.
    stamp = time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())
    snap = DATA / 'snapshot' / stamp
    snap.mkdir(parents=True, exist_ok=True)
    (snap / 'xy.f32').write_bytes(xy.tobytes())
    (snap / 'seg.u16').write_bytes(seg.astype(np.uint16).tobytes())
    for name, col in (('outraged', 'tone_outraged'), ('sentiment', 'sentiment'), ('critical', 'critical')):
        (snap / f'{name}.u8').write_bytes(to_u8(posts[col].to_numpy(), default=128 if name == 'sentiment' else 0).tobytes())
    offsets = np.zeros(len(posts) + 1, dtype=np.uint64)
    pos = 0
    at = ((posts['indexed_at'] - pd.Timestamp(0, tz='UTC')) // pd.Timedelta(seconds=1)).to_numpy().astype('int64')  # epoch seconds, whatever the datetime resolution
    docs, uris, nps, nas, tps = posts['doc'].tolist(), posts['uri'].tolist(), posts['n_posts'].tolist(), posts['n_authors'].tolist(), posts['top_path'].tolist()
    with open(snap / 'posts.jsonl', 'wb') as f, open(snap / 'texts.txt', 'w', encoding='utf-8') as ft:
        for i in range(len(posts)):
            line = (json.dumps({'i': i, 'u': uris[i], 't': docs[i], 'n': nps[i], 'a': nas[i], 'at': int(at[i]), 'p': tps[i], 's': int(seg[i])},
                               separators=(',', ':'), ensure_ascii=False) + '\n').encode()
            offsets[i] = pos
            pos += len(line)
            f.write(line)
            ft.write(text_line(docs[i]) + '\n')
        offsets[len(posts)] = pos
    (snap / 'posts.off').write_bytes(offsets.tobytes())
    meta = {
        'id': stamp, 'built_at': int(time.time()), 'models': list(labels), 'n_posts': len(posts), 'n_posts_total': int(posts['n_posts'].sum()),
        'n_segments': nseg, 'n_areas': len(areas), 'from': int(at.min()), 'to': int(at.max()),
        'areas': areas, 'segments': segments,
    }
    (snap / 'map.json').write_text(json.dumps(meta, separators=(',', ':')))
    latest = snap.parent / 'latest'
    if latest.is_symlink() or latest.exists():
        latest.unlink()
    latest.symlink_to(stamp)
    print(f'wrote {snap} ({time.time() - t0:.0f}s)', flush=True)


if __name__ == '__main__':
    main()
