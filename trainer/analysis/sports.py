import json, sys, collections
sys.path.insert(0, '/home/penguin/bluesky/topic-feed/trainer')
import clef_calibrate as cc, v2_gates as g, yaml
sets = json.load(open('test-sets.json'))
allu = sets['text'] + sets['pictures']
posts = {json.loads(l)['uri']: json.loads(l) for l in open('test-posts.jsonl')}
mode = dict(l.rstrip('\n').split('\t') for l in open('test-uris.txt'))
hs = {b['id']: bool(b.get('subtopics')) for b in yaml.safe_load(open('/home/penguin/bluesky/topic-feed/taxonomy/v1.yaml'))['broad']}
j1 = g.Answers(cc.read_jev(allu), hs); j2 = g.Answers(g.read_jev_v2(allu, 'c8c15812c7ab'), hs)
words = lambda u: len((posts[u].get('text') or '').split())
sp1 = [u for u in allu if j1.broad(u) == 'sports']
out = [u for u in sp1 if j2.broad(u) != 'sports']
into = [u for u in allu if j2.broad(u) == 'sports' and j1.broad(u) != 'sports']
print('all 2,500 test posts: Jev sports v1', len(sp1), 'v2', sum(j2.broad(u) == 'sports' for u in allu), '| moved out', len(out), 'moved in', len(into))
print('moved out to:', collections.Counter(j2.broad(u) for u in out).most_common())
print('moved in from:', collections.Counter(j1.broad(u) for u in into).most_common())
short = [u for u in sp1 if words(u) <= 8]
print('v1 sports posts with <= 8 words:', len(short), 'moved out:', sum(j2.broad(u) != 'sports' for u in short), '| longer ones:', len(sp1) - len(short), 'moved out:', sum(j2.broad(u) != 'sports' for u in sp1 if words(u) > 8))
print('input mode of moved-out posts:', collections.Counter(mode[u] for u in out), ' all sports v1:', collections.Counter(mode[u] for u in sp1))
print('\nmoved out (v1 sports p, v2 top p, v2 sports p, v2 unclear p):')
for u in sorted(out, key=lambda u: words(u)):
    p1, p2 = j1.rows[u]['broad_probs'], j2.rows[u]['broad_probs']
    t = ' '.join((posts[u].get('text') or '').split())[:80]
    print(f"  {p1['sports']:.2f} -> sports {p2['sports']:.2f}, {j2.broad(u)} {p2[j2.broad(u)]:.2f}, unclear {p2['unclear']:.2f} | {mode[u]} | {t!r}")
