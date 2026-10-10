"""A page to read the pilot's topics and the k-means segments side by side: for each labelled cluster, the title and
description Haiku 5.5 gave it, how coherent it rated it, and the posts it was shown.

Usage: pilot_sample.py [--out /data/reports/post-atlas-pilot] [--model haiku55] [--n 40]
The page holds the text of public posts: it goes where the internal-network report viewer serves it from.
"""
import argparse
import html
import json
import shutil
from pathlib import Path

import pandas as pd

from common import DATA


def load(dirname, model):
    labs = [json.loads(l) for l in (DATA / 'pilot' / dirname / f'labels-{model}.jsonl').open() if l.strip()]
    segs = pd.read_parquet(DATA / 'pilot' / dirname / 'segments.parquet').set_index('segment')
    return labs, segs


def section(title, intro, labs, segs, docs, n):
    out = [f'<h2>{html.escape(title)}</h2><p class="muted">{html.escape(intro)}</p>']
    for r in labs[:n]:
        size = int(segs.loc[r['segment'], 'n_texts'])
        posts = ''.join(f'<li>{html.escape(" ".join(docs.get(u, "(not found)").split())[:300])}</li>' for u in r['shown'][:10])
        dis = f'<div class="muted">posts disagree: {html.escape(" / ".join(r["disagreement"]))}</div>' if r['disagreement'] else ''
        out.append(f'<div class="card c{r["coherence"]}"><div class="head"><b>{html.escape(r["title"])}</b>'
                   f'<span class="chip">coherence {r["coherence"]}/5</span><span class="chip">{html.escape(r["kind"].replace("_", " "))}</span>'
                   f'<span class="chip">{size:,} posts</span></div><div>{html.escape(r["description"])}</div>{dis}'
                   f'<details><summary>posts it was shown ({min(len(r["shown"]), 10)})</summary><ul>{posts}</ul></details></div>')
    return '\n'.join(out)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--out', default='/data/reports/post-atlas-pilot')
    ap.add_argument('--model', default='haiku55')
    ap.add_argument('--n', type=int, default=40)
    ap.add_argument('--hdbscan', default='mcs60')
    ap.add_argument('--kmeans', default='kmeans428')
    a = ap.parse_args()
    docs = pd.read_parquet(DATA / 'posts' / 'shard-00.parquet', columns=['uri', 'doc']).set_index('uri')['doc'].to_dict()
    h, hs = load(a.hdbscan, a.model)
    k, ks = load(a.kmeans, a.model)
    out = Path(a.out)
    out.mkdir(parents=True, exist_ok=True)
    shutil.copy('/tmp/pilot-map.png', out / 'map.png') if Path('/tmp/pilot-map.png').exists() else None
    css = ('body{font:14px/1.45 system-ui,sans-serif;background:#0e1016;color:#e6e8ee;max-width:1100px;margin:0 auto;padding:20px}'
           'h1,h2{font-weight:650}.muted{color:#8b93a7}.card{background:#151821;border:1px solid #262b38;border-left:4px solid #555;border-radius:6px;padding:10px 12px;margin:10px 0}'
           '.c5,.c4{border-left-color:#4fd17a}.c3{border-left-color:#f1b93b}.c2,.c1{border-left-color:#ef5b5b}.head{display:flex;gap:8px;align-items:baseline;flex-wrap:wrap}'
           '.chip{background:#202638;border-radius:10px;padding:1px 8px;font-size:12px;color:#c9cfdd}details{margin-top:6px}summary{cursor:pointer;color:#6ea8ff}'
           'li{margin:4px 0;overflow-wrap:anywhere}img{max-width:100%;border-radius:6px}.cols{display:grid;grid-template-columns:1fr 1fr;gap:18px}@media(max-width:900px){.cols{grid-template-columns:1fr}}')
    body = [f'<h1>Pilot: topics made the delve-atlas way, next to k-means segments</h1>',
            '<p class="muted">The same 213,772 posts (one shard of five days of English top-level posts). Both made 428 clusters. A random sample of each was titled and rated by '
            'Claude Haiku 5.5; the left column is HDBSCAN on a 12-dimensional UMAP, the right is k-means in the embedding space, as the live map does. '
            'Green is rated coherent (4-5), amber middling, red incoherent. The samples are random, not picked.</p>']
    if (out / 'map.png').exists():
        body.append('<h2>The pilot\'s layout</h2><p class="muted">The 2D UMAP of all 213,772 posts, coloured by region (29 regions of topics).</p><img src="map.png" alt="the pilot layout">')
    body.append('<div class="cols"><div>' + section('HDBSCAN topics', f'{a.n} of the 150 rated, in random order.', h, hs, docs, a.n) + '</div><div>'
                + section('k-means segments', f'{a.n} of the 150 rated, in random order.', k, ks, docs, a.n) + '</div></div>')
    (out / 'index.html').write_text(f'<!doctype html><meta charset="utf-8"><title>Post atlas pilot</title><style>{css}</style>' + '\n'.join(body))
    print(f'wrote {out / "index.html"}')


if __name__ == '__main__':
    main()
