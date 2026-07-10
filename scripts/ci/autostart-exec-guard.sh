#!/usr/bin/env bash
# autostart-exec-guard.sh — keep the tray's XDG autostart entry pointing at the
# binary the package actually installs.
#
# build/autostart/waired-tray.desktop is shipped verbatim by BOTH install paths:
#   - the deb (packaging/nfpm/waired-tray.yaml.tmpl) → binary at /usr/bin/waired-tray
#   - the native tarball (build/install-desktop.sh)  → binary at /usr/local/bin/waired-tray
# When the desktop file hard-coded `Exec=/usr/local/bin/waired-tray`, the deb's
# autostart entry launched a path apt never creates and the tray silently never
# started (#491). The fix is a PATH-resolved bare `Exec=waired-tray`, which is
# correct for both layouts.
#
# This guard enforces the invariant so the bug can't come back:
#   - the Exec command's basename must equal the installed binary's basename, AND
#   - if Exec is written as a path (contains a slash), it must equal the deb's
#     install path exactly (no /usr/local vs /usr drift).
# A bare command (no slash) whose basename matches is allowed and preferred.
#
# Run from the repository root (CI does this in ci.yml's install-scripts job).
set -euo pipefail

desktop="build/autostart/waired-tray.desktop"
nfpm="packaging/nfpm/waired-tray.yaml.tmpl"

[ -f "$desktop" ] || { echo "::error::missing $desktop" >&2; exit 1; }
[ -f "$nfpm" ]    || { echo "::error::missing $nfpm" >&2; exit 1; }

# Exec command = first whitespace token after the first `Exec=` line (drops
# field codes like %U / arguments).
exec_cmd="$(grep -m1 '^Exec=' "$desktop" | cut -d= -f2- | awk '{print $1}')"
[ -n "$exec_cmd" ] || { echo "::error::no Exec= line in $desktop" >&2; exit 1; }

# Installed binary path = the `dst:` immediately following the nfpm content
# entry whose `src:` is the waired-tray binary (…/waired-tray, not the
# .desktop / .policy entries).
bin_dst="$(awk '
  /src:.*\/waired-tray[[:space:]]*$/ { found = 1; next }
  found && /dst:/ { sub(/.*dst:[[:space:]]*/, ""); print; exit }
' "$nfpm")"
[ -n "$bin_dst" ] || { echo "::error::could not find waired-tray binary dst in $nfpm" >&2; exit 1; }

fail() {
  echo "::error::autostart Exec (\"$exec_cmd\") does not match the installed binary (\"$bin_dst\") — $1 (regression of #491)" >&2
  exit 1
}

# basename must match in all cases.
[ "${exec_cmd##*/}" = "${bin_dst##*/}" ] || fail "command name mismatch"

# If Exec is written as a path, it must equal the deb install path exactly.
case "$exec_cmd" in
  */*) [ "$exec_cmd" = "$bin_dst" ] || fail "absolute path drift" ;;
esac

echo "autostart-exec-guard: ok — Exec=\"$exec_cmd\" resolves to \"$bin_dst\""
