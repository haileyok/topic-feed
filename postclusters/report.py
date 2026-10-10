"""Write a static HTML report for reviewing the clusters: the map, the areas, and every segment with what
the LLM(s) called it, how big and varied it is, and example posts, so a person can judge whether the
segments make sense.

Reads data/clusters/ (cluster.py, label.py) and writes data/clusters/report.html and map.png. Whatever has
been labelled is shown; a segment without a label shows the existing classifier's reading instead. Both
models' labels are shown side by side when both have been run.

The report contains the text of public posts and is meant to be read locally, not published; it is under
data/, which git ignores. Every piece of post and model text goes through html.escape.

Usage: report.py [--examples 8] [--models haiku]
"""
import argparse
import html
import json
import re

import matplotlib
matplotlib.use('Agg')
import matplotlib.pyplot as plt  # noqa: E402
import numpy as np  # noqa: E402
import pandas as pd  # noqa: E402

from common import DATA  # noqa: E402
from label import clip, load_posts, pick_representatives  # noqa: E402

AI_WORDS = re.compile(r'\b(ai|a\.i\.|artificial intelligence|llm|chatgpt|openai|anthropic|claude|gemini|copilot|generative|machine learning|genai|gen ai)\b', re.I)


def bsky_url(uri: str, did: str = '') -> str:
    """The bsky.app address of a post from its at:// URI (at://did/app.bsky.feed.post/rkey)."""
    m = re.match(r'^at://([^/]+)/app\.bsky\.feed\.post/([^/]+)$', uri)
    return f'https://bsky.app/profile/{m.group(1)}/post/{m.group(2)}' if m else ''


def flags(seg, labels):
    """Short reasons to look at a segment twice. `seg` is its row of segments.parquet, `labels` a list of label dicts."""
    out = []
    coh = [x['coherence'] for x in labels]
    if coh and min(coh) <= 2:
        out.append('low coherence')
    if len(coh) > 1 and max(coh) - min(coh) >= 2:
        out.append('models disagree')
    if seg['top_author_share'] >= 0.3:
        out.append('one account dominates')
    if seg['repeat_share'] >= 0.3:
        out.append('many repeats')
    if any(x['kind'] in ('automated_bot', 'spam') for x in labels):
        out.append('bot/spam')
    if seg.get('template_share', 0) >= 0.5:
        out.append('templated bot posts')
    if seg['n_texts'] < 60:
        out.append('tiny')
    return out


def template_shares(posts, min_repeats=50):
    """For each segment, the share of its posts that are one of the many near-identical posts a bot makes: posts
    that become the same once numbers and long random-looking strings are masked, and that there are at least
    `min_repeats` of. Exact duplicates are already collapsed; this catches the ones that differ only by an id."""
    key = (posts['doc'].str.replace(r'[A-Za-z0-9]{12,}', '<X>', regex=True).str.replace(r'\d+', '<N>', regex=True).str.slice(0, 80))
    is_template = key.map(key.value_counts()).ge(min_repeats)
    return is_template.groupby(posts['segment']).mean()


def esc(x):
    return html.escape(str(x), quote=True)


def read_labels(model, sub='clusters'):
    p = DATA / sub / f'labels-{model}.jsonl'
    if not p.exists():
        return {}
    return {d['segment']: d for d in (json.loads(l) for l in p.open() if l.strip())}


def draw_map(segs, area_titles, path):
    fig, ax = plt.subplots(figsize=(15, 11), dpi=100)
    cmap = plt.get_cmap('tab20')
    ax.scatter(segs['x'], segs['y'], s=np.sqrt(segs['n_texts']) * 3, c=[cmap(int(a) % 20) for a in segs['area']], alpha=0.75, linewidths=0.3, edgecolors='white')
    for a, g in segs.groupby('area'):
        if g['n_texts'].sum() < 0.01 * segs['n_texts'].sum():
            continue
        ax.text(g['x'].median(), g['y'].median(), area_titles.get(a, f'area {a}'), fontsize=9, weight='bold', ha='center',
                bbox=dict(boxstyle='round,pad=0.2', fc='white', ec='none', alpha=0.7))
    ax.axis('off')
    fig.savefig(path, bbox_inches='tight', facecolor='white')
    plt.close(fig)


CSS = """
body{font:14px/1.45 system-ui,sans-serif;max-width:1180px;margin:24px auto;padding:0 16px;color:#1d2330;background:#fafbfc}
h1{font-size:22px} h2{font-size:17px;margin-top:30px} h3{font-size:14px;margin:0}
details{border:1px solid #d9dde5;border-radius:8px;margin:6px 0;background:#fff} summary{cursor:pointer;padding:8px 12px}
details details{margin:4px 10px;border-color:#e6e9ef} .body{padding:4px 14px 12px}
.muted{color:#6b7385} .tag{display:inline-block;background:#eef1f6;border-radius:9px;padding:0 8px;margin-left:5px;font-size:12px}
.flag{background:#fde8e8;color:#8a1f1f} .ok{background:#e4f5e9;color:#1d5a2f}
table{border-collapse:collapse;width:100%;font-size:13px} td,th{padding:3px 8px;text-align:left;border-bottom:1px solid #eef0f4;vertical-align:top}
.post{margin:3px 0;padding:4px 8px;border-left:3px solid #c9d1e0;background:#f6f8fb;white-space:pre-wrap;word-break:break-word}
.post.near{border-color:#4c8bf5}.post.top{border-color:#e2a400}.post.rand{border-color:#9aa3b5}
.cols{display:grid;grid-template-columns:1fr 1fr;gap:12px}
img{max-width:100%;border:1px solid #d9dde5;border-radius:8px}
"""


def segment_html(seg, labs, examples, area_title):
    fl = flags(seg, list(labs.values()))
    names = ' / '.join(f'<b>{esc(l["title"])}</b>' for l in labs.values()) if labs else f'<i>not labelled</i> · classifier: {esc(seg["classifier_top3"][:70])}'
    coh = ' '.join(f'{k[0].upper()}{v["coherence"]}' for k, v in labs.items())
    head = (f'<summary>{names} <span class="muted">· {int(seg["n_texts"]):,} posts · {int(seg["n_authors"]):,} accounts</span>'
            f'{f"<span class=tag>coherence {esc(coh)}</span>" if coh else ""}'
            + ''.join(f'<span class="tag flag">{esc(f)}</span>' for f in fl) + '</summary>')
    parts = ['<div class="body">']
    for k, l in labs.items():
        dis = '; '.join(l['disagreement']) or 'none'
        parts.append(f'<p><b>{esc(k)}</b> ({esc(l["kind"])}, coherence {l["coherence"]}): {esc(l["description"])}<br>'
                     f'<span class="muted">unites them: {esc(l["common_thread"])} · they disagree on: {esc(dis)}</span></p>')
    parts.append(f'<p class="muted">segment {int(seg["segment"])} in area {esc(area_title)} · existing classifier: {esc(seg["classifier_top3"])} · '
                 f'repeat share {seg["repeat_share"]:.0%} · top account {seg["top_author_share"]:.0%} of texts · '
                 f'outraged {seg["outraged"]:.2f}, sentiment {seg["sentiment"]:.2f}, critical {seg["critical"]:.2f}, spam {seg["spam"]:.2f}</p>')
    for text, grp, uri, n_posts, n_auth in examples:
        extra = f' <span class="muted">×{n_posts} posts, {n_auth} accounts</span>' if n_posts > 1 else ''
        link = bsky_url(uri)
        parts.append(f'<div class="post {grp}">{esc(clip(text, 400))}{extra}'
                     + (f' <a href="{esc(link)}" target="_blank" rel="noopener noreferrer">open</a>' if link else '') + '</div>')
    parts.append('</div>')
    return f'<details>{head}{"".join(parts)}</details>'


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--examples', type=int, default=8)
    ap.add_argument('--models', default='haiku', help='comma-separated label sets to show (haiku, luna)')
    a = ap.parse_args()
    out = DATA / 'clusters'
    segs = pd.read_parquet(out / 'segments.parquet')
    posts = load_posts()
    labels = {m: read_labels(m) for m in a.models.split(',')}
    labels = {m: d for m, d in labels.items() if d}
    areas = {}
    for m in labels:
        p = out / f'areas-{m}.json'
        if p.exists():
            for k, v in json.loads(p.read_text()).items():
                areas.setdefault(int(k), {})[m] = v
    area_title = {k: ' / '.join(v['title'] for v in d.values()) for k, d in areas.items()}
    draw_map(segs, {k: next(iter(d.values()))['title'] for k, d in areas.items()}, out / 'map.png')
    by_seg = {s: g for s, g in posts.groupby('segment')}
    segs = segs.merge(template_shares(posts).rename('template_share'), left_on='segment', right_index=True, how='left').fillna({'template_share': 0})

    total = int(segs['n_posts'].sum())
    body = [f'<h1>Post clusters: review</h1>',
            f'<p>{len(posts):,} distinct posts ({total:,} counting repeats) in {len(segs):,} segments and {segs["area"].nunique()} areas. '
            f'Labels from: {esc(", ".join(labels) or "none yet")}. Blue-edged posts are the most typical of the segment, '
            f'yellow-edged were shared by several accounts, grey were picked at random. <b>Open a segment and ask: do these posts '
            f'belong together, and does the title describe them?</b> The classifier line is the existing 117-subtopic classifier, '
            f'shown only as a cross-check.</p><img src="map.png" alt="map of segments">']

    # Segments whose posts take opposing sides: where a stance classification would go.
    stance = []
    for s, row in segs.set_index('segment').iterrows():
        for m, d in labels.items():
            if s in d and d[s]['disagreement']:
                stance.append((int(row['n_texts']), s, m, d[s]['title'], d[s]['disagreement']))
    if stance:
        body.append('<h2>Segments where posts take opposing sides</h2><table><tr><th>posts</th><th>segment</th><th>model</th><th>axis</th></tr>')
        for n, s, m, t, dis in sorted(stance, reverse=True)[:60]:
            body.append(f'<tr><td>{n:,}</td><td>{esc(t)} <span class="muted">({s})</span></td><td>{esc(m)}</td><td>{esc("; ".join(dis))}</td></tr>')
        body.append('</table>')

    # Anything about AI, since that is a topic to study the stance on.
    ai = []
    for s, row in segs.set_index('segment').iterrows():
        text = ' '.join(d[s]['title'] + ' ' + d[s]['description'] for d in labels.values() if s in d)
        if AI_WORDS.search(text) or str(row['classifier_top']).startswith('technology/ai'):
            ai.append((int(row['n_texts']), s, text[:100] or str(row['classifier_top3'])))
    if ai:
        body.append(f'<h2>Segments that mention AI ({len(ai)})</h2><table>' + ''.join(
            f'<tr><td>{n:,}</td><td>{esc(t)} <span class="muted">({s})</span></td></tr>' for n, s, t in sorted(ai, reverse=True)[:40]) + '</table>')

    body.append('<h2>Areas</h2>')
    order = segs.groupby('area')['n_texts'].sum().sort_values(ascending=False).index
    for ar in order:
        g = segs[segs['area'] == ar].sort_values('n_texts', ascending=False)
        title = area_title.get(int(ar), f'area {ar}')
        desc = ' '.join(f'{esc(m)}: {esc(d["description"])} (coherence {d["coherence"]})' for m, d in areas.get(int(ar), {}).items())
        body.append(f'<details><summary><h3 style="display:inline">{esc(title)}</h3> <span class="muted">· {len(g)} segments · '
                    f'{int(g["n_texts"].sum()):,} posts ({g["n_texts"].sum() / segs["n_texts"].sum():.1%})</span></summary><div class="body">'
                    f'<p class="muted">{desc}</p>')
        for _, row in g.iterrows():
            s = int(row['segment'])
            labs = {m: d[s] for m, d in labels.items() if s in d}
            gp = by_seg[s]
            rep = pick_representatives(gp['cos'].to_numpy(), gp['n_authors'].to_numpy(), gp['doc'].tolist(), n_near=a.examples - 4, n_top=2, n_rand=2, seed=s)
            ex = [(gp['doc'].iloc[i], grp, gp['uri'].iloc[i], int(gp['n_posts'].iloc[i]), int(gp['n_authors'].iloc[i])) for i, grp in rep]
            body.append(segment_html(row, labs, ex, title))
        body.append('</div></details>')

    (out / 'report.html').write_text(f'<!doctype html><meta charset="utf-8"><title>Post clusters</title><style>{CSS}</style>' + ''.join(body))
    print(f'wrote {out / "report.html"} ({len(segs)} segments, {sum(len(d) for d in labels.values())} labels)')


if __name__ == '__main__':
    main()
