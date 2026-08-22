package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/buildinfo"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/platform/pwsh"
	"github.com/waired-ai/waired-agent/internal/update"
)

// runUpdate implements `waired update` (Tailscale-style manual update).
//
// The check is read from the local daemon's MGMT API (fast, cached). The
// apply delegates to the existing installer script under elevation — the
// daemon runs unprivileged and cannot install, so the CLI re-runs the
// official installer (install.sh / install.ps1), which self-elevates and
// owns download/verify/swap/restart. This reuses the signing-free,
// cross-OS installer machinery rather than reimplementing it.
//
//	waired update            check, then apply if an update is available
//	waired update --check    report only; never apply
//	waired update --yes      skip the installer's interactive confirmation
//	waired update --edge     update on the edge channel (switch to it if needed)
//	waired update --stable   update on the stable channel (switch to it if needed)
//	waired update --notify=on|off
//	                         toggle the tray's proactive "update available"
//	                         prompt (#294); persisted by the daemon, no apply
//
// By default (no --edge/--stable) the update stays on whatever channel the
// host already tracks — an edge build updates to the latest edge, a stable
// build to the latest stable — so `waired update` never silently moves an edge
// host onto stable.
const updateLong = `Check for and apply a waired update (Tailscale-style). Reads the available
version from the local daemon, then re-runs the official installer under
elevation to apply.

  waired update           Update within the current channel (edge stays edge).
  waired update --check   Report only; never apply.
  waired update --yes     Apply without an interactive prompt.
  waired update --edge    Update on / switch to the edge channel (latest main build).
  waired update --stable  Update on / switch to the stable channel.

Linux applies via apt (install.sh); Windows via the install.ps1 elevated
swap; macOS re-runs install.sh under administrator privileges. An engine
already installed here is brought to this build's pinned version at the
same time; a computer with no engine does not get one.`

func newUpdateCmd() *cobra.Command {
	var mgmt, notify string
	var checkOnly, yes, force, edge, stable bool
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Check for and apply a waired update (Tailscale-style).",
		Long:  updateLong,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if edge && stable {
				return fmt.Errorf("--edge and --stable are mutually exclusive")
			}
			return runUpdateBody(mgmt, checkOnly, yes, force, notify, requestedChannel(edge, stable))
		},
	}
	cmd.Flags().BoolVar(&checkOnly, "check", false, "report whether an update is available; do not apply")
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the installer's interactive confirmation (apply non-interactively)")
	cmd.Flags().BoolVar(&force, "force", false, "re-resolve from the authoritative source, not the daemon's cached result (Linux: refreshes the package index, so it asks for sudo)")
	cmd.Flags().BoolVar(&edge, "edge", false, "update on the edge channel (latest main build); switches an existing install to edge")
	cmd.Flags().BoolVar(&stable, "stable", false, "update on the stable channel; switches an existing install to stable")
	cmd.Flags().StringVar(&notify, "notify", "", "enable/disable the tray's proactive update prompt: on|off (sets the preference; no check/apply)")
	addMgmtFlag(cmd, &mgmt)
	return cmd
}

// requestedChannel maps the --edge/--stable flags to the channel string
// ("edge"/"stable") the installer understands, or "" when neither is set
// (preserve the host's current channel). The caller has already rejected the
// mutually-exclusive both-set case.
func requestedChannel(edge, stable bool) string {
	switch {
	case edge:
		return "edge"
	case stable:
		return "stable"
	default:
		return ""
	}
}

func runUpdateBody(mgmt string, checkOnlyVal, yesVal, forceVal bool, notifyVal, requested string) error {
	gf := globalFlags{Mgmt: mgmt}
	checkOnly := &checkOnlyVal
	yes := &yesVal
	force := &forceVal
	notify := &notifyVal

	// --notify is a standalone settings action: it persists the prompt
	// preference on the daemon and returns, never touching the check/apply
	// path. The preference lives on the daemon, so it needs a reachable
	// daemon (unlike --check, which can fall back to the installer).
	if *notify != "" {
		return runUpdateNotify(gf.Mgmt, *notify)
	}

	// Channel selection: an explicit --edge/--stable wins; otherwise preserve
	// whatever channel this host already tracks. host drives which mirror the
	// installer is fetched from; installerArg is the flag passed to it (edge is
	// made explicit even when only detected — --edge is an existing installer
	// flag — while a bare stable host passes nothing so the installer preserves
	// its channel and older installer scripts never see an unknown --stable).
	host := detectHostChannel(runtime.GOOS)
	installerArg := requested
	if installerArg == "" && host == "edge" {
		installerArg = "edge"
	}

	// Ask the daemon (cheap, cached). nil => daemon down or an older daemon
	// without the route; we fall back to the installer's own check.
	st := daemonUpdateCheck(gf.Mgmt, *force)

	if *checkOnly {
		useInstaller, note := checkRoute(st, requested, host, runtime.GOOS, *force, canElevateForCheck())
		if st != nil {
			// Only the current version when the installer is about to print
			// the authoritative verdict: two "latest" numbers that disagree
			// are worse than one (#726).
			fmt.Print(formatUpdateSummary(st, !useInstaller))
		}
		if note != "" {
			fmt.Println(note)
		}
		if !useInstaller {
			return nil
		}
		return runInstaller("", true, false, installerArg, host)
	}

	if st != nil {
		fmt.Print(formatUpdateSummary(st, true))
	}

	// Apply path.
	if shouldStopUpToDate(st, requested, host, *force) {
		fmt.Println("waired is already up to date.")
		return nil
	}
	if applyStopsForIndexRefresh(*force, canElevateForCheck()) {
		return errors.New(indexRefreshNoTerminalError)
	}
	// Deliberately no version here. The installer re-resolves the target
	// authoritatively and this process cannot make it honour a different
	// answer (runInstaller drops the argument), so naming one prints a
	// promise nothing enforces — and when the daemon's cached answer
	// happened to equal the installed version, that promise read as
	// "updating X to X" (waired-agent#1006). The installer reports the
	// version it actually installed.
	fmt.Println(applyingViaInstallerNote)
	target := ""
	if st != nil {
		target = st.LatestVersion
	}
	return runInstaller(target, false, *yes, installerArg, host)
}

// indexNoTerminalNote explains why `--check --force` did not do what its
// name says. Refreshing the package index runs `apt-get update` inside the
// installer, which needs root; without a terminal sudo has nothing to
// prompt on, so the run would fail instead of answering. A scripted check
// that reports an old answer honestly beats one that exits non-zero.
const indexNoTerminalNote = "Could not refresh the package index: that needs sudo, and there is no terminal to ask on. The answer above is only as current as the index."

// indexRefreshNoTerminalError is the apply-path twin. --check can answer
// from a stale index and say so; an apply cannot, because the run would
// announce a target taken from the daemon's cache and then fail inside the
// installer, which asks for the same sudo (waired-agent#1006).
const indexRefreshNoTerminalError = "could not refresh the package index: that needs sudo, and there is no terminal to ask on. --force asked for a fresh answer, so this run stops instead of updating towards a cached one. Re-run from a terminal, or as root"

// applyingViaInstallerNote names no version on purpose — see the call site.
const applyingViaInstallerNote = "Updating waired via the installer (it resolves the target version itself)..."

// applyStopsForIndexRefresh reports whether an apply run must stop before
// it announces anything. Pure so the decision is table-tested; the caller
// supplies canElevateForCheck()'s answer, which already reports true on
// the platforms whose update path needs no root.
func applyStopsForIndexRefresh(force, canElevate bool) bool {
	return force && !canElevate
}

// canElevateForCheck reports whether the installer's check can reach root
// without a prompt nothing is there to answer.
//
// A terminal is one way in, not the only one — already being root and sudo
// configured NOPASSWD both work without one — so gating on the TTY alone
// would refuse a refresh to precisely the hosts that automate this. Ask
// sudo instead of assuming: `sudo -n` never prompts, it fails, which is
// the answer we want.
//
// Asking from THIS process is what makes the answer faithful. Without a
// TTY sudo keys its timestamp to the parent process, so a ticket warmed by
// some earlier `sudo -v` in the calling script does not carry into the
// installer we would spawn. Verified on Ubuntu 26.04: with a warm ticket,
// `sudo -n true` succeeds from the calling shell and fails from a child
// process ("interactive authentication is required") — and the installer
// run really does fail there too. The probe agreeing with the installer is
// the point; a check run in the wrong process would answer for the wrong
// one.
func canElevateForCheck() bool {
	if runtime.GOOS != "linux" {
		return true // only the Linux check needs root at all
	}
	if os.Geteuid() == 0 || isTerminal(os.Stdin) {
		return true
	}
	return exec.Command("sudo", "-n", "true").Run() == nil
}

// checkRoute decides how `waired update --check` answers. useInstaller
// selects the installer's own channel-aware check, whose verdict supersedes
// the daemon's; note is a line to print when the daemon's answer has to
// stand for a reason the reader would otherwise have to guess.
//
// Pure, so the table is exercised without a daemon, an installer, or a
// 25-minute round trip to a real host. The four reasons to leave the daemon:
//
//   - no usable daemon answer — the installer is the only check left.
//   - an explicit --edge/--stable names a channel the daemon cannot report:
//     its answer always reflects the suite this host currently tracks.
//   - an edge host's answer cannot be ranked at all — isDevVersion in
//     internal/update never flags an edge build as an update.
//   - --force means "re-resolve from the authoritative source". On Linux
//     that source is the package index, and only the installer can refresh
//     it (waired-agent#726). Everywhere else the daemon already queries the
//     feed live, so bypassing its result cache is the whole of --force.
func checkRoute(st *management.UpdateStatus, requested, host, goos string, force, canElevate bool) (useInstaller bool, note string) {
	daemonUnusable := st == nil || st.Phase == management.UpdatePhaseError
	want := daemonUnusable || requested != "" || host == "edge"
	// Anything but a known-live answer counts: a daemon that predates this
	// change reports no source at all, yet on Linux it is reading the
	// package index — that is what the Linux resolver does. Requiring an
	// explicit "apt" would keep the old, wrong --force behaviour for
	// exactly the mixed-version window this has to work in. Unknown means
	// "cannot rule out a stale index", so honour --force.
	if !want && force && goos == "linux" && st.LatestSource != update.SourceGitHub {
		want = true
	}
	if !want {
		return false, ""
	}
	// On Linux every installer check refreshes the package index first, so
	// it needs root. When root is out of reach and there is a usable daemon
	// answer, degrade to that answer and say why rather than failing the
	// command. With no daemon answer there is nothing to degrade TO, so let
	// the installer run and report its own failure.
	if goos == "linux" && !canElevate && !daemonUnusable {
		return false, indexNoTerminalNote
	}
	return true, ""
}

// shouldStopUpToDate reports whether the apply path should short-circuit with
// "already up to date" instead of running the installer. It stops only for a
// stable host the daemon confirms is current: an explicit channel request or
// --force always proceeds, and an edge host always proceeds — the daemon's
// dotted-version compare can't rank timestamped edge builds, so it never
// reports edge updates as available, and the installer's apt check (which
// no-ops when already newest) is the authority instead.
func shouldStopUpToDate(st *management.UpdateStatus, requested, host string, force bool) bool {
	if requested != "" || force {
		return false
	}
	effective := requested
	if effective == "" {
		effective = host
	}
	if effective == "edge" {
		return false
	}
	return st != nil && st.Phase != management.UpdatePhaseError && !st.Available
}

// detectHostChannel reports the release channel this host currently tracks
// ("edge" / "stable"), or "" when it can't tell. It drives which installer
// mirror is fetched and whether the apply path may report "up to date". The
// compiled-in version is the most portable signal: every edge build carries
// "edge." in buildinfo.Version.
//
// The dpkg branch below predates that being true on Linux. Until #631 the
// .deb build never received the version ldflag, so Linux edge binaries
// reported a bare short SHA and the first check could not fire there; the
// installed package version was the only ground truth. It still earns its
// place — a prior buggy update may have left a stale stable apt source while
// an edge build is installed, and dpkg-first detection recovers edge — but it
// is now the recovery path it reads as, not the primary one. The apt source
// files remain the fallback when nothing is installed via dpkg.
func detectHostChannel(goos string) string {
	if strings.Contains(buildinfo.Version, "edge.") {
		return "edge"
	}
	if goos == "linux" {
		if out, err := exec.Command("dpkg-query", "-W", "-f=${Version}", "waired").Output(); err == nil {
			v := strings.TrimSpace(string(out))
			switch {
			case strings.Contains(v, "~edge"), strings.Contains(v, "-edge"):
				return "edge"
			case v != "":
				return "stable"
			}
		}
		if _, err := os.Stat("/etc/apt/sources.list.d/waired-edge.list"); err == nil {
			return "edge"
		}
		if _, err := os.Stat("/etc/apt/sources.list.d/waired.list"); err == nil {
			return "stable"
		}
	}
	return ""
}

// runUpdateNotify persists the tray's update-prompt preference via the
// daemon's POST /waired/v1/update/settings (#294). Unlike --check it has no
// installer fallback: the preference lives on the daemon, so an unreachable
// daemon is a hard error rather than a silent no-op.
func runUpdateNotify(mgmtURL, arg string) error {
	on, err := parseNotifyArg(arg)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(management.UpdateSettingsRequest{Notify: on})
	out, err := httpPost(mgmtURL+"/waired/v1/update/settings", body)
	if err != nil {
		return fmt.Errorf("set update-notify preference (is the daemon running?): %w", err)
	}
	var st management.UpdateStatus
	if json.Unmarshal(out, &st) == nil {
		if st.NotifyEnabled {
			fmt.Println("Update prompts: on — the tray will notify you when a new version is available.")
		} else {
			fmt.Println("Update prompts: off — run `waired update --check` to check manually.")
		}
		return nil
	}
	// Daemon accepted the change but returned an unexpected body; report the
	// requested state so the user still gets confirmation.
	fmt.Printf("Update prompts: %s.\n", arg)
	return nil
}

// parseNotifyArg maps the --notify value to a bool. Accepts on/off plus the
// common true/false / enable(d) / disable(d) synonyms so the flag is
// forgiving; anything else is an error.
func parseNotifyArg(arg string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "on", "true", "enable", "enabled", "yes":
		return true, nil
	case "off", "false", "disable", "disabled", "no":
		return false, nil
	default:
		return false, fmt.Errorf("invalid --notify value %q (use on|off)", arg)
	}
}

// daemonUpdateCheck POSTs /waired/v1/update/check and returns the status, or
// nil when the daemon is unreachable / predates the route (any error).
func daemonUpdateCheck(mgmtURL string, force bool) *management.UpdateStatus {
	body, _ := json.Marshal(management.UpdateCheckRequest{Force: force})
	out, err := httpPost(mgmtURL+"/waired/v1/update/check", body)
	if err != nil {
		return nil
	}
	var st management.UpdateStatus
	if json.Unmarshal(out, &st) != nil {
		return nil
	}
	return &st
}

// formatUpdateSummary renders the daemon's answer. full=false prints the
// current version only — see the call site in runUpdateBody.
func formatUpdateSummary(st *management.UpdateStatus, full bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Current version: %s\n", orDash(st.CurrentVersion))
	if st.Phase == management.UpdatePhaseError {
		fmt.Fprintf(&b, "Update check failed: %s\n", st.Error)
		return b.String()
	}
	if !full {
		return b.String()
	}
	fmt.Fprintf(&b, "Latest version:  %s\n", orDash(st.LatestVersion))
	b.WriteString(packageIndexLine(st, time.Now()))
	return b.String()
}

// indexStaleAfter is how old the package index may be before the summary
// says outright that the answer may be behind. Under it the age is still
// printed — the reader should never have to guess what a "latest" number is
// based on — but without the caution: a daily-ish refresh is what a healthy
// apt host looks like, and a warning on every check is a warning nobody
// reads.
const indexStaleAfter = 24 * time.Hour

// packageIndexLine renders the "Package index:" line for an answer read
// from the local package index, or "" for anything else — a live GitHub
// answer has no local index to age, and an unknown instant does not earn a
// line that says nothing. now is a parameter so the rendering is testable.
func packageIndexLine(st *management.UpdateStatus, now time.Time) string {
	if st.LatestSource != update.SourceAPT || st.IndexRefreshedAt == "" {
		return ""
	}
	at, err := time.Parse(time.RFC3339, st.IndexRefreshedAt)
	if err != nil {
		return ""
	}
	age := now.Sub(at)
	line := "Package index:   refreshed " + humanIndexAge(age)
	if age >= indexStaleAfter {
		line += " — a newer build may already be published; `waired update --check --force` refreshes it"
	}
	return line + "\n"
}

// humanIndexAge renders an index age at the granularity a reader acts on:
// days once there are any, hours below that. (The tray's humanAge is
// minutes-only — right for a fallback event a moment ago, useless for an
// index last touched last week.)
func humanIndexAge(d time.Duration) string {
	switch {
	case d < 0:
		return "just now" // clock skew; "-3h ago" helps nobody
	case d < time.Hour:
		return "less than an hour ago"
	case d < 2*time.Hour:
		return "1 hour ago"
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d/time.Hour))
	case d < 72*time.Hour:
		return "2 days ago"
	default:
		return fmt.Sprintf("%d days ago", int(d/(24*time.Hour)))
	}
}

func orDash(s string) string {
	if s == "" {
		return "(unknown)"
	}
	return s
}

// runInstaller downloads the official installer script for this OS and runs
// it (read-only check, or an elevating apply). It wires the child's stdio to
// the terminal so the installer's sudo/UAC prompt and progress are visible.
// The target version is informational only — the installer re-resolves
// "latest" authoritatively (so we never pass a possibly-mismatched pin); an
// operator who wants to pin sets WAIRED_VERSION, which passes through.
//
// channel is the update channel passed to the installer ("edge"/"stable"/""
// for preserve). hostChannel selects the mirror the installer is fetched from
// (the host's current channel), so the script is at least as new as the running
// binary and understands any newly-added flags.
func runInstaller(target string, checkOnly, yes bool, channel, hostChannel string) error {
	_ = target
	goos := runtime.GOOS
	scriptPath, err := downloadInstaller(goos, update.ScriptURLForChannel(goos, hostChannel))
	if err != nil {
		return fmt.Errorf("download installer: %w", err)
	}
	defer func() { _ = os.Remove(scriptPath) }()

	name, args := update.InstallerArgs(goos, scriptPath, checkOnly, yes, channel)
	cmd := exec.Command(name, args...)
	// On Windows this is Windows PowerShell 5.1, which must not inherit a
	// PowerShell 7 PSModulePath (#178) — see internal/platform/pwsh. The
	// helper is an identity transform on the sh branches.
	cmd.Env = pwsh.Env()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("installer (%s) failed: %w", name, err)
	}
	return nil
}

// downloadInstaller fetches url into a temp file with the right suffix.
func downloadInstaller(goos, url string) (string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("status %d fetching %s", resp.StatusCode, url)
	}
	suffix := ".sh"
	if goos == "windows" {
		suffix = ".ps1"
	}
	f, err := os.CreateTemp("", "waired-install-*"+suffix)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, io.LimitReader(resp.Body, 1<<20)); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}
