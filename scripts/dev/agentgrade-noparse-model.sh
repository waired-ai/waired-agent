#!/usr/bin/env bash
# Derive a parser-less twin of an ollama model.
#
# Newer ollama models declare a native Go RENDERER and PARSER rather than
# a Go-template (`ollama show --modelfile` shows `RENDERER qwen3.5` /
# `PARSER qwen3.5`). When such a model's tool calls fail, "the model
# emitted the wrong thing" and "the engine's parser rejected the right
# thing" look identical from outside.
#
# This builds a twin from the same blob that keeps the RENDERER — so the
# prompt the model sees is byte-identical — and drops the PARSER, so
# ollama hands the raw assistant text back instead of extracting tool
# calls from it. The only variable left is who parses the output, which
# is the question.
#
#   scripts/dev/agentgrade-noparse-model.sh qwen3.5:9b-q4_K_M qwen35-9b-noparse
#   make e2e-agentgrade MODEL=qwen35-9b-noparse TRIALS=12 NO_PULL=1
#
# NO_PULL is required: a locally created model is not in the registry, so
# `ollama pull` answers "pull model manifest: file does not exist".
#
# Measured with this (waired-ai/waired-agent#426): bypassing ollama's
# parser took qwen3.5:9b from 9 failures in 72 case-trials to 0, and the
# gateway's own recovery (#409) produced calls real Claude Code executed
# — while the visible text then carried the model's raw chain-of-thought,
# because the parser splits the reasoning channel too.
#
# `ollama create` only writes a manifest against blobs that already
# exist: no download, seconds to build. Remove with `ollama rm`.
set -euo pipefail

if [ "$#" -ne 2 ]; then
	echo "usage: $0 <source-tag> <derived-name>" >&2
	exit 2
fi
src=$1
dst=$2

command -v ollama >/dev/null 2>&1 || {
	echo "$0: ollama is not on PATH" >&2
	exit 2
}

modelfile=$(mktemp)
trap 'rm -f "$modelfile"' EXIT

# FROM carries the blob, PARAMETER the sampler settings the tag ships
# with. PARSER is what we are dropping; TEMPLATE is replaced below.
ollama show --modelfile "$src" | grep -E "^(FROM|RENDERER|PARAMETER) " >"$modelfile" || true

if ! grep -q "^RENDERER " "$modelfile"; then
	echo "$0: $src declares no native RENDERER, so there is no parser to drop." >&2
	echo "  Template-rendered models parse tool calls from the template itself;" >&2
	echo "  edit its TEMPLATE instead." >&2
	exit 1
fi

# The empty {{ if .Tools }}{{ end }} is load-bearing. ollama grants the
# "tools" capability from the variables a template mentions, so without
# it every request is refused with "<model> does not support tools" —
# and with it, the RENDERER still builds the real prompt.
printf 'TEMPLATE """{{ if .Tools }}{{ end }}{{ .Prompt }}"""\n' >>"$modelfile"

ollama create "$dst" -f "$modelfile"

if ollama show --modelfile "$dst" | grep -q "^PARSER "; then
	echo "$0: $dst still declares a PARSER — the twin is not parser-less" >&2
	exit 1
fi
echo "$dst created from $src with no PARSER"
