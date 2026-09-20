"""Score generate.py output: the last python block of the answer, run against every test.

Runs each test in `unshare -rn` (no network) with memory and CPU limits, 6 s per test as
LiveCodeBench does, stopping at the first failing test. A problem passes when every test passes.
"""
import argparse, json, os, re, resource, subprocess, sys, tempfile, collections
from concurrent.futures import ProcessPoolExecutor

PRELUDE = ("from typing import *\nfrom collections import *\nfrom itertools import *\nfrom functools import *\n"
           "from heapq import *\nfrom bisect import *\nimport math, re, string, sys, random, heapq, bisect, collections, itertools, functools\n"
           "sys.setrecursionlimit(10**6)\n")
FUNC_RUNNER = r'''
import json, sys
src = open(sys.argv[1]).read()
ns = {"__name__": "solution"}
exec(compile(src, "solution", "exec"), ns)
args = [json.loads(l) for l in open(sys.argv[3]).read().split("\n") if l.strip() != ""]
fn = getattr(ns["Solution"](), sys.argv[2]) if "Solution" in ns else ns[sys.argv[2]]
print("\n@@RESULT@@" + json.dumps(fn(*args)))
'''
PER_TEST_S = 6

def extract(content):
    blocks = re.findall(r"```(?:python|py|Python3|python3)?[ \t]*\n(.*?)```", content or '', re.S)
    return blocks[-1] if blocks else None

def limits():
    resource.setrlimit(resource.RLIMIT_AS, (4 << 30, 4 << 30))
    resource.setrlimit(resource.RLIMIT_CPU, (PER_TEST_S + 2, PER_TEST_S + 2))
    os.setsid()

def num_eq(x, y):
    try:
        fx, fy = float(x), float(y)
    except (TypeError, ValueError):
        return False
    return abs(fx - fy) <= 1e-6 * max(1.0, abs(fy))

def stdio_eq(got, exp):
    g = [l.strip() for l in got.strip().split('\n')]
    e = [l.strip() for l in exp.strip().split('\n')]
    if g == e:
        return True
    if len(g) != len(e):
        return False
    for gl, el in zip(g, e):
        if gl == el:
            continue
        gt, et = gl.split(), el.split()
        if len(gt) != len(et) or not all(a == b or num_eq(a, b) for a, b in zip(gt, et)):
            return False
    return True

def val_eq(a, b):
    if isinstance(a, bool) or isinstance(b, bool):
        return a == b
    if isinstance(a, (int, float)) and isinstance(b, (int, float)):
        return a == b or num_eq(a, b)
    if isinstance(a, list) and isinstance(b, list):
        return len(a) == len(b) and all(val_eq(x, y) for x, y in zip(a, b))
    return a == b

def run(cmd, cwd, stdin):
    try:
        p = subprocess.run(['unshare', '-rn'] + cmd, cwd=cwd, input=stdin, capture_output=True, text=True,
                           timeout=PER_TEST_S, preexec_fn=limits)
        return p.returncode, p.stdout, p.stderr[-300:]
    except subprocess.TimeoutExpired:
        return None, '', 'timeout'

def score_one(args):
    rec, prob = args
    out = {'id': rec['id'], 'difficulty': rec['difficulty'], 'model': rec['model']}
    if rec.get('error'):
        return {**out, 'pass': False, 'why': 'generation_error'}
    code = extract(rec.get('content'))
    if code is None:
        return {**out, 'pass': False, 'why': 'no_code' if rec.get('done_reason') != 'length' else 'truncated'}
    with tempfile.TemporaryDirectory() as d:
        sol = os.path.join(d, 'sol.py')
        open(sol, 'w').write(PRELUDE + code)
        if prob['starter_code']:
            open(os.path.join(d, 'runner.py'), 'w').write(FUNC_RUNNER)
        for i, t in enumerate(prob['tests']):
            if t['testtype'] == 'functional':
                inp = os.path.join(d, 'in.txt')
                open(inp, 'w').write(t['input'])
                rc, so, se = run([sys.executable, 'runner.py', sol, prob['func_name'], inp], d, None)
                ok = False
                if rc == 0 and '@@RESULT@@' in so:
                    try:
                        ok = val_eq(json.loads(so.rsplit('@@RESULT@@', 1)[1]), json.loads(t['output']))
                    except Exception:
                        ok = False
            else:
                rc, so, se = run([sys.executable, sol], d, t['input'])
                ok = rc == 0 and stdio_eq(so, t['output'])
            if not ok:
                why = 'timeout' if se == 'timeout' else ('runtime_error' if rc not in (0, None) else 'wrong_answer')
                return {**out, 'pass': False, 'why': why, 'failed_test': i, 'tests': len(prob['tests'])}
    return {**out, 'pass': True, 'why': '', 'tests': len(prob['tests'])}

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('results')
    ap.add_argument('--problems', default='problems.jsonl')
    ap.add_argument('--workers', type=int, default=8)
    a = ap.parse_args()
    probs = {json.loads(l)['id']: json.loads(l) for l in open(a.problems)}
    recs = {}
    for l in open(a.results):
        r = json.loads(l)
        recs[r['id']] = r  # the last line for an id wins (a retried error)
    with ProcessPoolExecutor(a.workers) as ex:
        scored = list(ex.map(score_one, [(r, probs[r['id']]) for r in recs.values()]))
    outp = a.results.replace('.jsonl', '.scored.jsonl')
    with open(outp, 'w') as f:
        for s in sorted(scored, key=lambda s: s['id']):
            f.write(json.dumps(s) + '\n')
    by = collections.defaultdict(lambda: [0, 0])
    for s in scored:
        by[s['difficulty']][0] += s['pass']; by[s['difficulty']][1] += 1
        by['all'][0] += s['pass']; by['all'][1] += 1
    print(os.path.basename(a.results), ' '.join(f"{k}={v[0]}/{v[1]}" for k, v in sorted(by.items())),
          'why', dict(collections.Counter(s['why'] for s in scored if not s['pass'])))

if __name__ == '__main__':
    main()
