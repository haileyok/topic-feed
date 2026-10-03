import json, sys, collections
sys.path.insert(0, '/home/penguin/bluesky/topic-feed/trainer')
import clef_calibrate as cc, v2_gates as g, yaml
sets = json.load(open('test-sets.json')); pics = sets['pictures']
posts = {json.loads(l)['uri']: json.loads(l) for l in open('test-posts.jsonl')}
hs = {b['id']: bool(b.get('subtopics')) for b in yaml.safe_load(open('/home/penguin/bluesky/topic-feed/taxonomy/v1.yaml'))['broad']}
j1 = g.Answers(cc.read_jev(pics), hs)
c1r = cc.read_clef('/data/clef/clef-flash-remote.jsonl', priority=0); c1 = g.Answers({u: c1r[u] for u in pics}, hs)
ju = [u for u in pics if j1.broad(u) == 'unclear']
print('picture posts', len(pics), 'Jev unclear', len(ju), 'Clef unclear on those', sum(c1.broad(u) == 'unclear' for u in ju))
print('Clef answers for Jev-unclear picture posts:', collections.Counter(c1.broad(u) for u in ju).most_common(8))
conf = [c1.top_prob(u) for u in ju if c1.broad(u) != 'unclear']
print('Clef confidence on the ones it placed: median %.2f, share >= 0.6: %.0f%%' % (sorted(conf)[len(conf)//2], 100*sum(c >= .6 for c in conf)/len(conf)))
for u in ju[:12]:
    t = ' '.join((posts[u].get('text') or '').split())[:70]
    print(f'  Clef {c1.broad(u)} {c1.top_prob(u):.2f} | {t!r}')
