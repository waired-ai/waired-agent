#!/usr/bin/env bash
# installtest-host-down.sh — tear down the host-side services started by
# installtest-host-up.sh: the apt-repo http server.
#
# By default it leaves the LXD bridge and any guests in place (guests are
# managed by installtest-run.sh, which deletes its own). Pass --all to
# also delete every harness guest (wired-it-*) and the bridge.
#
# Usage:
#   bash scripts/dev/installtest-host-down.sh
#   bash scripts/dev/installtest-host-down.sh --all
set -euo pipefail

ROOT="$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --show-toplevel)"
# shellcheck source=scripts/dev/lib/installtest-common.sh
source "$ROOT/scripts/dev/lib/installtest-common.sh"

ALL=0
while [ $# -gt 0 ]; do
  case "$1" in
    --all) ALL=1 ;;
    -h|--help) sed -n '2,14p' "$0"; exit 0 ;;
    *) it_die "unknown argument: $1 (try --help)" ;;
  esac
  shift
done

# apt-repo http server.
if [ -f "$IT_RUNDIR/httpd.pid" ]; then
  pid="$(cat "$IT_RUNDIR/httpd.pid")"
  if kill -0 "$pid" 2>/dev/null; then
    it_log "stopping apt repo http server (pid $pid)"
    kill "$pid" 2>/dev/null || true
  fi
  rm -f "$IT_RUNDIR/httpd.pid"
fi

if [ "$ALL" = 1 ]; then
  for g in $(it_list_guests); do
    it_log "deleting guest $g"
    lxc delete --force "$g" 2>/dev/null || true
  done
  if lxc network show "$IT_BRIDGE" >/dev/null 2>&1; then
    it_log "deleting bridge $IT_BRIDGE"
    lxc network delete "$IT_BRIDGE" 2>/dev/null || true
  fi
fi

it_step "host is down"
