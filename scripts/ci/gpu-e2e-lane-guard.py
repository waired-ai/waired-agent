#!/usr/bin/env python3
"""
Guard that every GPU e2e test is actually reachable from the nightly lane.

internal/e2e/inference is built with `//go:build e2e && gpu`, and each Makefile
target pins its own `-run <regex>`. So a new `func Test...` in that package
COMPILES under the tag and is then never executed by anything — it is not a
missing target, not a red test, and not a skip: it is silence, which is the one
outcome CI cannot report.

That is not hypothetical. waired-ai/waired#1229 found e2e-vllm-clamp, -fp8 and
-spec had existed as Makefile targets with no caller anywhere, so #675, #676
and #677 shipped with a harness nothing invoked. On the day those three were
wired, waired-ai/waired-agent#958 added three more test functions and would
have been the fourth orphan. A comment asking the next author to remember was
already there, in the workflow, when that happened.

Checks:

  * every top-level Test func in internal/e2e/inference (files tagged
    `e2e && gpu`) is matched by the `-run` regex of some e2e-vllm* Makefile
    target;
  * every such target is named in installtest-inference.yml — a target that
    exists but is never invoked is the exact shape of the original defect;
  * every target named in the workflow exists in the Makefile, so the lane
    cannot call a target that was renamed or removed.

Deliberately not run: this reads source, it does not need a GPU.

An intentionally unwired test declares itself in UNWIRED below, with a reason.
Exits non-zero with one line per problem. stdlib only.
"""

import re
import sys
from pathlib import Path

E2E_DIR = Path("internal/e2e/inference")
MAKEFILE = Path("Makefile")
WORKFLOW = Path(".github/workflows/installtest-inference.yml")

# Test functions deliberately reachable from no lane, each with the reason it
# is not simply deleted. Empty is the healthy state.
UNWIRED: dict[str, str] = {}

BUILD_TAG = re.compile(r"^//go:build\s+.*\be2e\b.*&&.*\bgpu\b", re.M)
TEST_FUNC = re.compile(r"^func (Test\w+)\(", re.M)
# `e2e-vllm-quick:` etc., then the recipe line carrying -run.
TARGET = re.compile(r"^(e2e-vllm[\w-]*):\n((?:\t.*\n)+)", re.M)
RUN_FLAG = re.compile(r"-run\s+(\S+)")
# targets="a b c" — both the dispatch default and the schedule list.
TARGETS_LINE = re.compile(r'targets="([^"]+)"')
# The list is computed on a hosted runner and has to reach the VM that runs it.
# Naming a target and passing it are different claims, and the gap between them
# is where a lane that names five targets runs none.
TARGETS_HANDOFF = re.compile(r"GPU_LANE_TARGETS:\s*\$\{\{\s*steps\.targets\.outputs\.targets\s*\}\}")
# The VM echoes back what it ran; something has to compare that to the request.
WATCHER = Path("scripts/ci/gpu-lane-watch.sh")
# Anchored so a rename cannot satisfy it: a bare "lane-targets" substring is
# still present in "lane-targets-renamed", and this rule passed on exactly
# that before the guard was tested by breaking it.
TARGETS_ECHO_CHECK = re.compile(r"attr lane-targets(?![\w-])")


def fail(problems: list[str]) -> None:
    for p in problems:
        print(f"gpu-e2e-lane-guard: {p}", file=sys.stderr)
    sys.exit(1)


def main() -> None:
    problems: list[str] = []

    if not E2E_DIR.is_dir():
        fail([f"{E2E_DIR} is missing; this guard is pointing at the wrong tree"])

    tests: dict[str, str] = {}  # func name -> file it came from
    for path in sorted(E2E_DIR.glob("*_test.go")):
        body = path.read_text(encoding="utf-8")
        if not BUILD_TAG.search(body):
            continue
        for name in TEST_FUNC.findall(body):
            tests[name] = str(path)

    if not tests:
        # The guard passing because it found nothing to check is the failure
        # mode it is most likely to acquire: a rename of the package or of the
        # build tag would silence it without anyone noticing.
        fail([f"no `e2e && gpu` test functions found under {E2E_DIR} — "
              "the guard is looking in the wrong place, or the build tag moved"])

    make_body = MAKEFILE.read_text(encoding="utf-8")
    patterns: dict[str, str] = {}  # target -> -run regex
    for target, recipe in TARGET.findall(make_body):
        m = RUN_FLAG.search(recipe)
        if m:
            patterns[target] = m.group(1)

    if not patterns:
        fail(["no e2e-vllm* Makefile targets carry a -run flag — "
              "the guard cannot tell what the lane invokes"])

    wf_body = WORKFLOW.read_text(encoding="utf-8")
    wired: set[str] = set()
    for line in TARGETS_LINE.findall(wf_body):
        wired.update(line.split())

    # A target the workflow names but the Makefile does not define: the lane
    # would die with "No rule to make target" on a nightly nobody watches.
    for target in sorted(wired):
        if target not in patterns:
            problems.append(
                f"{WORKFLOW} runs `make {target}`, which the Makefile does not define "
                f"(or which carries no -run flag)")

    # A target that exists but nothing invokes: the original defect.
    for target in sorted(patterns):
        if target not in wired:
            problems.append(
                f"Makefile target `{target}` is never named in {WORKFLOW} — "
                "it exists and nothing runs it, which is what waired-ai/waired#1229 was")

    # Named is not run. The list is built on a hosted runner and consumed on a
    # VM, so the handoff is a place the chain can break silently: the workflow
    # would still name five targets, the guard above would still pass, and the
    # lane would run whatever the VM defaulted to.
    if not TARGETS_HANDOFF.search(wf_body):
        problems.append(
            f"{WORKFLOW} computes a target list but does not pass it to the VM as "
            "GPU_LANE_TARGETS: ${{ steps.targets.outputs.targets }} — the lane would "
            "run something other than what this file just checked")
    if not WATCHER.is_file():
        problems.append(f"{WATCHER} is missing; nothing compares what ran to what was asked for")
    elif not TARGETS_ECHO_CHECK.search(WATCHER.read_text(encoding="utf-8")):
        problems.append(
            f"{WATCHER} no longer reads the lane-targets echo-back — without it "
            "'the workflow named it' is the only evidence that a target ran")

    # And the point of the whole file: a test no wired target selects.
    for name, path in sorted(tests.items()):
        if name in UNWIRED:
            continue
        if any(re.search(patterns[t], name) for t in wired if t in patterns):
            continue
        problems.append(
            f"{path}: {name} is matched by no wired target's -run regex. "
            "It compiles under `e2e && gpu` and nothing executes it. Add a Makefile "
            f"target and name it in {WORKFLOW}, or declare it in this guard's UNWIRED "
            "map with a reason")

    if problems:
        fail(problems)

    print(f"gpu-e2e-lane guard OK: {len(tests)} test(s), "
          f"{len(wired)} wired target(s)")


if __name__ == "__main__":
    main()
