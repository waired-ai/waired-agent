#!/usr/bin/env bash
# nightly-file-issue-test.sh — self-test for nightly-file-issue.sh.
#
# The interesting half of that script (dedupe vs create) only runs when it
# talks to GitHub, so DRY_RUN cannot reach it and neither can a nightly that
# is green. A stub `gh` on PATH exercises the real decision without a token,
# a network call, or a deliberately broken nightly.
#
# Same arrangement as base-ref-guard-test.sh / installer-mirror-guard-test.sh:
# a guard whose input comes from outside git gets a self-test so its decision
# table cannot ship unexercised.
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
subject="${root}/scripts/ci/nightly-file-issue.sh"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

pass=0; fail=0
ok()  { printf '  ok   %s\n' "$*"; pass=$((pass+1)); }
bad() { printf '  FAIL %s\n' "$*" >&2; fail=$((fail+1)); }

# Stub `gh`: `issue list` prints whatever OPEN_TITLES holds, `issue create`
# records that it was called (and with what title).
mkdir -p "${work}/bin"
cat >"${work}/bin/gh" <<'STUB'
#!/usr/bin/env bash
if [ "$1" = "issue" ] && [ "$2" = "list" ]; then
  printf '%s' "${OPEN_TITLES:-}"
  exit 0
fi
if [ "$1" = "issue" ] && [ "$2" = "create" ]; then
  while [ $# -gt 0 ]; do
    [ "$1" = "--title" ] && printf '%s' "$2" > "${CREATED_MARKER}"
    shift
  done
  exit 0
fi
exit 1
STUB
chmod +x "${work}/bin/gh"

run_subject() {
  CREATED_MARKER="${work}/created" \
  OPEN_TITLES="${1}" \
  PATH="${work}/bin:${PATH}" \
  GH_TOKEN=stub GITHUB_REPOSITORY=waired-ai/waired-agent \
  TITLE_TAG='[nightly-installtest]' \
  TITLE='[nightly-installtest] install+inference (linux) failed' \
  BODY='body' \
    bash "${subject}" >"${work}/out" 2>&1
}

# 1. Nothing open -> file one.
rm -f "${work}/created"
run_subject "" || bad "empty issue list: expected exit 0"
if [ -f "${work}/created" ]; then
  ok "files an issue when nothing is open"
else
  bad "did not file an issue when nothing is open: $(cat "${work}/out")"
fi

# 2. Same tag already open -> stay quiet. The whole point of the dedup key:
#    ten issues for one broken nightly is how a lane gets muted.
rm -f "${work}/created"
run_subject '[nightly-installtest] install+inference (macos) failed' || bad "dedupe: expected exit 0"
if [ -f "${work}/created" ]; then
  bad "filed a duplicate while a tagged issue was open"
else
  ok "does not file a duplicate while a tagged issue is open"
fi

# 3. A DIFFERENT nightly's issue is open -> still file ours. GitHub's search
#    tokenizer drops the brackets, so `in:title [nightly-installtest]` also
#    returns "[nightly-testnet]" rows; the literal client-side filter is what
#    keeps the two lanes from silencing each other.
rm -f "${work}/created"
run_subject '[nightly-testnet] agent main deadbee failed cross-repo testnet' || bad "cross-tag: expected exit 0"
if [ -f "${work}/created" ]; then
  ok "another lane's open issue does not suppress ours"
else
  bad "a [nightly-testnet] issue suppressed a [nightly-installtest] one: $(cat "${work}/out")"
fi

printf 'nightly-file-issue-test: %d passed, %d failed\n' "${pass}" "${fail}"
[ "${fail}" -eq 0 ]
