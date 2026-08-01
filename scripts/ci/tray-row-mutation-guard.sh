#!/usr/bin/env bash
# tray-row-mutation-guard.sh — keep every menu-row mutation behind the guarded
# row diff in internal/gui/tray/rows.go.
#
# WHY. fyne.io/systray v1.12.2's Windows backend implements Hide() as
# RemoveMenu + delFromVisibleItems, while SetTitle / SetTooltip / Enable /
# Disable all funnel into addOrUpdateMenuItem — which, for an item that is no
# longer in visibleItems, skips SetMenuItemInfo and falls into the
# InsertMenuItem branch (systray_windows.go:605-639). On Windows, mutating a
# hidden row IS un-hiding it. That is how the daemon-down menu grew two blank
# rows and enabled-but-empty "Models" / "Claude Code" parents (#317), and it is
# not a bug a Linux test leg can ever see: dbusmenu keeps `visible` as its own
# property and macOS uses -[NSMenuItem setHidden:].
#
# The fix is that t.setVisible / t.setTitle / t.setTooltip / t.setEnabled
# suppress every mutation aimed at a row the current model hides. This guard
# stops the next handler from going around them with a raw t.miFoo.SetTitle().
#
# TWO EXEMPTIONS, both narrow:
#
#   * onReady. Item creation is where each row's Disable()/Hide() baseline is
#     written, and it reads as documentation next to the AddMenuItem call.
#     It is also not load-bearing any more: paintCreationBaseline derives the
#     real baseline from the zero MenuModel at the end of onReady.
#
#   * The always-visible rows. miHeader / miSettings / miAbout / miAutostart /
#     miQuit are never gated on a model field, so no diff can hide them and a
#     direct SetTitle on them cannot resurrect anything (refreshAutostartLabel
#     is the live case). Adding a row here is a claim that nothing will ever
#     hide it — if that stops being true, the row belongs in the diff instead.
#
# Usage: tray-row-mutation-guard.sh [package-dir]   (default: internal/gui/tray)
# Run from the repository root; CI runs it in ci.yml's lint job.
set -euo pipefail

pkg="${1:-internal/gui/tray}"
[ -d "${pkg}" ] || { echo "::error::missing ${pkg} (run from repo root)" >&2; exit 1; }

# Rows no visibility diff ever touches. Keep in sync with the comment above.
always_visible='miHeader|miSettings|miAbout|miAutostart|miQuit'

hits=""
for f in "${pkg}"/*.go; do
  case "${f}" in
    */rows.go|*_test.go) continue ;;
  esac
  # Track the enclosing top-level func so onReady can be exempted without
  # brace counting: a `func ` at column 0 opens the next one.
  found="$(awk -v always="${always_visible}" -v file="${f}" '
    /^func / { outside_onready = ($0 ~ /onReady/) ? 0 : 1 }
    outside_onready && /t\.mi[A-Za-z0-9]*(\[[^]]*\])?\.(SetTitle|SetTooltip|Enable|Disable|Show|Hide)\(/ {
      line = $0
      match(line, /t\.mi[A-Za-z0-9]*/)
      row = substr(line, RSTART + 2, RLENGTH - 2)
      if (row ~ "^(" always ")$") next
      trimmed = line
      sub(/^[[:space:]]+/, "", trimmed)
      printf "%s:%d: %s\n", file, NR, trimmed
    }' "${f}")"
  [ -n "${found}" ] && hits="${hits}${found}"$'\n'
done

if [ -n "${hits%$'\n'}" ]; then
  echo "::error::menu rows must be mutated through the guarded diff in ${pkg}/rows.go." >&2
  printf '%s' "${hits}" | sed 's/^/  /' >&2
  echo "  Use t.setVisible / t.setTitle / t.setTooltip / t.setEnabled, which skip hidden rows (#317)." >&2
  exit 1
fi

echo "tray-row-mutation-guard: ok — every row mutation outside onReady goes through the guarded diff"
