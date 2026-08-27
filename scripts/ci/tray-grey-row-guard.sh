#!/usr/bin/env bash
# A greyed-out menu row must say why it is grey.
#
# Disable() means one thing to the person reading the menu: this is
# unavailable. Windows' own guidance is "refer to unavailable menu items
# as unavailable, not as dimmed, disabled, or grayed"; GNOME's is "make a
# menu item insensitive when its command is unavailable". So a row that
# reports a healthy state must never be grey — greyed good news reads as
# broken, which is what the owner reported on 2026-08-28 against the
# status rows waired-agent#1032 had added.
#
# Two kinds of row are legitimately grey: a section header, which names a
# group rather than a state, and an action that genuinely cannot be taken
# right now. Both are cheap to say in four words, so every Disable() on a
# menu row carries `// grey: <why>` and this guard fails the build for one
# that does not. The next person adding a row then has to make the same
# decision deliberately instead of copying the line above.
#
# Scope: the row creation block in onReady, where the baseline is written.
# A Disable() outside it is the row-mutation guard's business, not this
# one's — that guard requires it to go through t.setEnabled.
set -euo pipefail

file="${1:-internal/gui/tray/tray.go}"

# The onReady body: from `func (t *tray) onReady` to the next top-level
# func. Same window tray-row-mutation-guard.sh carves out for its
# exemption, so the two agree on where creation ends.
start=$(grep -nE '^func \(t \*tray\) onReady\(' "$file" | head -1 | cut -d: -f1 || true)
if [ -z "$start" ]; then
	echo "tray-grey-row-guard: no onReady in $file — the guard is not looking at what it thinks" >&2
	exit 1
fi
end=$(awk -v s="$start" 'NR>s && /^func /{print NR; exit}' "$file")
[ -n "$end" ] || end=$(wc -l <"$file")

checked=0
rc=0
while IFS=: read -r lineno text; do
	[ -n "$lineno" ] || continue
	checked=$((checked + 1))
	case "$text" in
	*"// grey:"*) ;;
	*)
		echo "$file:$lineno: this row is greyed out and does not say why." >&2
		echo "  Grey means unavailable to the person reading the menu, so a row that" >&2
		echo "  reports a working state must not be Disable()d — give it a click that" >&2
		echo "  opens the status report instead (statusReportRows). If it really is a" >&2
		echo "  section header or an action that cannot be taken, say so:" >&2
		echo "      t.miFoo.Disable() // grey: section header for the rows under it" >&2
		rc=1
		;;
	esac
done < <(awk -v s="$start" -v e="$end" 'NR>s && NR<e' "$file" |
	grep -nE '^[[:space:]]*t\.mi[A-Za-z0-9]+(\[[^]]*\])?\.Disable\(\)' |
	awk -v s="$start" -F: '{printf "%d:%s\n", $1 + s, substr($0, index($0, ":") + 1)}')

if [ "$checked" -eq 0 ]; then
	echo "tray-grey-row-guard: found no Disable()d rows in $file's onReady — the guard is not looking at what it thinks" >&2
	exit 1
fi

if [ "$rc" -eq 0 ]; then
	echo "tray-grey-row-guard: ok ($checked greyed rows, each with a reason)"
fi
exit "$rc"
