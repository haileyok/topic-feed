import json, random, subprocess, collections
test = {l.split('\t')[0] for l in open('test-uris.txt')}
q = ("SELECT uri, argMin(source, multiIf(source = 'window', 0, source = 'sample', 1, 2)) AS s FROM jev_labels "
     "WHERE taxonomy_version = 'v1' AND jev_model = 'jev-1.13.0' GROUP BY uri FORMAT TSV")
out = subprocess.run(['docker', 'exec', '-i', 'topic-feed-clickhouse', 'sh', '-c',
                      'clickhouse-client --user topicfeed --password "$CLICKHOUSE_PASSWORD" --database topicfeed'],
                     input=q, capture_output=True, text=True, check=True).stdout
v1src = dict(l.split('\t') for l in out.splitlines())
print('v1 Jev sources', collections.Counter(v1src.values()), 'uris', len(v1src))
rows = {1: [], 2: []}
for line in open('/data/clef/full_posts.jsonl'):
    r = json.loads(line)
    if r['priority'] in rows: rows[r['priority']].append(r)
print('priority 1', len(rows[1]), 'priority 2', len(rows[2]))
rnd = random.Random(20261002)
p2 = [r for r in rows[2] if r['uri'] not in test]
rnd.shuffle(p2)
calib = p2[:5000]
calib_uris = {r['uri'] for r in calib}
rest = [r['uri'] for r in rows[1] + rows[2] if r['uri'] not in calib_uris]
rnd.shuffle(rest)
order = [r['uri'] for r in calib] + rest
mode = lambda u: 'full' if v1src.get(u) in ('sample', 'uncertain') else 'plain'
with open('jev-main-uris.txt', 'w') as f:
    for u in order: f.write(f'{u}\t{mode(u)}\n')
print('main list', len(order), 'modes', collections.Counter(mode(u) for u in order), 'already in test (will be skipped):', sum(u in test for u in order))
with open('calib-posts.jsonl', 'w') as f:
    for r in calib: f.write(json.dumps(r, ensure_ascii=False) + '\n')
print('calibration posts', len(calib))
