#!/usr/bin/env bash
# installtest-host-up.sh — bring up the host-side services the LXD
# installer guests consume:
#
#   * a dedicated LXD bridge (wired-it0) with NAT, so guests can reach
#     the host (the default macvlan profile cannot);
#   * a freshly-built `waired` .deb from the current worktree;
#   * a throwaway-gpg-signed local apt repo serving that .deb over HTTP
#     on the bridge gateway, which install.sh consumes verbatim via
#     WAIRED_APT_BASE_URL / WAIRED_APT_KEY_URL.
#
# This is everything the harness needs — no Docker, no local control
# plane, no root. Tier 2/3 (headless enroll + data plane) enrol against
# the real dogfood CP (app.dev.waired.net, set IT_CONTROL_URL to override),
# so no extra host services are required here.
#
# Idempotent: re-running rebuilds the deb+repo and leaves a healthy http
# server alone. Writes $IT_WORKDIR/env with the exact knobs run.sh (and
# a manual `install.sh`) should use.
#
# Usage:
#   bash scripts/dev/installtest-host-up.sh                 # build + serve
#   bash scripts/dev/installtest-host-up.sh --with-tray     # also build+serve waired-tray
set -euo pipefail

ROOT="$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --show-toplevel)"
# shellcheck source=scripts/dev/lib/installtest-common.sh
source "$ROOT/scripts/dev/lib/installtest-common.sh"

WITH_TRAY=0
while [ $# -gt 0 ]; do
  case "$1" in
    --with-tray) WITH_TRAY=1 ;;
    -h|--help) sed -n '2,28p' "$0"; exit 0 ;;
    *) it_die "unknown argument: $1 (try --help)" ;;
  esac
  shift
done

export PATH="$HOME/go/bin:$PATH"   # nfpm / go-installed tools
it_require git go gpg dpkg-deb gzip python3 envsubst
[ "$IT_LOCAL" = 1 ] || it_require lxc   # --local serves on loopback, no LXD

mkdir -p "$IT_WORKDIR" "$IT_RUNDIR" "$IT_LOGDIR"

# --- nfpm (deb packager) ----------------------------------------------
NFPM_VERSION="${NFPM_VERSION:-v2.41.3}"   # keep in sync with reusable-build-artifacts.yml
if ! command -v nfpm >/dev/null 2>&1; then
  it_log "installing nfpm $NFPM_VERSION"
  go install "github.com/goreleaser/nfpm/v2/cmd/nfpm@$NFPM_VERSION"
fi

# --- repo host: loopback in --local, NATed LXD bridge otherwise --------
# The apt repo is served on $GW; install.sh fetches from it. In --local the
# installer runs on this host, so loopback suffices and no bridge is needed.
if [ "$IT_LOCAL" = 1 ]; then
  it_step "local mode: serving apt repo on loopback (no LXD bridge)"
  GW=127.0.0.1
else
  it_step "ensuring LXD bridge $IT_BRIDGE"
  it_ensure_bridge
  GW="$(it_bridge_gw)"
fi
it_log "repo host = $GW"

# --- build the waired deb from the current worktree --------------------
# Mirrors `make deb-amd64` (Makefile build-linux-amd64 + deb-amd64) but
# called directly so the harness needs no `make` (absent on the dev box)
# and can skip the cgo/GTK waired-tray build unless --with-tray.
build_deb() {
  local ver pkgver ldf
  ver="$(git -C "$ROOT" rev-parse --short HEAD)"
  pkgver="0.0.0-$ver"
  ldf="-s -w -X github.com/waired-ai/waired-agent/internal/buildinfo.Version=$ver -X github.com/waired-ai/waired-agent/internal/buildinfo.BuildSHA=$ver"

  ( cd "$ROOT"
    mkdir -p bin/linux_amd64 dist/nfpm
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="$ldf" -o bin/linux_amd64/waired ./cmd/waired
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="$ldf" -o bin/linux_amd64/waired-agent ./cmd/waired-agent
    # The nfpm templates bundle dist/{LICENSE,THIRD_PARTY_LICENSES} into
    # the .deb (#666); stage them before packaging (cached once present).
    [ -f dist/THIRD_PARTY_LICENSES ] && [ -f dist/LICENSE ] || \
      bash scripts/ci/gen-third-party-licenses.sh
    ARCH=amd64 PKG_VERSION="$pkgver" envsubst '$ARCH $PKG_VERSION' \
      < packaging/nfpm/waired.yaml.tmpl > dist/nfpm/waired-amd64.yaml
    nfpm pkg --config dist/nfpm/waired-amd64.yaml --packager deb \
      --target "dist/waired_${pkgver}_amd64.deb"
  )
  IT_DEBS=("$ROOT/dist/waired_${pkgver}_amd64.deb")

  if [ "$WITH_TRAY" = 1 ]; then
    it_require gcc   # systray needs cgo
    ( cd "$ROOT"
      GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="$ldf" -o bin/linux_amd64/waired-tray ./cmd/waired-tray
      ARCH=amd64 PKG_VERSION="$pkgver" envsubst '$ARCH $PKG_VERSION' \
        < packaging/nfpm/waired-tray.yaml.tmpl > dist/nfpm/waired-tray-amd64.yaml
      nfpm pkg --config dist/nfpm/waired-tray-amd64.yaml --packager deb \
        --target "dist/waired-tray_${pkgver}_amd64.deb"
    )
    IT_DEBS+=("$ROOT/dist/waired-tray_${pkgver}_amd64.deb")
  fi
}

it_step "building waired deb from worktree ($(git -C "$ROOT" rev-parse --short HEAD))"
build_deb
it_log "built: ${IT_DEBS[*]##*/}"

it_step "building signed local apt repo"
build_local_apt_repo amd64 "${IT_DEBS[@]}"

# --- serve the repo over HTTP on the bridge gateway --------------------
it_step "serving apt repo on http://$GW:$IT_REPO_PORT"
if [ -f "$IT_RUNDIR/httpd.pid" ] && kill -0 "$(cat "$IT_RUNDIR/httpd.pid")" 2>/dev/null; then
  it_log "http server already running (pid $(cat "$IT_RUNDIR/httpd.pid"))"
else
  nohup python3 -m http.server "$IT_REPO_PORT" --bind "$GW" --directory "$IT_REPO" \
    >"$IT_LOGDIR/httpd.log" 2>&1 &
  echo $! > "$IT_RUNDIR/httpd.pid"
  disown 2>/dev/null || true
fi
it_wait_url "http://$GW:$IT_REPO_PORT/key.asc" 15 \
  || it_die "apt repo http server did not come up (see $IT_LOGDIR/httpd.log)"

# --- write the env file run.sh / manual install.sh consume -------------
{
  printf 'IT_GW=%s\n' "$GW"
  printf 'WAIRED_APT_BASE_URL=http://%s:%s\n' "$GW" "$IT_REPO_PORT"
  printf 'WAIRED_APT_KEY_URL=http://%s:%s/key.asc\n' "$GW" "$IT_REPO_PORT"
  printf 'WAIRED_APT_SUITE=%s\n' "$IT_SUITE"
  printf 'WAIRED_APT_COMPONENT=%s\n' "$IT_COMPONENT"
} > "$IT_WORKDIR/env"

cat <<EOF

$(it_step "host is up")
  repo host      : $GW
  apt repo       : http://$GW:$IT_REPO_PORT  (suite=$IT_SUITE)
  packages       : ${IT_DEBS[*]##*/}
  env knobs      : $IT_WORKDIR/env

Next:
  bash scripts/dev/installtest-run.sh            # Tier 1 (install + systemd)
  # Tier 2/3 enrol (automated bypass): set the dev test endpoint + account
  IT_BYPASS_EMAIL=<test-acct> IT_CONTROL_URL=<bypass-cp> \\
    bash scripts/dev/installtest-run.sh --tier 2
  bash scripts/dev/installtest-host-down.sh      # when finished
EOF
