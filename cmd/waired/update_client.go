package main

import (
	"encoding/json"
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
swap; macOS re-runs install.sh under administrator privileges. Ollama is
notify-only.`

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
	cmd.Flags().BoolVar(&force, "force", false, "bypass the daemon's cached check result")
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
	if st != nil {
		fmt.Print(formatUpdateSummary(st))
	}

	if *checkOnly {
		// The daemon's cached answer reflects the host's *current* apt suite, so
		// it can't report a channel the caller explicitly asked about, and it
		// can't rank edge builds. For an explicit --edge/--stable, or an edge
		// host, run the installer's channel-aware check instead; otherwise the
		// daemon answer is enough (and needs no elevation).
		if requested == "" && host != "edge" && st != nil && st.Phase != management.UpdatePhaseError {
			return nil
		}
		return runInstaller("", true, false, installerArg, host)
	}

	// Apply path.
	if shouldStopUpToDate(st, requested, host, *force) {
		fmt.Println("waired is already up to date.")
		return nil
	}
	if st != nil && st.LatestVersion != "" {
		fmt.Printf("Updating waired to %s via the installer...\n", st.LatestVersion)
	} else {
		fmt.Println("Updating waired to the latest release via the installer...")
	}
	target := ""
	if st != nil {
		target = st.LatestVersion
	}
	return runInstaller(target, false, *yes, installerArg, host)
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
// compiled-in version is the most portable signal (macOS/Windows edge builds
// carry "edge." in buildinfo.Version); Linux .deb edge binaries carry only a
// short SHA, so there the installed package version is the ground truth (a
// prior buggy update may have left a stale stable apt source while an edge
// build is installed — dpkg-first detection recovers edge), with the apt
// source files as the fallback when nothing is installed via dpkg.
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

func formatUpdateSummary(st *management.UpdateStatus) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Current version: %s\n", orDash(st.CurrentVersion))
	if st.Phase == management.UpdatePhaseError {
		fmt.Fprintf(&b, "Update check failed: %s\n", st.Error)
		return b.String()
	}
	fmt.Fprintf(&b, "Latest version:  %s\n", orDash(st.LatestVersion))
	return b.String()
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
