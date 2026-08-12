#!/usr/bin/env bash
# Run the standalone guard and self-test scripts that ci.yml's `lint` job
# runs, so "local checks pass" can mean what CONTRIBUTING.md says it means.
#
# CONTRIBUTING.md §"Building and testing" opens with "the same commands CI
# runs" and lists eight. The lint job additionally runs twenty standalone
# scripts out of scripts/ci/, and did not mention them anywhere — so a
# contributor could run everything the document asked, truthfully report
# that local checks passed, and still take a red lint. On this repo that
# costs a second full CI wave against a hosted concurrency ceiling of about
# three jobs, delaying every other PR in flight (waired-agent#749).
#
# The list is DERIVED from ci.yml rather than kept here. The prior art is
# scripts/ci/install-script-lint.sh, whose hand-kept list "had drifted eight
# scripts behind — including the testnet gate's own — because a new guard is
# exactly the kind of file whose enumeration gets forgotten". Globbing is
# not available here (the guards live beside scripts that are NOT part of
# the lint job), so the job definition is the source of truth instead.
#
# Not covered, and said out loud at the end rather than quietly omitted:
# gofmt / go vet / go test / golangci-lint / the prod build smoke / the
# license check / the proto dependency allowlist / the agent-grade guard.
# Those are the CONTRIBUTING list plus the inline steps; this script is the
# part that had no local surface at all.
set -euo pipefail

# scripts/ci/testnet-gate-guard.sh and scripts/ci/routing-sentinel-paths-guard.sh
# use `mapfile`, a bash 4 builtin. The bash macOS ships is 3.2, and ci.yml
# invokes the guards as `bash scripts/ci/...` — an explicit interpreter, so
# their `#!/usr/bin/env bash` shebang does not get to pick a newer one.
# Re-exec under a bash that can run them rather than letting two guards die
# with "mapfile: command not found" halfway through.
if [ -z "${CI_LINT_LOCAL_REEXEC:-}" ] && [ "${BASH_VERSINFO[0]}" -lt 4 ]; then
  for candidate in /opt/homebrew/bin/bash /usr/local/bin/bash "$(command -v bash5 2>/dev/null || true)"; do
    [ -n "${candidate}" ] && [ -x "${candidate}" ] || continue
    export CI_LINT_LOCAL_REEXEC=1
    exec "${candidate}" "$0" "$@"
  done
  echo "error: this needs bash 4+ (two of the guards use mapfile) and only bash ${BASH_VERSION} was found." >&2
  echo "       On macOS: brew install bash. Refusing to run a subset that would look like a pass." >&2
  exit 1
fi

cd "$(dirname "${BASH_SOURCE[0]}")/../.."

workflow=".github/workflows/ci.yml"
[ -f "${workflow}" ] || { echo "error: ${workflow} not found" >&2; exit 1; }

# The `lint` job's line range. Jobs sit at two-space indent, so the job ends
# where the next one begins; taking the range rather than the whole file
# keeps out scripts/ci scripts other jobs run under conditions this cannot
# reproduce (install-scripts runs autostart-exec-guard.sh after apt-getting
# busybox, for one).
lint_start=$(grep -nE '^  lint:$' "${workflow}" | head -1 | cut -d: -f1 || true)
[ -n "${lint_start}" ] || { echo "error: no 'lint:' job in ${workflow}" >&2; exit 1; }
lint_end=$(awk -v s="${lint_start}" 'NR>s && /^  [a-z][a-z0-9_-]*:$/ {print NR-1; exit}' "${workflow}")
[ -n "${lint_end}" ] || lint_end=$(wc -l < "${workflow}")

# `run:` lines that invoke something under scripts/ci. Single-line steps
# only, which is every guard: the five multi-line `run: |` blocks in this
# job are gofmt / go vet / the prod smoke / the proto allowlist / the
# license check, none of which live in scripts/ci.
cmds=()
while IFS= read -r line; do
  [ -n "${line}" ] && cmds+=("${line}")
done < <(
  sed -n "${lint_start},${lint_end}p" "${workflow}" |
    sed -nE 's/^[[:space:]]*run:[[:space:]]+((bash|python3)[[:space:]]+scripts\/ci\/[^[:space:]]+|go run \.\/scripts\/ci\/[^[:space:]]+)[[:space:]]*$/\1/p'
)

if [ "${#cmds[@]}" -eq 0 ]; then
  echo "error: derived no guards from ${workflow} lines ${lint_start}-${lint_end}." >&2
  echo "       The job's shape changed; fix this script rather than reporting a pass." >&2
  exit 1
fi

# Prerequisites, checked up front so a missing interpreter is one clear line
# at the start rather than a confusing failure in the middle.
missing=()
command -v go >/dev/null 2>&1 || missing+=("go")
command -v python3 >/dev/null 2>&1 || missing+=("python3")
command -v node >/dev/null 2>&1 || missing+=("node (docs-preview-select-test.sh runs the selector helper under it)")

echo "ci-lint-local: ${#cmds[@]} guard(s) from ${workflow} lines ${lint_start}-${lint_end}"
if [ "${#missing[@]}" -gt 0 ]; then
  echo "ci-lint-local: missing prerequisites — the guards needing them will fail, not be skipped:"
  printf '  - %s\n' "${missing[@]}"
fi
echo

failed=()
for cmd in "${cmds[@]}"; do
  printf '==> %s\n' "${cmd}"
  # Word-split deliberately: these are fixed `bash <path>` / `python3 <path>`
  # / `go run <pkg>` forms taken from the workflow, not user input.
  # shellcheck disable=SC2086
  if ! ${cmd}; then
    failed+=("${cmd}")
  fi
  echo
done

echo "----"
if [ "${#failed[@]}" -gt 0 ]; then
  echo "ci-lint-local: ${#failed[@]} of ${#cmds[@]} failed:"
  printf '  - %s\n' "${failed[@]}"
  exit 1
fi
echo "ci-lint-local: all ${#cmds[@]} passed."
echo
echo "This is the scripts/ci half of the lint job only. Still to run, and NOT"
echo "covered here — see CONTRIBUTING.md §\"Building and testing\":"
echo "  gofmt -l .   go vet ./...   golangci-lint run   go test ./... -timeout 10m"
echo "  go build -tags prod ./...   make verify-cross"
echo "golangci-lint in particular is easy to believe you ran: if it is not"
echo "installed, nothing tells you its result is simply unknown (#737)."
