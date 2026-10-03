import json, sys, collections
sys.path.insert(0, '/home/penguin/bluesky/topic-feed/trainer')
import clef_calibrate as cc
clef = cc.read_clef('/data/clef/clef-flash-remote.jsonl', priority=2)
jev = cc.read_jev(clef)
verd = {}
for line in open('/data/clef/verdicts.jsonl'):
    v = json.loads(line); verd[v['uri']] = v
am = lambda d: max(d, key=d.get)
cnt = collections.Counter(); dirn = collections.defaultdict(collections.Counter); vb = collections.defaultdict(collections.Counter)
tot = 0; agree = 0
for u in clef:
    if u not in jev: continue
    tot += 1
    a, b = am(jev[u]['broad_probs']), am(clef[u]['broad_probs'])
    if a == b: agree += 1; continue
    p = tuple(sorted((a, b)))
    cnt[p] += 1
    dirn[p][(a, b)] += 1
    if u in verd: vb[p][verd[u].get('verdict')] += 1
print('both labelled', tot, 'agree', agree, 'disagree', tot - agree)
for p, n in cnt.most_common(14):
    print(p, n, dict(dirn[p]), 'owner verdicts:', dict(vb[p]))
# Jev v1 broad shares for the seven topics, on all 6,322 and overall
sh = collections.Counter(am(jev[u]['broad_probs']) for u in jev)
shc = collections.Counter(am(clef[u]['broad_probs']) for u in jev)
for t in ['us_politics','world_news','personal_life','humor','society','online_culture','unclear']:
    print(t, 'Jev', round(100*sh[t]/len(jev),1), 'Clef', round(100*shc[t]/len(jev),1))
