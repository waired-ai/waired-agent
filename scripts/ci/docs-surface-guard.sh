#!/usr/bin/env bash
# docs-surface-guard.sh — a PR that changes a user-visible surface must
# also change docs-site/, or say why it does not.
#
# Why this exists: docs-site/ was carried over from the monorepo as a
# CLI-first site, and the CLAUDE.md rule that keeps it current listed
# only CLI/installer examples. The GUI drifted out of the reader's
# scope entirely — the whole Waired-app surface (menu structure, icon
# states, status text) shipped across several PRs with no docs change,
# even while installer and init PRs updated the docs every time. Prose
# did not catch it; a check run does.
#
# Deliberately cheap to satisfy: touching ANY file under docs-site/
# clears it. When a change genuinely alters nothing a user reads —
# internal refactor, test-only, plumbing behind an unchanged surface —
# put a one-line
#
#     docs-not-needed: <reason>
#
# in the PR body. That is not a loophole to feel bad about; it is the
# record of a decision that was previously invisible.
#
# Runs unfiltered (no `paths:` in the workflow) so it always produces a
# check run and can be a required status check — same reasoning as
# installtest.yml. It exits 0 immediately when no surface is touched.
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

# Diff from the merge base, not from the base branch tip: the tip moves
# while a PR is open, and diffing against a moved tip would attribute
# other people's merges to this PR.
git fetch --no-tags --quiet origin "${base_ref}"
base=$(git merge-base FETCH_HEAD "${head_sha}")

# The surfaces a user actually sees. Keep this list short and honest:
# every entry must be somewhere a person outside the team can observe a
# difference. Adding a path here makes the guard louder, so add one only
# when its output is genuinely user-facing.
#
#   internal/gui/, cmd/waired-tray/  the Waired app — menus, icon
#                                    states, dialogs, status text
#   cmd/waired/                      the CLI — commands, flags, prompts,
#                                    printed wording, `waired doctor`
#   packaging/install/               the install / uninstall scripts
#                                    users run from the README
SURFACES='^(internal/gui/|cmd/waired-tray/|cmd/waired/|packaging/install/)'

changed=$(git diff --name-only "${base}" "${head_sha}")
touched=$(printf '%s\n' "${changed}" | grep -E "${SURFACES}" || true)

if [ -z "${touched}" ]; then
  echo "docs-surface-guard: no user-visible surface touched; nothing to check"
  exit 0
fi

echo "docs-surface-guard: user-visible surface touched by this PR:"
printf '%s\n' "${touched}" | sed 's/^/  /'
echo

if printf '%s\n' "${changed}" | grep -qE '^docs-site/'; then
  echo "docs-surface-guard: docs-site/ is part of this PR — OK"
  exit 0
fi

# Body opt-out. Case-insensitive, must carry a non-empty reason. The
# workflow listens for `edited`, so adding the line re-runs this check
# without a push.
reason=$(printf '%s\n' "${PR_BODY:-}" \
  | tr -d '\r' \
  | grep -ioE '^[[:space:]]*docs-not-needed:[[:space:]]*.+' \
  | head -1 \
  | sed -E 's/^[[:space:]]*[Dd][Oo][Cc][Ss]-[Nn][Oo][Tt]-[Nn][Ee][Ee][Dd][Ee][Dd]:[[:space:]]*//' \
  || true)

if [ -n "${reason}" ]; then
  echo "docs-surface-guard: opted out via PR body — ${reason}"
  exit 0
fi

cat >&2 <<'EOF'
::error::This PR changes a user-visible surface but does not touch docs-site/.

docs-site/ (https://docs.waired.ai) is the public user help site. If this
change alters anything a user sees — a menu, an icon state, a dialog, a
command, a flag, a prompt, printed wording, the install flow — update the
page that describes it, in English and in the ja/ mirror.

  docs-site/src/content/docs/            English (canonical)
  docs-site/src/content/docs/ja/         Japanese mirror
  npm --prefix docs-site run i18n:report shows which pairs are out of sync

"It only changed the GUI" is not an exemption: on a desktop the app is
what the user calls Waired.

If this change really does not alter anything a user reads, add a line to
the PR body and this check will pass:

  docs-not-needed: internal refactor, no change to any printed or shown text
EOF
exit 1
