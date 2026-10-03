import json, sys, random, collections
sys.path.insert(0, '/home/penguin/bluesky/topic-feed/trainer')
import clef_calibrate as cc
clef = cc.read_clef('/data/clef/clef-flash-remote.jsonl', priority=2)
jev = cc.read_jev(clef)
posts = {}
for line in open('/data/clef/full_posts.jsonl'):
    r = json.loads(line)
    if r['uri'] in clef: posts[r['uri']] = r
verd = {}
try:
    for line in open('/data/clef/verdicts.jsonl'):
        v = json.loads(line); verd[v['uri']] = v
except Exception as e: print('no verdicts', e)
am = lambda d: max(d, key=d.get)
pairs = collections.defaultdict(list)
for u in clef:
    if u not in jev: continue
    a, b = am(jev[u]['broad_probs']), am(clef[u]['broad_probs'])
    if a != b: pairs[tuple(sorted((a, b)))].append((u, a, b))
want = [tuple(x.split(",")) for x in sys.argv[2].split(";")]
rnd = random.Random(int(sys.argv[3]) if len(sys.argv) > 3 else 7)
n = int(sys.argv[1]) if len(sys.argv) > 1 else 8
for p in want:
    L = pairs[p]
    print(f'\n=== {p} {len(L)}')
    for u, a, b in rnd.sample(L, min(n, len(L))):
        t = ' '.join((posts[u].get('text') or '').split())[:170]
        extra = ''
        if posts[u].get('link_title'): extra += ' [link: ' + posts[u]['link_title'][:60] + ']'
        if posts[u].get('quote_text'): extra += ' [quote: ' + ' '.join(posts[u]['quote_text'].split())[:60] + ']'
        v = verd.get(u, {}).get('verdict', '')
        print(f'J={a} C={b} v={v} | {t}{extra}')
