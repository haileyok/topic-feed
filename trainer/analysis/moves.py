import json, sys, random, collections
sys.path.insert(0, '/home/penguin/bluesky/topic-feed/trainer')
import clef_calibrate as cc, v2_gates as g
sets = json.load(open('test-sets.json'))
rand = sets['random_text']
posts = {json.loads(l)['uri']: json.loads(l) for l in open('test-posts.jsonl')}
import yaml
hs = {b['id']: bool(b.get('subtopics')) for b in yaml.safe_load(open('/home/penguin/bluesky/topic-feed/taxonomy/v1.yaml'))['broad']}
j1 = g.Answers(cc.read_jev(rand), hs); j2 = g.Answers(g.read_jev_v2(rand, 'c8c15812c7ab'), hs)
c1r = cc.read_clef('/data/clef/clef-flash-remote.jsonl', priority=2); c2r = cc.read_clef('clef-v2-test.jsonl', priority=2)
c1 = g.Answers({u: c1r[u] for u in rand}, hs); c2 = g.Answers({u: c2r[u] for u in rand}, hs)
rnd = random.Random(3)
def show(title, cond, n=12):
    L = [u for u in rand if cond(u)]
    print(f'\n## {title} ({len(L)})')
    for u in rnd.sample(L, min(n, len(L))):
        t = ' '.join((posts[u].get('text') or '').split())[:140]
        print(f'- Jev {j1.broad(u)}>{j2.broad(u)} Clef {c1.broad(u)}>{c2.broad(u)} | {t}')
show('Jev moved to unclear', lambda u: j2.broad(u) == 'unclear' and j1.broad(u) != 'unclear')
show('Jev moved to humor', lambda u: j2.broad(u) == 'humor' and j1.broad(u) != 'humor')
show('Jev world_news>us_politics', lambda u: j1.broad(u) == 'world_news' and j2.broad(u) == 'us_politics', 8)
show('Clef moved to unclear', lambda u: c2.broad(u) == 'unclear' and c1.broad(u) != 'unclear', 8)
# which v1 topics did the unclear gain come from, for each model
for name, a, b in (('Jev', j1, j2), ('Clef', c1, c2)):
    print(name, 'unclear gains from', collections.Counter(a.broad(u) for u in rand if b.broad(u) == 'unclear' and a.broad(u) != 'unclear').most_common(8))
# agreement among posts either model moved vs not
