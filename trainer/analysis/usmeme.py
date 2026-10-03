import collections
import json
import re
import sys

sys.path.insert(0, '/home/penguin/bluesky/topic-feed/trainer')
import clef_calibrate as cc  # noqa: E402

rows = {}
for line in open('/data/clef/v2/app/clef-v21-pictures.jsonl'):
    try:
        r = json.loads(line)
    except ValueError:
        continue
    rows[r['uri']] = r
posts = {}
want = set(rows)
for line in open('/data/clef/full_posts.jsonl'):
    if not line.startswith('{"uri":"'):
        continue
    u = json.loads('"' + line[8:line.index('"', 8)] + '"')
    if u in want:
        posts[u] = json.loads(line)
jev = cc.read_jev(list(rows))
top = lambda d: max(d, key=d.get)  # noqa: E731
n = len([u for u in rows if u in jev])
A = [u for u in rows if u in jev and top(rows[u]['broad_probs']) == 'us_politics' and top(jev[u]['broad_probs']) != 'us_politics']
B = [u for u in rows if u in jev and top(jev[u]['broad_probs']) == 'us_politics' and top(rows[u]['broad_probs']) != 'us_politics']
both = [u for u in rows if u in jev and top(rows[u]['broad_probs']) == 'us_politics' and top(jev[u]['broad_probs']) == 'us_politics']
clef_pol = [u for u in rows if top(rows[u]['broad_probs']) == 'us_politics']
print(f'picture results {len(rows)}, with a Jev v1 answer {n}')
print(f'Clef us_politics: {len(clef_pol)} | both say us_politics: {len(both)} | A = Clef us_politics, Jev not: {len(A)} | B = Jev us_politics, Clef not: {len(B)}')
print('A: what Jev v1 said:', collections.Counter(top(jev[u]['broad_probs']) for u in A).most_common(8))
print('B: what Clef said  :', collections.Counter(top(rows[u]['broad_probs']) for u in B).most_common(8))

MEME = re.compile(r'meme|cartoon|comic|illustrat|screenshot|tweet|image macro|caption|reaction|photoshop|edited|poster|sign|protest', re.I)


def runner_up(r):
    items = sorted(r['broad_probs'].items(), key=lambda kv: -kv[1])
    return items[1]


def desc(u):
    p = posts.get(u, {})
    d = [t for t, s in zip(p.get('image_texts') or [], p.get('image_text_sources') or []) if s == 'luna']
    o = [t for t, s in zip(p.get('image_texts') or [], p.get('image_text_sources') or []) if s == 'ocr']
    return d, o


for name, S in (('A (Clef us_politics, Jev something else)', A), ('both say us_politics', both)):
    ru = collections.Counter(runner_up(rows[u])[0] for u in S)
    hum = [rows[u]['broad_probs'].get('humor', 0) for u in S]
    short = sum(1 for u in S if len((posts[u].get('text') or '').split()) <= 6)
    notext = sum(1 for u in S if not (posts[u].get('text') or '').strip())
    hasdesc = sum(1 for u in S if desc(u)[0])
    hasocr = sum(1 for u in S if desc(u)[1])
    memedesc = sum(1 for u in S if any(MEME.search(t) for t in desc(u)[0] + desc(u)[1]))
    print(f'\n{name}: {len(S)} posts')
    print('  Clef runner-up topic:', ru.most_common(5))
    print(f'  Clef P(humor): mean {sum(hum)/len(hum):.2f}, >=0.2 in {sum(h >= .2 for h in hum)} ({100*sum(h >= .2 for h in hum)/len(hum):.0f}%), >=0.4 in {sum(h >= .4 for h in hum)}')
    print(f'  text of 6 words or fewer: {short} ({100*short/len(S):.0f}%), no text at all: {notext}; pipeline had a picture description: {hasdesc}, read text in the picture (OCR): {hasocr}; description or OCR mentions meme/cartoon/screenshot/sign...: {memedesc}')
    print('  Jev v1 P(humor): mean %.2f' % (sum(jev[u]['broad_probs'].get('humor', 0) for u in S) / len(S)))

print('\n--- samples from A (Clef us_politics > Jev), random')
import random
rnd = random.Random(5)
for u in rnd.sample(A, 30):
    r, j = rows[u], jev[u]
    t = ' '.join((posts[u].get('text') or '').split())[:105]
    ru = runner_up(r)
    d, o = desc(u)
    extra = (' | desc: ' + d[0][:70]) if d else ((' | OCR: ' + o[0][:70]) if o else '')
    print(f"Clef {r['broad_probs']['us_politics']:.2f} (then {ru[0]} {ru[1]:.2f}) | Jev v1 {top(j['broad_probs'])} {max(j['broad_probs'].values()):.2f} | {t!r}{extra}")
print('\n--- samples from B (Jev us_politics > Clef), random')
for u in rnd.sample(B, min(12, len(B))):
    r, j = rows[u], jev[u]
    t = ' '.join((posts[u].get('text') or '').split())[:105]
    print(f"Clef {top(r['broad_probs'])} {max(r['broad_probs'].values()):.2f} | Jev v1 us_politics {j['broad_probs']['us_politics']:.2f} | {t!r}")
