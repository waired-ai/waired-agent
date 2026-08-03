#!/usr/bin/env bash
# Drive REAL Claude Code against one model through the real gateway.
#
# The probe grades what our classifier makes of a turn; the contract
# checker grades the bytes. Neither can settle what the CLIENT accepts —
# only the client can. This stands up the probe stack (TestHoldStack),
# points Claude Code at it, and reports which tools it actually ran.
#
#   scripts/dev/agentgrade-live-claude.sh qwen3.5:9b-q4_K_M
#   scripts/dev/agentgrade-live-claude.sh qwen35-9b-noparse /tmp/out
#
# Needs the `claude` CLI and a GPU host with the model present. Writes
# <out>/live.jsonl (the stream-json transcript) and prints the tool calls
# Claude Code executed.
set -uo pipefail

tag=${1:-}
out=${2:-$(mktemp -d)}
if [ -z "$tag" ]; then
	echo "usage: $0 <ollama tag> [output dir]" >&2
	exit 2
fi
for c in claude ollama go; do
	command -v "$c" >/dev/null 2>&1 || {
		echo "$0: $c is not on PATH" >&2
		exit 2
	}
done

repo=$(git rev-parse --show-toplevel)
mkdir -p "$out"
url_file="$out/gateway.url"
rm -f "$url_file"

# A sandbox with something findable in it, so the run exercises a real
# two-step search-then-read rather than a tool call against nothing.
work="$out/workspace"
rm -rf "$work"
mkdir -p "$work"
printf 'quality_tier lives here\n' >"$work/model_picker.go"

WAIRED_HOLD_URL_FILE="$url_file" WAIRED_HOLD_SECONDS=900 \
	WAIRED_AGENTGRADE_MODEL="$tag" WAIRED_AGENTGRADE_NO_PULL="${NO_PULL:-}" \
	go test -C "$repo" -tags e2e -count=1 -v -timeout 20m \
	-run TestHoldStack ./internal/e2e/agentgrade/... >"$out/hold.log" 2>&1 &
hold=$!
trap 'rm -f "$url_file"; wait "$hold" 2>/dev/null' EXIT

for _ in $(seq 1 180); do
	[ -s "$url_file" ] && break
	sleep 1
done
if [ ! -s "$url_file" ]; then
	echo "$0: the gateway never came up; see $out/hold.log" >&2
	exit 1
fi
base=$(cat "$url_file")
echo "gateway: $base"

# Warm the weights before the client's first turn. A cold model can take
# longer to load than the client's own timeout, and the client reports
# that as the model failing to answer rather than as a slow start.
echo "warming $tag …"
curl -sS --max-time 600 "$base/v1/messages" \
	-H 'content-type: application/json' \
	-d '{"model":"waired/test","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}' \
	>/dev/null || echo "$0: warm-up request failed; continuing anyway" >&2

# ANTHROPIC_API_KEY is a placeholder: the gateway is local and
# unauthenticated, and setting it stops the CLI reaching for a
# claude.ai login. stdin is closed so the CLI does not wait on a pipe.
(
	cd "$work" || exit 1
	ANTHROPIC_BASE_URL="$base" ANTHROPIC_API_KEY=dummy \
		timeout 900 claude --print --model waired/test \
		--allowedTools "Read,Grep,Glob" \
		--output-format stream-json --verbose \
		"Search this directory for files mentioning quality_tier, then read the first match. Use your tools." \
		</dev/null >"$out/live.jsonl" 2>"$out/live.err"
)
echo "claude exit=$? — transcript in $out/live.jsonl"

python3 - "$out/live.jsonl" <<'PY'
import json, sys
tools, texts, result = [], [], None
for line in open(sys.argv[1], encoding="utf-8"):
    d = json.loads(line)
    if d.get("type") == "assistant":
        for b in d.get("message", {}).get("content", []):
            if b.get("type") == "tool_use":
                tools.append(f"{b['name']}({json.dumps(b.get('input'))[:100]})")
            elif b.get("type") == "text" and b.get("text", "").strip():
                texts.append(b["text"][:160])
    elif d.get("type") == "result":
        result = d
print("\ntools Claude Code actually executed:")
for t in tools or ["  (none)"]:
    print(f"  {t}")
print("\nvisible assistant text:")
for t in texts or ["  (none)"]:
    print(f"  {t!r}")
if result:
    print(f"\nis_error={result.get('is_error')} turns={result.get('num_turns')}")
PY
