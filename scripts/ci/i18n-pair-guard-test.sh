#!/usr/bin/env bash
# Self-test for i18n-pair-guard.sh (#1215).
#
# The guard replaces a stored `sourceHash` line with a rule about the diff,
# so the cases worth pinning are the ones where a diff can lie about a pair:
# a nested path whose ja counterpart is not a sibling, a ja-only change
# (normal translation work, which must stay free), and a page outside the
# docs collection that merely looks like one.
#
# Each case builds a two-commit repository — a base and a head — and runs
# the guard over the diff, which is the shape the workflow gives it.
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
guard="${root}/scripts/ci/i18n-pair-guard.sh"

fail=0

# check <expected: pass|fail> <label> <PR body> <changed path>...
check() {
  local want="$1" label="$2" body="$3"; shift 3
  local tmp got f
  tmp="$(mktemp -d)"
  # shellcheck disable=SC2064
  trap "rm -rf '${tmp}'" RETURN

  ( cd "${tmp}"
    git init -q -b main .
    git config user.email t@example.com
    git config user.name t
    # A base commit holding every path the cases touch, so each head
    # commit is a MODIFICATION and the diff is exactly the argument.
    mkdir -p docs-site/src/content/docs/guides docs-site/src/content/docs/ja/guides \
             docs-site/src/data docs-site/scripts
    for f in docs-site/src/content/docs/faq.md \
             docs-site/src/content/docs/ja/faq.md \
             docs-site/src/content/docs/guides/claude-code.mdx \
             docs-site/src/content/docs/ja/guides/claude-code.mdx \
             docs-site/src/data/model-sizes.json \
             docs-site/scripts/i18n-sync.mjs \
             README.md; do
      printf 'base\n' > "${f}"
    done
    git add -A . >/dev/null
    git commit -qm base
    # The guard fetches origin/<BASE_REF>; a self-remote makes that real
    # rather than mocked.
    git remote add origin "${tmp}"
    git fetch -q origin main 2>/dev/null || true
    git checkout -q -b head
  )

  for f in "$@"; do printf 'changed\n' > "${tmp}/${f}"; done
  ( cd "${tmp}"
    git add -A . >/dev/null
    git commit -qm head )

  if ( cd "${tmp}" \
       && BASE_REF=main HEAD_SHA="$(git -C "${tmp}" rev-parse HEAD)" PR_BODY="${body}" \
          bash "${guard}" >/dev/null 2>&1 ); then got=pass; else got=fail; fi

  if [ "${got}" = "${want}" ]; then
    echo "ok   ${label} (${got})"
  else
    echo "FAIL ${label}: want ${want}, got ${got}"
    fail=1
  fi
}

# The rule itself.
check fail "an English page alone" "" docs-site/src/content/docs/faq.md
check pass "an English page with its ja page" "" \
      docs-site/src/content/docs/faq.md docs-site/src/content/docs/ja/faq.md

# A nested page: the ja counterpart is not a sibling, and a guard that
# paired by basename or by directory would clear this case wrongly.
check fail "a nested English page alone" "" \
      docs-site/src/content/docs/guides/claude-code.mdx
check fail "a nested English page paired with the WRONG ja page" "" \
      docs-site/src/content/docs/guides/claude-code.mdx \
      docs-site/src/content/docs/ja/faq.md
check pass "a nested English page with its own ja page" "" \
      docs-site/src/content/docs/guides/claude-code.mdx \
      docs-site/src/content/docs/ja/guides/claude-code.mdx

# Translation work must stay free: ja moves ahead of en all the time, and
# the mirror is not symmetric.
check pass "a ja page alone" "" docs-site/src/content/docs/ja/faq.md

# Not every file under docs-site/ is a page in the collection.
check pass "the generated size map alone" "" docs-site/src/data/model-sizes.json
check pass "the sync tool itself" "" docs-site/scripts/i18n-sync.mjs
check pass "a change touching no docs page at all" "" README.md

# The opt-out, and the fact that it needs a reason.
check pass "an English page with the opt-out line" \
      "translation-not-needed: reworded the English only" \
      docs-site/src/content/docs/faq.md
check fail "an English page with the opt-out keyword but no reason" \
      "translation-not-needed:" \
      docs-site/src/content/docs/faq.md
check fail "an English page with the OTHER guard's opt-out line" \
      "docs-not-needed: internal refactor" \
      docs-site/src/content/docs/faq.md

# One PR, several pages: one unpaired page is enough to fail.
check fail "two English pages, only one of them paired" "" \
      docs-site/src/content/docs/faq.md docs-site/src/content/docs/ja/faq.md \
      docs-site/src/content/docs/guides/claude-code.mdx

if [ "${fail}" -ne 0 ]; then
  echo "i18n-pair-guard-test: FAILED" >&2
  exit 1
fi
echo "i18n-pair-guard-test: all passed"
