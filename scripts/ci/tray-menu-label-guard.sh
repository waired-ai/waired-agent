#!/usr/bin/env bash
# Every menu label is escaped for the OS that draws it.
#
# Two of the three backends read a character in a label as markup rather
# than as text: Win32 menus treat `&` as the mnemonic prefix, and the
# dbusmenu spec gives `_` the same role. `Privacy & safety…` drew as
# `Privacy  safety…` on Windows for as long as the row existed
# (waired-agent#1096). internal/gui/tray/menulabel.go decides what to
# escape per OS; this keeps every label going through it.
#
# The dynamic labels are covered structurally — they all reach the widget
# through rows.go's setTitle, which escapes once for all of them. What
# needs policing is the two ways around that:
#
#   * a direct .SetTitle() outside the row diff, and
#   * a title written straight into AddMenuItem / AddSubMenuItem at
#     creation, which the diff never sees.
#
# For the second, only a literal that actually carries a special character
# is flagged: wrapping the other twenty static titles would be noise, and
# a title with nothing to escape cannot be drawn wrong. Both characters
# count — the underscore joined the escape set when the tray started
# writing each Linux renderer its own markup (waired-agent#1100).
set -euo pipefail

pkg="${1:-internal/gui/tray}"

rc=0

# --- 1. every SetTitle outside the row diff escapes -------------------
while IFS=: read -r file line text; do
	[ -n "${file:-}" ] || continue
	case "$text" in
	# systray.SetTitle sets the status item's own text (the macOS menu-bar
	# label), not a menu row. No backend reads a mnemonic there.
	*systray.SetTitle\(*) ;;
	*escapeMenuLabel\(*) ;;
	*)
		echo "::error::${file}:${line}: this SetTitle does not escape its label." >&2
		echo "  Win32 eats a lone '&' and dbusmenu eats a lone '_'. Wrap it:" >&2
		echo "      mi.SetTitle(escapeMenuLabel(runtime.GOOS, t.dialect, title))" >&2
		echo "  Dynamic labels are already covered — they go through rows.go's setTitle." >&2
		rc=1
		;;
	esac
done < <(
	find "$pkg" -maxdepth 1 -name '*.go' ! -name 'rows.go' ! -name '*_test.go' -print0 |
		xargs -0 grep -HnE '\.SetTitle\(' 2>/dev/null || true
)

# --- 2. a creation-time literal may not carry unescaped markup --------
# Scoped to onReady, where rows are built. Comment lines are skipped; a
# line that already calls escapeMenuLabel is the fixed form. '&' is eaten
# by Win32 and '_' by every dbusmenu renderer, so both are flagged.
tray="${pkg}/tray.go"
if [ ! -f "$tray" ]; then
	echo "tray-menu-label-guard: no ${tray} — the guard is not looking at what it thinks" >&2
	exit 1
fi
start=$(grep -nE '^func \(t \*tray\) onReady\(' "$tray" | head -1 | cut -d: -f1 || true)
if [ -z "$start" ]; then
	echo "tray-menu-label-guard: no onReady in ${tray} — the guard is not looking at what it thinks" >&2
	exit 1
fi
end=$(awk -v s="$start" 'NR>s && /^func /{print NR; exit}' "$tray")
[ -n "$end" ] || end=$(wc -l <"$tray")

while IFS=: read -r line text; do
	[ -n "${line:-}" ] || continue
	echo "::error::${tray}:${line}: this menu label carries a raw '&' or '_'." >&2
	echo "  Win32 reads '&' as the mnemonic prefix and every dbusmenu renderer" >&2
	echo "  reads '_' the same way; either is dropped. Wrap the literal:" >&2
	echo "      escapeMenuLabel(runtime.GOOS, t.dialect, \"Privacy & safety…\")" >&2
	echo "  (waired-agent#1096, waired-agent#1100)" >&2
	rc=1
done < <(
	awk -v s="$start" -v e="$end" 'NR>s && NR<e' "$tray" |
		grep -nE '"[^"]*[&_][^"]*"' |
		grep -vE '^[0-9]+:[[:space:]]*//' |
		grep -v 'escapeMenuLabel(' |
		awk -v s="$start" -F: '{printf "%d:%s\n", $1 + s, substr($0, index($0, ":") + 1)}' || true
)

if [ "$rc" -eq 0 ]; then
	echo "tray-menu-label-guard: ok — every menu label is escaped for whatever draws it"
fi
exit "$rc"
