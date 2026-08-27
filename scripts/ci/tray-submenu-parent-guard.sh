#!/usr/bin/env bash
# A submenu parent must not be Hide()n before its children are created.
#
# On fyne.io/systray's Windows backend the first AddSubMenuItem is what
# creates the submenu (convertToSubMenu -> SetMenuItemInfo on the parent),
# and Hide() is RemoveMenu — so a parent hidden first can never take a
# child, and endRowPass's Go-map iteration order then decides which rows
# survive the first paint. Three tray restarts on one host rendered the
# same submenu with 8, 5 and 4 rows (waired-agent#1063).
#
# paintCreationBaseline already hides every parent from the zero MenuModel,
# so the fix is simply to not hand-write the Hide(). This guard keeps it
# that way.
set -euo pipefail

file="${1:-internal/gui/tray/tray.go}"

# Every receiver that is used as a submenu parent, i.e. appears as
# `t.miFoo.AddSubMenuItem(`.
mapfile -t parents < <(grep -oE '\bt\.mi[A-Za-z0-9]+\.AddSubMenuItem\(' "$file" |
	sed -E 's/^t\.(mi[A-Za-z0-9]+)\.AddSubMenuItem\($/\1/' | sort -u)

if [ "${#parents[@]}" -eq 0 ]; then
	echo "tray-submenu-parent-guard: found no submenu parents in $file — the guard is not looking at what it thinks" >&2
	exit 1
fi

rc=0
for p in "${parents[@]}"; do
	hide_line=$(grep -nE "^[[:space:]]*t\.${p}\.Hide\(\)[[:space:]]*$" "$file" | head -1 | cut -d: -f1 || true)
	[ -n "$hide_line" ] || continue
	first_child=$(grep -nE "t\.${p}\.AddSubMenuItem\(" "$file" | head -1 | cut -d: -f1)
	if [ "$hide_line" -lt "$first_child" ]; then
		echo "$file:$hide_line: t.${p}.Hide() runs before its first AddSubMenuItem (line $first_child)." >&2
		echo "  On Windows every child added after this is silently dropped. Drop the Hide();" >&2
		echo "  paintCreationBaseline hides the parent from the zero MenuModel. (waired-agent#1063)" >&2
		rc=1
	fi
done

if [ "$rc" -eq 0 ]; then
	echo "tray-submenu-parent-guard: ok (${#parents[@]} submenu parents checked)"
fi
exit "$rc"
