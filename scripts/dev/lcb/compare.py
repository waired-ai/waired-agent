"""Pass@1 per model and a paired exact McNemar test for every pair, over the problems all runs share."""
import json, sys, itertools, math

def load(p):
    return {json.loads(l)['id']: json.loads(l) for l in open(p)}

def mcnemar_exact(b, c):
    n = b + c
    if n == 0:
        return 1.0
    k = min(b, c)
    tail = sum(math.comb(n, i) for i in range(k + 1)) / 2 ** n
    return min(1.0, 2 * tail)

runs = {p.split('/')[-1].replace('.scored.jsonl', ''): load(p) for p in sys.argv[1:]}
common = set.intersection(*(set(r) for r in runs.values()))
print(f"problems shared by all runs: {len(common)}")
for name, r in runs.items():
    by = {}
    for pid in common:
        d = r[pid]['difficulty']
        by.setdefault(d, [0, 0]); by[d][0] += r[pid]['pass']; by[d][1] += 1
    tot = sum(r[pid]['pass'] for pid in common)
    print(f"{name:28s} pass@1 {tot}/{len(common)} = {100*tot/len(common):.1f}%  " +
          ' '.join(f"{d}={v[0]}/{v[1]}" for d, v in sorted(by.items())))
for (a, ra), (b, rb) in itertools.combinations(runs.items(), 2):
    only_a = sum(1 for p in common if ra[p]['pass'] and not rb[p]['pass'])
    only_b = sum(1 for p in common if rb[p]['pass'] and not ra[p]['pass'])
    diff = 100 * (only_a - only_b) / len(common)
    print(f"{a} vs {b}: {a} only {only_a}, {b} only {only_b}, diff {diff:+.1f} pt, McNemar exact p={mcnemar_exact(only_a, only_b):.3f}")
