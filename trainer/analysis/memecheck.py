import json
import math
import statistics as st

new = {}
for line in open('/data/clef/v2/memetest-out.jsonl'):
    r = json.loads(line)
    new[r['uri']] = r
old = {}
for line in open('/data/clef/v2/app/clef-v21-pictures.jsonl'):
    try:
        r = json.loads(line)
    except ValueError:
        continue
    if r['uri'] in new:
        old[r['uri']] = r
sets = json.load(open('/data/clef/v2/memetest-sets.json'))
posts = {}
for line in open('/data/clef/v2/memetest-posts.jsonl'):
    p = json.loads(line)
    posts[p['uri']] = p
top = lambda d: max(d, key=d.get)  # noqa: E731

# 1. did adding the question (and a different GPU) disturb the other answers?
same_top = sum(top(new[u]['broad_probs']) == top(old[u]['broad_probs']) for u in new if u in old)
dmax = [max(abs(new[u]['broad_probs'][k] - old[u]['broad_probs'][k]) for k in old[u]['broad_probs']) for u in new if u in old]
sig_d = {}
for u in new:
    if u not in old:
        continue
    for k, v in old[u]['signals'].items():
        sig_d.setdefault(k, []).append(abs(new[u]['signals'][k] - v))
print(f'posts compared with the box run: {len(old)}')
print(f'top topic unchanged: {same_top}/{len(old)}; largest broad-probability change per post: median {st.median(dmax):.3f}, 90th pct {sorted(dmax)[int(.9*len(dmax))]:.3f}, max {max(dmax):.3f}')
print('mean |change| in the old signals:', {k: round(sum(v) / len(v), 3) for k, v in sig_d.items()})
print('new keys saved:', sorted(k for k in next(iter(new.values()))['signals'] if k.startswith('tone') or k == 'meme'))


# 2. the meme signal
def memep(u):
    return new[u]['signals']['meme']


for name in ('A', 'random'):
    ps = [memep(u) for u in sets[name]]
    print(f"\n{name} set ({len(ps)} posts): P(meme) mean {sum(ps)/len(ps):.2f}, median {st.median(ps):.2f}; >=0.5: {sum(p >= .5 for p in ps)}; >=0.8: {sum(p >= .8 for p in ps)}; <=0.1: {sum(p <= .1 for p in ps)}")
allp = [(memep(u), u) for u in new]
hum = [new[u]['signals']['tone.humorous'] for u in new]
mp = [memep(u) for u in new]
mx, my = sum(mp) / len(mp), sum(hum) / len(hum)
cov = sum((a - mx) * (b - my) for a, b in zip(mp, hum))
corr = cov / math.sqrt(sum((a - mx) ** 2 for a in mp) * sum((b - my) ** 2 for b in hum))
print(f'\ncorrelation of P(meme) with P(tone humorous): {corr:.2f}; P(meme)>=0.5 posts: {sum(p >= .5 for p in mp)}; of them tone humorous >=0.5: {sum(1 for u in new if memep(u) >= .5 and new[u]["signals"]["tone.humorous"] >= .5)}')
topics = {top(r['broad_probs']) for r in new.values()}
print('topic of the P(meme)>=0.5 posts:', sorted(((sum(1 for u in new if memep(u) >= .5 and top(new[u]['broad_probs']) == t), t) for t in topics), reverse=True)[:8])


def show(u):
    p = posts[u]
    t = ' '.join((p.get('text') or '').split())[:80]
    ocr = [x for x, s in zip(p.get('image_texts') or [], p.get('image_text_sources') or []) if s == 'ocr']
    o = (' | OCR: ' + ' '.join(ocr[0].split())[:70]) if ocr else ''
    r = new[u]
    return f"meme {memep(u):.2f} humorous {r['signals']['tone.humorous']:.2f} | {top(r['broad_probs'])} {max(r['broad_probs'].values()):.2f} | {t!r}{o}"


allp.sort(reverse=True)
print('\n--- highest P(meme)')
for _, u in allp[:14]:
    print(show(u))
print('\n--- between 0.3 and 0.6 (the unsure ones)')
for u in [u for p, u in allp if .3 <= p <= .6][:10]:
    print(show(u))
print('\n--- lowest P(meme) among the US-politics mismatches (A set), for contrast')
for _, u in [x for x in allp if x[1] in set(sets['A'])][-8:]:
    print(show(u))
