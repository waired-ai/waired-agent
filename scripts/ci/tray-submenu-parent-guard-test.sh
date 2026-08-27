#!/usr/bin/env bash
# Self-test for tray-submenu-parent-guard.sh: red on the shape that lost rows
# on Windows (parent hidden, then children added), green once the Hide() is
# gone, and green on a parent that is hidden after its children exist.
set -euo pipefail

guard="$(cd "$(dirname "$0")" && pwd)/tray-submenu-parent-guard.sh"
tmp=$(mktemp -d)
trap 'rm -rf "${tmp}"' EXIT

fail=0
check() { # check <expected: pass|fail> <label> <file>
	local want="$1" label="$2" file="$3" got
	if bash "${guard}" "${file}" >/dev/null 2>&1; then got=pass; else got=fail; fi
	if [ "${got}" = "${want}" ]; then
		echo "ok   ${label} (${got})"
	else
		echo "FAIL ${label}: want ${want}, got ${got}"
		fail=1
	fi
}

f="${tmp}/tray.go"

# The defect (waired-agent#1063): the parent leaves the menu before it has a
# submenu, so convertToSubMenu cannot attach one and every child is dropped.
cat >"${f}" <<'GO'
package tray

func (t *tray) onReady() {
	t.miDeviceLabel = systray.AddMenuItem("This device", "")
	t.miDeviceLabel.Hide()
	t.miDeviceName = t.miDeviceLabel.AddSubMenuItem("", "")
}
GO
check fail "parent hidden before its children" "${f}"

# The fix: paintCreationBaseline hides it instead.
cat >"${f}" <<'GO'
package tray

func (t *tray) onReady() {
	t.miDeviceLabel = systray.AddMenuItem("This device", "")
	t.miDeviceName = t.miDeviceLabel.AddSubMenuItem("", "")
}
GO
check pass "parent keeps its children" "${f}"

# Hiding after the submenu exists is harmless — t.menus[parent] is set and is
# never deleted, so later child Show()s insert into it.
cat >"${f}" <<'GO'
package tray

func (t *tray) onReady() {
	t.miDeviceLabel = systray.AddMenuItem("This device", "")
	t.miDeviceName = t.miDeviceLabel.AddSubMenuItem("", "")
	t.miDeviceLabel.Hide()
}
GO
check pass "parent hidden after its children" "${f}"

# A file with no submenu parents at all means the guard is pointed at the
# wrong thing; it must say so rather than pass silently.
cat >"${f}" <<'GO'
package tray

func (t *tray) onReady() {
	t.miQuit = systray.AddMenuItem("Quit", "")
}
GO
check fail "no submenu parents found" "${f}"

exit "${fail}"
