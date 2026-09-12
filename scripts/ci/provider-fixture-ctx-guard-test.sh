#!/usr/bin/env bash
# Self-test for provider-fixture-ctx-guard.sh (waired-agent#925).
#
# A guard that only passed on today's tree would pass just as well with no
# rule in it, and this guard exists precisely because a rule that was
# written down twice was forgotten twice. So every case below either breaks
# the rule and requires the guard to notice, or keeps it in a form the guard
# must accept — a false positive here would send the next author looking for
# a way around it.
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
guard="${root}/scripts/ci/provider-fixture-ctx-guard.sh"

fail=0
check() { # check <expected: pass|fail> <label> <mutation function|->
  local want="$1" label="$2" mutate="$3" got tmp
  tmp="$(mktemp -d)"
  # shellcheck disable=SC2064
  trap "rm -rf '${tmp}'" RETURN

  # The shape the guard reads: the helper, five fixtures taking their
  # lifetime from it, and no unexplained context.Background().
  mkdir -p "${tmp}/cmd/waired-agent"
  cat > "${tmp}/cmd/waired-agent/provider_lifetime_test.go" <<'EOF'
package main

func providerLifetime(t *testing.T) (string, context.Context, func(*agentInferenceProvider)) {
	return "", nil, nil
}
EOF
  cat > "${tmp}/cmd/waired-agent/fixtures_test.go" <<'EOF'
package main

func aProvider(t *testing.T)  { _, _, _ = providerLifetime(t) }
func bProvider(t *testing.T)  { _, _, _ = providerLifetime(t) }
func cProvider(t *testing.T)  { _, _, _ = providerLifetime(t) }
func dProvider(t *testing.T)  { _, _, _ = providerLifetime(t) }
func eProvider(t *testing.T)  { _, _, _ = providerLifetime(t) }
EOF

  [ "${mutate}" = "-" ] || "${mutate}" "${tmp}"
  if (cd "${tmp}" && bash "${guard}" >/dev/null 2>&1); then got=pass; else got=fail; fi
  if [ "${got}" = "${want}" ]; then
    echo "ok   ${label} (${got})"
  else
    echo "FAIL ${label}: want ${want}, got ${got}"
    fail=1
  fi
}

# The defect itself: a fixture whose agent context cannot be cancelled, with
# nothing said about why. This is probeOrderProvider, which failed on the
# windows leg twice after the two fixtures either side of it were fixed.
bare_background() {
  cat >> "$1/cmd/waired-agent/fixtures_test.go" <<'EOF'

func fProvider(t *testing.T) *agentInferenceProvider {
	return &agentInferenceProvider{
		agentCtx: context.Background(),
	}
}
EOF
}

# The same thing written as an assignment after the fact, which is how half
# the sites in the real package spell it.
bare_assignment() {
  cat >> "$1/cmd/waired-agent/fixtures_test.go" <<'EOF'

func gProvider(t *testing.T, p *agentInferenceProvider) {
	p.agentCtx = context.Background()
}
EOF
}

# Keyed at the site, on the line (CLAUDE.md §"Test discipline", #1103).
reason_inline() {
  cat >> "$1/cmd/waired-agent/fixtures_test.go" <<'EOF'

func hProvider(t *testing.T) *agentInferenceProvider {
	return &agentInferenceProvider{
		agentCtx: context.Background(), // background: nothing here detaches
	}
}
EOF
}

# Keyed at the site, in the comment block above — a reason worth giving
# rarely fits on one line, and the guard must read the whole block rather
# than only the line directly above.
reason_block() {
  cat >> "$1/cmd/waired-agent/fixtures_test.go" <<'EOF'

func iProvider(t *testing.T, p *agentInferenceProvider) {
	// background: this fixture has no engine adapter and no profiler, so
	// neither detached writer exists.
	p.agentCtx = context.Background()
}
EOF
}

# A comment that is merely NEAR the line is not a reason. Without this case
# the guard would accept any commented fixture at all.
unrelated_comment() {
  cat >> "$1/cmd/waired-agent/fixtures_test.go" <<'EOF'

func jProvider(t *testing.T, p *agentInferenceProvider) {
	// The engine is deliberately absent here.
	p.agentCtx = context.Background()
}
EOF
}

# The rot the guard is really for: the helper still exists, but the fixtures
# have drifted back to hand-rolled contexts, so nothing is armed any more.
helper_unused() {
  cat > "$1/cmd/waired-agent/fixtures_test.go" <<'EOF'
package main

func aProvider(t *testing.T) { _, _, _ = providerLifetime(t) }
EOF
}

# And the blunter version: the helper is gone, and the error message would
# otherwise point at nothing.
helper_missing() { rm "$1/cmd/waired-agent/provider_lifetime_test.go"; }

check pass "a tree that takes its lifetimes from the helper" -
check fail "an unexplained context.Background() in a literal"  bare_background
check fail "an unexplained context.Background() assignment"    bare_assignment
check pass "a reason keyed on the line"                        reason_inline
check pass "a reason keyed in the block above"                 reason_block
check fail "a comment that is not a reason"                    unrelated_comment
check fail "the helper exists but nothing calls it"            helper_unused
check fail "the helper is gone"                                helper_missing

[ "${fail}" -eq 0 ] || exit 1
echo "provider-fixture-ctx-guard-test: ok"
