"""Pick the problem set from LiveCodeBench v6's newest additions and decode their tests.

Newest contest date first within each difficulty; easy 30 / medium 35 / hard 35.
Private tests in code_generation_lite are base64(zlib(pickle(json str))).
"""
import base64, json, pickle, zlib, collections

QUOTA = {'easy': 30, 'medium': 35, 'hard': 35}

def decode_private(s):
    try:
        return json.loads(s)
    except Exception:
        return json.loads(pickle.loads(zlib.decompress(base64.b64decode(s.encode('utf-8')))))

rows = [json.loads(l) for l in open('test6.jsonl')]
rows.sort(key=lambda r: (r['contest_date'], r['question_id']), reverse=True)
picked, count = [], collections.Counter()
for r in rows:
    d = r['difficulty']
    if count[d] >= QUOTA[d]:
        continue
    count[d] += 1
    pub = json.loads(r['public_test_cases'])
    priv = decode_private(r['private_test_cases'])
    meta = json.loads(r['metadata']) if r['metadata'] else {}
    picked.append({
        'id': r['question_id'], 'title': r['question_title'], 'platform': r['platform'],
        'difficulty': d, 'contest_date': r['contest_date'][:10],
        'question': r['question_content'], 'starter_code': r['starter_code'],
        'func_name': meta.get('func_name'), 'tests': pub + priv,
    })
picked.sort(key=lambda p: p['id'])
with open('problems.jsonl', 'w') as f:
    for p in picked:
        f.write(json.dumps(p) + '\n')
print(len(picked), dict(count), 'tests total', sum(len(p['tests']) for p in picked),
      'types', collections.Counter(t['testtype'] for p in picked for t in p['tests']))
print('dates', min(p['contest_date'] for p in picked), max(p['contest_date'] for p in picked))
