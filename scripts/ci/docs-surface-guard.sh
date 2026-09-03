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
#   packaging/windows/               the Windows GUI installer — its
#                                    wizard pages, its checkbox, and what
#                                    it says when it stops
#   internal/router/                 which model the Auto-Selector picks
#   proto/hostfit/                   the rule deciding whether a model
#                                    suits this machine at all
#   proto/catalog/                   the models themselves — the bundled
#                                    manifests, the retirement list, the
#                                    tier table
#   internal/catalog/                what may be offered: the verdict and
#                                    request-shape stores, the withheld
#                                    and unmeasurable declarations
#   internal/agentgrade/             the harness whose answers those
#                                    stores hold
#
# The last five print no text of their own, which is exactly why they were
# missing and why the rule change in waired-ai/waired#988 shipped against
# a docs page that still described the old one. What a user receives by
# DEFAULT is a surface: a machine that used to be handed a 22.6 GB model
# and is now handed a smaller one saw a change no printed string
# announced.
#
# The catalog three were added by waired-ai/waired-agent#1119. This is not
# a new rule: CLAUDE.md's Documentation section has listed "the model
# catalog" among the surfaces that must keep docs-site/ current since it
# was written, and says docs-guard.yml enforces it. The regex simply never
# did — adding, retiring or withholding a manifest changes which model a
# user is handed, which is the same thing proto/hostfit/ is on this list
# for.
# packaging/windows/ was added by waired-ai/waired-agent#1181. The GUI
# installer is the only Windows install path a person can double-click, and
# it was the one install surface this list did not cover: its sibling
# packaging/install/ has been here from the start. That gap showed —
# getting-started/update.mdx already promised "A fresh install behaves the
# same way: it stops rather than leaving a computer with programs that
# cannot run", which was true of install.ps1 and not of the GUI installer,
# and nothing failed.
SURFACES='^(internal/gui/|cmd/waired-tray/|cmd/waired/|packaging/install/|packaging/windows/|internal/router/|proto/hostfit/|proto/catalog/|internal/catalog/|internal/agentgrade/)'

# GENERATED is subtracted from what counts as a docs change, never from
# what counts as a surface.
#
# `make catalog-docs` writes docs-site/src/data/model-sizes.json
# (cmd/catalog-tool/docs.go). It is mandatory after a manifest change and
# lands under docs-site/, so with a bare `^docs-site/` test the required
# regeneration step would have cleared this guard by itself — in exactly
# the case the guard was widened for. The lockfile is here for the same
# reason: an npm bump is not a sentence anybody reads.
#
# cmd/catalog-tool/docs_test.go asserts the path below still matches the
# constant the generator writes to.
GENERATED='^(docs-site/src/data/model-sizes\.json|docs-site/package(-lock)?\.json)$'

changed=$(git diff --name-only "${base}" "${head_sha}")
touched=$(printf '%s\n' "${changed}" | grep -E "${SURFACES}" || true)

if [ -z "${touched}" ]; then
  echo "docs-surface-guard: no user-visible surface touched; nothing to check"
  exit 0
fi

echo "docs-surface-guard: user-visible surface touched by this PR:"
printf '%s\n' "${touched}" | sed 's/^/  /'
echo

if printf '%s\n' "${changed}" | grep -E '^docs-site/' | grep -qvE "${GENERATED}"; then
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

Catalog change? `make catalog-docs` does NOT satisfy this: the file it
writes under docs-site/ is generated, and a generated file is not a
sentence a reader reads. If the set of models a user can be handed
changed, say so on reference/model-catalog.mdx (and its ja/ mirror). If
it did not — a re-measurement, a store import, an internal-only manifest,
a pure file move — say that in the PR body instead.

If this change really does not alter anything a user reads, add a line to
the PR body and this check will pass:

  docs-not-needed: internal refactor, no change to any printed or shown text
EOF
exit 1
