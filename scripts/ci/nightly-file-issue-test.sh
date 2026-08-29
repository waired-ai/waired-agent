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

# Stub `gh`: `issue list` prints whatever OPEN_ROWS holds (the number\ttitle
# TSV the subject asks for), `issue view` prints ALREADY_SAID as the issue's
# body, and `create` / `comment` each record that they were called.
mkdir -p "${work}/bin"
cat >"${work}/bin/gh" <<'STUB'
#!/usr/bin/env bash
if [ "$1" = "issue" ] && [ "$2" = "list" ]; then
  printf '%s' "${OPEN_ROWS:-}"
  exit 0
fi
if [ "$1" = "issue" ] && [ "$2" = "view" ]; then
  printf '%s' "${ALREADY_SAID:-}"
  exit 0
fi
if [ "$1" = "issue" ] && [ "$2" = "create" ]; then
  while [ $# -gt 0 ]; do
    [ "$1" = "--title" ] && printf '%s' "$2" > "${CREATED_MARKER}"
    shift
  done
  exit 0
fi
if [ "$1" = "issue" ] && [ "$2" = "comment" ]; then
  while [ $# -gt 0 ]; do
    [ "$1" = "--body" ] && printf '%s' "$2" > "${COMMENTED_MARKER}"
    shift
  done
  exit 0
fi
exit 1
STUB
chmod +x "${work}/bin/gh"

# run_subject <open-rows> [red-lanes] [already-said]
run_subject() {
  CREATED_MARKER="${work}/created" \
  COMMENTED_MARKER="${work}/commented" \
  OPEN_ROWS="${1}" \
  RED_LANES="${2:-}" \
  ALREADY_SAID="${3:-}" \
  PATH="${work}/bin:${PATH}" \
  GH_TOKEN=stub GITHUB_REPOSITORY=waired-ai/waired-agent \
  GITHUB_SERVER_URL=https://github.com GITHUB_RUN_ID=1 \
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
run_subject "$(printf '300\t[nightly-installtest] install+inference (macos) failed')" ||
  bad "dedupe: expected exit 0"
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
run_subject "$(printf '598\t[nightly-testnet] agent main deadbee failed cross-repo testnet')" ||
  bad "cross-tag: expected exit 0"
if [ -f "${work}/created" ]; then
  ok "another lane's open issue does not suppress ours"
else
  bad "a [nightly-testnet] issue suppressed a [nightly-installtest] one: $(cat "${work}/out")"
fi

# 4. An open issue whose recorded lanes DIFFER -> comment on it.
#    waired-agent#1112: the [nightly-installtest] issue filed 2026-07-28 was
#    still open a month later and had absorbed every red night since, while
#    two lanes that were green when it was filed had gone red. Its body still
#    named the July lanes.
rm -f "${work}/created" "${work}/commented"
run_subject "$(printf '300\t[nightly-installtest] install+inference (macos) failed')" \
  '- routing sentinel
' 'Red lanes:

- install+inference
' || bad "changed lanes: expected exit 0"
if [ -f "${work}/created" ]; then
  bad "filed a second issue instead of commenting on the open one"
elif [ -f "${work}/commented" ] && grep -q 'routing sentinel' "${work}/commented"; then
  ok "comments on the open issue when the red lanes have changed"
else
  bad "said nothing about a changed outage: $(cat "${work}/out")"
fi

# 5. And the other direction, which is what keeps the dedup rule intact: the
#    SAME lanes get no comment. One a morning restating an unchanged outage
#    is how a report gets muted, which is what this file exists to prevent.
rm -f "${work}/created" "${work}/commented"
run_subject "$(printf '300\t[nightly-installtest] install+inference (macos) failed')" \
  '- install+inference
' 'Red lanes:

- install+inference

<!-- red-lanes: install+inference -->
' || bad "unchanged lanes: expected exit 0"
if [ -f "${work}/commented" ] || [ -f "${work}/created" ]; then
  bad "spoke again about an unchanged outage: $(cat "${work}/out")"
else
  ok "stays quiet when the open issue already reports these lanes"
fi

# 5b. A legacy issue filed before the marker existed carries none, so the
#     first night after this change speaks once and records what it found.
#     That is the state waired-agent#1112 describes: an issue open since
#     2026-07-28 whose body still names the July lanes.
rm -f "${work}/created" "${work}/commented"
run_subject "$(printf '300\t[nightly-installtest] install+inference (macos) failed')" \
  '- install+inference
' 'Red lanes:

- install+inference
' || bad "legacy body: expected exit 0"
if [ -f "${work}/commented" ] && grep -q 'red-lanes:' "${work}/commented"; then
  ok "a pre-marker issue is told once, and the comment records the lanes"
else
  bad "a pre-marker issue stayed silent: $(cat "${work}/out")"
fi

# 6. With no RED_LANES the behaviour is exactly what it was, so a caller that
#    has not been wired up cannot start commenting by surprise.
rm -f "${work}/created" "${work}/commented"
run_subject "$(printf '300\t[nightly-installtest] install+inference (macos) failed')" ||
  bad "no RED_LANES: expected exit 0"
if [ -f "${work}/commented" ] || [ -f "${work}/created" ]; then
  bad "an unwired caller wrote to GitHub: $(cat "${work}/out")"
else
  ok "an unwired caller behaves as it did"
fi

printf 'nightly-file-issue-test: %d passed, %d failed\n' "${pass}" "${fail}"
[ "${fail}" -eq 0 ]
