#!/usr/bin/env bash
# installtest-status.sh — report the local installer harness state:
# the LXD bridge, the apt-repo http server, the (optional) control
# plane, and any harness guests (wired-it-*).
#
# Exit 0 = nothing running (clean), 1 = something is up.
#
# Usage: bash scripts/dev/installtest-status.sh
set -euo pipefail

ROOT="$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --show-toplevel)"
# shellcheck source=scripts/dev/lib/installtest-common.sh
source "$ROOT/scripts/dev/lib/installtest-common.sh"

up=0

if lxc network show "$IT_BRIDGE" >/dev/null 2>&1; then
  it_log "bridge   $IT_BRIDGE up (gateway $(it_bridge_gw))"; up=1
else
  it_log "bridge   $IT_BRIDGE down"
fi

if [ -f "$IT_RUNDIR/httpd.pid" ] && kill -0 "$(cat "$IT_RUNDIR/httpd.pid")" 2>/dev/null; then
  it_log "apt repo http server up (pid $(cat "$IT_RUNDIR/httpd.pid"))"; up=1
else
  it_log "apt repo http server down"
fi

guests="$(it_list_guests)"
if [ -n "$guests" ]; then
  it_log "guests:"; printf '%s\n' "$guests" | sed 's/^/    /'; up=1
else
  it_log "guests   none"
fi

[ "$up" -eq 0 ] && it_step "harness is down" || it_step "harness has live resources"
exit "$up"
