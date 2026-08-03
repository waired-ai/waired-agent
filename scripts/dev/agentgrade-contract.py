#!/usr/bin/env python3
"""Check captured Anthropic turns against the contract a client parses.

Not "did a tool call come back" — internal/agentgrade answers that, and
answers it by decoding with our own reader, which can only confirm that
our reader agrees with our writer. This reads the RAW bytes and asks
whether a client that had never seen our code would accept the turn:

  * unary  — a `message` with an assistant role, usage, known block
             types, a non-empty tool_use id, and a stop_reason that
             agrees with the blocks present
  * stream — message_start first and message_stop last, every
             content_block index opened before it is delta'd and closed
             before the turn ends, `input_json_delta` fragments that
             concatenate to valid JSON, a message_delta carrying the
             stop_reason
  * both   — tool arguments against the schema the tool was OFFERED
             with, when the capture carries one

It also counts, separately from violations, turns whose visible text
holds reasoning markup (`</think>` and friends). That is not a protocol
violation — the bytes are well formed — but it means the client renders
the model's private monologue as the answer, and no structural check
sees it. It is reported rather than failed for the same reason: whether
it is a defect depends on what the engine was asked to do.

Reads what internal/e2e/agentgrade's TestCaptureRawTurns writes:

    <case>.<unary|stream>.<trial>.<http status>.raw
    _tools.json                       (optional; enables the schema check)

Any producer emitting those names works — the format is the contract,
not the probe.

    python3 scripts/dev/agentgrade-contract.py /tmp/cap

Exits non-zero when a turn violates the contract, so it can gate.
"""
import json
import os
import re
import sys
from collections import Counter

RAW = re.compile(r"(?P<case>.+)\.(?P<transport>unary|stream)\.\d+\.(?P<status>\d+)\.raw$")
FRAME = re.compile(rb"event: *([^\n]+)\ndata: *([^\n]*)\n")
LEAK = re.compile(r"</?think>|<\|channel\|>|<\|start\|>assistant", re.I)

# JSON has one numeric type, so an integral number satisfies "number"
# as well as "integer"; rejecting 3 for a number would fail a correct
# call. bool is a Python int subclass and must not pass as a number.
JSON_TYPES = {"string": (str,), "boolean": (bool,), "array": (list,),
              "object": (dict,), "null": (type(None),)}

TEXT_DELTAS = ("text_delta", "thinking_delta", "signature_delta")


def type_ok(want, value):
    if want in ("number", "integer"):
        if isinstance(value, bool) or not isinstance(value, (int, float)):
            return False
        return want == "number" or float(value).is_integer()
    if want not in JSON_TYPES:
        return True  # a type we do not model is not a finding
    if want != "boolean" and isinstance(value, bool):
        return False
    return isinstance(value, JSON_TYPES[want])


def check_args(tools, name, args, errs, where):
    """Validate a call's arguments against the schema it was offered with.

    Deliberately shallow — required properties, the declared type of
    each present property, and undeclared properties only where the
    schema itself set additionalProperties:false. Every check follows
    something the schema DECLARES; nothing here invents a stricter
    contract than the model was shown.
    """
    if tools is None:
        return
    if name not in tools:
        errs.append(f"{where}: tool {name!r} was never offered")
        return
    schema = tools[name] or {}
    if not isinstance(args, dict):
        errs.append(f"{where}: {name} arguments are {type(args).__name__}, not an object")
        return
    props = schema.get("properties") or {}
    for req in schema.get("required") or []:
        if req not in args:
            errs.append(f"{where}: {name} is missing required argument {req!r}")
    closed = schema.get("additionalProperties") is False
    for k, v in sorted(args.items()):
        if k not in props:
            if closed:
                errs.append(f"{where}: {name} argument {k!r} is undeclared "
                            "and the schema forbids extras")
            continue
        want = props[k].get("type")
        wants = [want] if isinstance(want, str) else (want or [])
        if wants and not any(type_ok(w, v) for w in wants):
            errs.append(f"{where}: {name} argument {k!r} is "
                        f"{type(v).__name__}, the schema declares {' or '.join(wants)}")


def check_unary(raw, status, tools, errs, leaks):
    if status != 200:
        errs.append(f"HTTP {status}: {raw[:200]!r}")
        return 0
    try:
        m = json.loads(raw)
    except ValueError as e:
        errs.append(f"body is not JSON: {e}")
        return 0
    if m.get("type") != "message":
        errs.append(f"type is {m.get('type')!r}, want 'message'")
    if m.get("role") != "assistant":
        errs.append(f"role is {m.get('role')!r}, want 'assistant'")
    if not isinstance(m.get("usage"), dict):
        errs.append("no usage object")
    blocks = m.get("content")
    if not isinstance(blocks, list):
        errs.append("content is not a list")
        return 0

    ntool = nvisible = 0
    for i, b in enumerate(blocks):
        bt = b.get("type")
        if bt not in ("text", "thinking", "redacted_thinking", "tool_use"):
            errs.append(f"content[{i}]: unknown block type {bt!r}")
        if bt == "text":
            nvisible += 1
            if b.get("text") and LEAK.search(b["text"]):
                leaks.append(f"content[{i}]: {LEAK.search(b['text']).group(0)!r} in visible text")
        if bt == "tool_use":
            ntool += 1
            nvisible += 1
            if not isinstance(b.get("id"), str) or not b["id"]:
                errs.append(f"content[{i}]: tool_use id is {b.get('id')!r}")
            check_args(tools, b.get("name"), b.get("input"), errs, f"content[{i}]")
    if ntool and m.get("stop_reason") != "tool_use":
        errs.append(f"a tool_use block is present but stop_reason is {m.get('stop_reason')!r}")
    # Thinking is not an answer. A turn that produced only thinking and
    # reported a clean end_turn is indistinguishable from a model that
    # chose to say nothing (waired-ai/waired-agent#442).
    if not nvisible and m.get("stop_reason") == "end_turn":
        errs.append("no text and no tool_use, reported as a clean end_turn")
    return ntool


def check_stream(raw, status, tools, errs, leaks):
    if status != 200:
        errs.append(f"HTTP {status}: {raw[:200]!r}")
        return 0
    try:
        frames = [(e.decode(), json.loads(p)) for e, p in FRAME.findall(raw)]
    except ValueError as e:
        errs.append(f"an SSE frame's data is not JSON: {e}")
        return 0
    if not frames:
        errs.append("no SSE frames parsed")
        return 0

    kinds = [k for k, _ in frames]
    if kinds[0] != "message_start":
        errs.append(f"first frame is {kinds[0]!r}, want message_start")
    elif not isinstance(frames[0][1].get("message", {}).get("usage"), dict):
        errs.append("message_start carries no usage")
    if kinds[-1] != "message_stop":
        errs.append(f"last frame is {kinds[-1]!r}, want message_stop")
    if "message_delta" not in kinds:
        errs.append("no message_delta, so the client never learns stop_reason")

    open_blocks, closed, ntool, stop_reason = {}, [], 0, None
    for kind, p in frames:
        i = p.get("index")
        if kind == "content_block_start":
            if i in open_blocks:
                errs.append(f"index {i} started again without a stop")
            cb = p.get("content_block") or {}
            open_blocks[i] = {"type": cb.get("type"), "name": cb.get("name"), "json": "", "text": ""}
            if cb.get("type") == "tool_use":
                ntool += 1
                if not isinstance(cb.get("id"), str) or not cb["id"]:
                    errs.append(f"index {i}: tool_use id is {cb.get('id')!r}")
        elif kind == "content_block_delta":
            if i not in open_blocks:
                errs.append(f"delta for index {i} with no open block")
                continue
            d = p.get("delta") or {}
            if d.get("type") == "input_json_delta":
                open_blocks[i]["json"] += d.get("partial_json") or ""
            elif d.get("type") == "text_delta":
                open_blocks[i]["text"] += d.get("text") or ""
            elif d.get("type") not in TEXT_DELTAS:
                errs.append(f"index {i}: unknown delta type {d.get('type')!r}")
        elif kind == "content_block_stop":
            b = open_blocks.pop(i, None)
            if b is None:
                errs.append(f"stop for index {i} with no open block")
                continue
            if b["type"] == "tool_use":
                try:
                    args = json.loads(b["json"] or "{}")
                except ValueError as e:
                    errs.append(f"index {i}: the accumulated partial_json is not JSON: {e}")
                else:
                    check_args(tools, b["name"], args, errs, f"index {i}")
            if b["type"] == "text" and b["text"] and LEAK.search(b["text"]):
                leaks.append(f"index {i}: {LEAK.search(b['text']).group(0)!r} in visible text")
            closed.append(b["type"])
        elif kind == "message_delta":
            stop_reason = (p.get("delta") or {}).get("stop_reason", stop_reason)

    for i in sorted(open_blocks, key=str):
        errs.append(f"index {i} was never stopped")
    if ntool and stop_reason != "tool_use":
        errs.append(f"a tool_use was streamed but stop_reason is {stop_reason!r}")
    if not any(t in ("text", "tool_use") for t in closed) and stop_reason == "end_turn":
        errs.append("no text and no tool_use, reported as a clean end_turn")
    return ntool


def main(argv):
    if len(argv) != 2:
        print(__doc__.strip(), file=sys.stderr)
        return 2
    d = argv[1]

    tools = None
    tools_path = os.path.join(d, "_tools.json")
    if os.path.exists(tools_path):
        with open(tools_path, encoding="utf-8") as fh:
            tools = {t["name"]: t.get("input_schema") for t in json.load(fh)}
    else:
        print(f"note: no {tools_path} — structural checks only, no schema check\n")

    rows, violations, leaks = Counter(), [], []
    for fn in sorted(os.listdir(d)):
        if fn.endswith(".transport-error"):
            with open(os.path.join(d, fn), encoding="utf-8") as fh:
                violations.append((fn, ["transport error: " + fh.read()[:200]]))
            continue
        m = RAW.match(fn)
        if not m:
            continue
        case, transport, status = m["case"], m["transport"], int(m["status"])
        with open(os.path.join(d, fn), "rb") as fh:
            raw = fh.read()
        errs, leak = [], []
        check = check_unary if transport == "unary" else check_stream
        ntool = check(raw, status, tools, errs, leak)
        rows[(case, transport, "bad" if errs else "ok")] += 1
        rows[(case, transport, "tool")] += 1 if ntool else 0
        if leak:
            rows[(case, transport, "leak")] += 1
            leaks.append((fn, leak))
        if errs:
            violations.append((fn, errs))

    if not rows:
        print(f"no captured turns in {d}", file=sys.stderr)
        return 2

    head = (f"{'case':<18}{'transport':<10}{'contract OK':>12}"
            f"{'violations':>12}{'tool_use':>10}{'CoT leak':>10}")
    print(head)
    print("-" * len(head))
    for case in sorted({c for c, _, _ in rows}):
        for tr in ("unary", "stream"):
            ok, bad = rows[(case, tr, "ok")], rows[(case, tr, "bad")]
            if not (ok or bad):
                continue
            print(f"{case:<18}{tr:<10}{ok:>12}{bad:>12}"
                  f"{rows[(case, tr, 'tool')]:>10}{rows[(case, tr, 'leak')]:>10}")

    if leaks:
        print(f"\nreasoning markup in the visible answer ({len(leaks)} turns) — "
              "well-formed bytes, but the client renders the model's private "
              "monologue as its reply:")
        for fn, ls in leaks[:5]:
            for line in ls:
                print(f"  {fn}: {line}")
        if len(leaks) > 5:
            print(f"  … and {len(leaks) - 5} more turns")

    if violations:
        print(f"\ncontract violations ({len(violations)} turns):")
        for fn, errs in violations:
            for line in errs:
                print(f"  {fn}: {line}")
        return 1
    print("\nno contract violations")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
