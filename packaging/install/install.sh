#!/bin/sh
# install.sh — install Waired via the system package manager.
#
# Usage:
#   curl -fsSL https://github.com/waired-ai/waired-agent/releases/latest/download/install.sh | sh
#   curl -fsSL https://github.com/waired-ai/waired-agent/releases/latest/download/install.sh | sh -s -- --dry-run
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
#   darwin_*      macOS: download tarball, register LaunchDaemon
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
WAIRED_INSTALL_BASE_URL="${WAIRED_INSTALL_BASE_URL:-https://github.com/waired-ai/waired-agent/releases/latest/download}"
# GitHub repo (owner/name) whose Releases API resolves the latest version
# during --check / --update on macOS (Linux reads apt's candidate
# instead). Override alongside WAIRED_INSTALL_BASE_URL for a mirror.
WAIRED_INSTALL_REPO="${WAIRED_INSTALL_REPO:-waired-ai/waired-agent}"
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

# Both Unixes install waired's BUNDLED Ollama (a pinned official release
# into <state-dir>/runtimes/ollama/, supervised by waired-agent on :9475)
# via `waired runtimes install ollama`, NOT a system `ollama.com/install.sh`
# (#567) and not an Ollama.app in /Applications (#492). The download URL
# and its checksum are pinned inside the Go installer
# (internal/runtime/ollama_install.go), so there is no URL override knob on
# either — WAIRED_OLLAMA_LINUX_URL and WAIRED_OLLAMA_DARWIN_URL are both
# retired.

DRY_RUN=0
SUDO=""
CONTROL_URL=""
FLAG_USE_DEV=0
FLAG_CONTROL_URL=""
FLAG_NO_OLLAMA=0
# LOG_LEVEL, when set (--log-level or $WAIRED_LOG_LEVEL), starts the agent at
# that slog verbosity: debug|info|warn|error. On Linux it is written to
# /etc/waired/agent.env (systemd EnvironmentFile); on macOS it is baked into
# the LaunchDaemon's ProgramArguments as --log-level. Change it later at
# runtime (no restart) with `waired config log-level`.
LOG_LEVEL="${WAIRED_LOG_LEVEL:-}"
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
# FLAG_NON_INTERACTIVE (--non-interactive, mirroring install.ps1's
# -NonInteractive) is the explicit "no terminal, enrol anyway" opt-in. It does
# two things --yes does not:
#   * it runs `waired init` even when no terminal is available, where the
#     default is to skip sign-in entirely (linux_maybe_init / darwin_maybe_init
#     print a "finish later" note instead), and
#   * it feeds init stdin from /dev/null, since the usual </dev/tty redirect
#     cannot work on a host with no controlling terminal.
# --yes still implies init's own --non-interactive (skip its prompts, take
# hardware-derived defaults) but does NOT override the terminal gate: an
# unattended image build has to say so explicitly.
FLAG_NON_INTERACTIVE=0
# INFERENCE_ENABLED / SHARE_WITH_MESH pre-answer the two setup questions
# without prompting (--inference-enabled / --share-with-mesh, mirroring
# install.ps1's -InferenceEnabled / -ShareWithMesh). Empty = no override; the
# prompt or the hardware-derived default decides. Values are validated in
# main() and forwarded to `waired init` in the `=` form, which is mandatory:
# these are Go bool flags, and the space form leaves the value as a positional
# argument that `waired init` (cobra.NoArgs) rejects.
INFERENCE_ENABLED=""
SHARE_WITH_MESH=""
# FLAG_CLEAN: clean install — run the full-wipe uninstall (delegated to
# uninstall.sh --clean) before installing fresh. WAIRED_CLEAN is the
# env-var form, mirroring WAIRED_NO_OLLAMA (and it is how the Windows
# piped `iwr | iex` one-liner opts in, so both OSes accept it).
FLAG_CLEAN=0
if [ -n "${WAIRED_CLEAN:-}" ]; then FLAG_CLEAN=1; fi
# DARWIN_REGISTER_FAILED: set by darwin_register_agent when LaunchDaemon
# registration failed. The install deliberately continues (see that function),
# so darwin_next_steps repeats the recovery at the end rather than letting the
# user's last line of output be an unexplained warning from much earlier.
DARWIN_REGISTER_FAILED=0
# LOCAL_AI_DOWN: set when `waired init` exited WAIRED_INIT_LOCAL_AI_DOWN —
# it signed this device in, and then found that local AI is not running
# here (the engine could not be installed, or it installed and would not
# stay up). Sign-in SUCCEEDED, so this is not the "sign-in did not
# complete" case; the done banner adds a line rather than changing what
# it says about the install.
LOCAL_AI_DOWN=0
# WAIRED_INIT_LOCAL_AI_DOWN mirrors exitLocalAIDown in cmd/waired/main.go.
# Named rather than inline so the two `case` arms below cannot drift apart,
# and so a reader can grep for the constant on both sides.
WAIRED_INIT_LOCAL_AI_DOWN=3
OS_KIND=""
OS_FAMILY=""
OS_NAME=""
OS_VERSION=""
OS_CODENAME=""
OS_ARCH=""

# ---------------------------------------------------------------------
# common_* helpers
# ---------------------------------------------------------------------

# mask_pii <text> — best-effort masking of the invoking user's home dir and
# username (as a path segment) when --mask-pii / WAIRED_PII_MASK is on, for
# screenshots and bug reports. The Go binary masks its own output via the
# same env var (incl. hostname + account email); this covers only the
# script's log lines. awk index()/substr() replacement is literal — no
# regex-metacharacter surprises from a path.
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

# section <title> prints a blank line + a horizontal-rule heading so a run
# reads as distinct steps (several tools write to this terminal; the rules
# make it easy to see where one step ends, the next begins, and which output
# belongs to a prompt). Mirrors install.ps1's Section. Box-drawing U+2500 on
# a UTF-8 terminal, '-' otherwise; colour only on an interactive stdout with
# NO_COLOR unset (same rules as print_banner).
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
        _banner_row 112 120 134 "   Claude Code · OpenClaw — your own machine"
        printf '\n'
    else
        _banner_plain
    fi
}

show_help() {
    cat <<HELP
install.sh — install Waired via the system package manager.

Usage:
  curl -fsSL https://github.com/waired-ai/waired-agent/releases/latest/download/install.sh | sh
  curl -fsSL https://github.com/waired-ai/waired-agent/releases/latest/download/install.sh | sh -s -- --dev
  curl -fsSL https://github.com/waired-ai/waired-agent/releases/latest/download/install.sh | sh -s -- --clean
  curl -fsSL https://github.com/waired-ai/waired-agent/releases/latest/download/install.sh | sh -s -- --dry-run

Options:
  --dry-run        show every privileged command without running it
  --dev            enrol this device against the built-in dogfood Control
                   Plane (${WAIRED_DEV_CONTROL_URL}) — persists
                   WAIRED_CONTROL_URL to the agent env file (Linux:
                   /etc/waired/agent.env, macOS: the state dir) so
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
  --clean          clean install: run the uninstaller with --clean first
                   (PERMANENTLY deletes config, keys, state, the apt
                   source, and Ollama + its models), then install fresh.
                   Destructive — asks to confirm unless --yes. Same as
                   WAIRED_CLEAN=1. Cannot be combined with --check/--update.
  --skip-ollama    tell \`waired init\` not to install the Ollama engine
                   (same as WAIRED_NO_OLLAMA=1). Add it later with:
                   sudo waired runtimes install ollama
  --no-init        do not auto-run \`waired init\` after install (the
                   default runs sign-in + setup when a terminal is present)
  --yes, -y        assume "yes" for prompts (pre-install confirmation,
                   update, init non-interactive). Does NOT make sign-in
                   run on a host with no terminal — see --non-interactive.
  --non-interactive
                   never prompt: run \`waired init\` with --non-interactive
                   AND, unlike --yes, attempt sign-in even when no
                   terminal is available (unattended images / CI, where
                   the default is to skip sign-in and tell you to finish
                   later). Same as install.ps1's -NonInteractive.
  --inference-enabled true|false
                   answer "Run AI models on this computer?" without
                   prompting. Forwarded to \`waired init\`. Same as
                   install.ps1's -InferenceEnabled.
  --share-with-mesh true|false
                   answer "Let your other devices use this computer's
                   AI?" without prompting. Same as -ShareWithMesh.
  --mask-pii       mask personal information (home dir, username; the
                   sign-in step also masks hostname + account email) in
                   the output — for screenshots and bug reports.
                   Best-effort. Same as WAIRED_PII_MASK=1.
  --log-level LVL  start the agent at this log verbosity: debug, info,
                   warn, or error (default info). Use --log-level debug for
                   pre-release debugging. Same as WAIRED_LOG_LEVEL=LVL.
                   Change it later without reinstalling via
                   \`waired config log-level <level>\`.
  --skip-claude-proxy
                   leave Claude Code routed straight to the Anthropic API
                   (do not point ANTHROPIC_BASE_URL at local inference).
                   Forwarded to \`waired init\`, the single decider of
                   routing; enable later with \`waired claude enable\`.
                   Same as WAIRED_NO_CLAUDE_PROXY=1. (alias: --skip-proxy)
  -h, --help       print this help

Environment variables:
  WAIRED_VERSION           pin to a specific package version (e.g. 1.2.3),
                           or 'edge' for the latest main build (same as
                           --edge; works on every OS). Unset/'latest' =
                           the newest stable release.
  WAIRED_NO_TRAY           if set, do not install waired-tray (Linux + macOS)
  WAIRED_NO_OLLAMA         if set, \`waired init\` skips the Ollama engine
                           install (same as --skip-ollama; Linux + macOS)
  WAIRED_NO_CLAUDE_PROXY   if set, leave Claude Code on the Anthropic API
                           (same as --skip-claude-proxy); forwarded to
                           \`waired init\`, the single decider of routing
  WAIRED_CLEAN             if set, same as --clean (full wipe first, then
                           a fresh install)
  WAIRED_CONTROL_URL       Control Plane URL written to agent.env when
                           --dev / --control are not given (lower-priority
                           fallback for per-org installer wrappers)
  WAIRED_DEV_CONTROL_URL   override the URL --dev resolves to
                           (default: https://app.dev.waired.net)
  WAIRED_LOG_LEVEL         start the agent at this log verbosity
                           (debug|info|warn|error; same as --log-level)
  WAIRED_STATE_DIR         macOS only: where identity / keys / settings
                           live (default: /Library/Application Support/
                           waired). IGNORED on Linux — the systemd unit
                           ships in the package with
                           \`--state-dir /var/lib/waired\` already baked
                           into ExecStart, and the flag beats the
                           EnvironmentFile. uninstall.sh removes this
                           path too under --clean.
  WAIRED_INSTALL_BASE_URL  override URL for install.sh itself
                           (default: github.com/waired-ai/waired-agent releases)
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

# Confirm the destructive --clean wipe before anything runs. Mirrors
# uninstall.sh's confirm_clean (--yes bypass, /dev/tty prompt so the
# piped `curl | sh` case can still ask, die on a non-interactive shell
# without --yes) with the clean-INSTALL framing added.
confirm_clean_install() {
    [ "$FLAG_CLEAN" = 1 ] || return 0
    [ "$FLAG_YES" = 1 ] && return 0
    if tty_available; then
        common_warn "--clean will PERMANENTLY delete Waired config, keys and state"
        common_warn "(identity / secrets), the apt source, and Ollama + its models,"
        common_warn "then reinstall Waired fresh."
        printf '\033[1;33m[waired]\033[0m %s' "Continue? [y/N] " >/dev/tty
        read -r ans </dev/tty || ans=""
        case "$ans" in
            y|Y|yes|YES) return 0 ;;
            *) common_die "aborted — nothing was removed" ;;
        esac
    fi
    common_die "--clean is destructive; re-run with --yes to confirm on a non-interactive shell"
}

# already_installed — true when this host already has waired (the dispatch
# in main() then takes the update path, which asks its own prompt_update
# question — so the pre-install confirmation below skips to avoid asking
# twice). Requires detect_os to have run.
already_installed() {
    case "$OS_KIND" in
        linux)  [ -n "$(linux_apt_detect_installed)" ] ;;
        # Same predicate as main()'s darwin dispatch below — a half-installed
        # host must not skip the pre-install summary here and then be sent
        # down the fresh-install arm there.
        darwin) darwin_install_complete ;;
        *) return 1 ;;
    esac
}

# signin_summary_how says how sign-in will actually reach this operator, so
# the summary does not promise a browser that will never open. `waired init`
# decides that from the same two variables (internal/platform/browser
# browser_linux.go HasDisplay -> cmd/waired/login_gate.go resolveBrowserGate:
# no display means gatePrintOnly, link + pairing code only). Linux is the
# only OS with a headless shape here — a macOS install runs on the console.
signin_summary_how() {
    if [ "$OS_KIND" = linux ] && [ -z "${DISPLAY:-}" ] && [ -z "${WAYLAND_DISPLAY:-}" ]; then
        printf 'prints a sign-in link you can open on any device'
    else
        printf 'opens your web browser'
    fi
}

# show_install_summary tells the operator what a fresh install is about to
# do, BEFORE anything runs. Mirrors install.ps1's Show-InstallSummary.
show_install_summary() {
    section 'What this will do'
    case "$(channel_from_env)" in
        stable) _sum_ver='latest stable release' ;;
        edge)   _sum_ver='latest edge (main) build' ;;
        *)      _sum_ver="version $WAIRED_VERSION" ;;
    esac
    case "$OS_KIND" in
        linux)
            printf '  * Install Waired (%s) via apt (adds the Waired apt repository)\n' "$_sum_ver"
            printf '  * Register the waired-agent background service (starts at boot)\n'
            ;;
        darwin)
            printf '  * Download Waired (%s) and install it to %s\n' "$_sum_ver" "$WAIRED_DARWIN_BINDIR"
            printf '  * Register the waired-agent background service (starts at boot)\n'
            ;;
    esac
    # Sign-in comes BEFORE the engine, because that is the order the install
    # runs in: `waired init` asks whether this computer should run models and
    # only then installs an engine (#138). Promising the download first is how
    # the Linux installer used to describe a pre-install that bypassed the
    # question entirely.
    if [ "$FLAG_NO_INIT" != 1 ]; then
        printf '  * Sign you in (%s)\n' "$(signin_summary_how)"
    fi
    if ! ollama_skip_requested; then
        printf '  * Install the Ollama AI engine during sign-in, only if you\n'
        printf '    choose to run models here (a few GB download)\n'
    fi
    if [ "$(id -u)" -ne 0 ]; then
        printf '  * Ask for administrator rights (a sudo password prompt may appear)\n'
    fi
    if [ -n "$CONTROL_URL" ]; then
        printf '  * Enrol this device against: %s\n' "$CONTROL_URL"
    fi
}

# confirm_proceed is the single go / no-go gate for a fresh install: summary
# first, then an explicit [Y/n]. Skips: --yes / --dry-run (preview) /
# --clean (confirm_clean_install already collected consent) / --check /
# --update and an already-installed host (the update path asks its own
# question) / no controlling terminal (proceeds with a notice so CI
# one-liners keep working). Mirrors install.ps1's Confirm-Proceed.
confirm_proceed() {
    [ "$FLAG_CLEAN" = 1 ] && return 0
    [ "$FLAG_CHECK" = 1 ] && return 0
    [ "$FLAG_UPDATE" = 1 ] && return 0
    if already_installed; then return 0; fi
    show_install_summary
    [ "$FLAG_YES" = 1 ] && return 0
    [ "$DRY_RUN" = 1 ] && return 0
    if ! tty_available; then
        common_log "No terminal detected — proceeding without confirmation (use --yes to silence this notice)."
        return 0
    fi
    printf '\n\033[1;36m[waired]\033[0m Proceed with the install? [Y/n] (Enter = Yes) ' >/dev/tty
    read -r ans </dev/tty || ans=""
    case "$ans" in
        n|N|no|NO|No) common_die "aborted — nothing was installed" ;;
        *) return 0 ;;
    esac
}

# run_clean_wipe — the wipe half of --clean: delegate to uninstall.sh
# (published as a release asset next to install.sh on both channels)
# rather than re-implementing the purge here. Prefers a sibling
# uninstall.sh when install.sh itself runs from a file (a checkout, or
# the hermetic dash tests) — the piped `curl | sh` case has a shell name
# in $0 and never picks up a stray ./uninstall.sh from the cwd. Consent
# was already collected by confirm_clean_install, so the child gets
# --yes; under --dry-run the child previews its own wipe commands (this
# is deliberately NOT wrapped in common_run). Any failure aborts before
# install work starts, so nothing is left half-done.
run_clean_wipe() {
    [ "$FLAG_CLEAN" = 1 ] || return 0
    wipe_script=""
    wipe_tmp=""
    case "$0" in
        */install.sh|install.sh)
            if [ -f "$(dirname "$0")/uninstall.sh" ]; then
                wipe_script="$(dirname "$0")/uninstall.sh"
            fi
            ;;
    esac
    if [ -z "$wipe_script" ]; then
        common_require_cmd curl mktemp
        wipe_tmp="$(mktemp -d)"
        common_log "Fetching the uninstaller from $WAIRED_INSTALL_BASE_URL/uninstall.sh"
        curl -fsSL "$WAIRED_INSTALL_BASE_URL/uninstall.sh" -o "$wipe_tmp/uninstall.sh" \
            || common_die "failed to download uninstall.sh — aborting (nothing was changed)"
        [ -s "$wipe_tmp/uninstall.sh" ] \
            || common_die "downloaded uninstall.sh is empty — aborting (nothing was changed)"
        wipe_script="$wipe_tmp/uninstall.sh"
    fi
    common_log "Clean install: wiping the existing Waired install first"
    if [ "$DRY_RUN" = 1 ]; then
        sh "$wipe_script" --clean --yes --dry-run \
            || common_die "clean uninstall failed — aborting the install"
    else
        sh "$wipe_script" --clean --yes \
            || common_die "clean uninstall failed — aborting the install"
    fi
    if [ -n "$wipe_tmp" ]; then rm -rf "$wipe_tmp"; fi
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
    # zstd used to be installed here for upstream ollama.com/install.sh,
    # which ships a .tar.zst and aborts ("requires zstd for extraction")
    # without it. Nothing shells out to zstd any more: the engine tarball is
    # fetched and decompressed IN-PROCESS by internal/runtime/ollama_install.go
    # (extractTarZst, klauspost/compress), and that is the only zstd payload
    # the installer's world touches — dpkg handles zstd-compressed .debs with
    # its own linked libzstd. Dropped with the Linux engine pre-install (#138).
    common_log "Installing apt prerequisites (ca-certificates, curl, gnupg)..."
    common_run $SUDO env DEBIAN_FRONTEND=noninteractive apt-get update -qq
    common_run $SUDO env DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates curl gnupg

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
# AFTER linux_service_up has started the daemon, so init attaches to the
# running agent and takes the daemon-driven onboarding path (browser
# sign-in + setup, with the engine installed under that flow) rather than
# the legacy standalone enroll (waired#835 §11.2). The daemon boots
# identity-less and idles until sign-in (#177), so bringing it up first is
# safe; macOS starts its LaunchDaemon (RunAtLoad) before init for the same
# reason. Enrollment + state live in /var/lib/waired
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
    section 'Sign in and set up'
    if linux_enrolled; then
        common_log "$(emo '✅' '[ok]') Already enrolled — skipping sign-in."
        return 0
    fi
    # No controlling terminal: sign-in is browser-driven and interactive, so
    # the default is to skip it and say so. --non-interactive is the explicit
    # override for unattended images / CI, and it also decides the stdin
    # redirect below — </dev/tty cannot open on a host without one.
    init_stdin=/dev/tty
    if ! tty_available; then
        if [ "$FLAG_NON_INTERACTIVE" != 1 ]; then
            cat <<EOF

$(emo '💡' 'Note:') No terminal detected — sign-in skipped. To finish setup:
  - run:  sudo waired init
  - or open the tray app and pick "Sign in…"
  - or re-run the installer with --non-interactive to attempt it anyway
EOF
            return 0
        fi
        init_stdin=/dev/null
    fi
    set -- waired init --state-dir /var/lib/waired
    # --yes has always promised "init non-interactive" in --help; the explicit
    # --non-interactive means the same for init's prompts (and additionally
    # cleared the terminal gate above). install.ps1 applies the same rule.
    if [ "$FLAG_YES" = 1 ] || [ "$FLAG_NON_INTERACTIVE" = 1 ]; then
        set -- "$@" --non-interactive
    fi
    # The two pre-answered setup questions. The `=` form is mandatory: these
    # are Go bool flags, so `--inference-enabled false` would set the flag true
    # and leave "false" as a positional argument, which `waired init`
    # (cobra.NoArgs) rejects outright.
    if [ -n "$INFERENCE_ENABLED" ]; then
        set -- "$@" "--inference-enabled=$INFERENCE_ENABLED"
    fi
    if [ -n "$SHARE_WITH_MESH" ]; then
        set -- "$@" "--share-with-mesh=$SHARE_WITH_MESH"
    fi
    # init is the ONLY thing that installs the engine now (#138), so
    # --skip-ollama has to survive the sudo env_reset to reach it: thread it
    # through `env` as WAIRED_NO_OLLAMA. Same for the
    # PII-masking and Claude-routing opt-out requests: init is the single
    # decider of routing, so --skip-claude-proxy / WAIRED_NO_CLAUDE_PROXY must
    # reach it (it defaults --skip-claude-route from WAIRED_NO_CLAUDE_PROXY).
    if ollama_skip_requested; then
        set -- env WAIRED_NO_OLLAMA=1 "$@"
    fi
    if [ -n "${WAIRED_PII_MASK:-}" ]; then
        set -- env WAIRED_PII_MASK=1 "$@"
    fi
    if [ -n "${WAIRED_NO_CLAUDE_PROXY:-}" ]; then
        set -- env WAIRED_NO_CLAUDE_PROXY=1 "$@"
    fi
    # Print the argv actually built, not a fixed string (which drifted out of
    # date and hid the flags this function forwards) — mirrors darwin_maybe_init.
    if [ "$DRY_RUN" = 1 ]; then
        common_log "  (dry-run) would: $SUDO $* <$init_stdin"
        return 0
    fi
    common_log "$(emo '🔑' '>>') Starting sign-in (waired init)…"
    # Capture the code instead of collapsing every non-zero into one
    # message: `waired init` distinguishes "signed in, but local AI is not
    # running here" from a sign-in that really did not finish, and telling
    # the user to re-run `waired init` would be wrong advice for the first
    # (#310). `|| rc=$?` keeps this working under `set -e`.
    init_rc=0
    $SUDO "$@" <"$init_stdin" || init_rc=$?
    case "$init_rc" in
        0) ;;
        "$WAIRED_INIT_LOCAL_AI_DOWN") LOCAL_AI_DOWN=1 ;;
        *) common_warn "sign-in did not complete; finish later with: sudo waired init" ;;
    esac
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
    # Restart onto the new binary first, then finish sign-in if this host
    # was installed but never enrolled (no-op when already enrolled). With
    # the daemon already running, that sign-in takes the daemon-driven
    # onboarding path (waired#835 §11.2), matching a fresh install.
    linux_service_up update
    linux_maybe_init
    common_log "$(emo '🎉' '*') waired updated and the service restarted. Check: waired status"
}

# GNOME AppIndicator host extension (#295). GNOME ships no StatusNotifierItem
# host, so without one of these the waired-tray icon is silently absent. Kept in
# step with internal/platform/trayhost/repair_linux.go, which is what repairs the
# same thing later from waired-tray and `waired doctor --fix`.
TRAY_HOST_EXT_PKG='gnome-shell-extension-appindicator'
TRAY_HOST_EXT_UUID='appindicatorsupport@rgcjonas.gmail.com'

# linux_wants_tray_host_extension decides whether to add the extension to the
# apt transaction. Two conditions, and both matter:
#
#   1. gnome-shell must already be installed. This is the whole reason the
#      decision is made HERE rather than as a package dependency: apt has no
#      conditional-dependency form, and on Ubuntu 26.04 the package name above
#      is a *virtual* package whose only provider is gnome-shell-ubuntu-extensions,
#      which `Depends: gnome-shell (>= 49~)`. A Depends/Recommends in
#      packaging/nfpm/waired-tray.yaml.tmpl would therefore install GNOME Shell
#      onto every server that installs Waired. A server has no gnome-shell, so
#      this test is false there and the apt transaction is byte-for-byte what it
#      was before.
#   2. No known host extension may already be present. Ubuntu Desktop ships
#      ubuntu-appindicators (via ubuntu-desktop → gnome-shell-ubuntu-extensions)
#      and enables it through the `ubuntu` session mode, so there is nothing to
#      do on the commonest desktop of all.
#
# The gnome-shell test asks dpkg, not $PATH: this function only ever runs on the
# apt path, where the package database is the authority and a stray binary is
# not. internal/platform/trayhost asks $PATH for the same fact because it also
# has to answer on tarball installs and non-Debian hosts.
linux_wants_tray_host_extension() {
    dpkg-query -W gnome-shell >/dev/null 2>&1 || return 1
    for uuid in "$TRAY_HOST_EXT_UUID" 'ubuntu-appindicators@ubuntu.com'; do
        [ -d "/usr/share/gnome-shell/extensions/$uuid" ] && return 1
    done
    return 0
}

# linux_enable_tray_host_extension turns the extension on for the user who
# invoked the installer, so the icon is there at their next login rather than
# after a trip through `waired doctor`.
#
# Enabling is a per-user dconf write, so it has to happen AS that user, in their
# session — which is why it cannot ride along in the apt transaction above, and
# why waired-tray re-checks at every session start (this covers one user on one
# machine at one moment; nothing more).
#
# Best-effort throughout: every failure here still leaves a working Waired, and
# waired-tray or `waired doctor --fix` picks the same repair up later.
linux_enable_tray_host_extension() {
    [ -z "${WAIRED_NO_TRAY:-}" ] || return 0
    [ -n "${SUDO_USER:-}" ] || return 0
    [ "$SUDO_USER" != root ] || return 0
    dpkg-query -W gnome-shell >/dev/null 2>&1 || return 0
    command -v runuser >/dev/null 2>&1 || return 0

    uid="$(id -u "$SUDO_USER" 2>/dev/null)" || return 0
    [ -n "$uid" ] || return 0

    common_log "Enabling the tray icon extension for $SUDO_USER"
    common_run $SUDO runuser -u "$SUDO_USER" -- env \
        "XDG_RUNTIME_DIR=/run/user/$uid" \
        "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$uid/bus" \
        gnome-extensions enable "$TRAY_HOST_EXT_UUID" >/dev/null 2>&1 || \
        common_log "  (could not enable it now — waired-tray will do it at your next login)"
}

linux_apt_install() {
    section 'Installing Waired'
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

    if [ -z "${WAIRED_NO_TRAY:-}" ] && linux_wants_tray_host_extension; then
        common_log "GNOME detected — adding $TRAY_HOST_EXT_PKG so the Waired icon renders"
        pkgs="$pkgs $TRAY_HOST_EXT_PKG"
    fi

    common_log "Installing packages: $pkgs"
    # shellcheck disable=SC2086
    common_run $SUDO env DEBIAN_FRONTEND=noninteractive apt-get install -y $pkgs

    linux_enable_tray_host_extension

    linux_apt_write_control_url
    linux_write_log_level_env

    # Start the daemon FIRST, then drive first-run sign-in: with the agent
    # already running, `waired init` attaches to it and takes the
    # daemon-driven onboarding path (waired#835 §11.2) rather than the
    # legacy standalone enroll. linux_service_up is safe before sign-in (the
    # daemon idles until enrolment, #177) and is a no-op on non-systemd
    # hosts (e.g. container builds), where init falls back to standalone.
    linux_service_up install
    linux_maybe_init
    linux_done_banner
}

# linux_done_banner prints the friendly "what just happened / you're ready"
# summary after a fresh install. Branches on whether sign-in completed.
#
# The engine line is MEASURED here rather than set by an earlier step,
# set_local_ai_note fills $LOCAL_AI_NOTE with the warning block the done
# banners carry when `waired init` reported that this device has no local
# AI. Shared so the Linux and macOS banners cannot drift into saying
# different things about the same outcome.
#
# The headline above it stays "Waired is installed", because it is: the
# packages are on disk, the service is running, and the device is signed
# in and on the network. Only local inference is missing, and saying
# otherwise would send people hunting a broken install. It is also the
# string all three installtest harnesses grep for as the "the run reached
# its end" sentinel.
#
# A variable rather than a function that prints, because $(...) strips
# trailing newlines and the block needs a blank line on BOTH sides. Empty
# when nothing is wrong, which leaves the banner exactly as it was.
set_local_ai_note() {
    LOCAL_AI_NOTE=""
    [ "$LOCAL_AI_DOWN" = 1 ] || return 0
    LOCAL_AI_NOTE="
$(emo '⚠️' '!')  Local AI is not running on this device.
    Sign-in is finished; only local AI is missing.
    Details:      waired doctor
"
}

# because since #138 the installer does not decide it: `waired init` installs
# the bundled engine only when the operator said this computer should run
# models, so the honest answer is whatever is on disk once init has returned.
# Same three arms and the same strings as darwin_next_steps.
linux_done_banner() {
    section 'Done'
    set_local_ai_note
    party="$(emo '🎉' '*')"
    if ollama_skip_requested; then
        ollama_status="skipped (--skip-ollama / WAIRED_NO_OLLAMA; install the engine later: sudo waired runtimes install ollama)"
    elif $SUDO test -x /var/lib/waired/runtimes/ollama/bin/ollama; then
        ollama_status="installed (local AI engine)"
    else
        ollama_status="installed by sign-in when local inference is on (sudo waired init)"
    fi
    if linux_enrolled; then
        ready="$(emo '✅' '[ok]') Enrolled — the agent service is running."
        nextline="Check it:     waired status        (try: waired infer \"hello, world!\")"
    else
        ready="$(emo '🔧' '[*]') The agent service is running — ready for sign-in."
        nextline="Sign in:      sudo waired init     (or open the tray app → \"Sign in…\")"
    fi
    cat <<EOF

$party Waired is installed.
$ready
$LOCAL_AI_NOTE
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

# Persist $LOG_LEVEL into /etc/waired/agent.env so the systemd daemon starts
# at that verbosity (the unit's EnvironmentFile is read at boot). Parallels
# linux_apt_write_control_url: append-only, and an existing active setting is
# left alone. Runtime changes go through `waired config log-level`.
linux_write_log_level_env() {
    [ -z "$LOG_LEVEL" ] && return 0
    env_file=/etc/waired/agent.env

    if [ "$DRY_RUN" = 1 ]; then
        common_log "Would write WAIRED_LOG_LEVEL=$LOG_LEVEL to $env_file"
        return 0
    fi

    if [ ! -f "$env_file" ]; then
        common_warn "$env_file not present after install — skipping log-level auto-config"
        return 0
    fi

    if $SUDO grep -Eq '^[[:space:]]*WAIRED_LOG_LEVEL=.+' "$env_file"; then
        common_warn "$env_file already sets WAIRED_LOG_LEVEL — leaving it as-is"
        return 0
    fi

    common_log "Writing WAIRED_LOG_LEVEL=$LOG_LEVEL to $env_file"
    printf 'WAIRED_LOG_LEVEL=%s\n' "$LOG_LEVEL" | $SUDO tee -a "$env_file" >/dev/null
}

# The Linux engine pre-install lived here (linux_install_ollama, #567) and
# was removed in #138. It ran `waired runtimes install ollama` from inside
# linux_apt_install — before the daemon was up and long before anyone was
# asked whether this computer should run models — which meant a `curl | sh`
# spent ~1.4 GB answering a question on the operator's behalf. macOS and
# Windows dropped their pre-install in #55/#73 already; Linux kept it on the
# assumption that init took the standalone path, which stopped being true
# when #119 made the daemon path the default on all three OSes. `waired init`
# now owns both the decision and the install on every OS: the wizard's engine
# step (cmd/waired/setup_install.go runSetupEngineInstall) or, with no browser
# driving, cmd/waired/init_daemon_inference.go ensureDaemonPathEngine — both
# install into the daemon-declared state dir, which is the same
# /var/lib/waired/runtimes/ollama/bin/ollama the strict bundled resolver
# requires. Hosts that never reach init (--no-init, no terminal, non-systemd)
# end with no engine until the first `sudo waired init`; that is the consent
# gate working, and it matches Windows -SkipInit.

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
# Where identity / keys / settings live on macOS. WAIRED_STATE_DIR overrides it
# (parity with install.ps1, and uninstall.sh already removes the override under
# --clean). It works here — and NOT on Linux — because of who writes the
# service definition: macOS registers the LaunchDaemon at install time via
# `waired-agent install --state-dir <dir>`, so the path is ours to choose,
# whereas the Linux systemd unit ships inside the .deb/.rpm with
# `--state-dir /var/lib/waired` already baked into ExecStart (and the flag beats
# the EnvironmentFile). Honouring it there would mean a second, drop-in unit
# definition to keep in sync; see `show_help`.
DARWIN_STATE_DIR="${WAIRED_STATE_DIR:-/Library/Application Support/waired}"
DARWIN_LABEL=com.waired.agent
# WAIRED_DARWIN_PLIST is a test seam, not part of the documented option
# surface (it is absent from show_help and the docs reference on purpose):
# it lets the dash dispatch matrix drive darwin_install_complete on a Linux
# runner without a /Library to point at.
DARWIN_PLIST="${WAIRED_DARWIN_PLIST:-/Library/LaunchDaemons/$DARWIN_LABEL.plist}"

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
    state_dir="$DARWIN_STATE_DIR"

    section 'Installing Waired'
    darwin_install_binaries
    # The Ollama engine is NOT pre-installed here any more: `waired init`
    # owns both the decision (its "run local inference?" answers) and the
    # install (the official Ollama.app, with a live progress bar). Installing
    # it here made init re-detect waired's own install as a "foreign"
    # Ollama. --skip-ollama is forwarded
    # to init as WAIRED_NO_OLLAMA (darwin_maybe_init).
    section 'Background service'
    darwin_register_agent "$state_dir"
    darwin_retire_log_rotation
    darwin_write_control_url "$state_dir"
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
    # Single-line progress bar (-#) on a terminal so the multi-MB fetch is
    # visibly alive; stay fully silent when piped/CI so logs don't fill up.
    if [ -t 2 ]; then
        curl -f#SL "$url" -o "$tmp/$tarball"
    else
        curl -fsSL "$url" -o "$tmp/$tarball"
    fi
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

# Register the system LaunchDaemon. Needs root: the plist lands in
# /Library/LaunchDaemons and bootstrap targets the system domain, and the
# state dir is the root-owned /Library/Application Support/waired.
# darwin_register_agent registers the system LaunchDaemon. Non-fatal by
# design: install.sh runs under `set -eu`, so an unguarded failure here used
# to abort the whole installer, skipping log rotation, the control-URL write,
# `waired init` and the next-steps block — and leaving a raw Go error as the
# last thing on screen with no guidance. Linux does not behave that way
# (linux_service_up warns and continues) and neither does Windows; macOS was
# the outlier. Warn, record it for darwin_next_steps, and let the rest of the
# configuration land.
darwin_register_agent() {
    state_dir="$1"
    common_log "Registering waired-agent system LaunchDaemon (sudo)"
    # The macOS LaunchDaemon does not read agent.env at runtime, so unlike
    # Linux the log level cannot ride an EnvironmentFile. Bake it into the
    # plist's ProgramArguments instead by passing --log-level as an install
    # ExtraArg (everything after `--` becomes ExecStart/ProgramArguments
    # tokens; the agent flag wins over agent.json). Runtime changes still go
    # through `waired config log-level`.
    if [ "$DRY_RUN" = 1 ]; then
        common_log "  (dry-run) would: $SUDO $WAIRED_DARWIN_BINDIR/waired-agent install --state-dir \"$state_dir\"${LOG_LEVEL:+ -- --log-level $LOG_LEVEL}"
        return 0
    fi
    _reg_rc=0
    if [ -n "$LOG_LEVEL" ]; then
        $SUDO "$WAIRED_DARWIN_BINDIR/waired-agent" install --state-dir "$state_dir" -- --log-level "$LOG_LEVEL" || _reg_rc=$?
    else
        $SUDO "$WAIRED_DARWIN_BINDIR/waired-agent" install --state-dir "$state_dir" || _reg_rc=$?
    fi
    if [ "$_reg_rc" -ne 0 ]; then
        DARWIN_REGISTER_FAILED=1
        common_warn "could not register the background service (exit $_reg_rc) — continuing with the rest of the install."
        darwin_register_recovery_hint
    fi
    return 0
}

# darwin_register_recovery_hint prints the actionable recovery for a failed
# LaunchDaemon registration. Shared by the warning at failure time and by
# darwin_next_steps, so the instruction is also the LAST thing on screen —
# a warning 200 lines up is a warning nobody reads.
darwin_register_recovery_hint() {
    common_warn "  Retry with: sudo $WAIRED_DARWIN_BINDIR/waired-agent install --state-dir \"$DARWIN_STATE_DIR\""
    common_warn "  If it says 'bootstrap: exit status 5', clear a stale launchd override first:"
    common_warn "    sudo launchctl enable system/com.waired.agent"
}

# darwin_retire_log_rotation removes the newsyslog(8) drop-in this installer
# used to write for /Library/Logs/waired-agent.{out,err}.log. The agent now
# rotates those files itself (internal/platform/logrotate), at the same 1 MB /
# 5-archive bound the drop-in configured.
#
# Why it had to go rather than stay as a backstop (#331): launchd owns the
# descriptor the daemon writes through, so newsyslog's rename does not reach
# it. The daemon kept writing into the renamed inode and lost every line after
# the rotation — the drop-in's own comment assumed a restart would reattach the
# descriptor "soon", which is false on precisely the wedged host whose logs
# matter. Leaving it installed would also race the in-process rotation, with two
# rotators renaming the same file.
#
# Called on both fresh install and update, so a host that only ever updates
# converges too (same reason darwin_restart_agent re-registers a missing job).
# Idempotent, and a no-op on a host that never had the drop-in.
darwin_retire_log_rotation() {
    conf=/etc/newsyslog.d/waired-agent.conf
    if [ "$DRY_RUN" = 1 ]; then
        common_log "  (dry-run) would remove the legacy newsyslog rotation at $conf (the agent rotates its own logs)"
        return 0
    fi
    # shellcheck disable=SC2086  # $SUDO is intentionally word-split (empty when root)
    if ! $SUDO test -e "$conf"; then
        return 0
    fi
    common_log "Removing the legacy newsyslog rotation ($conf) — the agent rotates its own logs"
    # Non-fatal, the same shape the write it replaces had: under `set -eu` an
    # unguarded failure here would abort the installer over log rotation,
    # cosmetic next to the sign-in and next-steps blocks that come after it.
    # shellcheck disable=SC2086
    if ! $SUDO rm -f "$conf"; then
        common_warn "could not remove $conf — newsyslog will keep rotating the daemon logs alongside the agent; remove it by hand with: sudo rm -f $conf"
    fi
    return 0
}

# darwin_write_control_url persists $CONTROL_URL into the macOS state-dir
# agent.env, the darwin analog of Linux's /etc/waired/agent.env. `waired
# init` reads it as the --control default via controlurl.PlatformDefault
# (internal/controlurl), so a later bare `sudo waired init` — where sudo
# has stripped the caller's $WAIRED_CONTROL_URL — still enrolls against the
# right Control Plane. The daemon reads the same file for sign-in from the
# app (#174): the launchd plist cannot consume an env file the way Linux's
# systemd unit does, so reading it directly is the only way daemon-driven
# login can honour a --dev/--control install. Must run after
# darwin_register_agent has created the (0700, root-owned) state dir — and
# since that step is now non-fatal, the dir may legitimately be absent here.
darwin_write_control_url() {
    state_dir="$1"
    [ -z "$CONTROL_URL" ] && return 0
    env_file="$state_dir/agent.env"

    if [ "$DRY_RUN" = 1 ]; then
        common_log "  (dry-run) would write WAIRED_CONTROL_URL=$CONTROL_URL to $env_file"
        return 0
    fi

    # No state dir means darwin_register_agent did not get far enough to
    # create it. Skip rather than abort (parity with linux_apt_write_control_url's
    # "env_file not present after install — skipping auto-config"); `waired
    # init` still takes --control directly from darwin_maybe_init.
    # shellcheck disable=SC2086
    if ! $SUDO test -d "$state_dir"; then
        common_warn "$state_dir does not exist — skipping the control-URL write."
        return 0
    fi

    # An existing *active* setting means the operator already configured
    # this host — leave it alone (parity with linux_apt_write_control_url).
    if $SUDO grep -Eq '^[[:space:]]*WAIRED_CONTROL_URL=.+' "$env_file" 2>/dev/null; then
        common_warn "$env_file already has an active WAIRED_CONTROL_URL — leaving it as-is"
        CONTROL_URL=""   # don't claim we wrote it in Next steps
        return 0
    fi

    common_log "Writing WAIRED_CONTROL_URL=$CONTROL_URL to $env_file"
    if ! printf 'WAIRED_CONTROL_URL=%s\n' "$CONTROL_URL" | $SUDO tee -a "$env_file" >/dev/null; then
        common_warn "could not write $env_file — pass --control to 'sudo waired init' instead."
        return 0
    fi
    # Keep it owner-only, consistent with the 0700 state dir.
    $SUDO chmod 0600 "$env_file" 2>/dev/null || true
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
    section 'Sign in and set up'
    if [ -e "$state_dir/identity.json" ]; then
        common_log "$(emo '✅' '[ok]') Already enrolled — skipping sign-in."
        return 0
    fi
    # Same terminal rule as linux_maybe_init: skip by default, and let the
    # explicit --non-interactive override it (feeding init /dev/null, since
    # </dev/tty cannot open without a controlling terminal).
    init_stdin=/dev/tty
    if ! tty_available; then
        if [ "$FLAG_NON_INTERACTIVE" != 1 ]; then
            common_log "$(emo '💡' 'Note:') No terminal detected — run 'sudo waired init' (or use the tray) to sign in, or re-run the installer with --non-interactive."
            return 0
        fi
        init_stdin=/dev/null
    fi
    # Build the init argv (mirrors linux_maybe_init's `set --`). Pass
    # --control only when a CP URL was resolved (--dev / --control /
    # WAIRED_CONTROL_URL); darwin_write_control_url has also persisted it to
    # agent.env, so this is belt-and-suspenders on the first sign-in and the
    # reader covers later bare re-runs.
    #
    # init installs the Ollama engine itself when its answers call for one,
    # so --skip-ollama must survive the sudo env_reset: thread it through
    # `env` as WAIRED_NO_OLLAMA=1.
    set -- "$WAIRED_DARWIN_BINDIR/waired" init --state-dir "$state_dir"
    [ -n "$CONTROL_URL" ] && set -- "$@" --control "$CONTROL_URL"
    if [ "$FLAG_YES" = 1 ] || [ "$FLAG_NON_INTERACTIVE" = 1 ]; then
        set -- "$@" --non-interactive
    fi
    # `=` form is mandatory for these two — Go bool flags; see linux_maybe_init.
    if [ -n "$INFERENCE_ENABLED" ]; then
        set -- "$@" "--inference-enabled=$INFERENCE_ENABLED"
    fi
    if [ -n "$SHARE_WITH_MESH" ]; then
        set -- "$@" "--share-with-mesh=$SHARE_WITH_MESH"
    fi
    if ollama_skip_requested; then
        set -- env WAIRED_NO_OLLAMA=1 "$@"
    fi
    if [ -n "${WAIRED_PII_MASK:-}" ]; then
        set -- env WAIRED_PII_MASK=1 "$@"
    fi
    # Claude-routing opt-out (--skip-claude-proxy / WAIRED_NO_CLAUDE_PROXY):
    # init is the single decider of routing and defaults --skip-claude-route
    # from this env, so thread it through the sudo env_reset like the others.
    if [ -n "${WAIRED_NO_CLAUDE_PROXY:-}" ]; then
        set -- env WAIRED_NO_CLAUDE_PROXY=1 "$@"
    fi
    if [ "$DRY_RUN" = 1 ]; then
        common_log "  (dry-run) would: $SUDO $* <$init_stdin"
        return 0
    fi
    common_log "$(emo '🔑' '>>') Starting sign-in (waired init)…"
    # Capture the code instead of collapsing every non-zero into one
    # message: `waired init` distinguishes "signed in, but local AI is not
    # running here" from a sign-in that really did not finish, and telling
    # the user to re-run `waired init` would be wrong advice for the first
    # (#310). `|| rc=$?` keeps this working under `set -e`.
    init_rc=0
    $SUDO "$@" <"$init_stdin" || init_rc=$?
    case "$init_rc" in
        0) ;;
        "$WAIRED_INIT_LOCAL_AI_DOWN") LOCAL_AI_DOWN=1 ;;
        *) common_warn "sign-in did not complete; finish later with: sudo waired init" ;;
    esac
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

# darwin_install_complete is true only when this host carries a COMPLETE
# install: the binary, the LaunchDaemon plist, and a job launchd actually
# knows about. darwin_detect_installed alone is the wrong signal for the
# install-vs-update dispatch, because an install that aborted after
# darwin_install_binaries leaves a working binary behind — so a plain re-run
# was dispatched to the update path, which installs none of the missing
# pieces, and the host could never converge no matter how often it ran.
darwin_install_complete() {
    [ -n "$(darwin_detect_installed)" ] || return 1
    [ -f "$DARWIN_PLIST" ] || return 1
    # launchd's own view of the job. This one needs root, and
    # already_installed() asks before common_elevate has resolved $SUDO — so
    # there the two checks above are the answer, while main()'s dispatch runs
    # post-elevation and gets the full signal.
    [ -n "$SUDO" ] || [ "$(id -u)" -eq 0 ] || return 0
    # shellcheck disable=SC2086
    $SUDO launchctl print "system/$DARWIN_LABEL" >/dev/null 2>&1 || return 1
}

# darwin_restart_agent reloads the system LaunchDaemon so the freshly
# swapped binary takes effect, falling back to (re-)registration if the
# job is not currently loaded. The system domain needs root, so it runs
# under $SUDO.
darwin_restart_agent() {
    common_log "Restarting waired-agent (launchctl kickstart, sudo)"
    if [ "$DRY_RUN" = 1 ]; then
        common_log "  (dry-run) would: $SUDO launchctl kickstart -k system/$DARWIN_LABEL"
        return 0
    fi
    if ! $SUDO launchctl kickstart -k "system/$DARWIN_LABEL" 2>/dev/null; then
        common_warn "LaunchDaemon not loaded; (re-)registering it."
        darwin_register_agent "$DARWIN_STATE_DIR"
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
    # Converge on the complete state rather than only swapping binaries: a host
    # installed before #331 still carries the newsyslog drop-in, and would
    # otherwise keep a second rotator racing the agent's own however many times
    # it updated. Idempotent, and the analog of linux_apt_update re-running
    # linux_service_up. darwin_restart_agent above covers the plist the same
    # way, by re-registering when the job is absent.
    darwin_retire_log_rotation
    # Finish sign-in if this host was installed but never enrolled (no-op
    # when already enrolled). Persist any resolved CP first so a not-yet-
    # enrolled host picks it up, matching the fresh-install path.
    darwin_write_control_url "$DARWIN_STATE_DIR"
    darwin_maybe_init "$DARWIN_STATE_DIR"
    common_log "Ollama: managed separately; not modified by update."
    common_log "$(emo '🎉' '*') waired updated to $latest. Check: waired status"
}

darwin_next_steps() {
    state_dir="$1"
    section 'Done'
    set_local_ai_note
    party="$(emo '🎉' '*')"
    if [ -e "$state_dir/identity.json" ]; then
        get_started="$(emo '✅' '[ok]') Enrolled — the agent is running.
  Check it:  waired status   (try: waired infer \"hello, world!\")"
    else
        get_started="Get started:
  1. Sign in: sudo waired init  (or open the tray app → \"Sign in…\")
  2. Verify:  waired status     (then: waired infer \"hello, world!\")"
    fi
    if ollama_skip_requested; then
        ollama_status="skipped (--skip-ollama / WAIRED_NO_OLLAMA)"
    elif $SUDO test -x "/Library/Application Support/waired/runtimes/ollama/bin/ollama"; then
        ollama_status="installed (local AI engine)"
    else
        ollama_status="installed by sign-in when local inference is on (sudo waired init)"
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
$LOCAL_AI_NOTE
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
    # Registration failed earlier and the install deliberately carried on, so
    # say so last — everything printed above assumes a running daemon.
    if [ "$DARWIN_REGISTER_FAILED" = 1 ]; then
        common_warn "The background service is NOT registered — waired status will report it as not running."
        darwin_register_recovery_hint
    fi
}

# ---------------------------------------------------------------------
# main
# ---------------------------------------------------------------------

main() {
    while [ $# -gt 0 ]; do
        case "$1" in
            --dry-run) DRY_RUN=1 ;;
            # Export so children (waired init, the engine installer) mask
            # their output through the same env contract.
            --mask-pii) WAIRED_PII_MASK=1; export WAIRED_PII_MASK ;;
            # Leave Claude Code on the Anthropic API. Exported so it survives
            # the sudo env_reset and reaches `waired init` (the single decider
            # of routing, which defaults --skip-claude-route from this env).
            # Mirrors install.ps1's -SkipClaudeProxy / WAIRED_NO_CLAUDE_PROXY.
            --skip-claude-proxy|--skip-proxy) WAIRED_NO_CLAUDE_PROXY=1; export WAIRED_NO_CLAUDE_PROXY ;;
            --skip-ollama) FLAG_NO_OLLAMA=1 ;;
            --check) FLAG_CHECK=1 ;;
            --update) FLAG_UPDATE=1 ;;
            --yes|-y) FLAG_YES=1 ;;
            --no-init) FLAG_NO_INIT=1 ;;
            --clean) FLAG_CLEAN=1 ;;
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
            --log-level)
                shift
                [ "$#" -gt 0 ] || common_die "--log-level requires an argument (debug|info|warn|error)"
                LOG_LEVEL="$1"
                ;;
            --log-level=*)
                LOG_LEVEL="${1#--log-level=}"
                ;;
            # Mirrors install.ps1's -NonInteractive. More than a --yes alias:
            # it is also the opt-in that lets sign-in run on a host with no
            # terminal (see linux_maybe_init / darwin_maybe_init).
            --non-interactive) FLAG_NON_INTERACTIVE=1 ;;
            --inference-enabled)
                shift
                [ "$#" -gt 0 ] || common_die "--inference-enabled requires an argument (true|false)"
                INFERENCE_ENABLED="$1"
                ;;
            --inference-enabled=*)
                INFERENCE_ENABLED="${1#--inference-enabled=}"
                ;;
            --share-with-mesh)
                shift
                [ "$#" -gt 0 ] || common_die "--share-with-mesh requires an argument (true|false)"
                SHARE_WITH_MESH="$1"
                ;;
            --share-with-mesh=*)
                SHARE_WITH_MESH="${1#--share-with-mesh=}"
                ;;
            -h|--help) show_help; exit 0 ;;
            *) common_die "unknown argument: $1 (try --help)" ;;
        esac
        shift
    done

    # Validate --log-level / $WAIRED_LOG_LEVEL now so a typo fails at install
    # time rather than silently at daemon boot (the agent tolerates a bad env
    # value by falling back to info).
    if [ -n "$LOG_LEVEL" ]; then
        case "$LOG_LEVEL" in
            debug|info|warn|error) : ;;
            *) common_die "--log-level must be one of: debug info warn error (got: $LOG_LEVEL)" ;;
        esac
    fi

    # Same reasoning for the two pre-answered setup questions: `waired init`
    # takes them as Go bool flags, so anything other than true|false is a
    # parse error deep inside init, long after the privileged steps started.
    if [ -n "$INFERENCE_ENABLED" ]; then
        case "$INFERENCE_ENABLED" in
            true|false) : ;;
            *) common_die "--inference-enabled must be true or false (got: $INFERENCE_ENABLED)" ;;
        esac
    fi
    if [ -n "$SHARE_WITH_MESH" ]; then
        case "$SHARE_WITH_MESH" in
            true|false) : ;;
            *) common_die "--share-with-mesh must be true or false (got: $SHARE_WITH_MESH)" ;;
        esac
    fi

    # --clean always wipes and installs fresh, so the read-only --check
    # and the in-place --update contradict it.
    if [ "$FLAG_CLEAN" = 1 ] && { [ "$FLAG_CHECK" = 1 ] || [ "$FLAG_UPDATE" = 1 ]; }; then
        common_die "--clean cannot be combined with --check/--update (a clean install always installs fresh)"
    fi

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
            WAIRED_INSTALL_BASE_URL=https://github.com/waired-ai/waired-agent/releases/download/edge
        fi
    fi

    resolve_control_url

    # Pre-install review: show what is about to happen and ask before ANY
    # work (repo changes, sudo) starts. Skips itself for --clean (the
    # dedicated consent below), --check/--update and already-installed
    # hosts (the update path prompts on its own).
    confirm_proceed

    # Clean install: collect consent before elevating (mirrors
    # uninstall.sh's confirm-then-elevate order), then wipe. This runs
    # after the edge base-URL rewiring above so an --edge clean install
    # fetches the matching edge uninstall.sh.
    confirm_clean_install

    common_elevate

    run_clean_wipe

    # Under --clean the dispatch below must take the fresh-install arm:
    # on a real run the wipe already emptied the installed state, but a
    # --dry-run host still looks installed and would misleadingly
    # preview the update path.
    case "$OS_KIND:$OS_FAMILY" in
        linux:debian)
            if [ "$FLAG_CLEAN" != 1 ] && { [ "$FLAG_CHECK" = 1 ] || [ "$FLAG_UPDATE" = 1 ] || [ -n "$(linux_apt_detect_installed)" ]; }; then
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
            # darwin_install_complete, not darwin_detect_installed: a host
            # whose install aborted part-way still has the binary, and
            # dispatching that to the update path is how it got stuck (the
            # update path never installed the pieces it was missing). An
            # explicit --check/--update still wins, as on Linux.
            if [ "$FLAG_CLEAN" != 1 ] && { [ "$FLAG_CHECK" = 1 ] || [ "$FLAG_UPDATE" = 1 ] || darwin_install_complete; }; then
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
