#!/usr/bin/env bash
# Self-test for tray-menu-label-guard.sh: red on each of the two ways a
# label can reach a backend unescaped, green once each is wrapped, and red
# when the guard is pointed somewhere it cannot see what it thinks.
set -euo pipefail

guard="$(cd "$(dirname "$0")" && pwd)/tray-menu-label-guard.sh"
tmp=$(mktemp -d)
trap 'rm -rf "${tmp}"' EXIT

fail=0
check() { # check <expected: pass|fail> <label> <pkgdir>
	local want="$1" label="$2" dir="$3" got
	if bash "${guard}" "${dir}" >/dev/null 2>&1; then got=pass; else got=fail; fi
	if [ "${got}" = "${want}" ]; then
		echo "ok   ${label} (${got})"
	else
		echo "FAIL ${label}: want ${want}, got ${got}"
		fail=1
	fi
}

pkg="${tmp}/tray"
mkdir -p "${pkg}"

# rows.go is the escape's home and is exempt from rule 1 — without it the
# guard would flag the one call that does the escaping.
cat >"${pkg}/rows.go" <<'GO'
package tray

func (t *tray) setTitle(mi menuRow, prev, next string) {
	mi.SetTitle(escapeMenuLabel(runtime.GOOS, next))
}
GO

onready() { # onready <body>
	cat >"${pkg}/tray.go" <<GO
package tray

func (t *tray) onReady(ctx context.Context) func() {
	return func() {
${1}
	}
}

func (t *tray) other() {}
GO
}

# Rule 2, the defect: a creation-time literal carrying an ampersand.
onready '		t.miPublicMore = t.miPublicShare.AddSubMenuItem("Privacy & safety…", "tip")'
check fail "a creation literal with a raw ampersand" "${pkg}"

# Rule 2, fixed.
onready '		t.miPublicMore = t.miPublicShare.AddSubMenuItem(escapeMenuLabel(runtime.GOOS, "Privacy & safety…"), "tip")'
check pass "the same literal, wrapped" "${pkg}"

# A comment quoting the label is not a label.
onready '		// miPublicMore opens the served "Privacy & safety…" link.
		t.miPublicMore = t.miPublicShare.AddSubMenuItem("", "tip")'
check pass "a comment quoting the label" "${pkg}"

# Static titles with nothing to escape are left alone — wrapping twenty of
# them would be noise, and none of them can be drawn wrong.
onready '		t.miQuit = systray.AddMenuItem("Quit", "Exit the Waired tray")'
check pass "an ordinary static title needs no wrapper" "${pkg}"

# Rule 1, the defect: a direct SetTitle outside the row diff.
onready '		t.miQuit = systray.AddMenuItem("Quit", "")'
cat >>"${pkg}/tray.go" <<'GO'

func (t *tray) refreshAutostartLabel() {
	t.miAutostart.SetTitle("Start Waired on login")
}
GO
check fail "a direct SetTitle that does not escape" "${pkg}"

# Rule 1, fixed.
onready '		t.miQuit = systray.AddMenuItem("Quit", "")'
cat >>"${pkg}/tray.go" <<'GO'

func (t *tray) refreshAutostartLabel() {
	t.miAutostart.SetTitle(escapeMenuLabel(runtime.GOOS, "Start Waired on login"))
}
GO
check pass "the same SetTitle, wrapped" "${pkg}"

# The tray icon's own title is not a menu row.
onready '		systray.SetTitle("Waired")'
check pass "systray.SetTitle is not a menu label" "${pkg}"

# Pointed at a package with no onReady, the guard must say so rather than
# pass by looking at nothing.
empty="${tmp}/empty"
mkdir -p "${empty}"
cat >"${empty}/tray.go" <<'GO'
package tray

func helper() {}
GO
check fail "no onReady to scan" "${empty}"

check fail "no tray.go at all" "${tmp}/missing"

exit "${fail}"
