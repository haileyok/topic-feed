import json, sys, random, collections
sys.path.insert(0, '/home/penguin/bluesky/topic-feed/trainer')
import clef_calibrate as cc

posts = {}
for line in open('/data/clef/full_posts.jsonl'):
    r = json.loads(line); posts[r['uri']] = r
verd = {}
for line in open('/data/clef/verdicts.jsonl'):
    v = json.loads(line); verd[v['uri']] = v['verdict']      # later lines win
print('verdict counts', collections.Counter(verd.values()))
judged = set(verd)

clef_t = cc.read_clef('/data/clef/clef-flash-remote.jsonl', priority=2)
clef_p = cc.read_clef('/data/clef/clef-flash-remote.jsonl', priority=0)
print('clef v1 results: text', len(clef_t), 'pictures', len(clef_p))
jev = cc.read_jev(list(clef_t) + list(clef_p))
text_ok = sorted(u for u in clef_t if u in jev)
pic_ok = sorted(u for u in clef_p if u in jev)
print('with Jev v1: text', len(text_ok), 'pictures', len(pic_ok))
jt = sorted(u for u in text_ok if u in judged)
jp = sorted(u for u in pic_ok if u in judged)
print('judged (non-skip): text', len(jt), 'pictures', len(jp), 'total in verdicts non-skip', len(judged))

rnd = random.Random(20261002)
rest = [u for u in text_ok if u not in judged]
rnd.shuffle(rest)
text_test = jt + rest[:2000 - len(jt)]
print('text test', len(text_test), 'of which random', 2000 - len(jt))
pic_test = pic_ok
src = collections.Counter(jev[u]['source'] for u in text_test + pic_test)
print('v1 Jev sources in test', src)

def mode(u): return 'plain' if jev[u]['source'] == 'window' else 'full'
with open('test-uris.txt', 'w') as f:
    for u in text_test + pic_test:
        f.write(f'{u}\t{mode(u)}\n')
json.dump({'text': text_test, 'pictures': pic_test, 'judged_text': jt, 'judged_pictures': jp,
           'random_text': [u for u in text_test if u not in judged]}, open('test-sets.json', 'w'))
with open('test-posts.jsonl', 'w') as f:
    for u in text_test + pic_test:
        f.write(json.dumps(posts[u], ensure_ascii=False) + '\n')
print('wrote', len(text_test) + len(pic_test))
