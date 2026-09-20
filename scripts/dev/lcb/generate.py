"""Ask one ollama model every problem in problems.jsonl, thinking on when the model has it.

One line per problem is appended to --out, so a run resumes where it stopped.
Same prompt as LiveCodeBench's code-generation runner; the same sampling for every model.
"""
import argparse, json, time, urllib.request

SYSTEM = ("You are an expert Python programmer. You will be given a question (problem specification) "
          "and will generate a correct Python program that matches the specification and passes all tests.")
FMT_STARTER = ("### Format: You will use the following starter code to write the solution to the problem "
               "and enclose your code within delimiters.\n```python\n{starter}\n```\n\n")
FMT_STDIN = ("### Format: Read the inputs from stdin solve the problem and write the answer to stdout "
             "(do not directly test on the sample inputs). Enclose your code within delimiters as follows. "
             "Ensure that when the python program runs, it reads the inputs, runs the algorithm and writes "
             "output to STDOUT.\n```python\n# YOUR CODE HERE\n```\n\n")

def user_prompt(p):
    s = "### Question:\n" + p['question'] + "\n\n"
    s += FMT_STARTER.format(starter=p['starter_code']) if p['starter_code'] else FMT_STDIN
    return s + "### Answer: (use the provided format with backticks)\n\n"

def post(url, body, timeout):
    req = urllib.request.Request(url, data=json.dumps(body).encode(), headers={'Content-Type': 'application/json'})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read())

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--endpoint', required=True)
    ap.add_argument('--model', required=True)
    ap.add_argument('--out', required=True)
    ap.add_argument('--problems', default='problems.jsonl')
    ap.add_argument('--num-ctx', type=int, default=40960)
    ap.add_argument('--num-predict', type=int, default=32768)
    ap.add_argument('--timeout', type=int, default=7200)
    ap.add_argument('--limit', type=int, default=0)
    ap.add_argument('--keep-alive', default=None, help='omit to leave the server default (the product keeps its model loaded)')
    a = ap.parse_args()

    caps = post(a.endpoint + '/api/show', {'model': a.model}, 120).get('capabilities', [])
    think = 'thinking' in caps
    done = set()
    try:
        for l in open(a.out):
            r = json.loads(l)
            if not r.get('error'):
                done.add(r['id'])
    except FileNotFoundError:
        pass
    probs = [json.loads(l) for l in open(a.problems)]
    if a.limit:
        probs = probs[:a.limit]
    print(f"model={a.model} think={think} caps={caps} todo={len([p for p in probs if p['id'] not in done])}", flush=True)
    for p in probs:
        if p['id'] in done:
            continue
        body = {'model': a.model, 'stream': False,
                'messages': [{'role': 'system', 'content': SYSTEM}, {'role': 'user', 'content': user_prompt(p)}],
                'options': {'temperature': 0.6, 'top_p': 0.95, 'top_k': 20, 'seed': 1,
                            'num_ctx': a.num_ctx, 'num_predict': a.num_predict}}
        if think:
            body['think'] = True
        if a.keep_alive is not None:
            body['keep_alive'] = a.keep_alive
        t0 = time.time()
        rec = {'id': p['id'], 'difficulty': p['difficulty'], 'model': a.model, 'think': think}
        try:
            r = post(a.endpoint + '/api/chat', body, a.timeout)
            m = r.get('message', {})
            rec.update({'content': m.get('content', ''), 'thinking': m.get('thinking', ''),
                        'done_reason': r.get('done_reason'), 'eval_count': r.get('eval_count'),
                        'prompt_eval_count': r.get('prompt_eval_count'),
                        'eval_duration': r.get('eval_duration'), 'prompt_eval_duration': r.get('prompt_eval_duration')})
        except Exception as e:
            rec['error'] = repr(e)[:500]
        rec['wall_s'] = round(time.time() - t0, 1)
        with open(a.out, 'a') as f:
            f.write(json.dumps(rec) + '\n')
        print(f"{p['id']} {p['difficulty']} {rec['wall_s']}s eval={rec.get('eval_count')} done={rec.get('done_reason')} err={rec.get('error','')[:80]}", flush=True)

if __name__ == '__main__':
    main()
