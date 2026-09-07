#!/usr/bin/env bash
# Self-test for installer-copy-guard.py: red on each kind of hit (a helper
# line, a heredoc body, a here-string body, an Inno literal), green once the
# line is fixed, green when a same-line `copy-ok:` says why, red on a stale
# marker, quiet on comments, and red when pointed at a missing file.
set -euo pipefail

guard="$(cd "$(dirname "$0")" && pwd)/installer-copy-guard.py"
rules="$(cd "$(dirname "$0")" && pwd)/installer-copy-rules.txt"
tmp=$(mktemp -d)
trap 'rm -rf "${tmp}"' EXIT

fail=0
check() { # check <expected: pass|fail> <label> <file...>
	local want="$1" label="$2" got
	shift 2
	if python3 "${guard}" --rules "${rules}" --root "${tmp}" "$@" >/dev/null 2>&1; then got=pass; else got=fail; fi
	if [ "${got}" = "${want}" ]; then
		echo "ok   ${label} (${got})"
	else
		echo "FAIL ${label}: want ${want}, got ${got}"
		fail=1
	fi
}

# --- shell: one hit per rule, on helper lines -------------------------------
cat >"${tmp}/please.sh" <<'EOF'
common_die "This OS is not supported. Please file an issue."
EOF
check fail "sh: please + 'is not' on a die line" please.sh

cat >"${tmp}/fixed.sh" <<'EOF'
common_die "This OS isn't supported. File an issue at https://example.com/issues"
common_log "Stopping the Waired app (waired-tray, PID 4321)"
common_warn "Couldn't set the log level; set it later with: waired config log-level debug"
nextline="Check it:     waired status        (try: waired infer \"hello, world!\")"
EOF
check pass "sh: fixed lines, the process name and the command example pass" fixed.sh

cat >"${tmp}/machine.sh" <<'EOF'
common_log "What is on this machine now:"
EOF
check fail "sh: machine" machine.sh

cat >"${tmp}/tray.sh" <<'EOF'
common_log "open the tray app and pick Sign in"
EOF
check fail "sh: bare tray" tray.sh

cat >"${tmp}/shout.sh" <<'EOF'
common_warn "The background service is NOT registered."
EOF
check fail "sh: shouted word" shout.sh

cat >"${tmp}/warnlabel.sh" <<'EOF'
common_warn "Warning: the label is the helper's job"
EOF
check fail "sh: Warning: repeated in the body" warnlabel.sh

cat >"${tmp}/dash.sh" <<'EOF'
common_log "GNOME detected — adding the extension"
EOF
check fail "sh: em dash" dash.sh

cat >"${tmp}/heredoc.sh" <<'EOF'
cat <<HELP
  --dev   enrol this device against the dogfood Control Plane
HELP
EOF
check fail "sh: hit inside a heredoc body" heredoc.sh

cat >"${tmp}/agent.sh" <<'EOF'
common_log "Setting the agent log level to debug"
EOF
check fail "sh: 'the agent' for the background service" agent.sh

# --- exemptions -------------------------------------------------------------
cat >"${tmp}/excused.sh" <<'EOF'
common_warn() { printf '[waired] Warning: %s\n' "$*" >&2; }  # copy-ok: the one place the label is written
EOF
check pass "sh: same-line copy-ok excuses the hit" excused.sh

cat >"${tmp}/stale.sh" <<'EOF'
common_log "Nothing to see here."  # copy-ok: nothing to excuse
EOF
check fail "sh: a stale copy-ok marker is itself a hit" stale.sh

cat >"${tmp}/comment.sh" <<'EOF'
# please do not flag comments: machine, tray, could not, NOT
common_log "Comments are quiet."
EOF
check pass "sh: comments are not printed text" comment.sh

# --- PowerShell ---------------------------------------------------------------
cat >"${tmp}/ps.ps1" <<'EOF'
Common-Log "Adding $InstallDir to machine PATH (open a NEW shell)."
EOF
check fail "ps1: machine + shouting on a helper line" ps.ps1

cat >"${tmp}/psdash.ps1" <<'EOF'
Common-Die 'aborted - nothing was installed'
EOF
check fail "ps1: ' - ' as a sentence break" psdash.ps1

cat >"${tmp}/pshere.ps1" <<'EOF'
$help = @"
-NoInit   do not auto-run waired init after install
"@
EOF
check fail "ps1: hit inside a here-string" pshere.ps1

cat >"${tmp}/psok.ps1" <<'EOF'
Common-Log "Stopping the Waired app (waired-tray, PID $($p.Id)) so the update can replace it"
Common-Die 'Aborted. Nothing was installed.'
$lnk.Description = 'Waired app'
EOF
check pass "ps1: fixed lines pass" psok.ps1

# --- Inno -------------------------------------------------------------------
cat >"${tmp}/setup.iss" <<'EOF'
[Code]
  Result := 'Setup could not create ' + StagingDir() + '.';
EOF
check fail "iss: 'could not' in a [Code] literal" setup.iss

cat >"${tmp}/setupok.iss" <<'EOF'
[Code]
  Result := 'Setup couldn''t create ' + StagingDir() + '.';  // copy-ok: nothing to excuse
  Log('Checking the new programs run on this computer before replacing anything');
EOF
check fail "iss: a stale marker is a hit even when the literal is clean" setupok.iss

cat >"${tmp}/setupclean.iss" <<'EOF'
[Code]
  Result := 'Setup couldn''t create ' + StagingDir() + '.';
EOF
check pass "iss: fixed literal passes" setupclean.iss

# --- missing file -----------------------------------------------------------
check fail "a missing file is an error" does-not-exist.sh

exit "${fail}"
