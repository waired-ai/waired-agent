#!/usr/bin/env bash
# Re-measure the whole measurable catalog, both transports.
#
# `make e2e-agentgrade` runs ONE model on ONE transport. A catalog sweep is
# every ollama-servable bundled variant times two transports — 34 invocations
# today — and typing those out is how a model gets skipped, or measured on one
# path only, which is the silent mixing #426 introduced the transport field to
# end.
#
# The target list comes from `catalog-tool agentgrade --sweep-targets`, which
# derives it from the manifests and the store. A new manifest is therefore in
# the sweep the day it lands, without anyone remembering to add it here.
#
# Reports land as <dir>/<model-id>.<variant-id>.<transport>.json — what
# scripts/dev/agentgrade-pool.py reads for the side-by-side view, and what
# `catalog-tool agentgrade --import a --import b` pools into the store.
#
#   scripts/dev/agentgrade-sweep.sh <out-dir> [TRIALS]
#
# Resumable: a target whose report already exists is skipped, so an
# interrupted sweep continues rather than restarting. Delete a report to
# re-measure just that one.
set -euo pipefail

out=${1:?usage: agentgrade-sweep.sh <out-dir> [TRIALS]}
trials=${2:-12}
mkdir -p "$out"

# The probe finds its engine with exec.LookPath and SKIPS when it misses, so a
# sweep run without ollama on PATH reports 34 skips and exits 0. That reads as
# a completed sweep with no reports, which is the worst possible outcome of a
# three-hour run — hence a hard stop here instead.
if ! command -v ollama >/dev/null 2>&1; then
	echo "agentgrade-sweep: ollama is not on PATH." >&2
	echo "  The probe would skip every target and still exit 0." >&2
	echo "  export PATH=<dir containing the agent-managed ollama>:\$PATH" >&2
	exit 2
fi

# Every report in one sweep must carry the same agent_revision or --import
# refuses to pool them ("not samples of the same thing"). The Makefile stamps
# it from HEAD at each invocation, so a commit mid-sweep splits the run in two.
if [ -n "$(git status --porcelain --untracked-files=no)" ]; then
	echo "agentgrade-sweep: working tree is dirty — every report would be" >&2
	echo "  stamped <sha>-dirty, which is not a revision anyone can go back to." >&2
	echo "  Commit first, and do not commit again until the sweep finishes." >&2
	exit 2
fi
head_sha=$(git rev-parse HEAD)

mapfile -t targets < <(go run ./cmd/catalog-tool agentgrade --sweep-targets)
echo "agentgrade-sweep: ${#targets[@]} variants x 2 transports at ${trials} trials -> ${out}"
echo "agentgrade-sweep: HEAD ${head_sha}"

for t in "${targets[@]}"; do
	read -r model variant tag <<<"$t"
	for transport in unary stream; do
		json="${out}/${model}.${variant}.${transport}.json"
		if [ -s "$json" ]; then
			echo "  have  ${model}/${variant} ${transport}"
			continue
		fi
		if [ "$(git rev-parse HEAD)" != "$head_sha" ]; then
			echo "agentgrade-sweep: HEAD moved mid-sweep; the reports would not pool." >&2
			exit 2
		fi
		echo "  run   ${model}/${variant} ${transport} (${tag})"
		stream=""
		[ "$transport" = stream ] && stream=1
		make e2e-agentgrade MODEL="$tag" TRIALS="$trials" STREAM="$stream" JSON="$json"
	done
done

echo
echo "sweep complete. Review with:"
echo "  python3 scripts/dev/agentgrade-pool.py ${out}"
echo
echo "Then import each variant (both transports in ONE command — they pool):"
for t in "${targets[@]}"; do
	read -r model variant _ <<<"$t"
	echo "  go run ./cmd/catalog-tool agentgrade \\"
	echo "    --import ${out}/${model}.${variant}.unary.json \\"
	echo "    --import ${out}/${model}.${variant}.stream.json \\"
	echo "    --engine-version <ver> --host <hardware class> --retrieved <YYYY-MM-DD>"
done
