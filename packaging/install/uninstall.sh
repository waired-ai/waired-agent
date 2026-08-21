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
#               the legacy Claude-proxy trust, and the bundled Ollama with
#               its downloaded models (it lives inside the state dir).
#               Linux: `apt-get purge` + repo cleanup.  Destructive and
#               irreversible — guarded by a confirmation (see --yes).
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
# re-implements service / deregistration removal.
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
# And where install.sh put the menu-bar app (waired-agent#833). Mirrors
# install.sh's WAIRED_DARWIN_APPDIR so the uninstall targets the same path.
WAIRED_DARWIN_APPDIR="${WAIRED_DARWIN_APPDIR:-/Applications}"

DRY_RUN=0
SUDO=""
# Same bound as install.sh's, for the same reason (#893): apt waits for the
# dpkg lock for ever, so an uninstall run while another package manager is
# busy looks hung rather than saying so. No Acquire options here — remove
# and purge fetch nothing.
APT_BOUNDS="-o DPkg::Lock::Timeout=120"
# And a wall clock over the top, as in install.sh: the options bound what
# apt knows it is doing, and the stall behind #893 was not one of those.
APT_TIMEOUT="${WAIRED_APT_TIMEOUT:-300}"
FLAG_CLEAN=0
FLAG_YES=0
OS_KIND=""
OS_FAMILY=""
OS_NAME=""

# ---------------------------------------------------------------------
# common_* helpers (kept byte-compatible with install.sh)
# ---------------------------------------------------------------------

# mask_pii — mirror of install.sh's (see there for docs): best-effort
# masking of the home dir + username path segments for screenshots/reports.
mask_pii() {
    if [ -z "${WAIRED_PII_MASK:-}" ]; then
        printf '%s' "$*"
        return 0
    fi
    printf '%s' "$*" | awk \
        -v h="${HOME:-}" -v u="$(id -un 2>/dev/null || echo '')" -v s="${SUDO_USER:-}" '
    function repl(str, pat, rep,   out, i) {
        if (pat == "") return str
        out = ""
        while ((i = index(str, pat)) > 0) {
            out = out substr(str, 1, i - 1) rep
            str = substr(str, i + length(pat))
        }
        return out str
    }
    {
        if (length(h) >= 3) $0 = repl($0, h, "<home>")
        if (length(u) >= 3) $0 = repl($0, "/" u, "/<user>")
        if (length(s) >= 3 && s != u) $0 = repl($0, "/" s, "/<user>")
        print
    }'
}

common_log()  { printf '\033[1;36m[waired]\033[0m %s\n' "$(mask_pii "$*")"; }
common_warn() { printf '\033[1;33m[waired]\033[0m %s\n' "$(mask_pii "$*")" >&2; }
common_die()  { printf '\033[1;31m[waired]\033[0m %s\n' "$(mask_pii "$*")" >&2; exit 1; }

# What this run actually did, so print_done can describe it instead of
# asserting it. print_done used to claim "Waired fully removed" and "This
# device was deregistered from your Waired account" on every run, including a
# run on a machine with nothing installed and no identity — claims with no
# object. Reported against uninstall.ps1 (waired-agent#793); the POSIX side
# carried the same defect, unreported.
#
# common_run is the chokepoint every mutating step passes through, so the
# count cannot drift from what the steps did.
DID_COUNT=0
DEREGISTERED=0

# Run a command, or print it in dry-run mode.
common_run() {
    DID_COUNT=$((DID_COUNT + 1))
    if [ "$DRY_RUN" = 1 ]; then
        printf '\033[1;90m[dry-run]\033[0m %s\n' "$*"
        return 0
    fi
    "$@"
}

# The only way this script runs apt-get (#893). Same shape as install.sh's:
# bounded by options, bounded by the clock, retried once when the clock is
# what stopped it. A removal that cannot get the lock must say so rather
# than sit there.
apt_bounded() {
    _apt_try=1
    while :; do
        # shellcheck disable=SC2086  # both are option lists, split on purpose
        if common_run $SUDO env DEBIAN_FRONTEND=noninteractive \
            timeout "$APT_TIMEOUT" apt-get $APT_BOUNDS "$@"; then
            return 0
        fi
        _apt_rc=$?
        if [ "$_apt_rc" -eq 124 ] && [ "$_apt_try" -lt 2 ]; then
            common_warn "apt made no progress for ${APT_TIMEOUT}s; trying once more"
            _apt_try=$((_apt_try + 1))
            continue
        fi
        return "$_apt_rc"
    done
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

# supports_emoji / section — mirror install.sh (see there for docs): a blank
# line + horizontal-rule heading that splits the output into readable steps.
supports_emoji() {
    [ -n "${WAIRED_NO_EMOJI:-}" ] && return 1
    case "${LC_ALL:-${LC_CTYPE:-${LANG:-}}}" in
        *UTF-8*|*UTF8*|*utf-8*|*utf8*) return 0 ;;
        *) return 1 ;;
    esac
}

section() {
    _s_d='-'
    if supports_emoji; then _s_d='─'; fi
    _s_n=$((49 - ${#1}))
    [ "$_s_n" -lt 3 ] && _s_n=3
    _s_tail=''
    while [ "$_s_n" -gt 0 ]; do
        _s_tail="$_s_tail$_s_d"
        _s_n=$((_s_n - 1))
    done
    if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
        printf '\n\033[36m%s %s %s\033[0m\n' "$_s_d$_s_d$_s_d" "$1" "$_s_tail"
    else
        printf '\n%s %s %s\n' "$_s_d$_s_d$_s_d" "$1" "$_s_tail"
    fi
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
                   added, the legacy Claude-proxy trust, and the bundled
                   Ollama with its downloaded models. Destructive — asks to
                   confirm unless --yes is given.
  --yes, -y        assume "yes" to the pre-uninstall confirmation (--clean
                   requires it on a non-interactive / piped shell)
  --dry-run        show every privileged command without running it
  --mask-pii       mask personal information (home dir, username) in the
                   output — for screenshots and bug reports. Best-effort.
                   Same as WAIRED_PII_MASK=1.
  -h, --help       print this help

Environment variables (shared with install.sh):
  WAIRED_DARWIN_BINDIR   macOS: where the binaries were installed
                         (default: /usr/local/bin)
  WAIRED_STATE_DIR       if set, this path is also removed under --clean
HELP
}

# confirm_proceed shows what is about to be removed, then asks before
# anything runs. Default is NO — uninstalling is destructive, so a bare
# Enter aborts. --yes bypasses; --dry-run previews without asking. A
# non-interactive shell proceeds for the plain tier (preserves piped
# `curl | sh` uninstalls) but still refuses --clean without --yes, so
# `curl | sh -s -- --clean` can never silently nuke state. Mirrors
# uninstall.ps1's Confirm-Uninstall.
confirm_proceed() {
    section 'What this will remove'
    case "$OS_KIND" in
        linux)  printf '  * The Waired apt packages (waired, waired-tray) and background service\n' ;;
        darwin) printf '  * The Waired binaries under %s and the background service\n' "$WAIRED_DARWIN_BINDIR" ;;
    esac
    printf '  * The Claude Code / coding-agent integration for this user\n'
    printf "  * This device's registration in your Waired account (best-effort)\n"
    if [ "$FLAG_CLEAN" = 1 ]; then
        # Linux spells out the directories because --clean now takes the whole
        # of /etc/waired, files waired never installed included -- an operator
        # who put an enrolment key there has to be told before agreeing, not
        # after (waired-agent#792).
        case "$OS_KIND" in
            linux)
                printf '  * ALL local state: everything under /etc/waired and /var/lib/waired,\n'
                printf '    including keys you placed there yourself (PERMANENT)\n' ;;
            *)
                printf '  * ALL local state: config, keys, identity (PERMANENT)\n' ;;
        esac
        printf '  * Ollama and its downloaded models (PERMANENT)\n'
    else
        printf '  (local config + state are KEPT; re-run with --clean to wipe them)\n'
    fi

    [ "$FLAG_YES" = 1 ] && return 0
    [ "$DRY_RUN" = 1 ] && return 0
    if ! tty_available; then
        if [ "$FLAG_CLEAN" = 1 ]; then
            common_die "--clean is destructive; re-run with --yes to confirm on a non-interactive shell"
        fi
        common_log "No terminal detected — proceeding without confirmation (use --yes to silence this notice)."
        return 0
    fi
    printf '\n\033[1;33m[waired]\033[0m %s' "Proceed with the uninstall? [y/N] (Enter = No) " >/dev/tty
    read -r ans </dev/tty || ans=""
    case "$ans" in
        y|Y|yes|YES) return 0 ;;
        *) common_die "aborted — nothing was removed" ;;
    esac
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

    # Remove the per-user Claude Code / coding-agent integration while the
    # `waired` binary is still installed: `claude disable` (root, SUDO_USER
    # preserved) for the managed settings + routing skill/statusline and any
    # retired-MITM proxy artifacts; `unlink` (as the invoking user) for the
    # ledger'd adapters (~/.claude skills, ~/.openclaw) plus the withdrawn
    # OpenCode integration's leftovers (~/.config/opencode; waired-agent#333,
    # drop one release after it shipped).
    # Best-effort; the apt purge below does not reach per-user homes. waired#754.
    if command -v waired >/dev/null 2>&1; then
        common_log "Removing the Claude Code / coding-agent integration"
        # shellcheck disable=SC2086
        common_run $SUDO waired claude disable 2>/dev/null || true
        common_run_user waired unlink 2>/dev/null || true
    fi

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
        # The deb's prerm runs `waired logout --revoke --server-only`, so
        # reaching apt at all is what entitles print_done to say the device
        # was deregistered. With no packages installed there is nothing to
        # revoke and nothing to claim (waired-agent#793).
        DEREGISTERED=1
        if [ "$FLAG_CLEAN" = 1 ]; then
            # Says what purge itself does. It removes /var/lib/waired and the
            # waired user/group outright, but in /etc/waired the postrm only
            # deletes the agent.env it wrote and then rmdir's the directory
            # --ignore-fail-on-non-empty -- so anything else in there survives
            # this step. linux_purge_config_dir below is what makes --clean's
            # "ALL local state" true (waired-agent#792).
            common_log "apt-get purge$pkgs (removes /var/lib/waired, the waired user/group, and the packaged config)"
            # shellcheck disable=SC2086
            apt_bounded purge -y $pkgs
        else
            common_log "apt-get remove$pkgs (keeps /etc/waired + /var/lib/waired)"
            # shellcheck disable=SC2086
            apt_bounded remove -y $pkgs
        fi
    else
        common_log "no Waired apt packages installed"
    fi

    if [ "$FLAG_CLEAN" = 1 ]; then
        linux_purge_config_dir
        linux_apt_remove_repo
        linux_remove_ollama
        if [ -n "${WAIRED_STATE_DIR:-}" ]; then
            common_log "Removing WAIRED_STATE_DIR ($WAIRED_STATE_DIR)"
            common_run $SUDO rm -rf "$WAIRED_STATE_DIR"
        fi
    fi
}

# --clean promises "ALL local state: config, keys, identity (PERMANENT)".
# apt purge cannot keep that promise on its own: the deb's postrm removes the
# one file it wrote (/etc/waired/agent.env) and then calls
#
#     rmdir --ignore-fail-on-non-empty /etc/waired
#
# which by construction does nothing when anything else is in there. So an
# operator-placed enrolment key (/etc/waired/authkey, the path
# docs-site's first-run page tells people to use) and any agent.env backup
# survived a wipe that had just said it removed keys -- silently, because the
# rmdir failure is discarded. /var/lib/waired next to it is an unconditional
# rm -rf, and Windows (-Clean removes %ProgramData%\waired whole) and macOS
# (rm -rf of the state dirs) both already remove the directory rather than the
# files they recognise; Linux was the outlier (waired-agent#792).
#
# The fix belongs here rather than in the postrm: a package must not delete
# files it does not own, and plenty of hosts install the deb through apt
# directly and never run this script. The promise was made by the installer,
# so the installer keeps it -- and names what it is taking, since these are by
# definition files waired did not put there.
#
# One `find` answers both questions -- is the directory there, and what is
# left in it -- so the two can never disagree, and so the whole decision is
# driven by a command the hermetic harness can stub from outside rather than
# by a test-only branch in here (installtest-dash.sh's `uname` idiom). No
# sudo: /etc/waired is mode 0755, and listing names needs nothing more.
# Removal is still privileged.
linux_purge_config_dir() {
    _found=$(find /etc/waired 2>/dev/null | sort)
    [ -n "$_found" ] || return 0
    _leftover=$(printf '%s\n' "$_found" | grep -v '^/etc/waired$' || true)
    if [ -n "$_leftover" ]; then
        common_log "Removing /etc/waired, including files the packages did not install:"
        printf '%s\n' "$_leftover" | while IFS= read -r _f; do
            [ -n "$_f" ] && common_log "  $_f"
        done
    else
        common_log "Removing the empty /etc/waired"
    fi
    # shellcheck disable=SC2086
    common_run $SUDO rm -rf /etc/waired
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
        # `waired-agent uninstall` self-revokes before tearing the service
        # down, so this is the darwin deregistration point (waired-agent#793).
        DEREGISTERED=1
        # shellcheck disable=SC2086
        common_run $SUDO "$bindir/waired-agent" uninstall || \
            common_warn "waired-agent uninstall failed — cleaning up by hand"
    fi
    # shellcheck disable=SC2086
    common_run $SUDO launchctl bootout system/com.waired.agent 2>/dev/null || true
    # Clear any persistent launchd override (#176). waired-agent builds between
    # 2026-07-15 and that fix ran `launchctl disable` here, which WRITES an
    # entry into /var/db/com.apple.xpc.launchd/disabled.plist — it outlives the
    # plist removed below, the state dir, --clean and a reboot, and makes every
    # later `launchctl bootstrap` fail with EIO(5). The binary we just delegated
    # to may well be one of those builds, so undo it unconditionally rather than
    # leaving the host unable to reinstall.
    # shellcheck disable=SC2086
    common_run $SUDO launchctl enable system/com.waired.agent 2>/dev/null || true
    # shellcheck disable=SC2086
    common_run $SUDO rm -f /Library/LaunchDaemons/com.waired.agent.plist
    # newsyslog log-rotation drop-in. Retired in #331 (the agent rotates its own
    # logs now — install.sh's darwin_retire_log_rotation removes it on install
    # and update), but an uninstall still has to clear it from hosts that were
    # installed before that and never updated.
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

    # 3. Claude Code + coding-agent integration. `claude disable` (as root, with
    #    SUDO_USER preserved so its ~/.claude edits hop to the human) removes the
    #    managed settings + routing skill/statusline and sweeps any retired-MITM
    #    proxy artifacts; `unlink` (as the invoking user) removes the ledger'd
    #    coding-agent adapters (~/.claude skills, ~/.openclaw) plus the
    #    withdrawn OpenCode integration's leftovers (waired-agent#333).
    #    Replaces the removed `waired proxy uninstall` (waired#750/#754).
    if [ -x "$bindir/waired" ]; then
        common_log "Removing the Claude Code / coding-agent integration"
        # shellcheck disable=SC2086
        common_run $SUDO "$bindir/waired" claude disable 2>/dev/null || true
        common_run_user "$bindir/waired" unlink 2>/dev/null || true
    fi

    # 4. Binaries, and the app bundle they now live in.
    #
    # /Applications/Waired.app is ours unconditionally, unlike the Ollama.app
    # left alone in step 5: install.sh builds this one (darwin_install_app,
    # waired-agent#833) and nothing else on the machine puts a bundle at that
    # path. $bindir/waired-tray is a symlink into it on any host installed
    # since, and a regular file on an older one; rm -f handles both.
    common_log "Removing binaries from $bindir"
    # shellcheck disable=SC2086
    common_run $SUDO rm -f "$bindir/waired" "$bindir/waired-agent" "$bindir/waired-tray"
    if [ -e "$WAIRED_DARWIN_APPDIR/Waired.app" ]; then
        common_log "Removing $WAIRED_DARWIN_APPDIR/Waired.app"
        # shellcheck disable=SC2086
        common_run $SUDO rm -rf "$WAIRED_DARWIN_APPDIR/Waired.app"
    fi

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
        # The trailing globs catch the rotated archives (waired-agent.out.log.0.gz
        # …); without them a --clean left most of the log data on disk. `.log.*`
        # rather than `.log.*.gz` so it also takes the uncompressed `.log.0` a
        # rotation interrupted between the rename and the gzip leaves behind
        # (#331). An unmatched glob passes through literally and `rm -f` ignores
        # it, so this is safe when the host never rotated. The tray's logs rotate
        # the same way now, so they get the same treatment.
        # shellcheck disable=SC2086
        common_run $SUDO rm -f /Library/Logs/waired-agent.out.log /Library/Logs/waired-agent.err.log \
            /Library/Logs/waired-agent.out.log.* /Library/Logs/waired-agent.err.log.*
        [ -n "$home" ] && common_run rm -f \
            "$home/Library/Logs/waired-tray.out.log" \
            "$home/Library/Logs/waired-tray.err.log" \
            "$home/Library/Logs/waired-tray.out.log."* \
            "$home/Library/Logs/waired-tray.err.log."*
        # /Applications/Ollama.app is NOT removed. Since #492 waired never
        # installs one, and an Ollama.app on this host cannot be attributed
        # to us — the in-bundle ownership marker had to go, because writing
        # it is what broke the bundle's code signature (#329). Deleting a
        # user's own install on the way out would be the worse mistake. The
        # engine waired DID install went with the state dir above; the
        # uninstall page says how to remove a leftover app by hand.
    fi
}

# print_done reports what happened. Every claim is conditioned on a step
# having actually run (see DID_COUNT / DEREGISTERED above). Mirrors
# uninstall.ps1's Show-Done. waired-agent#793.
print_done() {
    section 'Done'
    _tag=''
    [ "$DRY_RUN" = 1 ] && _tag='[dry-run] '

    if [ "$DID_COUNT" -eq 0 ]; then
        if [ "$DRY_RUN" = 1 ]; then
            common_log "${_tag}Nothing would be removed — Waired is not installed on this computer."
        else
            common_log "Nothing to remove — Waired was not installed on this computer."
        fi
        return 0
    fi

    if [ "$DRY_RUN" = 1 ]; then
        if [ "$FLAG_CLEAN" = 1 ]; then
            common_log "${_tag}Waired would be fully removed (config + state wiped)."
        else
            common_log "${_tag}Waired would be removed. Local config + state would be kept; re-run with --clean to wipe them."
        fi
    elif [ "$FLAG_CLEAN" = 1 ]; then
        common_log "Waired fully removed (config + state wiped)."
    else
        common_log "Waired removed. Local config + state were kept; re-run with --clean to wipe them."
    fi

    if [ "$DEREGISTERED" = 1 ]; then
        if [ "$DRY_RUN" = 1 ]; then
            common_log "${_tag}This device would be deregistered from your Waired account (best-effort)."
        else
            common_log "This device was deregistered from your Waired account (best-effort). If it was"
            common_log "offline during uninstall, remove it from the web admin device list."
        fi
    else
        common_log "No Waired registration was found on this computer, so nothing was deregistered."
    fi
}

main() {
    while [ $# -gt 0 ]; do
        case "$1" in
            --clean)    FLAG_CLEAN=1 ;;
            --yes|-y)   FLAG_YES=1 ;;
            --dry-run)  DRY_RUN=1 ;;
            --mask-pii) WAIRED_PII_MASK=1; export WAIRED_PII_MASK ;;
            -h|--help)  show_help; exit 0 ;;
            *) common_die "unknown argument: $1 (try --help)" ;;
        esac
        shift
    done

    detect_os
    confirm_proceed
    common_elevate

    section 'Removing Waired'
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
