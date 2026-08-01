#!/usr/bin/env bash
# Self-test for tray-row-mutation-guard.sh: red on a raw row mutation in a
# handler, green on the same call through the guarded diff, and green on the
# two exemptions (onReady, the always-visible rows).
set -euo pipefail

guard="$(cd "$(dirname "$0")" && pwd)/tray-row-mutation-guard.sh"
tmp=$(mktemp -d)
trap 'rm -rf "${tmp}"' EXIT

fail=0
check() { # check <expected: pass|fail> <label> <pkgdir>
  local want="$1" label="$2" pkg="$3" got
  if bash "${guard}" "${pkg}" >/dev/null 2>&1; then got=pass; else got=fail; fi
  if [ "${got}" = "${want}" ]; then
    echo "ok   ${label} (${got})"
  else
    echo "FAIL ${label}: want ${want}, got ${got}"
    fail=1
  fi
}

pkg="${tmp}/tray"
mkdir -p "${pkg}"
# rows.go is the guarded diff itself and is always exempt.
cat >"${pkg}/rows.go" <<'GO'
package tray

func (t *tray) setTitle(mi menuRow, prev, next string) { mi.SetTitle(next) }
GO

write_tray() { cat >"${pkg}/tray.go"; }

# The defect: a handler retitles a row directly, bypassing the hidden-row
# suppression. On Windows that re-inserts the row into the menu (#317).
write_tray <<'GO'
package tray

func (t *tray) onSomething() {
	t.miCatalog.SetTitle("Models")
}
GO
check fail "raw SetTitle in a handler" "${pkg}"

# The same intent, routed through the diff.
write_tray <<'GO'
package tray

func (t *tray) onSomething() {
	t.setTitle(t.miCatalog, "", "Models")
}
GO
check pass "same call through the guarded diff" "${pkg}"

# Enable/Disable/Show/Hide resurrect a hidden row on Windows just as SetTitle
# does — the guard must not stop at titles.
for op in 'Enable()' 'Disable()' 'Show()' 'Hide()' 'SetTooltip("x")'; do
  write_tray <<GO
package tray

func (t *tray) onSomething() {
	t.miCatalog.${op}
}
GO
  check fail "raw ${op} in a handler" "${pkg}"
done

# Slot rows are indexed; the receiver still has to be caught.
write_tray <<'GO'
package tray

func (t *tray) applySlots() {
	t.miCatalogEntries[3].SetTitle("gpt")
}
GO
check fail "raw mutation on an indexed slot row" "${pkg}"

# Exemption 1: onReady writes each row's creation baseline in place.
write_tray <<'GO'
package tray

func (t *tray) onReady() func() {
	return func() {
		t.miCatalog.Hide()
		t.miCatalogActive.Disable()
	}
}
GO
check pass "creation baseline inside onReady" "${pkg}"

# ...but only until the next top-level func begins.
write_tray <<'GO'
package tray

func (t *tray) onReady() func() {
	return func() {
		t.miCatalog.Hide()
	}
}

func (t *tray) onSomething() {
	t.miCatalog.Hide()
}
GO
check fail "handler following onReady is still policed" "${pkg}"

# Exemption 2: rows no diff can hide (refreshAutostartLabel is the live case).
write_tray <<'GO'
package tray

func (t *tray) refreshAutostartLabel() {
	t.miAutostart.SetTitle("✓ Start Waired on login")
	t.miHeader.SetTitle("● Connected")
}
GO
check pass "always-visible rows" "${pkg}"

# Tests may drive rows directly — they are not what ships.
write_tray <<'GO'
package tray
GO
cat >"${pkg}/tray_test.go" <<'GO'
package tray

func exercise(t *tray) { t.miCatalog.SetTitle("Models") }
GO
check pass "_test.go files" "${pkg}"

# A renamed package directory must fail loudly, not silently pass.
check fail "missing package dir" "${tmp}/nope"

[ "${fail}" -eq 0 ] || exit 1
echo "tray-row-mutation-guard-test: ok"
