#!/usr/bin/env bash
# Self-test for hostname-guard.py: red on a host-shaped name in a tracked
# file, green once it is described instead, green when the line says why it
# is excused, red on a marker with no reason and on a stale marker, quiet on
# words that only look similar (csv-, a target triple, a locale code) and on
# untracked files, and red when there is nothing tracked to look at.
#
# The host-shaped strings are assembled at run time ("s""v-") so this file
# does not trip the guard it tests.
set -euo pipefail

guard="$(cd "$(dirname "$0")" && pwd)/hostname-guard.py"
tmp=$(mktemp -d)
trap 'rm -rf "${tmp}"' EXIT

sv="s""v-box1"
pc="p""c-lab-box2"
ok="hostname""-ok:"

fail=0
check() { # check <expected: pass|fail> <label>
	local want="$1" label="$2" got
	if python3 "${guard}" --root "${tmp}" >/dev/null 2>&1; then got=pass; else got=fail; fi
	if [ "${got}" = "${want}" ]; then
		echo "ok   ${label} (${got})"
	else
		echo "FAIL ${label}: want ${want}, got ${got}"
		fail=1
	fi
}
track() { git -C "${tmp}" add -A >/dev/null; }

git -C "${tmp}" init -q
check fail "nothing tracked"

printf 'measured on the Linux host with a 24 GB GPU\n' >"${tmp}/a.md"
track
check pass "hardware description"

printf 'measured on %s\n' "${sv}" >"${tmp}/a.md"
track
check fail "sv- shaped name"

printf 'DeviceName: "alice-%s",\n' "${sv}" >"${tmp}/a.md"
track
check fail "sv- shaped name after a hyphen"

printf 'ssh %s true\n' "${pc}" >"${tmp}/a.md"
track
check fail "pc- shaped name"

printf 'csv-export x86_64-%s-msvc sv-SE\n' "p""c-windows" >"${tmp}/a.md"
track
check pass "look-alikes that are not host names"

printf 'lang: %s # %s a lower-case locale tag, not a host\n' "s""v-se" "${ok}" >"${tmp}/a.md"
track
check pass "excused with a reason"

printf 'lang: %s # %s\n' "s""v-se" "${ok}" >"${tmp}/a.md"
track
check fail "marker without a reason"

printf 'lang: sv-SE <!-- %s stale -->\n' "${ok}" >"${tmp}/a.md"
track
check fail "stale marker"

bt='`'
printf 'the marker is written %s%s <why>%s\n' "${bt}" "${ok}" "${bt}" >"${tmp}/a.md"
track
check pass "a marker quoted in a code span is documentation"

printf 'clean\n' >"${tmp}/a.md"
track
printf 'measured on %s\n' "${sv}" >"${tmp}/untracked.md"
check pass "untracked file is not scanned"

exit "${fail}"
