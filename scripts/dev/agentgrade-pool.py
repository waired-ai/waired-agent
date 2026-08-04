#!/usr/bin/env python3
"""Pool a directory of probe reports and print the per-case ratios.

`catalog-tool agentgrade --import` pools too, but it pools INTO the
catalog: it is how a measurement becomes a stored verdict. This is the
read-only view — several runs of the same model side by side, before
anything is committed to, which is what a comparison needs.

Pools by the same rule the probe uses: trials and failed_trials summed,
the WORST verdict kept. Two runs of one model are two samples of one
thing, not two things to compare (waired-ai/waired-agent#440).

Reads `<label>.<transport>.json` reports, as written by

    make e2e-agentgrade MODEL=<tag> TRIALS=12 JSON=<dir>/<label>.unary.json
    make e2e-agentgrade MODEL=<tag> TRIALS=12 STREAM=1 JSON=<dir>/<label>.stream.json

    python3 scripts/dev/agentgrade-pool.py <dir>

Labels are yours: name the two arms of a comparison and they sort next
to each other.

Read the RATIO, not the verdict. The verdict is worst-across-trials and
therefore a function of the trial count — any nonzero per-call failure
rate converges to a failure as trials grow.
"""
import json
import os
import sys
from collections import defaultdict

# The probe's ladder (internal/agentgrade/probe.go severity).
SEVERITY = {
    "pass": 0,
    "warn_unprompted_tool_call": 1,
    "fail_invalid_tool_arguments": 2,
    # Pre-#483 spelling, kept so an old report still ranks as the failure
    # it is rather than at the pass end of the ladder.
    "warn_invalid_tool_arguments": 2,
    "fail_no_tool_call": 3,
    "fail_unstructured_tool_call": 4,
    "fail_unknown_tool": 5,
    "fail_malformed_tool_call": 5,
    "error": 6,
}


def main(argv):
    if len(argv) != 2:
        print(__doc__.strip(), file=sys.stderr)
        return 2
    d = argv[1]

    pool = defaultdict(lambda: defaultdict(
        lambda: {"failed": 0, "trials": 0, "verdict": "pass", "kinds": set()}))
    models, transports = defaultdict(set), defaultdict(set)
    order = []

    for fn in sorted(os.listdir(d)):
        if not fn.endswith(".json") or fn.startswith("_"):
            continue
        parts = fn[: -len(".json")].split(".")
        label = ".".join(parts[:-1]) if len(parts) > 1 else parts[0]
        with open(os.path.join(d, fn), encoding="utf-8") as fh:
            rep = json.load(fh)
        models[label].add(rep.get("model", "?"))
        transports[label].add(rep.get("transport") or "?")
        if label not in order:
            order.append(label)
        for r in rep.get("results", []):
            c = pool[label][r["case"]]
            c["trials"] += r.get("trials") or rep.get("trials", 0)
            failed = r.get("failed_trials")
            if failed is None:
                # Reports predating per-trial counts say only pass/fail.
                failed = 0 if r["verdict"] == "pass" else (r.get("trials") or rep.get("trials", 0))
            c["failed"] += failed
            if failed:
                c["kinds"].add(r["verdict"])
            if SEVERITY.get(r["verdict"], 9) > SEVERITY.get(c["verdict"], 9):
                c["verdict"] = r["verdict"]

    if not pool:
        print(f"no probe reports in {d}", file=sys.stderr)
        return 2

    head = f"{'label':<16}{'case':<19}{'failed/trials':>15}   verdict / failure kinds"
    print(head)
    print("-" * len(head))
    for label in order:
        tf = tt = 0
        for case, c in pool[label].items():
            tf += c["failed"]
            tt += c["trials"]
            kinds = ", ".join(sorted(c["kinds"])) or "-"
            print(f"{label:<16}{case:<19}{c['failed']}/{c['trials']:<13}   {c['verdict']}"
                  + (f"  [{kinds}]" if c["kinds"] else ""))
        model = ", ".join(sorted(models[label]))
        tr = "+".join(sorted(transports[label]))
        print(f"{label:<16}{'TOTAL':<19}{tf}/{tt:<13}   {model} over {tr}")
        print()
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
