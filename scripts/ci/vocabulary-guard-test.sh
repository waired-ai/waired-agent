#!/usr/bin/env bash
# Self-test for vocabulary-guard.py: red on a banned term in a string literal,
# green when the literal is fixed, green when the site says why it is excused,
# red on a stale `// vocab:` marker, green for the log-prefix allowance, quiet
# on comments and _test.go files, and red when pointed at nothing.
set -euo pipefail

guard="$(cd "$(dirname "$0")" && pwd)/vocabulary-guard.py"
tmp=$(mktemp -d)
trap 'rm -rf "${tmp}"' EXIT

fail=0
check() { # check <expected: pass|fail> <label>
	local want="$1" label="$2" got
	if python3 "${guard}" --rules "${tmp}/rules.txt" --root "${tmp}" cmd/waired internal/gui/tray >/dev/null 2>&1; then got=pass; else got=fail; fi
	if [ "${got}" = "${want}" ]; then
		echo "ok   ${label} (${got})"
	else
		echo "FAIL ${label}: want ${want}, got ${got}"
		fail=1
	fi
}

mkdir -p "${tmp}/cmd/waired" "${tmp}/internal/gui/tray"
printf '*\t(?<![\\w-])tray\\b\t^tray( host)?: |waired-tray\tthe app is the Waired app\n' >"${tmp}/rules.txt"
printf '*\t\\bplease\\b\t-\tno please\n' >>"${tmp}/rules.txt"
printf 'internal/gui/tray\t\\.\\.\\.$\t-\tellipsis character\n' >>"${tmp}/rules.txt"

f="${tmp}/cmd/waired/a.go"
t="${tmp}/internal/gui/tray/b.go"

# A banned term inside a printed string.
cat >"${f}" <<'GO'
package main

func f() { println("You can switch later from the tray.") }
GO
echo 'package tray' >"${t}"
check fail "banned term in a literal"

# The same word in a comment is not a hit.
cat >"${f}" <<'GO'
package main

// the tray shows it too
func f() { println("You can switch later from the Waired app.") }
GO
check pass "term only in a comment"

# Excused at the site.
cat >"${f}" <<'GO'
package main

func f() { println("tray host repair failed") } // vocab: names the GNOME AppIndicator host, not the app
GO
check pass "excused with // vocab:"

# A marker that excuses nothing is a stale claim.
cat >"${f}" <<'GO'
package main

func f() { println("all good") } // vocab: leftover
GO
check fail "stale marker"

# The log prefix allowance.
cat >"${f}" <<'GO'
package main

func f() { slog.Info("tray: menu action") ; println("waired-tray is the binary") }
GO
check pass "log prefix and binary name allowed"

# _test.go files are never read.
cat >"${f}" <<'GO'
package main

func f() { println("fine") }
GO
cat >"${tmp}/cmd/waired/a_test.go" <<'GO'
package main

func g() { println("please, from a test") }
GO
check pass "_test.go is not read"
rm "${tmp}/cmd/waired/a_test.go"

# Scoped rule: three periods at the end of an app row.
cat >"${t}" <<'GO'
package tray

func h() { println("Sign in...") }
GO
check fail "three periods on an app row"
cat >"${t}" <<'GO'
package tray

func h() { println("Sign in…") }
GO
check pass "ellipsis character on an app row"

# Same three periods in the CLI are not this rule's business.
cat >"${f}" <<'GO'
package main

func f() { println("Starting the inference engine...") }
GO
check pass "three periods in the CLI"

# Pointed at nothing.
rm "${f}" "${t}"
check fail "no Go files at all"

[ "${fail}" -eq 0 ] && echo "vocabulary-guard-test: ok"
exit "${fail}"
