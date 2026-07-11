#!/usr/bin/env bash
# Hermetic self-test for the renovate-review harness (issue #532): drives run.sh
# end-to-end with a stubbed `gh` and an injected MOCK_VERDICT — no network, no
# Anthropic API, no GitHub. Asserts the stage -> render -> idempotent-upsert path:
#   - with no existing comment, run.sh CREATEs one;
#   - with an existing marked comment, run.sh PATCHes it (no duplicate).
#
# Needs: bash, jq, coreutils (timeout). Run locally or as the workflow's guard step.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT
fail() { echo "SELFTEST FAIL: $*" >&2; exit 1; }

command -v jq >/dev/null 2>&1 || fail "jq is required"

# Mock PR metadata: a Renovate branch carrying the dependencies label.
cat > "${WORK}/meta.json" <<'JSON'
{"number":123,"title":"chore(deps): update github.com/foo/bar to v1.3.0",
 "body":"Release Notes for foo/bar v1.3.0 ...","headRefName":"renovate/foo-bar-1.x",
 "labels":[{"name":"dependencies"}],"url":"https://example.test/pr/123",
 "author":{"login":"app/renovate"},"additions":1,"deletions":1,"changedFiles":1}
JSON

# Mock verdict (already carries the marker, like a real claude run would).
cat > "${WORK}/verdict.md" <<'MD'
<!-- renovate-review-bot -->
### 🤖 Renovate breakage review — **PASS**

mock verdict body.
MD

# Stub `gh`: records the write action taken (CREATE / PATCH) to STUB_LOG, and
# emulates the comment-list `--jq` output via STUB_EXISTING_COMMENT_ID.
mkdir -p "${WORK}/bin"
cat > "${WORK}/bin/gh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
log_file="${STUB_LOG:?}"
if [ "${1:-}" = "api" ]; then
  is_patch=no
  for a in "$@"; do
    if [ "$a" = "PATCH" ]; then is_patch=yes; fi
  done
  if [ "${is_patch}" = "yes" ]; then
    echo "PATCH" >> "${log_file}"
  elif [ -n "${STUB_EXISTING_COMMENT_ID:-}" ]; then
    # what `--jq '.[] | select(...) | .id'` would print for an existing comment
    printf '%s\n' "${STUB_EXISTING_COMMENT_ID}"
  fi
  exit 0
fi
case "${1:-} ${2:-}" in
  "pr view")    cat "${STUB_META:?}" ;;
  "pr diff")    printf 'diff --git a/go.mod b/go.mod\n-\tgithub.com/foo/bar v1.2.3\n+\tgithub.com/foo/bar v1.3.0\n' ;;
  "pr checks")  printf 'ci\tpass\n' ;;
  "pr list")    printf '123\n' ;;
  "pr comment") echo "CREATE" >> "${log_file}" ;;
  *) echo "gh stub: unhandled invocation: $*" >&2; exit 1 ;;
esac
SH
chmod +x "${WORK}/bin/gh"

run_one() {
  : > "${WORK}/log"
  rm -rf "${WORK}/out"
  OUT_DIR="${WORK}/out" \
  PATH="${WORK}/bin:${PATH}" \
  GH_TOKEN=fake GH_REPO=test/repo WR_PR=123 \
  STUB_LOG="${WORK}/log" STUB_META="${WORK}/meta.json" \
  STUB_EXISTING_COMMENT_ID="${1:-}" \
  MOCK_VERDICT="${WORK}/verdict.md" SETTLE_TIMEOUT=5 \
    bash "${HERE}/run.sh" >/dev/null 2>&1
}

# Scenario 1: no existing comment -> CREATE.
run_one "" || fail "run.sh failed (create scenario)"
[ -f "${WORK}/out/verdict.md" ] || fail "verdict.md not staged"
grep -q 'renovate-review-bot' "${WORK}/out/verdict.md" || fail "marker missing from verdict"
grep -qx 'CREATE' "${WORK}/log" || fail "expected a comment CREATE"
grep -qx 'PATCH' "${WORK}/log" && fail "unexpected PATCH on first run"

# Scenario 2: existing marked comment -> PATCH (idempotent, no duplicate).
run_one "999" || fail "run.sh failed (update scenario)"
grep -qx 'PATCH' "${WORK}/log" || fail "expected a comment PATCH"
grep -qx 'CREATE' "${WORK}/log" && fail "must not CREATE when a comment already exists"

echo "SELFTEST PASS"
