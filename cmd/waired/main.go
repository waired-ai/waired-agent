// Command waired is the Waired CLI. It drives both the local
// waired-agent daemon (status / ping over the Local Management API on
// 127.0.0.1:9476) and the Control Plane during enrollment (`waired init`).
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/curve25519"

	"github.com/waired-ai/waired-agent/internal/controlurl"
	"github.com/waired-ai/waired-agent/internal/identity"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/management/ipcclient"
	"github.com/waired-ai/waired-agent/internal/platform/console"
	"github.com/waired-ai/waired-agent/internal/platform/elevation"
	"github.com/waired-ai/waired-agent/internal/platform/paths"
	"github.com/waired-ai/waired-agent/internal/platform/secrets"
	"github.com/waired-ai/waired-agent/internal/platform/service"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// exitLocalAIDown is `waired init`'s "signed in, but this device has no
// local AI" — the engine could not be installed (#188), or it installed
// and would not stay up (#310).
//
// Its own code because the two answers an installer could give with only
// 0 and 1 are both wrong: 0 is what let install.sh print
// "🎉 Waired is installed." over a device whose engine never came up,
// and 1 would have it report a sign-in that plainly succeeded as
// "sign-in did not complete".
//
// 1 stays every other failure. 130 is taken by the Ctrl-C path
// (setup_executor.go), which is a real interruption and must keep saying
// so. 2 is left alone: shells and getopt conventionally use it for usage
// errors, and this is not one.
const exitLocalAIDown = 3

// exitPlanFor is everything main does about a command's error: the process
// exit code, and whether to print the error at all.
//
// Both halves are split out of main because main calls os.Exit, so nothing
// can observe either through it — and asserting on the sentinel instead
// would pin the plumbing while leaving unpinned both the number the
// installers branch on and the line the user sees.
func exitPlanFor(err error) (code int, printErr bool) {
	switch {
	case err == nil:
		return 0, false
	case errors.Is(err, errLocalAIDown):
		// The one failure that says nothing. The closing box has already
		// said it in the words a person reads, and a "waired: ..." line
		// after it would read as a second, separate problem.
		return exitLocalAIDown, false
	default:
		return 1, true
	}
}

func main() {
	// Before anything prints: put the Windows console on CP_UTF8 so the UTF-8
	// bytes below are decoded as UTF-8 and not as the machine's ANSI code page
	// (#629). Inert on Linux and macOS. restore is called explicitly rather
	// than deferred because os.Exit below does not run defers, and a console
	// left on CP_UTF8 would outlive this process.
	restoreConsole := console.SetOutputUTF8()

	err := newRootCmd().Execute()
	code, printErr := exitPlanFor(err)
	if printErr {
		fmt.Fprintln(os.Stderr, "waired:", friendlyError(err))
	}
	restoreConsole()
	if code != 0 {
		os.Exit(code)
	}
}

// ---------------- waired init ----------------

// initFlags holds every `waired init` flag value. The tri-state
// inferenceEnabled / inferenceShare are *bool (nil unless the operator
// passed the flag), matching the old flagBoolPtr semantics.
type initFlags struct {
	control          string
	deviceName       string
	noBrowser        bool
	stateDir         string
	skipIntegration  bool
	gatewayBaseURL   string
	nonInteractive   bool
	inferenceEnabled *bool
	inferenceShare   *bool
	bundledModelID   string
	mgmtURL          string
	authKey          string
	forceReauth      bool
	maskPII          bool
	skipClaudeRoute  bool
}

const initLong = `Enroll this device into a Waired network (Google sign-in).

Re-run 'waired init' on an already-enrolled device to refresh tokens +
Device Certificate without losing the DeviceID.`

func newInitCmd() *cobra.Command {
	o := &initFlags{}
	var infEnabled, infShare bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Enroll this device into a Waired network (Google sign-in).",
		Long:  initLong,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Reconstruct the tri-state: nil unless the flag was passed.
			if cmd.Flags().Changed("inference-enabled") {
				o.inferenceEnabled = &infEnabled
			}
			if cmd.Flags().Changed("share-with-mesh") {
				o.inferenceShare = &infShare
			}
			if o.maskPII {
				restore := enablePIIMask()
				defer restore()
			}
			return runInitBody(o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.control, "control", os.Getenv("WAIRED_CONTROL_URL"),
		"Control Plane base URL (e.g., http://127.0.0.1:9477)")
	f.StringVar(&o.deviceName, "device-name", "",
		"name to report for this computer at enrollment (default: hostname)")
	f.BoolVar(&o.noBrowser, "no-browser", false,
		"don't open the browser; print the URL and code instead")
	f.StringVar(&o.stateDir, "state-dir", defaultInitStateDir(),
		"directory for identity / secrets / cache files")
	f.BoolVar(&o.skipIntegration, "skip-integration", false,
		"skip the coding-agent integration phase (Claude Code / OpenClaw auto-config)")
	f.StringVar(&o.gatewayBaseURL, "gateway-base-url", defaultGatewayURL,
		"Local Gateway base URL the integration phase wires into the agents (Claude proxy / OpenClaw plugin)")
	f.BoolVar(&o.nonInteractive, "non-interactive", false,
		"skip all interactive prompts; use hardware-derived defaults for inference choices")
	f.BoolVar(&infEnabled, "inference-enabled", false,
		"answer \"Run AI models on this computer?\" without prompting: --inference-enabled=true / =false")
	f.BoolVar(&infShare, "share-with-mesh", false,
		"answer \"Let your other devices use this computer's AI?\" without prompting: --share-with-mesh=true / =false. The shorter name (vs --inference-share-with-mesh) is intentional: under 'waired init' the 'inference-' prefix is redundant.")
	f.StringVar(&o.bundledModelID, "inference-bundled-model-id", "",
		"pin the bundled model to pre-pull (manifest model_id); empty auto-selects the largest model that fits this host above the coding-quality floor (#517). Combine with --inference-enabled=true to force-install on a host below the recommended spec.")
	f.StringVar(&o.mgmtURL, "mgmt", defaultMgmtURL,
		"Local Management API base URL. Sign-in is driven through the waired-agent running here (the Tailscale model); if nothing answers, init reports it instead of enrolling locally")
	f.StringVar(&o.authKey, "auth-key", "",
		"enroll with an auth key instead of a browser sign-in, for servers and containers. Accepts the key itself, \"file:/path/to/key\" (preferred on a shared host — a key in argv is visible in `ps`), or $WAIRED_AUTH_KEY when the flag is omitted. Create one in the Waired console. Requires the background service to be running: an auth key is a credential, not a way to skip the daemon.")
	f.BoolVar(&o.forceReauth, "force-reauth", false,
		"sign in again even though this device is already signed in. Without it, `waired init` on an enrolled device resumes setup and leaves the existing credentials alone — including when an --auth-key is passed, which is then not used. Re-authentication also happens on its own when the device's sign-in has expired beyond repair.")
	f.BoolVar(&o.maskPII, "mask-pii", os.Getenv("WAIRED_PII_MASK") != "",
		"mask personal information (home directory, username, hostname, account email) in init's output — for screenshots and bug reports. Best-effort; env form: WAIRED_PII_MASK=1 (set by the installers' --mask-pii / -MaskPII). Progress rendering falls back to plain lines while masking.")
	f.BoolVar(&o.skipClaudeRoute, "skip-claude-route", os.Getenv("WAIRED_NO_CLAUDE_PROXY") != "",
		"do not point Claude Code at local inference; leave Claude Code talking to the Anthropic API directly. The rest of the coding-agent integration (skills / plugins) still installs. Enable routing later with an elevated `waired claude enable`. Env form: WAIRED_NO_CLAUDE_PROXY=1 (set by the installers' -SkipClaudeProxy / --skip-claude-proxy).")
	return cmd
}

// runInitBody resolves the control URL and device name, confirms a
// re-auth, and hands the run to the daemon. It performs no enrollment
// itself: the daemon owns that (#175), and a daemon that is not answering
// is an error rather than a reason to do it here.
//
// The leading block re-aliases the parsed flags (fields of o) to the
// pointer names the body uses.
func runInitBody(o *initFlags) error {
	control := &o.control
	deviceName := &o.deviceName
	noBrowser := &o.noBrowser
	stateDir := &o.stateDir
	skipIntegration := &o.skipIntegration
	gatewayBaseURL := &o.gatewayBaseURL
	nonInteractive := &o.nonInteractive
	inferenceEnabled := &o.inferenceEnabled
	inferenceShare := &o.inferenceShare
	bundledModelID := &o.bundledModelID
	mgmtURL := &o.mgmtURL
	authKeyFlag := &o.authKey
	forceReauth := &o.forceReauth

	// Fall back to the installer-configured control URL (e.g. what
	// `install.sh --control <URL>` wrote to /etc/waired/agent.env) when the
	// operator passed neither --control nor $WAIRED_CONTROL_URL, so the
	// common `sudo waired init` (no flag) just works. The daemon's
	// login controller resolves the same three tiers through the same
	// package (#174).
	var controlSource controlurl.Source
	*control, controlSource = controlurl.ResolveWithSource(*control, controlurl.PlatformDefault())
	// Normalize the scheme up front (bare "dev.waired.net" -> https://...,
	// loopback -> http://...). Done before the renew comparison below so a
	// scheme-less flag matches the stored (already-normalized) ControlURL
	// instead of tripping a spurious "already enrolled to X" error.
	if *control != "" {
		norm, err := controlurl.Normalize(*control)
		if err != nil {
			return err
		}
		*control = norm
	}

	// One reader owns stdin for this whole run, and every prompt below —
	// the re-auth confirmation, the sign-in gate, the inference answers,
	// the benchmark, the routing and statusline questions — reads through
	// it. `waired init` used to layer four independent readers over the
	// same fd, so a keystroke aimed at one step was spent on the next
	// (#223, the standalone twin of #184/#185). Terminal-only: a piped
	// stdin belongs to the script driving init, and reading ahead from it
	// would swallow input meant for a later command in that script.
	var stdinOwner *stdinReader
	if isTerminal(os.Stdin) {
		stdinOwner = newStdinReader(os.Stdin)
	}
	in := promptReader(stdinOwner)

	// `waired init` is the entry point for an already-enrolled device too
	// (gcloud-init style). When one is found we:
	//   - Reuse its ControlURL / DeviceName when the operator didn't
	//     pass new ones explicitly.
	//   - Refuse to silently move the device to a *different* CP — the
	//     operator must run `waired logout` first.
	//
	// Whether it also re-authenticates is a separate question, decided
	// below from what the daemon says about its credentials (#313).
	existing, idErr := identity.Load(*stateDir)
	if idErr != nil {
		return fmt.Errorf("load existing identity: %w", idErr)
	}
	// #313: the daemon is the authority on enrollment. This CLI's state
	// dir can be the wrong one (an unelevated Windows run resolves
	// %AppData% while the daemon reads %ProgramData%) or unreadable (a
	// standard user against the ACL'd tree, waired#751) — and reading
	// "not enrolled" off either used to turn a resume into a protocol
	// error. Nil means "no answer", which leaves the disk's verdict.
	view := daemonIdentity(*mgmtURL)
	if existing == nil {
		existing = identityFromView(view)
	}
	renewing := existing != nil
	if renewing {
		// Nobody on this computer said which control plane to use, so the
		// one it is ALREADY ENROLLED TO wins — that is not a switch, it is
		// the absence of a request to switch (waired-agent#800).
		//
		// This is the state-dir-loss shape: agent.env lives in the state
		// dir on macOS and Windows, so losing the dir loses the record of
		// which control plane this device belongs to, and Resolve falls
		// through to the production default. Before #803 the daemon's view
		// was never read here and the run went silently to production.
		// After it, the guard below fires instead and tells the operator to
		// `waired logout` — advice for a problem they do not have. They did
		// not switch control planes; they lost the file that named one.
		*control = controlForRenew(*control, controlSource, existing.ControlURL)
		if existing.ControlURL != "" && *control != "" && existing.ControlURL != *control {
			return fmt.Errorf(
				"already enrolled to %s — run `waired logout` first to switch control planes (requested %s)",
				existing.ControlURL, *control)
		}
		if *control == "" {
			*control = existing.ControlURL
		}
		// The stored name is not re-sent — see reportedDeviceName.
	}
	// Re-authenticating rotates this device's credentials, so it happens
	// only when asked for or when the credentials are what is broken —
	// the `tailscale up` split, where re-auth is its own flag and the
	// plain command is idempotent. Everything else resumes.
	reauth := reauthWanted(*forceReauth, view)
	if reauth && renewing {
		// Auth-only refresh: whatever hardware / integration state is
		// already on disk stays untouched. A resume must NOT skip it —
		// the coding-tool step is part of the setup being resumed.
		*skipIntegration = true
		if !confirmRenew(in, os.Stdout, existing, *forceReauth, *nonInteractive) {
			fmt.Println("Nothing changed.")
			return nil
		}
	}

	if *control == "" {
		return errors.New("--control or WAIRED_CONTROL_URL is required")
	}

	*deviceName = reportedDeviceName(*deviceName, existing, hostnameOrEmpty())

	// Friendly intro for a fresh interactive first-run. Skipped on re-auth
	// (quieter). Renders a framed banner on a capable TTY, a single plain
	// line otherwise.
	if !renewing {
		welcomeBanner(os.Stdout)
	}

	// Which enrollment journey this run takes. Enrollment is daemon-owned
	// (the Tailscale model): the running waired-agent performs it and this
	// process is a thin client over its login API. There is no fallback to
	// the standalone path below — a probe that failed because the service
	// never started used to silently produce a registered but
	// capability-less device (#175). See init_route_daemon.go.
	authKey, err := authKeyFromFlags(*authKeyFlag)
	if err != nil {
		return err
	}
	route := chooseEnrollRoute(enrollFacts{
		serviceInstalled: serviceInstalledFn(),
	}, func(serviceInstalled bool) bool {
		return waitForDaemonStartup(*mgmtURL, serviceInstalled, os.Stdout)
	})
	switch route {
	case routeDaemon:
		if authKey == "" {
			fmt.Println("waired-agent is running; signing in via the daemon (no local enrollment).")
		}
		return runInitViaDaemon(daemonInitOpts{
			MgmtURL:         *mgmtURL,
			Control:         *control,
			DeviceName:      *deviceName,
			GatewayBaseURL:  *gatewayBaseURL,
			StateDir:        *stateDir,
			NoBrowser:       *noBrowser,
			NonInteractive:  *nonInteractive,
			SkipIntegration: *skipIntegration,
			SkipClaudeRoute: o.skipClaudeRoute,
			AuthKey:         authKey,
			Reauth:          reauth,
			AccountEmail:    accountEmailFromView(view),
			Inference: daemonInitInference{
				Enabled: *inferenceEnabled,
				Share:   *inferenceShare,
				ModelID: *bundledModelID,
			},
			Owner: stdinOwner,
		})
	default:
		// Every other route means "no agent to talk to", and there is no
		// longer anywhere else for enrollment to happen. The second return
		// exists so that a fourth route added without a case here fails
		// loudly instead of reporting a successful sign-in for a run that
		// enrolled nothing — which is the shape of the bug #175 started from.
		if err := daemonRequiredError(route, runtime.GOOS, serviceStartHintFn()); err != nil {
			return err
		}
		return fmt.Errorf("internal: unhandled enrollment route %d", route)
	}
}

// Service-control seams, overridable in tests so the enrollment-route
// decision can be exercised without a real systemd/launchd/SCM.
var (
	serviceInstalledFn = service.Installed
	serviceStartHintFn = service.StartHint
)

// ---------------- waired status ----------------

// globalFlags carries the shared daemon-facing flag values (--mgmt /
// --state-dir) that several subcommand bodies read. The flags themselves
// are now declared per-command via addMgmtFlag / addStateDirFlag (root.go);
// this struct just keeps those bodies referencing gf.Mgmt / gf.StateDir.
type globalFlags struct {
	Mgmt     string
	StateDir string
}

func newStatusCmd() *cobra.Command {
	var mgmt, stateDir, output string
	var observability bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show daemon + identity status.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStatusBody(mgmt, stateDir, observability, output)
		},
	}
	addMgmtFlag(cmd, &mgmt)
	addStateDirFlag(cmd, &stateDir, "directory holding identity.json")
	cmd.Flags().BoolVar(&observability, "observability", false,
		"include the Phase 9 /observability/state dump (engine, mesh, last inference)")
	cmd.Flags().StringVarP(&output, "output", "o", "",
		"output format for --observability: \"\" (text, default) or \"json\"")
	return cmd
}

func runStatusBody(mgmt, stateDir string, observability bool, output string) error {
	gf := globalFlags{Mgmt: mgmt, StateDir: stateDir}
	observabilityFlag := &observability
	jsonFlag := &output
	id, err := identity.Load(gf.StateDir)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			if notice, ok := unreadableSystemStateNotice(gf.StateDir, "waired status"); ok {
				fmt.Println(notice)
				return nil
			}
			return fmt.Errorf("permission denied reading state in %s — %s",
				gf.StateDir, elevationHint("waired status"))
		}
		return err
	}
	if id == nil {
		// This user's per-user dir is empty. Before declaring the machine
		// not enrolled, fall back to the platform SYSTEM state dir: an
		// enrolled service install lives there, and on Windows even an
		// elevated `waired status` resolves to the admin's empty %AppData%
		// first, so the fallback is the only way it sees the enrollment.
		// Every branch exits 0 — a status query is informational, not a
		// failure (waired#751).
		// status makes no further state-dir read past this point (it renders
		// from id, then queries the local daemon), so the fallback dir itself
		// is not needed here — only the loaded identity.
		_, fbID, notice := resolveSystemFallback(gf.StateDir, "waired status")
		switch {
		case fbID != nil:
			id = fbID // enrolled system-wide and readable — render it
		case notice != "":
			fmt.Println(notice)
			return nil
		default:
			fmt.Println("Not enrolled. Run `waired init` to connect this device.")
			return nil
		}
	}
	// "Control Plane" rather than the "Control" this printed before: the
	// sign-in prompt names the same thing (login_gate.go) and the two must
	// not use different words for it. It is the glossary's own headword,
	// cross-linked to "coordination service" (docs-site glossary), so this
	// is the established term rather than a new one. The other labels are
	// re-padded to keep the value column straight.
	fmt.Println("Account:      ", id.AccountEmail)
	fmt.Println("Network:      ", id.NetworkName, "("+id.NetworkID+")")
	fmt.Println("Device:       ", id.DeviceID)
	fmt.Println("Overlay IP:   ", id.OverlayIP)
	fmt.Println("Endpoint:     ", id.Endpoint)
	fmt.Println("Control Plane:", id.ControlURL)
	fmt.Println()
	fmt.Println("Daemon status:")
	body, err := httpGet(gf.Mgmt + "/waired/v1/status")
	if err != nil {
		if errors.Is(err, errAgentDown) {
			fmt.Fprintln(os.Stderr, "  (waired-agent is not running — daemon status unavailable; run `waired doctor`)")
		} else {
			fmt.Fprintln(os.Stderr, "  (daemon unreachable:", err, ")")
		}
		return nil
	}
	if err := prettyPrintStatus(body); err != nil {
		return err
	}

	// Best-effort 1-2 line inference summary (silent if the agent
	// has the inference subsystem disabled or doesn't expose it).
	if infBody, err := httpGet(gf.Mgmt + "/waired/v1/inference/status"); err == nil {
		printInferenceSummary(infBody)
	}

	if *observabilityFlag {
		printObservabilitySection(gf.Mgmt, *jsonFlag)
	}
	return nil
}

func printInferenceSummary(body []byte) {
	var s struct {
		SubsystemState string `json:"subsystem_state"`
		Runtimes       map[string]struct {
			Installed bool   `json:"installed"`
			Version   string `json:"version"`
			State     string `json:"state"`
			// Provenance (new fields; absent from old agents).
			Mode           string `json:"mode"`
			LiveVersion    string `json:"live_version"`
			VersionWarning string `json:"version_warning"`
			LastError      string `json:"last_error"`
			// Serve tuning (#621; absent from old agents).
			ContextLength int    `json:"context_length"`
			KVCacheType   string `json:"kv_cache_type"`
			NumBatch      int    `json:"num_batch"`
			TuningWarning string `json:"tuning_warning"`
			// Model residency (#879; absent from old agents, which is
			// why ModelResident is a pointer — nil means "no claim",
			// not "not resident").
			ModelResident      *bool  `json:"model_resident"`
			ModelResidentModel string `json:"model_resident_model"`
			ModelResidentUntil string `json:"model_resident_until"`
			// #910: an indefinite hold is a flag, not a date. Absent from
			// agents that predate it, which is why the date branch below
			// still has to survive a far-future value gracefully.
			ModelResidentIndefinitely bool `json:"model_resident_indefinitely"`
		} `json:"runtimes"`
		Models struct {
			Ready       []string `json:"ready"`
			Downloading []string `json:"downloading"`
			Downloads   []struct {
				Model          string `json:"model"`
				CompletedBytes int64  `json:"completed_bytes"`
				TotalBytes     int64  `json:"total_bytes"`
			} `json:"downloads"`
		} `json:"models"`
		// The last first-token wait and the fastest comparable one this
		// host has seen (#912). Absent from agents that predate it, and
		// deliberately absent from agents that have nothing honest to
		// report — a nil here is "not observed", never "instant".
		FirstToken *struct {
			Ms            uint32 `json:"ms"`
			At            string `json:"at"`
			BestMs        uint32 `json:"best_ms"`
			BestOfSamples int    `json:"best_of_samples"`
		} `json:"first_token"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		return
	}
	fmt.Println()
	fmt.Println("Inference:")
	fmt.Printf("  state:          %s\n", stateOrDashStr(s.SubsystemState))
	parts := []string{}
	warnings := []string{}
	residency := []string{}
	for name, r := range s.Runtimes {
		if !r.Installed {
			continue
		}
		// Prefer the live (serving) version over the binary probe; in
		// adopted mode only the former tells the truth.
		version := r.Version
		if r.LiveVersion != "" {
			version = r.LiveVersion
		}
		detail := r.State
		if r.Mode != "" && r.Mode != "spawned" {
			detail += ", " + r.Mode
		}
		// #621: show the effective context window + KV type so a
		// clamped/floored window is visible at a glance.
		if r.ContextLength > 0 {
			detail += fmt.Sprintf(", ctx %dk", r.ContextLength/1024)
			if r.KVCacheType != "" {
				detail += " " + r.KVCacheType
			}
			if r.NumBatch > 0 { // #642: forced generation ubatch
				detail += fmt.Sprintf(" b%d", r.NumBatch)
			}
		}
		parts = append(parts, fmt.Sprintf("%s %s (%s)", name, version, detail))
		// #879: an idle-expired model is otherwise indistinguishable
		// here from one that answers in half a second.
		if r.ModelResident != nil {
			residency = append(residency, fmt.Sprintf("%s: %s", name,
				residencyLine(*r.ModelResident, r.ModelResidentModel, r.ModelResidentUntil, r.ModelResidentIndefinitely)))
		}
		if r.VersionWarning != "" {
			warnings = append(warnings, fmt.Sprintf("%s: %s", name, r.VersionWarning))
		}
		if r.TuningWarning != "" {
			warnings = append(warnings, fmt.Sprintf("%s: %s", name, r.TuningWarning))
		}
		if r.LastError != "" {
			warnings = append(warnings, fmt.Sprintf("%s: %s", name, r.LastError))
		}
	}
	if len(parts) > 0 {
		fmt.Printf("  runtimes:       %s\n", strings.Join(parts, ", "))
	}
	if len(residency) > 0 {
		fmt.Printf("  model loaded:   %s\n", strings.Join(residency, ", "))
	}
	if ft := s.FirstToken; ft != nil {
		if line := firstTokenLine(ft.Ms, ft.At, ft.BestMs, time.Now()); line != "" {
			fmt.Printf("  first token:    %s\n", line)
		}
	}
	for _, w := range warnings {
		fmt.Printf("  %s %s\n", emo("⚠", "!"), w)
	}
	if len(s.Models.Ready) > 0 {
		fmt.Printf("  models ready:   %s\n", strings.Join(s.Models.Ready, ", "))
	}
	if len(s.Models.Downloading) > 0 {
		// Index byte progress by model so each downloading entry can show a
		// percentage + size when the agent reports it (older agents omit
		// the "downloads" field, so we fall back to the bare model name).
		prog := make(map[string]struct{ completed, total int64 }, len(s.Models.Downloads))
		for _, d := range s.Models.Downloads {
			prog[d.Model] = struct{ completed, total int64 }{d.CompletedBytes, d.TotalBytes}
		}
		entries := make([]string, 0, len(s.Models.Downloading))
		for _, m := range s.Models.Downloading {
			if p, ok := prog[m]; ok && p.total > 0 {
				pct := int(float64(p.completed) / float64(p.total) * 100)
				entries = append(entries, fmt.Sprintf("%s %d%% (%s / %s)",
					m, pct, humanGB(p.completed), humanGB(p.total)))
			} else {
				entries = append(entries, m)
			}
		}
		fmt.Printf("  downloading:    %s\n", strings.Join(entries, ", "))
	}
}

func stateOrDashStr(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// humanGB formats a byte count as decimal gigabytes ("2.3 GB"), matching
// the GB→1e9 convention ollama prints download sizes in.
func humanGB(bytes int64) string {
	return fmt.Sprintf("%.1f GB", float64(bytes)/1e9)
}

func newPingCmd() *cobra.Command {
	var mgmt string
	cmd := &cobra.Command{
		Use:   "ping <peer>",
		Short: "Send an overlay ping to a peer via the daemon.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body, _ := json.Marshal(map[string]string{"peer": args[0]})
			// pingWriteTimeout, not the default: the daemon spends up to
			// 15s waiting on the overlay, and its answer names the peer.
			resp, err := httpPostWithin(mgmt+"/waired/v1/ping", body, pingWriteTimeout)
			if err != nil {
				return err
			}
			return prettyPrint(resp)
		},
	}
	addMgmtFlag(cmd, &mgmt)
	return cmd
}

// ---------------- waired pause / resume ----------------

func newPauseCmd() *cobra.Command {
	return newPhaseCmd("pause", "Pause Waired routing — new shells stop redirecting Anthropic / OpenAI calls through the local gateway.", state.PhasePaused)
}

func newResumeCmd() *cobra.Command {
	return newPhaseCmd("resume", "Undo 'waired pause' — restore overlay routing.", state.PhaseActive)
}

func newPhaseCmd(verb, short string, target state.Phase) *cobra.Command {
	var mgmt, stateDir string
	cmd := &cobra.Command{
		Use:   verb,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPhaseTransition(mgmt, stateDir, target, verb)
		},
	}
	addMgmtFlag(cmd, &mgmt)
	addStateDirFlag(cmd, &stateDir, "directory holding identity.json")
	return cmd
}

// runPhaseTransition implements both `waired pause` and `waired resume`.
// Tries the running daemon first; on connection failure, persists the
// operator's intent locally so the next daemon start picks it up.
func runPhaseTransition(mgmt, stateDir string, target state.Phase, verb string) error {
	gf := globalFlags{Mgmt: mgmt, StateDir: stateDir}

	endpoint := "/waired/v1/pause"
	if target == state.PhaseActive {
		endpoint = "/waired/v1/resume"
	}

	body, err := httpPost(gf.Mgmt+endpoint, nil)
	if err == nil {
		fmt.Printf("%s ok.\n", verb)
		return prettyPrint(body)
	}

	// Daemon unreachable. Persist desired phase so the next start
	// picks it up. Don't error out — this is the documented fallback.
	if !isConnectionRefused(err) {
		// Other errors (auth, malformed response, etc.) are surfaced;
		// the daemon is reachable but something else broke.
		return fmt.Errorf("waired %s: daemon returned: %w", verb, err)
	}
	if writeErr := state.WriteDesiredPhase(gf.StateDir, target); writeErr != nil {
		return fmt.Errorf("waired %s: daemon unreachable AND could not write desired-phase: %w", verb, writeErr)
	}
	fmt.Printf("waired-agent not running — %s persisted; will apply on next start.\n", verb)
	return nil
}

// isConnectionRefused identifies the "daemon is not running" case so
// the pause/resume fallback can kick in. errors.Is(syscall.ECONNREFUSED)
// is the portable path (matches WSAECONNREFUSED 10061 on Windows and
// ECONNREFUSED 111 on Linux); the string fallbacks catch wrapped
// errors that didn't preserve the underlying syscall.Errno (some
// transport layers stringify before returning).
func isConnectionRefused(err error) bool {
	if err == nil {
		return false
	}
	// Already classified by wrapDaemonDialError. Needed for wrapped
	// errors whose cause was stringified (no Errno in the chain).
	if errors.Is(err, errAgentDown) {
		return true
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "connection refused") ||
		strings.Contains(s, "actively refused it") || // Windows wording
		strings.Contains(s, "no such file or directory") ||
		strings.Contains(s, "EOF")
}

// ---------------- waired keygen ----------------

func newKeygenCmd() *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "keygen",
		Short: "Generate a WireGuard key pair (init normally handles this for you).",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if out == "" {
				return errors.New("--out is required")
			}
			priv, pub, err := generateWGKey()
			if err != nil {
				return err
			}
			if err := secrets.SecureDir(filepath.Dir(out)); err != nil {
				return err
			}
			if err := secrets.WriteSecret(out, []byte(base64.StdEncoding.EncodeToString(priv)+"\n")); err != nil {
				return err
			}
			fmt.Println(base64.StdEncoding.EncodeToString(pub))
			return nil
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "private key output path (required)")
	return cmd
}

func generateWGKey() (priv, pub []byte, err error) {
	priv = make([]byte, 32)
	if _, err = rand.Read(priv); err != nil {
		return nil, nil, err
	}
	priv[0] &= 248
	priv[31] &= 127
	priv[31] |= 64
	pubArr, err := curve25519.X25519(priv, curve25519.Basepoint)
	if err != nil {
		return nil, nil, err
	}
	return priv, pubArr, nil
}

// ---------------- helpers ----------------

func httpGet(rawURL string) ([]byte, error) {
	target, client, err := mgmtReadRoute(rawURL, 5*time.Second)
	if err != nil {
		return nil, err
	}
	resp, err := client.Get(target)
	if err != nil {
		return nil, wrapDaemonDialError(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		// The typed error from public_http.go, whose whole purpose is
		// "branch on the status instead of string-matching" — a caller
		// can now tell 404 (a daemon older than the route) from a daemon
		// that did not answer at all (waired-agent#746). The body is
		// carried verbatim rather than through parseMgmtError, so the
		// rendered text stays what every caller of this helper already
		// prints.
		return nil, &mgmtStatusError{StatusCode: resp.StatusCode, Message: string(body)}
	}
	return body, nil
}

// engineWriteTimeout comes from the daemon rather than being transcribed:
// it MUST exceed the daemon's own stop budget, and that budget grew per
// engine when vLLM gained a power axis (waired-agent#945).
var engineWriteTimeout = management.EngineStopClientBudget()

// mgmtPingPath is the one mutating-verb management route that stays on the
// loopback TCP port: POST /waired/v1/ping is a liveness/diagnostic probe,
// not a state change, and the daemon's writeGuard exempts it too
// (internal/management/socket.go).
const mgmtPingPath = "/waired/v1/ping"

// Management writes travel over the local IPC socket (unix socket on
// Linux/macOS, named pipe on Windows) since waired#838: browsers and
// network peers cannot open it, so a cross-site write is structurally
// impossible rather than merely header-checked. --mgmt still governs
// reads; $WAIRED_MGMT_SOCKET overrides the write endpoint. Both are vars
// so tests can redirect writes at an httptest TCP server.
var (
	mgmtWriteBase   = ipcclient.BaseURL
	mgmtWriteClient = func(timeout time.Duration) *http.Client { return ipcclient.NewHTTPClient(timeout) }
	mgmtReadClient  = func(tcpBase string, timeout time.Duration) *http.Client {
		return ipcclient.NewReadClient(tcpBase, timeout)
	}
	// mgmtReadDefaultBase is the one base URL a read may be redirected to
	// the socket for. A var only so the routing test can point it at an
	// httptest server; the predicate it feeds is covered directly by
	// ipcclient.TestSameAuthority.
	mgmtReadDefaultBase = defaultMgmtURL
)

// mgmtReadRoute decides where a management read goes. Production sends it
// over the local IPC socket with a loopback-TCP fallback (waired#836): the
// daemon serves only the compatibility routes on TCP while the socket is
// up, and serves every read there while it is not, so the fallback is what
// finds a daemon that could not bind a socket. Two cases stay on plain TCP:
// a --mgmt the operator pointed elsewhere (a mock daemon on another port, a
// debug tunnel), which must not be quietly redirected to THIS machine's
// socket, and tests, which clear mgmtWriteBase to address httptest servers.
// The URL is returned unchanged: unlike a write, a read needs no rewriting
// because the socket transport ignores the authority it is handed, and
// leaving the loopback URL in place is also what the fallback wants.
func mgmtReadRoute(rawURL string, timeout time.Duration) (target string, client *http.Client, err error) {
	u, perr := url.Parse(rawURL)
	if perr != nil {
		return "", nil, perr
	}
	tcpBase := u.Scheme + "://" + u.Host
	if mgmtWriteBase == "" || !ipcclient.SameAuthority(tcpBase, mgmtReadDefaultBase) {
		return rawURL, &http.Client{Timeout: timeout}, nil
	}
	return rawURL, mgmtReadClient(tcpBase, timeout), nil
}

// readMgmtResponse drains a management response, mapping a transport error
// to wording that names the right endpoint for the transport used.
func readMgmtResponse(resp *http.Response, err error, viaSocket bool) ([]byte, error) {
	if err != nil {
		if viaSocket {
			return nil, ipcclient.WrapDialError(err)
		}
		return nil, wrapDaemonDialError(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		// Same as httpGet above: typed so the status is inspectable,
		// body verbatim so the text does not move (waired-agent#746).
		return nil, &mgmtStatusError{StatusCode: resp.StatusCode, Message: string(out)}
	}
	return out, nil
}

// mgmtWriteRoute decides where a management write goes. Production sends it
// over the local IPC socket (waired#838). Two cases stay on the loopback TCP
// port: the /ping liveness probe, which the daemon's writeGuard also exempts,
// and tests, which clear mgmtWriteBase so they can address httptest servers
// directly (the socket transport has its own coverage).
func mgmtWriteRoute(rawURL string, timeout time.Duration) (target string, client *http.Client, viaSocket bool, err error) {
	u, perr := url.Parse(rawURL)
	if perr != nil {
		return "", nil, false, perr
	}
	if mgmtWriteBase == "" || u.Path == mgmtPingPath {
		return rawURL, &http.Client{Timeout: timeout}, false, nil
	}
	return mgmtWriteBase + u.RequestURI(), mgmtWriteClient(timeout), true, nil
}

// Budgets for management writes. A CLI budget shorter than the daemon's
// own budget for the same endpoint does not make the answer late, it
// destroys it: net/http cancels the request context when the client gives
// up, so the daemon aborts the work AND the reply that would have
// explained it, and the operator is left holding a timeout the CLI
// inflicted on itself. The tray hit this first and fixed it the same way
// — http.Client.Timeout is a hard wall-clock cap that no caller can widen
// with a longer ctx, so the only remedy is a bigger budget for the
// endpoints that need one (waired#316, internal/gui/tray/mgmt.go).
//
// writeTimeout suits the cheap endpoints (a flag flip plus a small file
// write). The other two must outlast a daemon-side budget of 15s:
// pingWriteTimeout the overlay ping client (inference.NewClient in
// cmd/waired-agent/main.go), engineWriteTimeout the hard engine stop
// (engineStopBudget in cmd/waired-agent/engine_control.go, whose doc
// states outright that it is "deliberately larger than any client
// budget"). 20s matches the tray's engine budget rather than inventing a
// second number for the same 15s ceiling.
const (
	writeTimeout     = 10 * time.Second
	pingWriteTimeout = 20 * time.Second
)

func httpPost(rawURL string, body []byte) ([]byte, error) {
	return httpPostWithin(rawURL, body, writeTimeout)
}

// httpPostWithin is httpPost with an explicit budget, for the endpoints
// whose daemon-side work outlasts writeTimeout. Same shape as benchPost
// (cmd/waired/init_benchmark.go), which is the existing precedent for
// handing mgmtWriteRoute a longer budget.
func httpPostWithin(rawURL string, body []byte, timeout time.Duration) ([]byte, error) {
	target, client, viaSocket, err := mgmtWriteRoute(rawURL, timeout)
	if err != nil {
		return nil, err
	}
	resp, perr := client.Post(target, "application/json", bytes.NewReader(body))
	return readMgmtResponse(resp, perr, viaSocket)
}

func httpDelete(rawURL string) ([]byte, error) {
	target, client, viaSocket, err := mgmtWriteRoute(rawURL, 10*time.Second)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodDelete, target, nil)
	if err != nil {
		return nil, err
	}
	resp, derr := client.Do(req)
	return readMgmtResponse(resp, derr, viaSocket)
}

func prettyPrint(body []byte) error {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		fmt.Println(string(body))
		return nil
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// prettyPrintStatus is prettyPrint with §8.5 applied to the peer rows.
//
// `waired status` forwards the daemon's status document, and that document
// carries one entry per peer with a real device_id — including, for a host
// with an active Public Share grant, a stranger's machine. `waired peers
// list` has substituted the grant pseudonym since #739; this command never
// did, because it does not read the rows at all, it prints the whole
// document (waired-agent#809).
//
// The substitution is keyed on the row's `public` flag, so a peer of your
// own renders byte for byte as it did before. It cannot be keyed on
// anything else the row carries: display_id EQUALS device_id for your own
// machines, so a comparison would find no difference to act on.
//
// A body that does not decode prints raw, exactly as prettyPrint has
// always done — that is a daemon fault, and the peer rows are not
// readable either way.
func prettyPrintStatus(body []byte) error {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		fmt.Println(string(body))
		return nil
	}
	scrubStatusPeersForDisplay(v)
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// scrubStatusPeersForDisplay replaces the device_id of every PUBLIC peer
// row in a decoded status document with the identifier a person may see.
//
// It walks the decoded document rather than a typed struct on purpose: the
// command's contract is that it shows what the daemon said, so a field
// this build of the CLI has never heard of must still reach the terminal.
// Decoding into management.Status would silently drop it.
//
// The daemon always fills display_id for a public row — with the grant
// pseudonym, or with the public-machine label naming the grant when there
// is none. An empty one therefore means an agent predating that, and the
// row falls back to the bare label rather than to the id it is here to
// withhold.
func scrubStatusPeersForDisplay(v any) {
	doc, ok := v.(map[string]any)
	if !ok {
		return
	}
	peers, ok := doc["peers"].([]any)
	if !ok {
		return
	}
	for _, entry := range peers {
		row, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if public, _ := row["public"].(bool); !public {
			continue
		}
		display, _ := row["display_id"].(string)
		if display == "" {
			display = inferencemesh.PublicPeerLabel
		}
		row["device_id"] = display
	}
}

// defaultStateDir is the --state-dir default for the daemon-interacting
// subcommands (status / use / runtimes / worker). It must resolve to the
// SAME directory the daemon and `sudo waired init` use, or — as root on a
// .deb/service install — these commands read an empty per-user dir and
// wrongly report "Not enrolled" against a device that is in fact enrolled
// and serving. So it mirrors init's resolution: root on Linux/macOS →
// System (/var/lib/waired or /Library/Application Support/waired),
// everything else → Interactive. $WAIRED_STATE_DIR and an explicit
// --state-dir still override (paths.StateDir honours the env var first).
func defaultStateDir() string {
	return paths.StateDir(initStateDirMode(runtime.GOOS, os.Geteuid(), elevation.IsElevated()))
}

// defaultInitStateDir is the --state-dir default for `waired init`. It
// shares initStateDirMode with defaultStateDir: init run as root on
// Linux/macOS targets the SYSTEM state dir — the same path the service
// (systemd unit / macOS LaunchDaemon) bakes into the daemon's command
// line. Without this, `sudo waired init` would write identity to
// /root/.config/waired (or root's ~/Library) and the daemon never sees
// it: the device enrolls at the Control Plane but the local agent stays
// unenrolled. On Windows the same split runs off elevation instead of
// euid — see initStateDirMode.
func defaultInitStateDir() string {
	return paths.StateDir(initStateDirMode(runtime.GOOS, os.Geteuid(), elevation.IsElevated()))
}

// initStateDirMode is the testable core of defaultInitStateDir: an
// administrator targets the service-owned System dir the daemon reads,
// everyone else gets their per-user one. macOS joined Linux here when its
// agent became a system LaunchDaemon (#520); before that it was a
// per-user LaunchAgent with no root/state split.
//
// Windows needs its own fact (#313). `os.Geteuid()` is -1 there, so a
// euid guard is dead code — an elevated `waired init` resolved
// %AppData%\waired while the daemon reads %ProgramData%\waired, and the
// CLI then found no identity, sent Reauth=false, and reported the
// daemon's idempotent no-op as a protocol error. Every invocation on an
// enrolled Windows device failed that way; the installer's own init only
// worked because it passes --state-dir explicitly.
//
// The old code deferred to "Windows resolves System via the SCM probe",
// which is true of paths.AutoDetect and only of paths.AutoDetect — this
// decision passes Interactive, so the probe never ran.
func initStateDirMode(goos string, euid int, elevated bool) paths.Mode {
	switch goos {
	case "windows":
		if elevated {
			return paths.System
		}
	case "linux", "darwin":
		if euid == 0 {
			return paths.System
		}
	}
	return paths.Interactive
}

// claudeManagedEligibleFor is the testable core of init's
// claudeManagedEligible gate: the managed-settings file lives at a
// machine-wide OS path (root-owned on Linux/macOS,
// %ProgramFiles%\ClaudeCode on Windows), so writing it needs an elevated
// init. elevated comes from elevation.IsElevated() at the call site — NOT
// a bare euid check, which is -1 on Windows and previously excluded it
// entirely even when run as Administrator (waired#749). managedPath is
// empty only on an OS with no managed-settings location, which can't be
// written regardless.
func claudeManagedEligibleFor(elevated bool, managedPath string) bool {
	return elevated && managedPath != ""
}

// residencyLine renders one engine's model residency for `waired status`
// (waired-agent#879).
//
// Before this, the line above it said `ready` whether or not any weights
// were in memory, so the 17-56 s a reload costs (waired-agent#861) had no
// visible cause. "until" is deliberately absolute rather than a countdown:
// an indefinite keep-alive renders as a date centuries out, and a relative
// "in 292 years" reads as a bug where the date reads as the policy.
func residencyLine(resident bool, model, until string, indefinite bool) string {
	if !resident {
		return "no (the next request reloads it)"
	}
	if model == "" {
		return "yes"
	}
	// The product default holds the model with no expiry, and the engine
	// says so by naming a date centuries out. Printing that date is how
	// the default came to render as "until 2318-11-30" — a line that
	// reads as a bug rather than as the thing the operator asked for
	// (waired-agent#910). Say it in the words the rest of the product
	// uses for this state instead.
	if indefinite {
		return model + " (kept until unloaded)"
	}
	if until == "" {
		return model
	}
	return fmt.Sprintf("%s (until %s)", model, until)
}

// firstTokenLine renders the wait before the last answer began, and the
// fastest comparable wait this host has seen, for the line under
// `model loaded:` (waired-agent#912).
//
// It states two measurements and no verdict. The words "cold" and "warm"
// are deliberately absent: naming one needs a threshold, and a fixed
// threshold is wrong on at least one reference host — the 4 B model's
// WARM first token is 1,960 ms and the 35 B-A3B's is 259 ms, so any
// constant that calls 1,960 ms cold is wrong on the one and any constant
// that calls it warm is wrong on the other. What survives the hardware
// differences is the RATIO on a given host, so the host's own best is the
// yardstick and the reader draws the conclusion.
//
// The age is not decoration. `model loaded:` above is a statement about
// right now; a first-token time from an hour ago sitting under it would
// read as a promise about the next request, which it is not. An
// unparseable or absent timestamp drops the clause rather than guessing.
func firstTokenLine(ms uint32, at string, bestMs uint32, now time.Time) string {
	if ms == 0 {
		return ""
	}
	line := firstTokenDuration(ms)
	if then, err := time.Parse(time.RFC3339Nano, at); err == nil && !then.IsZero() && !then.After(now) {
		line += ", " + humanAge(now, then)
	}
	if bestMs > 0 {
		line += fmt.Sprintf(" (fastest seen here: %s)", firstTokenDuration(bestMs))
	}
	return line
}

// firstTokenDuration renders a first-token wait at the precision a person
// reads it at. Sub-second waits are the warm case and differ by hundreds
// of milliseconds, so they keep their millisecond form; anything longer is
// the case where a tenth of a second is noise.
func firstTokenDuration(ms uint32) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return fmt.Sprintf("%.1fs", float64(ms)/1000)
}
