#!/bin/sh
# install.sh — install Waired via the system package manager.
#
# Usage:
#   curl -fsSL https://github.com/waired-ai/waired-install/releases/latest/download/install.sh | sh
#   curl -fsSL https://github.com/waired-ai/waired-install/releases/latest/download/install.sh | sh -s -- --dry-run
#
# This script is intentionally OS-agnostic in shape. Linux + apt
# (Debian / Ubuntu) and macOS are wired up. New operating systems plug
# in by adding three things:
#
#   1. a new branch in detect_os to set OS_FAMILY
#   2. a handler function named <kind>_<pkgmgr>_install (or <kind>_install)
#   3. a new arm in the case statement at the bottom of main()
#
# Function namespaces:
#   common_*      shared helpers — log, run, sudo, etc.
#   detect_*      probe the host (kernel, distro, arch)
#   linux_apt_*   Debian / Ubuntu installer
#   linux_dnf_*   Fedora / RHEL                  (future)
#   linux_apk_*   Alpine                          (future)
#   darwin_*      macOS: download tarball + Ollama.app, register LaunchDaemon
#   windows_*     handled by a separate .ps1

set -eu

# Record whether the operator set these explicitly *before* we apply the
# defaults below. The edge-channel auto-wiring (see main()) only fills in
# the edge suite / edge asset base when the operator did NOT pin them, so
# an explicit override always wins. `${VAR+x}` is empty iff VAR is unset,
# and is safe under `set -u`.
_WAIRED_APT_SUITE_SET="${WAIRED_APT_SUITE+x}"
_WAIRED_INSTALL_BASE_URL_SET="${WAIRED_INSTALL_BASE_URL+x}"

# GitHub Releases asset URL (hosts install.sh itself). `latest` resolves
# to the most recent tagged release.
WAIRED_INSTALL_BASE_URL="${WAIRED_INSTALL_BASE_URL:-https://github.com/waired-ai/waired-install/releases/latest/download}"
# GitHub repo (owner/name) whose Releases API resolves the latest version
# during --check / --update on macOS (Linux reads apt's candidate
# instead). Override alongside WAIRED_INSTALL_BASE_URL for a mirror.
WAIRED_INSTALL_REPO="${WAIRED_INSTALL_REPO:-waired-ai/waired-install}"
# Artifact Registry APT endpoint that hosts the actual .deb packages.
# Repo is publicly readable via roles/artifactregistry.reader on allUsers
# (see infra/terraform/modules/artifact-registry/main.tf).
#
# AR's APT format publishes one suite per repository, so the URL stops
# at the project level and the suite name *is* the AR repository ID.
# Components are always `main` today. End users override these three
# vars when pinning to a future `waired-dev-apt-beta` track or a
# separately-provisioned prod repo.
WAIRED_APT_BASE_URL="${WAIRED_APT_BASE_URL:-https://asia-northeast1-apt.pkg.dev/projects/dev-waired}"
WAIRED_APT_SUITE="${WAIRED_APT_SUITE:-waired-dev-apt}"
WAIRED_APT_COMPONENT="${WAIRED_APT_COMPONENT:-main}"
# Public signing key URL. AR signs every APT repo in a region with the
# same Google-managed key, exposed at this well-known path. Derived from
# WAIRED_APT_BASE_URL so the region stays consistent.
WAIRED_APT_KEY_URL="${WAIRED_APT_KEY_URL:-https://asia-northeast1-apt.pkg.dev/doc/repo-signing-key.gpg}"

# Built-in dogfood Control Plane URL surfaced via `--dev`. Shell-level
# only; never compiled into the waired binary (spec §10.4 keeps
# staging/prd binary hashes identical, so the URL stays in the wrapper).
WAIRED_DEV_CONTROL_URL="${WAIRED_DEV_CONTROL_URL:-https://app.dev.waired.net}"

# macOS only: the official Ollama.app download (universal binary, both
# arches). darwin_install fetches this when no ollama is already present
# and installs it into /Applications so waired's ResolveBinary finds the
# CLI at /Applications/Ollama.app/Contents/Resources/ollama. Override to
# pin a version or point at an internal mirror.
WAIRED_OLLAMA_DARWIN_URL="${WAIRED_OLLAMA_DARWIN_URL:-https://github.com/ollama/ollama/releases/latest/download/Ollama-darwin.zip}"

# Linux installs waired's BUNDLED Ollama (pinned official release into
# <state-dir>/runtimes/ollama/, supervised by waired-agent on :9475) via
# `waired runtimes install ollama`, NOT a system `ollama.com/install.sh`
# (#567). The download URL is pinned inside the Go installer
# (internal/runtime/ollama_install.go), so there is no Linux URL override
# knob — the former WAIRED_OLLAMA_LINUX_URL is retired. macOS keeps
# WAIRED_OLLAMA_DARWIN_URL (Ollama.app, PATH-resolved).

DRY_RUN=0
SUDO=""
CONTROL_URL=""
FLAG_USE_DEV=0
FLAG_CONTROL_URL=""
FLAG_NO_OLLAMA=0
# FLAG_CHECK / FLAG_UPDATE / FLAG_YES default to 0 so they can be read
# under `set -u` even when the corresponding flag is not passed. Without
# FLAG_CHECK/FLAG_UPDATE defaults a fresh `curl | sh` aborts with
# "FLAG_CHECK: unbound variable" before reaching darwin_install /
# linux_apt_install; without the FLAG_YES default a plain re-run on an
# already-installed host aborts inside prompt_update ("FLAG_YES: unbound
# variable") the first time the update path is taken without --yes.
FLAG_CHECK=0
FLAG_UPDATE=0
FLAG_YES=0
# FLAG_STABLE forces the stable channel on --update/--check, overriding the
# channel-preservation that would otherwise keep an edge host on edge. Top-level
# default so it's readable under set -u even when --stable isn't passed.
FLAG_STABLE=0
# FLAG_NO_INIT defaults to 0 (auto-run `waired init` after install when a
# terminal is available). Top-level default so it's readable under set -u
# even when --no-init isn't passed.
FLAG_NO_INIT=0
OS_KIND=""
OS_FAMILY=""
OS_NAME=""
OS_VERSION=""
OS_CODENAME=""
OS_ARCH=""

# ---------------------------------------------------------------------
# common_* helpers
# ---------------------------------------------------------------------

common_log()  { printf '\033[1;36m[waired]\033[0m %s\n' "$*"; }
common_warn() { printf '\033[1;33m[waired]\033[0m %s\n' "$*" >&2; }
common_die()  { printf '\033[1;31m[waired]\033[0m %s\n' "$*" >&2; exit 1; }

# Run a command, or print it in dry-run mode.
common_run() {
    if [ "$DRY_RUN" = 1 ]; then
        printf '\033[1;90m[dry-run]\033[0m %s\n' "$*"
        return 0
    fi
    "$@"
}

common_require_cmd() {
    for c in "$@"; do
        command -v "$c" >/dev/null 2>&1 || \
            common_die "required command not found: $c"
    done
}

# Find a privilege-escalation strategy. After this, "$SUDO cmd args"
# works whether the user is already root or not.
common_elevate() {
    if [ "$(id -u)" -eq 0 ]; then
        SUDO=""
        return
    fi
    if command -v sudo >/dev/null 2>&1; then
        SUDO=sudo
        return
    fi
    common_die "this installer needs root privileges. Install sudo, or re-run as root."
}

# supports_emoji reports whether the terminal/locale can render the emoji
# used in the friendly banners. Falls back to ASCII otherwise (non-UTF-8
# locale, or WAIRED_NO_EMOJI set) so logs stay readable.
supports_emoji() {
    [ -n "${WAIRED_NO_EMOJI:-}" ] && return 1
    case "${LC_ALL:-${LC_CTYPE:-${LANG:-}}}" in
        *UTF-8*|*UTF8*|*utf-8*|*utf8*) return 0 ;;
        *) return 1 ;;
    esac
}

# emo <emoji> <ascii-fallback> prints whichever the terminal can render.
emo() {
    if supports_emoji; then printf '%s' "$1"; else printf '%s' "$2"; fi
}

# tty_available reports whether we can interact with the user even when
# stdin is a pipe — the `curl | sh` case. /dev/tty is the controlling
# terminal, so prompts/redirects use it directly. We must actually try to
# OPEN it for read+write: in a context with no controlling terminal (CI,
# Docker build, a daemon) the device node still exists with rw permission
# bits but open() fails with ENXIO, so a `[ -r /dev/tty ]` permission test
# gives a false positive. The subshell exec applies both redirections and
# returns non-zero if either fails.
tty_available() {
    ( exec </dev/tty >/dev/tty ) 2>/dev/null
}

# _banner_row prints one row of the rich banner: a 24-bit (truecolor) RGB
# foreground when colour is on, plain text otherwise. The row text is
# backslash-free (box-drawing glyphs only) so %s carries it verbatim and
# the \033 lives only in the format string.
_banner_row() {
    if [ "${_b_color:-0}" -eq 1 ]; then
        printf '\033[38;2;%s;%s;%sm%s\033[0m\n' "$1" "$2" "$3" "$4"
    else
        printf '%s\n' "$4"
    fi
}

# _banner_plain is the ASCII fallback (non-UTF-8 / narrow / redirected):
# a figlet "standard" WAIRED wordmark in a single brand cyan. The art is
# single-quoted (fully literal, backslashes and all) and printed as its
# own %s arg, separate from the colour args, so nothing is mangled.
_banner_plain() {
    _bp=''
    _bq=''
    if [ "${_b_color:-0}" -eq 1 ]; then
        _bp=$(printf '\033[1;36m')
        _bq=$(printf '\033[0m')
    fi
    printf '%s%s%s\n' "$_bp" '__        ___    ___ ____  _____ ____  ' "$_bq"
    printf '%s%s%s\n' "$_bp" '\ \      / / \  |_ _|  _ \| ____|  _ \ ' "$_bq"
    printf '%s%s%s\n' "$_bp" ' \ \ /\ / / _ \  | || |_) |  _| | | | |' "$_bq"
    printf '%s%s%s\n' "$_bp" '  \ V  V / ___ \ | ||  _ <| |___| |_| |' "$_bq"
    printf '%s%s%s\n' "$_bp" '   \_/\_/_/   \_\___|_| \_\_____|____/ ' "$_bq"
    printf '%s\n\n' '   Local-first AI gateway'
}

# print_banner prints the WAIRED "GATE" splash at the start of a run.
# Two tiers, chosen by terminal capability:
#   * rich  — a block WAIRED wordmark + GATE emblem ( ● ) with a
#             blue→cyan truecolor gradient, on a UTF-8, wide-enough term.
#   * plain — a figlet ASCII wordmark, for non-UTF-8 / narrow / piped.
# Self-contained and `set -eu` safe: only function-local vars, every
# external read carries a `${VAR:-}` default. Colour is applied only on
# an interactive terminal with NO_COLOR unset, so piped/CI output stays
# plain.
print_banner() {
    _b_color=0
    if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then _b_color=1; fi
    _b_cols=80
    if command -v tput >/dev/null 2>&1; then
        _b_cols=$(tput cols 2>/dev/null) || _b_cols=80
    fi
    case "${_b_cols:-80}" in ''|*[!0-9]*) _b_cols=80 ;; esac

    if supports_emoji && [ "$_b_cols" -ge 60 ]; then
        _banner_row 127 233 255 "       ·  ⟨ ● ⟩  ·"
        _banner_row  72 105 140 "   ┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄"
        _banner_row 143 189 240 " ██╗    ██╗ █████╗ ██╗██████╗ ███████╗██████╗ "
        _banner_row 140 198 243 " ██║    ██║██╔══██╗██║██╔══██╗██╔════╝██╔══██╗"
        _banner_row 137 207 246 " ██║ █╗ ██║███████║██║██████╔╝█████╗  ██║  ██║"
        _banner_row 134 215 249 " ██║███╗██║██╔══██║██║██╔══██╗██╔══╝  ██║  ██║"
        _banner_row 130 224 252 " ╚███╔███╔╝██║  ██║██║██║  ██║███████╗██████╔╝"
        _banner_row 127 233 255 "  ╚══╝╚══╝ ╚═╝  ╚═╝╚═╝╚═╝  ╚═╝╚══════╝╚═════╝ "
        _banner_row  72 105 140 "   ┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄"
        _banner_row 150 160 175 "   Local-first AI gateway  ·  \$0 per token"
        _banner_row 112 120 134 "   Claude Code · OpenCode · OpenClaw — your own machine"
        printf '\n'
    else
        _banner_plain
    fi
}

show_help() {
    cat <<HELP
install.sh — install Waired via the system package manager.

Usage:
  curl -fsSL https://github.com/waired-ai/waired-install/releases/latest/download/install.sh | sh
  curl -fsSL https://github.com/waired-ai/waired-install/releases/latest/download/install.sh | sh -s -- --dev
  curl -fsSL https://github.com/waired-ai/waired-install/releases/latest/download/install.sh | sh -s -- --dry-run

Options:
  --dry-run        show every privileged command without running it
  --dev            enrol this device against the built-in dogfood Control
                   Plane (${WAIRED_DEV_CONTROL_URL}) — writes
                   WAIRED_CONTROL_URL into /etc/waired/agent.env so
                   \`sudo waired init\` (no --control) just works
  --control <URL>  same as --dev but with an explicit URL; takes
                   precedence over --dev when both are given
  --edge, --latest install/switch to the latest main build (same as
                   WAIRED_VERSION=edge) — rebuilt on every merge to main;
                   NOT a stable release. Selects the edge apt suite on
                   Linux and the edge prerelease assets on macOS for you.
  --stable         install/switch to the latest stable release. On
                   --update/--check this overrides the default, which is
                   to *preserve* the channel the host already tracks
                   (edge stays edge, stable stays stable).
  --skip-ollama    do not install Ollama (same as WAIRED_NO_OLLAMA=1)
  --no-init        do not auto-run \`waired init\` after install (the
                   default runs sign-in + setup when a terminal is present)
  --yes, -y        assume "yes" for prompts (update, init non-interactive)
  -h, --help       print this help

Environment variables:
  WAIRED_VERSION           pin to a specific package version (e.g. 1.2.3),
                           or 'edge' for the latest main build (same as
                           --edge; works on every OS). Unset/'latest' =
                           the newest stable release.
  WAIRED_NO_TRAY           if set, do not install waired-tray (Linux + macOS)
  WAIRED_NO_OLLAMA         if set, do not install Ollama (same as
                           --skip-ollama; Linux + macOS)
  WAIRED_CONTROL_URL       Control Plane URL written to agent.env when
                           --dev / --control are not given (lower-priority
                           fallback for per-org installer wrappers)
  WAIRED_DEV_CONTROL_URL   override the URL --dev resolves to
                           (default: https://app.dev.waired.net)
  WAIRED_INSTALL_BASE_URL  override URL for install.sh itself
                           (default: github.com/waired-ai/waired-install releases)
  WAIRED_OLLAMA_DARWIN_URL macOS only: override the Ollama.app download URL
                           (default: ollama/ollama latest Ollama-darwin.zip)
  WAIRED_APT_BASE_URL      override the apt repository base URL
                           (default: asia-northeast1-apt.pkg.dev/projects/dev-waired)
  WAIRED_APT_SUITE         override the apt suite (= AR repository id)
                           (default: waired-dev-apt; WAIRED_VERSION=edge
                           selects waired-dev-apt-edge automatically)
  WAIRED_APT_COMPONENT     override the apt component (default: main)
  WAIRED_APT_KEY_URL       override the GPG signing-key URL
                           (default: asia-northeast1-apt.pkg.dev/doc/repo-signing-key.gpg)
HELP
}

# Resolve the Control Plane URL using [flag > --dev preset > env]
# precedence; sets the global $CONTROL_URL. An empty result is fine —
# the installer falls back to the placeholder Next steps in that case.
resolve_control_url() {
    if [ -n "$FLAG_CONTROL_URL" ] && [ "$FLAG_USE_DEV" = 1 ]; then
        common_warn "--control overrides --dev (both were given)"
    fi
    if [ -n "$FLAG_CONTROL_URL" ]; then
        CONTROL_URL="$FLAG_CONTROL_URL"
    elif [ "$FLAG_USE_DEV" = 1 ]; then
        if [ -z "$WAIRED_DEV_CONTROL_URL" ]; then
            common_die "--dev requires WAIRED_DEV_CONTROL_URL but it is empty"
        fi
        CONTROL_URL="$WAIRED_DEV_CONTROL_URL"
    elif [ -n "${WAIRED_CONTROL_URL:-}" ]; then
        CONTROL_URL="$WAIRED_CONTROL_URL"
    fi
}

# True (exit 0) when the operator asked to skip the Ollama install via
# either the --skip-ollama flag or the WAIRED_NO_OLLAMA env var. Both
# forms are accepted on every platform (the Windows install.ps1 mirrors
# this with -SkipOllama + $env:WAIRED_NO_OLLAMA).
ollama_skip_requested() {
    [ "$FLAG_NO_OLLAMA" = 1 ] || [ -n "${WAIRED_NO_OLLAMA:-}" ]
}

# ---------------------------------------------------------------------
# update_* — shared version helpers for --check / --update. The compare
# semantics mirror internal/version (Go) so the installer, `waired
# update` (#293) and the auto-check (#294) all agree on "is X older
# than Y".
# ---------------------------------------------------------------------

# version_strip <raw> — leading dotted-numeric only: drop a "v" prefix
# and any "-rc1" / ".post1" suffix. Callers pass already-clean strings
# (the .version JSON field, an apt version, or a release tag).
version_strip() {
    s="${1#v}"
    printf '%s' "$s" | sed -E 's/[^0-9.].*$//'
}

# version_lt A B — exit 0 (true) iff A < B, comparing dotted components
# numerically and zero-padding the shorter side. Empty/unparseable A is
# treated as "older" (offer the update); empty B as "not older". awk
# avoids macOS `sort -V` gaps.
version_lt() {
    a="$(version_strip "$1")"
    b="$(version_strip "$2")"
    [ -z "$a" ] && return 0
    [ -z "$b" ] && return 1
    [ "$a" = "$b" ] && return 1
    awk -v a="$a" -v b="$b" 'BEGIN{
        na=split(a,A,"."); nb=split(b,B,".");
        n=(na>nb?na:nb);
        for(i=1;i<=n;i++){x=(i<=na?A[i]:0)+0; y=(i<=nb?B[i]:0)+0;
            if(x<y) exit 0; if(x>y) exit 1}
        exit 1}'
}

# channel_from_env — stable | edge | <explicit pin>, from WAIRED_VERSION.
channel_from_env() {
    case "${WAIRED_VERSION:-}" in
        ""|latest) printf 'stable' ;;
        edge)      printf 'edge' ;;
        *)         printf '%s' "$WAIRED_VERSION" ;;  # explicit pin
    esac
}

# detect_installed_channel — echo 'edge' or 'stable' for the channel this host
# is *currently* tracking, so an --update/--check that names no channel stays
# on it (edge->edge, stable->stable) instead of silently defaulting to stable.
# Requires detect_os to have run (reads OS_KIND). Linux is authoritative from
# the mutually-exclusive apt source files linux_apt_ensure_repo writes; the
# installed dpkg version shape is the fallback. macOS reads the installed
# binary's version string. Anything unknown is treated as stable.
detect_installed_channel() {
    case "$OS_KIND" in
        linux)
            # The installed package version is the ground truth: an edge build
            # is `<core>~edge...`. Prefer it over the apt source files, which a
            # prior (buggy) stable-defaulting update may have rewritten to
            # waired.list even while an edge build is installed — dpkg-first
            # detection self-heals that state back to edge. Fall back to the
            # configured source only when nothing is installed via dpkg.
            installed_pkg="$(linux_apt_detect_installed)"
            case "$installed_pkg" in
                *~edge*|*-edge*) printf 'edge'; return ;;
            esac
            if [ -n "$installed_pkg" ]; then
                printf 'stable'; return
            fi
            if [ -f /etc/apt/sources.list.d/waired-edge.list ]; then
                printf 'edge'
            else
                printf 'stable'
            fi
            ;;
        darwin)
            case "$(darwin_detect_installed)" in
                *edge*) printf 'edge' ;;
                *)      printf 'stable' ;;
            esac
            ;;
        *) printf 'stable' ;;
    esac
}

# resolve_latest_version <channel> — echo the latest version for the
# channel via the GitHub Releases API (empty on failure; non-fatal). An
# explicit pin is echoed verbatim with no network call. edge is a moving
# prerelease tag (no comparable version) so it is treated as "always
# offer".
resolve_latest_version() {
    case "$1" in
        stable)
            curl -fsSL "https://api.github.com/repos/$WAIRED_INSTALL_REPO/releases/latest" 2>/dev/null \
                | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1 ;;
        edge) printf 'edge' ;;
        *)    printf '%s' "$1" ;;  # explicit pin
    esac
}

# apt_version_pin — the literal apt version to pin to (`waired=<pin>`), or
# empty for the stable / edge channels which install their suite's
# candidate. Crucially this keeps `WAIRED_VERSION=edge` a *channel*
# selector rather than a literal apt version (`waired=edge` would 404).
apt_version_pin() {
    case "$(channel_from_env)" in
        stable|edge) printf '' ;;
        *)           printf '%s' "$WAIRED_VERSION" ;;  # explicit pin
    esac
}

# prompt_update <from> <to> — exit 0 to proceed. Default-YES when a
# controlling terminal is available (read from /dev/tty so the piped
# `curl | sh` one-liner can still prompt); --yes forces yes; a truly
# non-interactive run without --yes reports and declines (safe, reversible).
prompt_update() {
    if [ "$FLAG_YES" = 1 ]; then
        return 0
    fi
    if ! tty_available; then
        common_warn "Update available: $1 -> $2. Re-run with --update --yes to apply (non-interactive)."
        return 1
    fi
    printf '\033[1;36m[waired]\033[0m %s Update waired %s -> %s? [Y/n] (Enter = Yes) ' \
        "$(emo '⬆️' '*')" "$1" "$2" > /dev/tty
    read -r ans < /dev/tty || ans=""
    case "$ans" in
        n|N|no|NO|No) return 1 ;;
        *) return 0 ;;
    esac
}

# ---------------------------------------------------------------------
# detect_* — fill in OS_KIND / OS_FAMILY / OS_NAME / OS_VERSION /
#            OS_CODENAME / OS_ARCH. Everything below dispatches on
#            these.
# ---------------------------------------------------------------------

detect_os() {
    case "$(uname -s)" in
        Linux)
            OS_KIND=linux
            if [ ! -r /etc/os-release ]; then
                common_die "/etc/os-release is missing — unsupported Linux distribution."
            fi
            # shellcheck disable=SC1091
            . /etc/os-release
            OS_NAME="${ID:-unknown}"
            OS_VERSION="${VERSION_ID:-unknown}"
            OS_CODENAME="${VERSION_CODENAME:-${UBUNTU_CODENAME:-}}"
            case "$OS_NAME" in
                debian|ubuntu|linuxmint|pop|elementary) OS_FAMILY=debian ;;
                fedora|rhel|centos|rocky|almalinux)     OS_FAMILY=rhel ;;
                alpine)                                  OS_FAMILY=alpine ;;
                arch|manjaro|endeavouros)                OS_FAMILY=arch ;;
                *)
                    case "${ID_LIKE:-}" in
                        *debian*)        OS_FAMILY=debian ;;
                        *rhel*|*fedora*) OS_FAMILY=rhel ;;
                        *arch*)          OS_FAMILY=arch ;;
                        *)               OS_FAMILY=unknown ;;
                    esac
                    ;;
            esac
            ;;
        Darwin)
            OS_KIND=darwin
            OS_FAMILY=darwin
            OS_NAME=macos
            OS_VERSION="$(sw_vers -productVersion 2>/dev/null || echo unknown)"
            ;;
        *)
            common_die "unsupported OS: $(uname -s)"
            ;;
    esac
}

detect_arch() {
    case "$(uname -m)" in
        x86_64|amd64)  OS_ARCH=amd64 ;;
        aarch64|arm64) OS_ARCH=arm64 ;;
        *) common_die "unsupported CPU architecture: $(uname -m). Waired ships amd64 and arm64 packages." ;;
    esac
}

# ---------------------------------------------------------------------
# linux_apt_* — Debian / Ubuntu handler
# ---------------------------------------------------------------------

# linux_apt_ensure_repo installs apt prerequisites, the Waired signing
# key and the sources.list entry, then refreshes just the waired repo.
# Idempotent and shared by both the fresh install and the update path
# (which needs the repo present to read apt's candidate version). Sets
# $list_file as a side effect for callers' scoped apt-get invocations.
linux_apt_ensure_repo() {
    # zstd is required by upstream ollama.com/install.sh: it ships a
    # .tar.zst and aborts ("requires zstd for extraction") instead of
    # falling back to .tgz when zstd is absent. It is not in Ubuntu's
    # minimal base, so install it here. (macOS/Windows extract with the
    # built-in unzip / Expand-Archive and need nothing extra.)
    common_log "Installing apt prerequisites (ca-certificates, curl, gnupg, zstd)..."
    common_run $SUDO env DEBIAN_FRONTEND=noninteractive apt-get update -qq
    common_run $SUDO env DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates curl gnupg zstd

    keyring_dir=/etc/apt/keyrings
    keyring_file="$keyring_dir/waired-archive-keyring.gpg"
    key_url="$WAIRED_APT_KEY_URL"
    # stable and edge are registered as separate, mutually-exclusive apt
    # sources so a host tracks exactly one channel. Removing the opposite
    # list means a later `apt-get upgrade` only ever sees the active
    # channel's versions (edge `~edge` builds sort below stable, so leaving
    # both would let stable silently win). The signing key is shared — both
    # AR suites are signed with the same regional repo-signing-key.
    if [ "$(channel_from_env)" = edge ]; then
        list_file=/etc/apt/sources.list.d/waired-edge.list
        other_list_file=/etc/apt/sources.list.d/waired.list
    else
        list_file=/etc/apt/sources.list.d/waired.list
        other_list_file=/etc/apt/sources.list.d/waired-edge.list
    fi

    common_log "Installing Waired signing key into $keyring_file"
    common_run $SUDO install -d -m 0755 "$keyring_dir"

    if [ "$DRY_RUN" = 1 ]; then
        common_log "  (dry-run) would fetch $key_url, dearmor if needed, and install into $keyring_file"
    else
        tmp_key="$(mktemp)"
        # shellcheck disable=SC2064
        trap "rm -f '$tmp_key' '$tmp_key.gpg'" EXIT
        curl -fsSL "$key_url" -o "$tmp_key"
        if head -c 64 "$tmp_key" | grep -q -- '-----BEGIN PGP'; then
            gpg --dearmor <"$tmp_key" >"$tmp_key.gpg"
            $SUDO install -m 0644 "$tmp_key.gpg" "$keyring_file"
        else
            $SUDO install -m 0644 "$tmp_key" "$keyring_file"
        fi
    fi

    list_line="deb [signed-by=$keyring_file arch=$OS_ARCH] $WAIRED_APT_BASE_URL $WAIRED_APT_SUITE $WAIRED_APT_COMPONENT"
    common_log "Writing $list_file (suite $WAIRED_APT_SUITE)"
    if [ "$DRY_RUN" = 1 ]; then
        common_log "  (dry-run) would write: $list_line"
        common_log "  (dry-run) would remove the other channel's source: $other_list_file"
    else
        printf '%s\n' "$list_line" | $SUDO tee "$list_file" >/dev/null
        $SUDO chmod 0644 "$list_file"
        # Drop the opposite channel's source so the two never coexist.
        $SUDO rm -f "$other_list_file"
    fi

    common_log "Refreshing apt indexes (only the waired repo)"
    common_run $SUDO env DEBIAN_FRONTEND=noninteractive apt-get update -qq \
        -o Dir::Etc::sourcelist="$list_file" \
        -o Dir::Etc::sourceparts=- \
        -o APT::Get::List-Cleanup=0
}

# linux_apt_detect_installed echoes the installed waired apt version, or
# empty when the package is absent.
linux_apt_detect_installed() {
    dpkg-query -W -f='${Version}' waired 2>/dev/null || true
}

# linux_enrolled reports whether this host already has an agent identity,
# so auto-init is skipped on re-runs / updates of an enrolled host and the
# done-banner shows the "already enrolled" next steps. The state dir is
# intentionally 0700 waired:waired, so a bare `[ -e ]` run by the non-root
# installer user can't traverse it and false-negatives even right after a
# successful enrol — read it through $SUDO (empty when already root, set by
# common_elevate before any linux_* runs).
# shellcheck disable=SC2086
linux_enrolled() {
    $SUDO test -e /var/lib/waired/identity.json
}

# linux_maybe_init runs `waired init` right after install so a single
# `curl | sh` takes the user all the way to a working setup. It runs
# BEFORE the service is started so init takes the full standalone path
# (inference prompts, model download, benchmark) rather than the
# thin/daemon login path. Enrollment + state live in /var/lib/waired
# (root-owned, read by the daemon), so init runs under $SUDO. The
# coding-agent integration is handled inside init itself: it asks one
# consent question (default Yes) and — running under sudo — applies the
# per-user pieces as $SUDO_USER via runuser, so config lands in the
# invoking user's home, not root's. This makes the installer journey
# identical to a plain `sudo waired init`. Skipped when --no-init,
# already enrolled, or there is no controlling terminal (init's sign-in
# is interactive).
linux_maybe_init() {
    [ "$FLAG_NO_INIT" = 1 ] && return 0
    if linux_enrolled; then
        common_log "$(emo '✅' '[ok]') Already enrolled — skipping sign-in."
        return 0
    fi
    if ! tty_available; then
        cat <<EOF

$(emo '💡' 'Note:') No terminal detected — sign-in skipped. To finish setup:
  - run:  sudo waired init
  - or open the tray app and pick "Log in…"
EOF
        return 0
    fi
    if [ "$DRY_RUN" = 1 ]; then
        common_log "  (dry-run) would: $SUDO waired init --state-dir /var/lib/waired </dev/tty"
        return 0
    fi
    common_log "$(emo '🔑' '>>') Starting sign-in (waired init)…"
    set -- init --state-dir /var/lib/waired
    [ "$FLAG_YES" = 1 ] && set -- "$@" --non-interactive
    $SUDO waired "$@" </dev/tty || \
        common_warn "sign-in did not complete; finish later with: sudo waired init"
}

# linux_service_up makes sure the agent service is enabled at boot and
# running now — regardless of whether init ran. Safe even unenrolled: the
# daemon boots identity-less and idles until login (#177), so a non-root
# desktop user can finish via the tray. On update it also restarts to pick
# up the new binary. No-op on non-systemd hosts (e.g. container builds).
linux_service_up() {
    [ -d /run/systemd/system ] || return 0
    mode="${1:-install}"
    if [ "$DRY_RUN" = 1 ]; then
        common_log "  (dry-run) would: $SUDO systemctl enable --now waired-agent"
        [ "$mode" = update ] && common_log "  (dry-run) would: $SUDO systemctl try-restart waired-agent"
        return 0
    fi
    # shellcheck disable=SC2086
    $SUDO systemctl enable --now waired-agent 2>/dev/null || \
        common_warn "could not enable/start waired-agent; start it with: sudo systemctl enable --now waired-agent"
    if [ "$mode" = update ]; then
        # shellcheck disable=SC2086
        $SUDO systemctl try-restart waired-agent 2>/dev/null || true
    fi
}

# linux_apt_update upgrades an existing apt install to the candidate
# version (apt owns version resolution; --only-upgrade never *adds* a
# package the host lacks). On upgrade the .deb postinst preserves
# /etc/waired and restarts the systemd unit onto the new binary (#737), so
# identity/state survive untouched; linux_service_up's own try-restart below
# is belt-and-braces (also covers older debs whose postinst didn't restart).
linux_apt_update() {
    common_log "Detected $OS_NAME $OS_VERSION on $OS_ARCH"
    linux_apt_ensure_repo

    installed="$(linux_apt_detect_installed)"
    candidate="$(apt-cache policy waired 2>/dev/null | awk '/Candidate:/{print $2}')"
    if [ -z "$candidate" ] || [ "$candidate" = "(none)" ]; then
        common_die "no installable waired candidate found in the apt repo."
    fi

    pin="$(apt_version_pin)"
    if [ -z "$pin" ] && [ "$installed" = "$candidate" ]; then
        common_log "waired $installed is already the latest available."
        return 0
    fi

    if [ "$FLAG_CHECK" = 1 ]; then
        common_log "Update available: ${installed:-not installed} -> $candidate"
        return 0
    fi

    prompt_update "${installed:-not installed}" "$candidate" || {
        common_log "Update declined."
        return 0
    }

    pkgs="waired"
    if [ -n "$pin" ]; then
        pkgs="waired=$pin"
    fi
    # Only refresh waired-tray if it is already installed (mirror the
    # host's current footprint; --only-upgrade won't add it otherwise,
    # but naming it keeps the version pin consistent).
    if dpkg-query -W waired-tray >/dev/null 2>&1; then
        if [ -n "$pin" ]; then
            pkgs="$pkgs waired-tray=$pin"
        else
            pkgs="$pkgs waired-tray"
        fi
    fi

    # A channel switch (stable <-> edge) crosses the now-mutually-exclusive
    # apt sources. Switching *to* edge is a downgrade in apt's eyes (an
    # edge `~edge` build sorts below the stable it is based on) and
    # `--only-upgrade` refuses to cross it. Detect the switch from the
    # installed version's shape and fall back to a plain install with
    # --allow-downgrades so the target channel's candidate lands in either
    # direction; otherwise keep the conservative --only-upgrade.
    installed_is_edge=0
    case "$installed" in
        *~edge*|*-edge*) installed_is_edge=1 ;;
    esac
    target_is_edge=0
    if [ "$(channel_from_env)" = edge ]; then
        target_is_edge=1
    fi
    if [ "$installed_is_edge" != "$target_is_edge" ]; then
        apt_mode="--allow-downgrades"
        common_log "Switching apt channel — allowing a version downgrade."
    else
        apt_mode="--only-upgrade"
    fi

    common_log "Updating: $pkgs"
    # shellcheck disable=SC2086
    common_run $SUDO env DEBIAN_FRONTEND=noninteractive apt-get install $apt_mode -y $pkgs
    common_log "Ollama: managed separately; not modified by update."
    # If this host was installed but never enrolled, finish sign-in now
    # (no-op when already enrolled). Then ensure the new binary is running.
    linux_maybe_init
    linux_service_up update
    common_log "$(emo '🎉' '*') waired updated and the service restarted. Check: waired status"
}

linux_apt_install() {
    common_log "Detected $OS_NAME $OS_VERSION (${OS_CODENAME:-unknown codename}) on $OS_ARCH"

    if [ -z "$OS_CODENAME" ]; then
        common_die "could not determine the apt suite for $OS_NAME $OS_VERSION (VERSION_CODENAME missing in /etc/os-release)."
    fi

    linux_apt_ensure_repo

    pin="$(apt_version_pin)"
    pkgs="waired"
    if [ -n "$pin" ]; then
        pkgs="waired=$pin"
    fi
    if [ -z "${WAIRED_NO_TRAY:-}" ]; then
        if [ -n "$pin" ]; then
            pkgs="$pkgs waired-tray=$pin"
        else
            pkgs="$pkgs waired-tray"
        fi
    else
        common_log "WAIRED_NO_TRAY set — skipping waired-tray"
    fi

    common_log "Installing packages: $pkgs"
    # shellcheck disable=SC2086
    common_run $SUDO env DEBIAN_FRONTEND=noninteractive apt-get install -y $pkgs

    linux_apt_write_control_url

    if ollama_skip_requested; then
        common_log "Ollama install skipped (--skip-ollama / WAIRED_NO_OLLAMA)"
        ollama_status="skipped (--skip-ollama / WAIRED_NO_OLLAMA; bundled engine later: sudo waired runtimes install ollama — or bring your own and pick \"reuse\" at sign-in)"
    else
        linux_install_ollama
        ollama_status="installed (local inference engine)"
    fi

    # Drive the rest of first-run setup: sign-in (when interactive), then
    # make sure the daemon is enabled + running regardless.
    linux_maybe_init
    linux_service_up install
    linux_done_banner
}

# linux_done_banner prints the friendly "what just happened / you're ready"
# summary after a fresh install. Branches on whether sign-in completed.
linux_done_banner() {
    party="$(emo '🎉' '*')"
    if linux_enrolled; then
        ready="$(emo '✅' '[ok]') Enrolled — the agent service is running."
        nextline="Check it:     waired status        (try: waired infer \"hello, world!\")"
    else
        ready="$(emo '🔧' '[*]') The agent service is running — ready for sign-in."
        nextline="Sign in:      sudo waired init     (or open the tray app → \"Log in…\")"
    fi
    cat <<EOF

$party Waired is installed.
$ready

$nextline
Ollama:       $ollama_status
Diagnostics:  waired doctor    (logs: journalctl -u waired-agent -e)
Uninstall:    sudo apt purge waired waired-tray
More:         waired init --help
Quickstart:   https://github.com/waired-ai/waired/blob/main/docs/quickstarts/README.md

EOF
}

# Persist $CONTROL_URL into /etc/waired/agent.env so the systemd
# daemon picks it up. The .deb postinst seeds agent.env from
# agent.env.example, which contains only commented-out WAIRED_CONTROL_URL
# lines, so an existing *live* setting means the operator already
# configured this host — in which case we leave it alone.
linux_apt_write_control_url() {
    [ -z "$CONTROL_URL" ] && return 0
    env_file=/etc/waired/agent.env

    if [ "$DRY_RUN" = 1 ]; then
        common_log "Would write WAIRED_CONTROL_URL=$CONTROL_URL to $env_file"
        printf '\033[1;90m[dry-run]\033[0m %s\n' \
            "printf 'WAIRED_CONTROL_URL=%s\\n' '$CONTROL_URL' | $SUDO tee -a $env_file >/dev/null"
        return 0
    fi

    if [ ! -f "$env_file" ]; then
        common_warn "$env_file not present after install — skipping auto-config"
        return 0
    fi

    if $SUDO grep -Eq '^[[:space:]]*WAIRED_CONTROL_URL=.+' "$env_file"; then
        common_warn "$env_file already has an active WAIRED_CONTROL_URL — leaving it as-is"
        CONTROL_URL=""   # don't claim we wrote it in Next steps
        return 0
    fi

    common_log "Writing WAIRED_CONTROL_URL=$CONTROL_URL to $env_file"
    printf 'WAIRED_CONTROL_URL=%s\n' "$CONTROL_URL" | $SUDO tee -a "$env_file" >/dev/null
}

# Install waired's BUNDLED Ollama on Linux via `waired runtimes install
# ollama` (the deb already put `waired` on PATH). It downloads the pinned
# official release into <state-dir>/runtimes/ollama/bin/ollama and hands
# the state dir back to the service user (#484), so waired-agent can
# supervise it as a foreground child on the waired-owned port :9475 — no
# system service, no systemctl. On Linux the agent's bundled resolver
# STRICTLY requires that state-dir binary (it never falls back to a system
# ollama on PATH), so the old `ollama.com/install.sh` system install left
# :9475 unprovisioned: the agent stayed at no_engine and the bundled-model
# pull never ran (#567). The model itself is pulled later by waired-agent
# at boot. Set WAIRED_NO_OLLAMA / --skip-ollama to skip — that is also the
# "reuse" path: bring your own Ollama and choose "reuse" at `waired init`.
# A failure here is non-fatal: waired-agent retries once a usable engine
# appears.
linux_install_ollama() {
    bundled_bin=/var/lib/waired/runtimes/ollama/bin/ollama
    if [ -x "$bundled_bin" ]; then
        common_log "Bundled Ollama already present at $bundled_bin — skipping install"
        return 0
    fi
    common_log "Installing waired's bundled Ollama (waired runtimes install ollama)"
    if [ "$DRY_RUN" = 1 ]; then
        common_log "  (dry-run) would: $SUDO waired runtimes install ollama --yes --state-dir /var/lib/waired"
        return 0
    fi
    # shellcheck disable=SC2086  # $SUDO is intentionally word-split (empty when root)
    if ! $SUDO waired runtimes install ollama --yes --state-dir /var/lib/waired; then
        common_warn "Bundled Ollama install failed; waired-agent will retry at boot. Install by hand later: sudo waired runtimes install ollama"
    fi
}

# ---------------------------------------------------------------------
# darwin_* — macOS handler
#
# Unlike Linux (apt) there is no native package manager path, so we
# download the ad-hoc (unsigned) tarball that release.yml publishes —
# the darwin analogue of the Windows zip — verify its SHA-256, drop the
# binaries into /usr/local/bin, install Ollama.app, and register the
# system LaunchDaemon via `sudo waired-agent install` (#520). curl-
# downloaded binaries do NOT get the Gatekeeper quarantine xattr (only
# browser / LSFileQuarantine downloads do), so unsigned binaries run fine
# here, including as a root LaunchDaemon; code signing / notarization is a
# follow-up (#262).
# ---------------------------------------------------------------------

WAIRED_DARWIN_BINDIR="${WAIRED_DARWIN_BINDIR:-/usr/local/bin}"

darwin_install() {
    common_log "Detected macOS $OS_VERSION on $OS_ARCH"
    common_require_cmd curl shasum tar

    # waired-agent is a system LaunchDaemon (root, boot-time, login-
    # independent — parity with Linux systemd / Windows SCM; #520). The
    # privileged steps below (binary copy, daemon registration, init into
    # the root-owned state dir) run under $SUDO; the integration is then
    # applied as the invoking user via $SUDO_USER. Both `bash install.sh`
    # (non-root, $SUDO=sudo) and `sudo bash install.sh` (already root,
    # $SUDO empty, $SUDO_USER set) work.
    state_dir="/Library/Application Support/waired"

    darwin_install_binaries
    if ollama_skip_requested; then
        common_log "Ollama install skipped (--skip-ollama / WAIRED_NO_OLLAMA)"
    else
        darwin_install_ollama
    fi
    darwin_register_agent "$state_dir"
    darwin_maybe_init "$state_dir"
    darwin_next_steps "$state_dir"
}

# Download + verify waired-darwin-<arch>.tar.gz, place waired +
# waired-agent (+ waired-tray unless WAIRED_NO_TRAY) into
# $WAIRED_DARWIN_BINDIR (on PATH, so the CLI is usable immediately). The
# copy needs sudo for /usr/local/bin. The tray binary is unsigned ad-hoc
# (matching the CLI/agent); the user launches it once and it registers a
# per-user LaunchAgent (com.waired.tray.waired-tray) itself.
darwin_install_binaries() {
    install_mode="${1:-install}"   # "install" (fresh) or "update"
    tarball="waired-darwin-${OS_ARCH}.tar.gz"
    url="$WAIRED_INSTALL_BASE_URL/$tarball"
    sha_url="$url.sha256"

    common_log "Downloading $tarball from $WAIRED_INSTALL_BASE_URL"
    if [ "$DRY_RUN" = 1 ]; then
        common_log "  (dry-run) would: curl -fsSL $url -o <tmp>/$tarball (+ .sha256), verify, tar xzf"
        if [ -n "${WAIRED_NO_TRAY:-}" ]; then
            common_log "  (dry-run) would: $SUDO install -m 0755 waired waired-agent $WAIRED_DARWIN_BINDIR/ (WAIRED_NO_TRAY set — no tray)"
        else
            common_log "  (dry-run) would: $SUDO install -m 0755 waired waired-agent waired-tray $WAIRED_DARWIN_BINDIR/"
        fi
        return 0
    fi

    tmp="$(mktemp -d)"
    # shellcheck disable=SC2064
    trap "rm -rf '$tmp'" EXIT
    curl -fsSL "$url" -o "$tmp/$tarball"
    curl -fsSL "$sha_url" -o "$tmp/$tarball.sha256"

    expected="$(awk '{print $1}' "$tmp/$tarball.sha256")"
    actual="$(shasum -a 256 "$tmp/$tarball" | awk '{print $1}')"
    if [ -z "$expected" ] || [ "$expected" != "$actual" ]; then
        common_die "checksum mismatch for $tarball (expected '$expected', got '$actual')"
    fi
    common_log "Checksum OK ($actual)"

    tar xzf "$tmp/$tarball" -C "$tmp"
    common_log "Installing waired + waired-agent into $WAIRED_DARWIN_BINDIR (sudo)"
    $SUDO install -d -m 0755 "$WAIRED_DARWIN_BINDIR"
    $SUDO install -m 0755 "$tmp/waired"       "$WAIRED_DARWIN_BINDIR/waired"
    $SUDO install -m 0755 "$tmp/waired-agent" "$WAIRED_DARWIN_BINDIR/waired-agent"
    # Tray: install unless WAIRED_NO_TRAY, and only when present in the
    # tarball (graceful with pre-tray tarballs). On update we only
    # refresh the tray when it is already installed — mirroring the
    # Linux apt path, so `--update` never silently adds a tray the user
    # opted out of. Self-registers its LaunchAgent on first launch — see
    # darwin_next_steps.
    if [ -n "${WAIRED_NO_TRAY:-}" ]; then
        common_log "WAIRED_NO_TRAY set — skipping waired-tray"
    elif [ ! -f "$tmp/waired-tray" ]; then
        common_warn "waired-tray not present in $tarball — skipping (older release?)"
    elif [ "$install_mode" = update ] && [ ! -x "$WAIRED_DARWIN_BINDIR/waired-tray" ]; then
        common_log "waired-tray not currently installed — leaving it out (re-run install.sh to add it)"
    else
        common_log "Installing waired-tray into $WAIRED_DARWIN_BINDIR (sudo)"
        $SUDO install -m 0755 "$tmp/waired-tray" "$WAIRED_DARWIN_BINDIR/waired-tray"
    fi
}

# Install Ollama. Reuse an existing install if one is resolvable on
# $PATH or at the well-known paths waired's ResolveBinary checks;
# otherwise download the official Ollama.app into /Applications. No
# Homebrew dependency.
darwin_install_ollama() {
    for cand in \
        "$(command -v ollama 2>/dev/null || true)" \
        /Applications/Ollama.app/Contents/Resources/ollama \
        /usr/local/bin/ollama \
        /opt/homebrew/bin/ollama; do
        if [ -n "$cand" ] && [ -x "$cand" ]; then
            common_log "Ollama already present at $cand — skipping download"
            return 0
        fi
    done

    common_log "Installing Ollama.app from $WAIRED_OLLAMA_DARWIN_URL"
    if [ "$DRY_RUN" = 1 ]; then
        common_log "  (dry-run) would: download Ollama-darwin.zip, verify against sha256sum.txt,"
        common_log "  (dry-run) would: unzip into /Applications, then 'open -a Ollama'"
        return 0
    fi
    common_require_cmd unzip

    otmp="$(mktemp -d)"
    curl -fsSL "$WAIRED_OLLAMA_DARWIN_URL" -o "$otmp/Ollama-darwin.zip"
    # Best-effort integrity check against Ollama's published checksum file
    # (same release dir). Skipped silently if the file is unavailable.
    if curl -fsSL "${WAIRED_OLLAMA_DARWIN_URL%/*}/sha256sum.txt" -o "$otmp/sha256sum.txt" 2>/dev/null; then
        want="$(awk '/Ollama-darwin\.zip/{print $1; exit}' "$otmp/sha256sum.txt")"
        got="$(shasum -a 256 "$otmp/Ollama-darwin.zip" | awk '{print $1}')"
        if [ -n "$want" ] && [ "$want" != "$got" ]; then
            rm -rf "$otmp"
            common_die "Ollama.app checksum mismatch (expected '$want', got '$got')"
        fi
        [ -n "$want" ] && common_log "Ollama.app checksum OK"
    fi

    unzip -q -o "$otmp/Ollama-darwin.zip" -d "$otmp"
    if [ ! -d "$otmp/Ollama.app" ]; then
        rm -rf "$otmp"
        common_die "Ollama-darwin.zip did not contain Ollama.app (layout changed?)"
    fi
    # /Applications is admin-writable; try unprivileged first, fall back
    # to sudo for standard (non-admin) users.
    if ! cp -R "$otmp/Ollama.app" /Applications/ 2>/dev/null; then
        $SUDO cp -R "$otmp/Ollama.app" /Applications/
    fi
    rm -rf "$otmp"
    common_log "Starting Ollama (menu-bar app + 127.0.0.1:11434 server)"
    open -a Ollama 2>/dev/null || \
        common_warn "could not auto-start Ollama; launch it from /Applications once."
}

# Register the system LaunchDaemon. Needs root: the plist lands in
# /Library/LaunchDaemons and bootstrap targets the system domain, and the
# state dir is the root-owned /Library/Application Support/waired.
darwin_register_agent() {
    state_dir="$1"
    common_log "Registering waired-agent system LaunchDaemon (sudo)"
    if [ "$DRY_RUN" = 1 ]; then
        common_log "  (dry-run) would: $SUDO $WAIRED_DARWIN_BINDIR/waired-agent install --state-dir \"$state_dir\""
        return 0
    fi
    $SUDO "$WAIRED_DARWIN_BINDIR/waired-agent" install --state-dir "$state_dir"
}

# darwin_maybe_init finishes first-run setup on macOS. Enrollment + state
# live in the root-owned /Library/Application Support/waired (read by the
# system LaunchDaemon), so init runs under $SUDO — mirroring the Linux
# path. The coding-agent integration is handled inside init itself: it
# asks one consent question (default Yes) and, running under sudo, applies
# the per-user pieces as $SUDO_USER, so config lands in the invoking
# user's home, not root's. Skipped when --no-init, already enrolled, or
# there is no controlling terminal (init's sign-in is interactive).
darwin_maybe_init() {
    state_dir="$1"
    [ "$FLAG_NO_INIT" = 1 ] && return 0
    if [ -e "$state_dir/identity.json" ]; then
        common_log "$(emo '✅' '[ok]') Already enrolled — skipping sign-in."
        return 0
    fi
    if ! tty_available; then
        common_log "$(emo '💡' 'Note:') No terminal detected — run 'sudo waired init' (or use the tray) to sign in."
        return 0
    fi
    if [ "$DRY_RUN" = 1 ]; then
        common_log "  (dry-run) would: $SUDO $WAIRED_DARWIN_BINDIR/waired init --state-dir \"$state_dir\" </dev/tty"
        return 0
    fi
    common_log "$(emo '🔑' '>>') Starting sign-in (waired init)…"
    $SUDO "$WAIRED_DARWIN_BINDIR/waired" init --state-dir "$state_dir" </dev/tty || \
        common_warn "sign-in did not complete; finish later with: sudo waired init"
}

# darwin_detect_installed echoes the installed waired version (via
# `waired version --json`), "unknown" for a pre-version binary, or empty
# when waired is not installed.
darwin_detect_installed() {
    bin=""
    if [ -x "$WAIRED_DARWIN_BINDIR/waired" ]; then
        bin="$WAIRED_DARWIN_BINDIR/waired"
    elif command -v waired >/dev/null 2>&1; then
        bin="$(command -v waired)"
    fi
    [ -z "$bin" ] && return 0
    ver="$("$bin" version --json 2>/dev/null \
        | sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1)"
    if [ -z "$ver" ]; then
        printf 'unknown'
    else
        printf '%s' "$ver"
    fi
}

# darwin_restart_agent reloads the system LaunchDaemon so the freshly
# swapped binary takes effect, falling back to (re-)registration if the
# job is not currently loaded. The system domain needs root, so it runs
# under $SUDO.
darwin_restart_agent() {
    common_log "Restarting waired-agent (launchctl kickstart, sudo)"
    if [ "$DRY_RUN" = 1 ]; then
        common_log "  (dry-run) would: $SUDO launchctl kickstart -k system/com.waired.agent"
        return 0
    fi
    if ! $SUDO launchctl kickstart -k "system/com.waired.agent" 2>/dev/null; then
        common_warn "LaunchDaemon not loaded; (re-)registering it."
        darwin_register_agent "/Library/Application Support/waired"
    fi
}

# darwin_update swaps the /usr/local/bin binaries for the latest release
# (download + SHA-256 verify is shared with the fresh install) and
# reloads the agent. State under /Library/Application Support/waired is
# never touched.
darwin_update() {
    common_log "Detected macOS $OS_VERSION on $OS_ARCH"
    common_require_cmd curl shasum tar

    installed="$(darwin_detect_installed)"
    latest="$(resolve_latest_version "$(channel_from_env)")"
    if [ -z "$latest" ]; then
        common_warn "could not determine the latest version; nothing to do."
        return 0
    fi

    if [ -z "${WAIRED_VERSION:-}" ] && [ -n "$installed" ] && [ "$installed" != "unknown" ] \
        && ! version_lt "$installed" "$latest"; then
        common_log "waired $installed is already up to date."
        return 0
    fi

    if [ "$FLAG_CHECK" = 1 ]; then
        common_log "Update available: ${installed:-unknown} -> $latest"
        return 0
    fi

    prompt_update "${installed:-unknown}" "$latest" || {
        common_log "Update declined."
        return 0
    }

    # "update" mode → refresh-if-present semantics for the tray (see
    # darwin_install_binaries).
    darwin_install_binaries update
    darwin_restart_agent
    # Finish sign-in if this host was installed but never enrolled (no-op
    # when already enrolled).
    darwin_maybe_init "/Library/Application Support/waired"
    common_log "Ollama: managed separately; not modified by update."
    common_log "$(emo '🎉' '*') waired updated to $latest. Check: waired status"
}

darwin_next_steps() {
    state_dir="$1"
    party="$(emo '🎉' '*')"
    if [ -e "$state_dir/identity.json" ]; then
        get_started="$(emo '✅' '[ok]') Enrolled — the agent is running.
  Check it:  waired status   (try: waired infer \"hello, world!\")"
    else
        get_started="Get started:
  1. Sign in: sudo waired init  (or open the tray app → \"Log in…\")
  2. Verify:  waired status     (then: waired infer \"hello, world!\")"
    fi
    if ollama_skip_requested; then
        ollama_status="skipped (--skip-ollama / WAIRED_NO_OLLAMA)"
    else
        ollama_status="installed (local inference engine)"
    fi
    # Tray: bundled unless WAIRED_NO_TRAY. Like the Windows installer we
    # do not auto-launch it (launching a menu-bar app from `curl | sh`
    # is unreliable outside an Aqua session); the user starts it once and
    # it registers its own per-user LaunchAgent.
    if [ -n "${WAIRED_NO_TRAY:-}" ]; then
        tray_line="Tray:        skipped (WAIRED_NO_TRAY)"
        tray_step=""
    else
        tray_line="Tray:        $WAIRED_DARWIN_BINDIR/waired-tray (menu-bar app, unsigned)"
        tray_step="Tray (optional): launch it once; it then returns at every login:
       \"$WAIRED_DARWIN_BINDIR/waired-tray\" >/dev/null 2>&1 &
"
    fi
    cat <<EOF

$party Waired is installed (macOS, $OS_ARCH).

Binaries:    $WAIRED_DARWIN_BINDIR/waired, $WAIRED_DARWIN_BINDIR/waired-agent
$tray_line
State dir:   $state_dir
LaunchDaemon: /Library/LaunchDaemons/com.waired.agent.plist (system, starts at boot)
Ollama:      $ollama_status

$get_started

The agent runs as a system LaunchDaemon and starts at boot, independent of login.
$tray_step
Diagnostics:  waired doctor
              log show --predicate 'process == "waired-agent"' --last 5m
Uninstall:    $SUDO waired-agent uninstall
              launchctl bootout gui/\$(id -u)/com.waired.tray.waired-tray 2>/dev/null
              rm -f ~/Library/LaunchAgents/com.waired.tray.waired-tray.plist
              $SUDO rm -f $WAIRED_DARWIN_BINDIR/waired $WAIRED_DARWIN_BINDIR/waired-agent $WAIRED_DARWIN_BINDIR/waired-tray
More:         waired init --help
Quickstart:   https://github.com/waired-ai/waired/blob/main/docs/quickstarts/README.md

EOF
}

# ---------------------------------------------------------------------
# main
# ---------------------------------------------------------------------

main() {
    while [ $# -gt 0 ]; do
        case "$1" in
            --dry-run) DRY_RUN=1 ;;
            --skip-ollama) FLAG_NO_OLLAMA=1 ;;
            --check) FLAG_CHECK=1 ;;
            --update) FLAG_UPDATE=1 ;;
            --yes|-y) FLAG_YES=1 ;;
            --no-init) FLAG_NO_INIT=1 ;;
            --dev) FLAG_USE_DEV=1 ;;
            # The "latest main build": same as WAIRED_VERSION=edge, but one
            # switch that works on every OS. main() derives the per-OS opt-in
            # (edge apt suite / edge asset base) from it below.
            --edge|--latest) WAIRED_VERSION=edge ;;
            # Force the stable channel on --update/--check, overriding the
            # channel-preservation that would otherwise keep an edge host on
            # edge. The counterpart to --edge; main() clears any edge selection.
            --stable) FLAG_STABLE=1 ;;
            --control)
                shift
                [ "$#" -gt 0 ] || common_die "--control requires a URL argument"
                FLAG_CONTROL_URL="$1"
                ;;
            --control=*)
                FLAG_CONTROL_URL="${1#--control=}"
                [ -n "$FLAG_CONTROL_URL" ] || common_die "--control= requires a URL"
                ;;
            -h|--help) show_help; exit 0 ;;
            *) common_die "unknown argument: $1 (try --help)" ;;
        esac
        shift
    done

    print_banner

    # detect_os/detect_arch run first (before the channel block below) because
    # detect_installed_channel reads OS_KIND. Neither needs elevation.
    detect_os
    detect_arch

    # Channel resolution. --stable forces stable (clearing any edge selection);
    # otherwise, an --update/--check that named no channel *preserves* the
    # channel this host already tracks (edge stays edge) so `waired update`
    # never silently moves an edge host to stable. An explicit pin
    # (WAIRED_VERSION=1.2.3) or --edge/WAIRED_VERSION=edge is left untouched.
    if [ "$FLAG_STABLE" = 1 ]; then
        WAIRED_VERSION=""
    elif [ "$(channel_from_env)" != edge ] && [ -z "${WAIRED_VERSION:-}" ] \
        && { [ "$FLAG_UPDATE" = 1 ] || [ "$FLAG_CHECK" = 1 ]; } \
        && [ "$(detect_installed_channel)" = edge ]; then
        WAIRED_VERSION=edge
    fi

    # Edge channel unification: a bare `WAIRED_VERSION=edge` (or --edge /
    # --latest, or a preserved edge host above) is enough on every OS. Derive
    # the per-OS opt-in the user would otherwise have to know — the edge apt
    # suite (Linux) and the edge prerelease asset base (macOS) — unless they
    # pinned those explicitly (in which case the explicit value wins).
    if [ "$(channel_from_env)" = edge ]; then
        if [ -z "$_WAIRED_APT_SUITE_SET" ]; then
            WAIRED_APT_SUITE=waired-dev-apt-edge
        fi
        if [ -z "$_WAIRED_INSTALL_BASE_URL_SET" ]; then
            WAIRED_INSTALL_BASE_URL=https://github.com/waired-ai/waired-install/releases/download/edge
        fi
    fi

    resolve_control_url

    common_elevate

    case "$OS_KIND:$OS_FAMILY" in
        linux:debian)
            if [ "$FLAG_CHECK" = 1 ] || [ "$FLAG_UPDATE" = 1 ] || [ -n "$(linux_apt_detect_installed)" ]; then
                linux_apt_update
            else
                linux_apt_install
            fi
            ;;
        linux:rhel)
            common_die "Fedora / RHEL support is not yet available. Follow https://github.com/waired-ai/waired/issues for updates."
            ;;
        linux:alpine)
            common_die "Alpine support is not yet available."
            ;;
        linux:arch)
            common_die "Arch support is not yet available. Track it via the AUR — coming later."
            ;;
        darwin:*)
            if [ "$FLAG_CHECK" = 1 ] || [ "$FLAG_UPDATE" = 1 ] || [ -n "$(darwin_detect_installed)" ]; then
                darwin_update
            else
                darwin_install
            fi
            ;;
        *)
            common_die "$OS_NAME ($OS_KIND/$OS_FAMILY) is not yet supported. Please file an issue."
            ;;
    esac
}

main "$@"
