#!/bin/sh
# uninstall.sh — remove Waired (Linux apt / macOS tarball install).
#
# Usage:
#   curl -fsSL https://github.com/waired-ai/waired-agent/releases/latest/download/uninstall.sh | sh
#   curl -fsSL https://github.com/waired-ai/waired-agent/releases/latest/download/uninstall.sh | sh -s -- --clean --yes
#   curl -fsSL https://github.com/waired-ai/waired-agent/releases/latest/download/uninstall.sh | sh -s -- --dry-run
#
# Counterpart to install.sh. Two tiers, matching apt's remove / purge split:
#
#   default   — remove the binaries + service registration but KEEP local
#               config and state (identity, keys, settings).  Linux:
#               `apt-get remove`.
#   --clean   — also delete config + state, the apt source install.sh added,
#               the legacy Claude-proxy trust, and Ollama (app + downloaded
#               models).  Linux: `apt-get purge` + repo cleanup.  Destructive
#               and irreversible — guarded by a confirmation (see --yes).
#
# Both tiers also best-effort DEREGISTER this device from the Control Plane
# (it's revoked — removed from the account's device list and dropped from
# peers). That happens inside the delegated removal step below, not here:
# the deb `prerm` runs `waired logout --revoke --server-only` on apt
# remove/purge, and `waired-agent uninstall` self-revokes before tearing the
# service down. It's best-effort — offline / CP-unreachable never blocks the
# uninstall; the device can be removed from the web admin instead.
#
# The privileged removal logic lives in the binaries / package, not here:
# this script prefers `waired-agent uninstall`, `waired proxy uninstall` and
# the deb maintainer scripts, then cleans up only the residue install.sh
# itself scattered (apt source list, Ollama, per-user autostart). It never
# re-implements service / keychain / deregistration removal.
#
# Shares install.sh's structure + env contract. New OSes plug in the same way
# install.sh documents: a detect_os branch + a <kind>_uninstall handler + a
# case arm in main().
#
# Function namespaces (mirror install.sh):
#   common_*      shared helpers — log, run, sudo, tty, confirm
#   detect_*      probe the host (kernel, distro)
#   linux_apt_*   Debian / Ubuntu remover
#   darwin_*      macOS: launchd + binaries + tarball residue

set -eu

# macOS: where install.sh placed the binaries. Mirror its default so the
# uninstall targets the same paths.
WAIRED_DARWIN_BINDIR="${WAIRED_DARWIN_BINDIR:-/usr/local/bin}"

DRY_RUN=0
SUDO=""
FLAG_CLEAN=0
FLAG_YES=0
OS_KIND=""
OS_FAMILY=""
OS_NAME=""

# ---------------------------------------------------------------------
# common_* helpers (kept byte-compatible with install.sh)
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
    common_die "this uninstaller needs root privileges. Install sudo, or re-run as root."
}

# tty_available reports whether we can prompt the user even when stdin is a
# pipe — the `curl | sh` case. Same open()-both-ends check install.sh uses
# (a bare `[ -r /dev/tty ]` false-positives in CI / containers).
tty_available() {
    ( exec </dev/tty >/dev/tty ) 2>/dev/null
}

# real_user_home echoes the home directory of the human running the
# uninstall, even under sudo (where $HOME is root's). Used (macOS only) to
# reach the per-user LaunchAgent / Application Support / ~/.ollama. Falls
# back to $HOME when there is no SUDO_USER. dscl is the macOS directory
# query; the function is never called on Linux.
real_user_home() {
    if [ -n "${SUDO_USER:-}" ] && [ "$SUDO_USER" != root ]; then
        dscl . -read "/Users/$SUDO_USER" NFSHomeDirectory 2>/dev/null \
            | awk '{print $2}'
        return
    fi
    printf '%s\n' "${HOME:-}"
}

# Run a command as the invoking (non-root) user. macOS per-user launchd and
# dotfiles must NOT be touched as root. No-op-safe under dry-run.
common_run_user() {
    if [ -n "${SUDO_USER:-}" ] && [ "$SUDO_USER" != root ] && [ "$(id -u)" -eq 0 ]; then
        common_run sudo -u "$SUDO_USER" "$@"
    else
        common_run "$@"
    fi
}

show_help() {
    cat <<HELP
uninstall.sh — remove Waired (Linux apt / macOS tarball install).

Usage:
  curl -fsSL https://github.com/waired-ai/waired-agent/releases/latest/download/uninstall.sh | sh
  curl -fsSL .../uninstall.sh | sh -s -- --clean --yes
  curl -fsSL .../uninstall.sh | sh -s -- --dry-run

By default this removes the Waired binaries and unregisters the service but
KEEPS your local config and state (identity, keys, settings). Either tier also
best-effort deregisters this device from your Waired account (removed from your
device list). Pass --clean for a full local wipe.

Options:
  --clean          also delete config + state, the apt source install.sh
                   added, the legacy Claude-proxy trust, and Ollama (app +
                   downloaded models). Destructive — asks to confirm unless
                   --yes is given.
  --yes, -y        assume "yes" to the --clean confirmation (required to
                   --clean on a non-interactive / piped shell)
  --dry-run        show every privileged command without running it
  -h, --help       print this help

Environment variables (shared with install.sh):
  WAIRED_DARWIN_BINDIR   macOS: where the binaries were installed
                         (default: /usr/local/bin)
  WAIRED_STATE_DIR       if set, this path is also removed under --clean
HELP
}

# Confirm the destructive --clean wipe. Skipped entirely without --clean.
# --yes bypasses the prompt; a non-interactive shell without --yes aborts so
# `curl | sh -s -- --clean` can never silently nuke state.
confirm_clean() {
    [ "$FLAG_CLEAN" = 1 ] || return 0
    [ "$FLAG_YES" = 1 ] && return 0
    if tty_available; then
        common_warn "--clean will PERMANENTLY delete Waired config, keys and state"
        common_warn "(identity / secrets), the apt source, and Ollama + its models."
        printf '\033[1;33m[waired]\033[0m %s' "Continue? [y/N] " >/dev/tty
        read -r ans </dev/tty || ans=""
        case "$ans" in
            y|Y|yes|YES) return 0 ;;
            *) common_die "aborted — nothing was removed" ;;
        esac
    fi
    common_die "--clean is destructive; re-run with --yes to confirm on a non-interactive shell"
}

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
            ;;
        *)
            common_die "unsupported OS: $(uname -s)"
            ;;
    esac
}

# ---------------------------------------------------------------------
# linux_apt_* — Debian / Ubuntu remover
# ---------------------------------------------------------------------

# Echo the dpkg Status of $1 ("install ok installed", "deinstall ok
# config-files", …) or nothing if dpkg has never heard of it.
linux_pkg_status() {
    dpkg-query -W -f='${Status}' "$1" 2>/dev/null || true
}

linux_apt_uninstall() {
    common_require_cmd dpkg-query apt-get

    # Build the package set to act on. For a plain remove only
    # currently-installed packages count; for --clean (purge) we also catch
    # packages left in config-files state by an earlier remove.
    pkgs=""
    for pkg in waired waired-tray; do
        case "$(linux_pkg_status "$pkg")" in
            "install ok installed")
                pkgs="$pkgs $pkg" ;;
            *config-files)
                [ "$FLAG_CLEAN" = 1 ] && pkgs="$pkgs $pkg" ;;
        esac
    done

    if [ -n "$pkgs" ]; then
        if [ "$FLAG_CLEAN" = 1 ]; then
            common_log "apt-get purge$pkgs (removes /etc/waired, /var/lib/waired, waired user/group)"
            # shellcheck disable=SC2086
            common_run $SUDO apt-get purge -y $pkgs
        else
            common_log "apt-get remove$pkgs (keeps /etc/waired + /var/lib/waired)"
            # shellcheck disable=SC2086
            common_run $SUDO apt-get remove -y $pkgs
        fi
    else
        common_log "no Waired apt packages installed"
    fi

    if [ "$FLAG_CLEAN" = 1 ]; then
        linux_apt_remove_repo
        linux_remove_ollama
        if [ -n "${WAIRED_STATE_DIR:-}" ]; then
            common_log "Removing WAIRED_STATE_DIR ($WAIRED_STATE_DIR)"
            common_run $SUDO rm -rf "$WAIRED_STATE_DIR"
        fi
    fi
}

# Remove the apt source list + signing key install.sh wrote. The deb's
# postrm does NOT touch these (they belong to the installer, not the
# package), so a purge alone leaves the repo wired up.
linux_apt_remove_repo() {
    common_log "Removing the Waired apt source + signing key"
    # shellcheck disable=SC2086
    common_run $SUDO rm -f \
        /etc/apt/sources.list.d/waired.list \
        /etc/apt/sources.list.d/waired-edge.list \
        /etc/apt/keyrings/waired-archive-keyring.gpg
    # shellcheck disable=SC2086
    common_run $SUDO apt-get update || common_warn "apt-get update failed (non-fatal)"
}

# Best-effort removal of an Ollama installed by upstream ollama.com/install.sh
# (the path install.sh uses on Linux). Existence-gated so it is a no-op when
# Ollama was never installed; tolerant of every step so a partial install
# still cleans up.
linux_remove_ollama() {
    if ! command -v ollama >/dev/null 2>&1 \
        && [ ! -e /usr/local/bin/ollama ] && [ ! -e /usr/bin/ollama ]; then
        common_log "Ollama not present — skipping"
        return 0
    fi
    common_log "Removing Ollama (binary, models, service, user)"
    if [ -d /run/systemd/system ]; then
        # shellcheck disable=SC2086
        common_run $SUDO systemctl stop ollama 2>/dev/null || true
        # shellcheck disable=SC2086
        common_run $SUDO systemctl disable ollama 2>/dev/null || true
    fi
    # shellcheck disable=SC2086
    common_run $SUDO rm -f \
        /etc/systemd/system/ollama.service \
        /usr/local/bin/ollama /usr/bin/ollama
    # shellcheck disable=SC2086
    common_run $SUDO rm -rf /usr/share/ollama
    if getent passwd ollama >/dev/null 2>&1; then
        # shellcheck disable=SC2086
        common_run $SUDO userdel ollama 2>/dev/null || true
    fi
    if getent group ollama >/dev/null 2>&1; then
        # shellcheck disable=SC2086
        common_run $SUDO groupdel ollama 2>/dev/null || true
    fi
}

# ---------------------------------------------------------------------
# darwin_* — macOS handler
# ---------------------------------------------------------------------

darwin_uninstall() {
    bindir="$WAIRED_DARWIN_BINDIR"

    # 1. System LaunchDaemon (com.waired.agent). Prefer the binary's own
    #    uninstall — it boots out the job and removes the plist exactly as it
    #    installed them. Fall back to manual launchctl/rm if the binary is
    #    already gone.
    if [ -x "$bindir/waired-agent" ]; then
        common_log "Unregistering the waired-agent LaunchDaemon"
        # shellcheck disable=SC2086
        common_run $SUDO "$bindir/waired-agent" uninstall || \
            common_warn "waired-agent uninstall failed — cleaning up by hand"
    fi
    # shellcheck disable=SC2086
    common_run $SUDO launchctl bootout system/com.waired.agent 2>/dev/null || true
    # shellcheck disable=SC2086
    common_run $SUDO rm -f /Library/LaunchDaemons/com.waired.agent.plist
    # newsyslog log-rotation drop-in (install.sh's darwin_install_log_rotation).
    # It is config the installer added, so remove it on any uninstall — like
    # the plist above — not just under --clean (which handles the log data).
    # shellcheck disable=SC2086
    common_run $SUDO rm -f /etc/newsyslog.d/waired-agent.conf

    # 2. Per-user tray LaunchAgent (com.waired.tray.waired-tray). Must be
    #    touched as the invoking user, not root.
    common_log "Removing the waired-tray menu-bar autostart"
    target_user="${SUDO_USER:-$(id -un)}"
    uid="$(id -u "$target_user" 2>/dev/null || id -u)"
    common_run_user launchctl bootout "gui/$uid/com.waired.tray.waired-tray" 2>/dev/null || true
    home="$(real_user_home)"
    [ -n "$home" ] && common_run rm -f "$home/Library/LaunchAgents/com.waired.tray.waired-tray.plist"

    # 3. Legacy transparent Claude proxy (#488, removal-only). The binary
    #    knows the keychain cert CN + /etc/zshenv block, so let it undo them
    #    while it is still present.
    if [ -x "$bindir/waired" ]; then
        common_log "Removing legacy Claude-proxy trust (if any)"
        # shellcheck disable=SC2086
        common_run $SUDO "$bindir/waired" proxy uninstall 2>/dev/null || true
    fi

    # 4. Binaries.
    common_log "Removing binaries from $bindir"
    # shellcheck disable=SC2086
    common_run $SUDO rm -f "$bindir/waired" "$bindir/waired-agent" "$bindir/waired-tray"

    # 5. --clean: state, logs, Ollama.
    if [ "$FLAG_CLEAN" = 1 ]; then
        common_log "Removing state directories (identity, keys, settings)"
        # shellcheck disable=SC2086
        common_run $SUDO rm -rf "/Library/Application Support/waired"
        if [ -n "$home" ]; then
            common_run rm -rf "$home/Library/Application Support/waired"
            common_run rm -rf "$home/.ollama"
        fi
        if [ -n "${WAIRED_STATE_DIR:-}" ]; then
            # shellcheck disable=SC2086
            common_run $SUDO rm -rf "$WAIRED_STATE_DIR"
        fi
        common_log "Removing logs"
        # shellcheck disable=SC2086
        common_run $SUDO rm -f /Library/Logs/waired-agent.out.log /Library/Logs/waired-agent.err.log
        [ -n "$home" ] && common_run rm -f \
            "$home/Library/Logs/waired-tray.out.log" \
            "$home/Library/Logs/waired-tray.err.log"
        common_log "Removing Ollama.app"
        # shellcheck disable=SC2086
        common_run $SUDO rm -rf /Applications/Ollama.app
    fi
}

print_done() {
    if [ "$FLAG_CLEAN" = 1 ]; then
        common_log "Waired fully removed (config + state wiped)."
    else
        common_log "Waired removed. Local config + state were kept; re-run with --clean to wipe them."
    fi
    common_log "This device was deregistered from your Waired account (best-effort). If it was"
    common_log "offline during uninstall, remove it from the web admin device list."
}

main() {
    while [ $# -gt 0 ]; do
        case "$1" in
            --clean)    FLAG_CLEAN=1 ;;
            --yes|-y)   FLAG_YES=1 ;;
            --dry-run)  DRY_RUN=1 ;;
            -h|--help)  show_help; exit 0 ;;
            *) common_die "unknown argument: $1 (try --help)" ;;
        esac
        shift
    done

    detect_os
    confirm_clean
    common_elevate

    case "$OS_KIND:$OS_FAMILY" in
        linux:debian)
            linux_apt_uninstall
            ;;
        darwin:*)
            darwin_uninstall
            ;;
        *)
            common_die "$OS_NAME ($OS_KIND/$OS_FAMILY) is not supported by this uninstaller."
            ;;
    esac

    print_done
}

main "$@"
