#!/usr/bin/env bash
# i18n-pair-guard.sh — a PR that changes an English docs page must change
# its Japanese one too, or say why it does not.
#
# Why this exists: `docs-site/src/content/docs/ja/` is a 1:1 mirror, and the
# freshness of a pair used to be recorded in the ja page itself, as a
# `sourceHash` digest of the English page. That is a derived value stored in
# a versioned file: two PRs touching one English page always rewrote it to
# two different values, so they always conflicted — on that line and nothing
# else, while the prose merged cleanly. At this repository's lane count that
# stopped being a per-collision cost and became a condition on landing
# (waired-agent#1215: a wide docs PR went CONFLICTING four times in one
# afternoon, each time within nine minutes of a competing PR merging, and
# the answer of the day was to hold everyone else's merges until it landed).
#
# The same question asked of the DIFF needs no stored value and nothing
# conflicts. It is also the stronger question: the hash could be refreshed
# by a command that reads nothing, and this cannot.
#
# Deliberately narrow: it says nothing about ja-only changes (fixing a
# translation is normal work), nothing about pages outside the docs
# collection, and nothing about whether the translation is any good — that
# is what `npm --prefix docs-site run i18n:check` is for.
#
# When an English edit genuinely needs no Japanese one — a reworded English
# sentence whose translation was already right is the case that actually
# occurs — put a one-line
#
#     translation-not-needed: <reason>
#
# in the PR body. Replayed over every commit since the mirror was enforced
# (2026-07-24, #147), this rule would have needed that line six times across
# 405 page pairs, and would have failed nothing else.
#
# Runs in docs-guard.yml's `surface` job, which is unfiltered so it always
# produces a check run. It exits 0 immediately when no English page changed.
#
# Environment (set by .github/workflows/docs-guard.yml):
#   BASE_REF  — the PR's target branch name (e.g. `main`)
#   HEAD_SHA  — the PR head commit
#   PR_BODY   — the pull request body, verbatim
set -euo pipefail

base_ref="${BASE_REF:-}"
head_sha="${HEAD_SHA:-HEAD}"
if [ -z "${base_ref}" ]; then
  echo "::error::BASE_REF is not set (this script expects the PR context)" >&2
  exit 1
fi

DOCS='docs-site/src/content/docs'

# Diff from the merge base, not from the base branch tip: the tip moves
# while a PR is open, and diffing against a moved tip would attribute other
# people's merges to this PR.
git fetch --no-tags --quiet origin "${base_ref}"
base=$(git merge-base FETCH_HEAD "${head_sha}")

changed=$(git diff --name-only "${base}" "${head_sha}")
english=$(printf '%s\n' "${changed}" \
  | grep -E "^${DOCS}/.*\.(md|mdx)$" \
  | grep -vE "^${DOCS}/ja/" || true)

if [ -z "${english}" ]; then
  echo "i18n-pair-guard: no English docs page touched; nothing to check"
  exit 0
fi

unpaired=""
while IFS= read -r en; do
  [ -n "${en}" ] || continue
  ja="${DOCS}/ja/${en#"${DOCS}/"}"
  if ! printf '%s\n' "${changed}" | grep -qxF "${ja}"; then
    unpaired="${unpaired}${en} -> ${ja}"$'\n'
  fi
done <<EOF
${english}
EOF

if [ -z "${unpaired}" ]; then
  echo "i18n-pair-guard: every English page this PR changes has its ja page in the same PR — OK"
  exit 0
fi

# Body opt-out. Case-insensitive, must carry a non-empty reason. The
# workflow listens for `edited`, so adding the line re-runs this check
# without a push — which is the whole point: the exception must not cost
# the CI cycle this guard exists to remove.
reason=$(printf '%s\n' "${PR_BODY:-}" \
  | tr -d '\r' \
  | grep -ioE '^[[:space:]]*translation-not-needed:[[:space:]]*.+' \
  | head -1 \
  | sed -E 's/^[[:space:]]*[Tt][Rr][Aa][Nn][Ss][Ll][Aa][Tt][Ii][Oo][Nn]-[Nn][Oo][Tt]-[Nn][Ee][Ee][Dd][Ee][Dd]:[[:space:]]*//' \
  || true)

if [ -n "${reason}" ]; then
  echo "i18n-pair-guard: opted out via PR body — ${reason}"
  exit 0
fi

echo "::error::This PR changes an English docs page without changing its Japanese one." >&2
echo >&2
echo "Unpaired pages:" >&2
printf '%s' "${unpaired}" | sed 's/^/  /' >&2
cat >&2 <<'EOF'

docs-site/src/content/docs/ja/ is a 1:1 mirror: English is canonical and the
Japanese page is written from it, in the same pull request. Nothing records
which English version a translation was made from any more, so this is the
moment the pair is kept honest.

Keep the pinned terminology in docs-site/TRANSLATION.md — never re-derive a
term choice while translating a page.

If this English edit genuinely needs no Japanese change — a reworded English
sentence whose translation was already right — add a line to the PR body and
this check will pass:

  translation-not-needed: reworded the English only; the ja sentence already says this
EOF
exit 1
