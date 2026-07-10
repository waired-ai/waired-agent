# shellcheck shell=bash
# installtest-common.sh — shared helpers for the local LXD installer
# harness (installtest-*.sh).
#
# Source-only. The caller is expected to have run `set -euo pipefail`.
# Everything here is dependency-light on purpose: the apt repo is
# hand-rolled with dpkg-deb + gpg + coreutils so the harness needs no
# apt-utils / dpkg-dev (which would require a password-gated apt install
# on the dev box). LXD is driven over the lxd-group socket, so no sudo.
#
# Naming: every LXD object the harness creates is prefixed `wired-it`
# (bridge `wired-it0`, guests `wired-it-<name>`) so it never collides
# with unrelated containers/VMs on the host.

# --- names / constants -------------------------------------------------

IT_PREFIX="${IT_PREFIX:-wired-it}"
IT_BRIDGE="${IT_BRIDGE:-wired-it0}"
IT_SUITE="${IT_SUITE:-waired-local}"
IT_COMPONENT="${IT_COMPONENT:-main}"
IT_IMAGE="${IT_IMAGE:-ubuntu:24.04}"          # apt-based, systemd, cloud-init
IT_REPO_PORT="${IT_REPO_PORT:-8099}"          # local apt repo http server

# --local run-in-place mode: when 1, the harness installs + asserts directly
# on THIS host (a disposable CI runner that is already systemd-PID1) instead
# of launching an LXD guest. gx() routes to `sudo` instead of `lxc exec`, and
# host-up serves the apt repo on loopback. Refuses to run without
# IT_ALLOW_LOCAL_DESTRUCTIVE=1 (it root-installs in place; CI sets it). Keep
# the LXD path (IT_LOCAL=0, the default) for local developers — their box
# isn't disposable, and it retains the clean-image / Tier-3 fidelity.
IT_LOCAL="${IT_LOCAL:-0}"

# Harness scratch lives outside the repo so it never shows up in
# `git status`. Override IT_WORKDIR to relocate.
IT_WORKDIR="${IT_WORKDIR:-$HOME/.cache/waired-installtest}"
IT_REPO="${IT_REPO:-$IT_WORKDIR/apt-repo}"
IT_GNUPGHOME="${IT_GNUPGHOME:-$IT_WORKDIR/gnupg}"
IT_RUNDIR="${IT_RUNDIR:-$IT_WORKDIR/run}"
IT_LOGDIR="${IT_LOGDIR:-$IT_WORKDIR/logs}"

# --- logging -----------------------------------------------------------

it_log()  { printf '\033[1;36m[installtest]\033[0m %s\n' "$*"; }
it_warn() { printf '\033[1;33m[installtest]\033[0m %s\n' "$*" >&2; }
it_die()  { printf '\033[1;31m[installtest]\033[0m %s\n' "$*" >&2; exit 1; }
it_step() { printf '\033[1;32m[installtest]\033[0m ==> %s\n' "$*"; }

it_require() {
  local c
  for c in "$@"; do
    command -v "$c" >/dev/null 2>&1 || it_die "required command not found: $c"
  done
}

# Repo root = the worktree this lib lives in.
it_root() {
  git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --show-toplevel
}

# --- LXD bridge --------------------------------------------------------

# Create a dedicated managed bridge if absent. The host's default LXD
# profile uses a macvlan nic which blocks host<->guest traffic; a bridge
# (with NAT for outbound apt) lets the guest reach the host-side control
# plane + apt repo via the gateway IP.
it_ensure_bridge() {
  if ! lxc network show "$IT_BRIDGE" >/dev/null 2>&1; then
    it_log "creating LXD bridge $IT_BRIDGE"
    lxc network create "$IT_BRIDGE" \
      ipv4.address=auto ipv4.nat=true ipv6.address=none >/dev/null
  fi
}

# Echo the bridge gateway IPv4 (no mask). Host-side services bind here.
it_bridge_gw() {
  lxc network get "$IT_BRIDGE" ipv4.address | sed 's#/.*##'
}

# --- gpg (throwaway signing key for the local apt repo) ----------------

it_gpg() { gpg --homedir "$IT_GNUPGHOME" --batch --no-tty "$@"; }

# Ensure a throwaway signing key exists; echo its key id (fingerprint).
it_ensure_gpg_key() {
  mkdir -p "$IT_GNUPGHOME"
  chmod 700 "$IT_GNUPGHOME"
  local keyid
  keyid=$(it_gpg --list-secret-keys --with-colons 2>/dev/null \
            | awk -F: '/^sec:/{print $5; exit}')
  if [ -z "$keyid" ]; then
    it_gpg --gen-key >/dev/null 2>&1 <<'EOF'
%no-protection
Key-Type: RSA
Key-Length: 3072
Key-Usage: sign
Name-Real: Waired Installtest Local Repo
Name-Email: installtest@waired.local
Expire-Date: 0
%commit
EOF
    keyid=$(it_gpg --list-secret-keys --with-colons \
              | awk -F: '/^sec:/{print $5; exit}')
  fi
  [ -n "$keyid" ] || it_die "failed to create/find a gpg signing key"
  printf '%s' "$keyid"
}

# --- hand-rolled signed apt repo --------------------------------------

# _it_release_hashes <hasher> <dist-root> — emit one apt Release hash
# line per index file, paths relative to <dist-root>.
_it_release_hashes() {
  local hasher="$1" base="$2" f
  ( cd "$base" || exit 1
    for f in "$IT_COMPONENT"/binary-*/Packages "$IT_COMPONENT"/binary-*/Packages.gz; do
      [ -f "$f" ] || continue
      printf ' %s %s %s\n' \
        "$($hasher "$f" | cut -d' ' -f1)" "$(stat -c%s "$f")" "$f"
    done
  )
}

# build_local_apt_repo <arch> <deb> [<deb> ...] — (re)build a signed apt
# repo under $IT_REPO that install.sh's apt path can consume verbatim:
#   dists/$IT_SUITE/$IT_COMPONENT/binary-<arch>/Packages[.gz]
#   dists/$IT_SUITE/{Release,Release.gpg,InRelease}
#   pool/main/*.deb
#   key.asc                       (armored pubkey; install.sh dearmors it)
# Point install.sh at it via:
#   WAIRED_APT_BASE_URL=http://<gw>:<port>
#   WAIRED_APT_KEY_URL=http://<gw>:<port>/key.asc
#   WAIRED_APT_SUITE=$IT_SUITE  WAIRED_APT_COMPONENT=$IT_COMPONENT
build_local_apt_repo() {
  it_require dpkg-deb gpg gzip md5sum sha1sum sha256sum
  local arch="$1"; shift
  [ "$#" -ge 1 ] || it_die "build_local_apt_repo: no .deb files given"

  local keyid; keyid=$(it_ensure_gpg_key)
  local dist="$IT_REPO/dists/$IT_SUITE"
  local bindir="$dist/$IT_COMPONENT/binary-$arch"

  rm -rf "$IT_REPO"
  mkdir -p "$IT_REPO/pool/main" "$bindir"

  local deb
  for deb in "$@"; do
    [ -f "$deb" ] || it_die "deb not found: $deb"
    cp "$deb" "$IT_REPO/pool/main/"
  done

  local pkgs="$bindir/Packages"
  : > "$pkgs"
  ( cd "$IT_REPO" || exit 1
    for deb in pool/main/*.deb; do
      dpkg-deb -f "$deb" | sed -e '/^[[:space:]]*$/d' >> "$pkgs"
      {
        printf 'Filename: %s\n' "$deb"
        printf 'Size: %s\n' "$(stat -c%s "$deb")"
        printf 'MD5sum: %s\n' "$(md5sum "$deb" | cut -d' ' -f1)"
        printf 'SHA1: %s\n' "$(sha1sum "$deb" | cut -d' ' -f1)"
        printf 'SHA256: %s\n' "$(sha256sum "$deb" | cut -d' ' -f1)"
        printf '\n'
      } >> "$pkgs"
    done
  )
  gzip -kf "$pkgs"

  local rel="$dist/Release"
  {
    printf 'Origin: waired-installtest\n'
    printf 'Label: waired-installtest\n'
    printf 'Suite: %s\n' "$IT_SUITE"
    printf 'Codename: %s\n' "$IT_SUITE"
    printf 'Version: 1.0\n'
    printf 'Architectures: %s\n' "$arch"
    printf 'Components: %s\n' "$IT_COMPONENT"
    printf 'Date: %s\n' "$(date -Ru | sed 's/+0000/GMT/')"
    printf 'MD5Sum:\n';  _it_release_hashes md5sum "$dist"
    printf 'SHA256:\n';  _it_release_hashes sha256sum "$dist"
  } > "$rel"

  it_gpg --default-key "$keyid" --yes --clearsign -o "$dist/InRelease" "$rel"
  it_gpg --default-key "$keyid" --yes --detach-sign --armor -o "$dist/Release.gpg" "$rel"
  it_gpg --armor --export "$keyid" > "$IT_REPO/key.asc"

  it_log "built local apt repo at $IT_REPO (suite=$IT_SUITE arch=$arch, $# package(s))"
}

# --- waiters -----------------------------------------------------------

it_wait_url() {
  local url="$1" timeout="${2:-30}" _
  for _ in $(seq 1 "$timeout"); do
    curl -fsS -o /dev/null "$url" 2>/dev/null && return 0
    sleep 1
  done
  return 1
}

# Wait until the guest's systemd is up and outbound DNS resolves (so the
# subsequent apt-get prereq install can reach the distro archive).
#
# VM guests additionally hold until uptime >= 90s: lxd-agent restarts
# once ~30-60s after boot (device settle), which SIGHUPs every in-flight
# `lxc exec` session — a long-running install step started before that
# window dies with exit 129 mid-download. Containers don't run lxd-agent,
# so they return as soon as systemd + DNS are up.
it_wait_guest_ready() {
  local name="$1" st up _
  for _ in $(seq 1 240); do
    if lxc exec "$name" -- test -d /run/systemd/system 2>/dev/null; then
      st=$(lxc exec "$name" -- systemctl is-system-running 2>/dev/null || true)
      case "$st" in
        running|degraded)
          if lxc exec "$name" -- getent hosts archive.ubuntu.com >/dev/null 2>&1; then
            if lxc exec "$name" -- test -e /dev/lxd/sock 2>/dev/null &&
               lxc exec "$name" -- systemctl is-enabled lxd-agent >/dev/null 2>&1; then
              # shellcheck disable=SC2016 # awk program, not a shell expansion
              up=$(lxc exec "$name" -- awk '{print int($1)}' /proc/uptime 2>/dev/null || echo 0)
              if [ "${up:-0}" -lt 90 ]; then
                sleep 1
                continue
              fi
            fi
            return 0
          fi
          ;;
      esac
    fi
    sleep 1
  done
  return 1
}

# List harness-owned guests (one per line), optionally filtered to VM or
# container with $1 = vm|container.
it_list_guests() {
  lxc list "^${IT_PREFIX}-" --format csv -c n 2>/dev/null || true
}
