#!/usr/bin/env bash
# Self-test for tray-grey-row-guard.sh: red on a row greyed for no stated
# reason, green once it says one, green for a row that is not greyed at
# all, and red when the guard is pointed somewhere it finds nothing to
# check (so it cannot pass by looking at the wrong file).
set -euo pipefail

guard="$(cd "$(dirname "$0")" && pwd)/tray-grey-row-guard.sh"
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

# The defect the owner reported: a row that reports a healthy state,
# greyed, with nothing saying why.
cat >"${f}" <<'GO'
package tray

func (t *tray) onReady(ctx context.Context) func() {
	t.miStatusEngine = systray.AddMenuItem("", "")
	t.miStatusEngine.Disable()
}
GO
check fail "greyed row with no reason" "${f}"

# The two legitimate shapes, each saying which one it is.
cat >"${f}" <<'GO'
package tray

func (t *tray) onReady(ctx context.Context) func() {
	t.miClaudeMainHeader = systray.AddMenuItem("Main conversation", "")
	t.miClaudeMainHeader.Disable() // grey: section header for the routes under it
	t.miUnloadModel = systray.AddMenuItem("", "")
	t.miUnloadModel.Disable() // grey: unavailable until a model is loaded
}
GO
check pass "greyed rows that say why" "${f}"

# Indexed slots are rows too.
cat >"${f}" <<'GO'
package tray

func (t *tray) onReady(ctx context.Context) func() {
	for i := range MaxPeerRows {
		t.miPeerEntries[i] = t.miDeviceLabel.AddSubMenuItem("", "")
		t.miPeerEntries[i].Disable()
	}
	t.miRecent.Disable() // grey: section header
}
GO
check fail "greyed slot row with no reason" "${f}"

# A Disable() AFTER onReady is the row-mutation guard's business — this
# one must not reach past the end of the creation block.
cat >"${f}" <<'GO'
package tray

func (t *tray) onReady(ctx context.Context) func() {
	t.miRecent.Disable() // grey: section header
}

func (t *tray) applyLater() {
	t.miWorkerPinEntries[0].Disable()
}
GO
check pass "Disable() outside onReady is not this guard's business" "${f}"

# A file with no greyed rows at all means the guard is not looking at the
# tray — it must say so rather than pass.
cat >"${f}" <<'GO'
package tray

func (t *tray) onReady(ctx context.Context) func() {
	t.miQuit = systray.AddMenuItem("Quit", "")
}
GO
check fail "nothing to check" "${f}"

# No onReady at all: same reasoning, louder.
cat >"${f}" <<'GO'
package tray

func helper() {}
GO
check fail "no onReady" "${f}"

exit "${fail}"
