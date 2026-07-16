// Command waired is the Waired CLI. It drives both the local
// waired-agent daemon (status / ping over the Local Management API on
// 127.0.0.1:9476) and the Control Plane during enrollment (`waired init`).
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/curve25519"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/buildflag"
	"github.com/waired-ai/waired-agent/internal/buildinfo"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/identity"
	"github.com/waired-ai/waired-agent/internal/integration/claudemanaged"
	"github.com/waired-ai/waired-agent/internal/platform/elevation"
	"github.com/waired-ai/waired-agent/internal/platform/paths"
	"github.com/waired-ai/waired-agent/internal/platform/secrets"
	"github.com/waired-ai/waired-agent/internal/platform/service"
	"github.com/waired-ai/waired-agent/internal/proxy/legacycleanup"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
	"github.com/waired-ai/waired-agent/internal/setup"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "waired:", friendlyError(err))
		os.Exit(1)
	}
}

// ---------------- waired init ----------------

// defaultControlURL is the production Control Plane `waired init` falls
// back to when the operator passed no --control, set no
// $WAIRED_CONTROL_URL, and the installer recorded none in agent.env. It
// lets a bare `waired init` (or the installer's auto-init) work with no
// flags. Overridable by any of the three higher-priority sources.
const defaultControlURL = "https://app.waired.ai"

// resolveControlURL applies the control-URL precedence: an explicit
// --control / $WAIRED_CONTROL_URL (explicit) wins, then the
// installer-recorded value from agent.env (platformDefault), then the
// baked-in production default. The result still flows through
// normalizeControlURL afterwards.
func resolveControlURL(explicit, platformDefault string) string {
	if explicit != "" {
		return explicit
	}
	if platformDefault != "" {
		return platformDefault
	}
	return defaultControlURL
}

// initFlags holds every `waired init` flag value. The tri-state
// inferenceEnabled / inferenceShare are *bool (nil unless the operator
// passed the flag), matching the old flagBoolPtr semantics.
type initFlags struct {
	control          string
	deviceName       string
	listen           string
	endpoint         string
	noBrowser        bool
	stateDir         string
	bypassMode       bool
	bypassEmail      string
	googleSALogin    bool
	impersonateSA    string
	oidcIDToken      string
	oidcAudience     string
	skipDeploy       bool
	skipIntegration  bool
	gatewayBaseURL   string
	nonInteractive   bool
	startAgent       bool
	noWaitModel      bool
	resetConfig      bool
	inferenceEnabled *bool
	inferenceShare   *bool
	ollamaSource     string
	bundledModelID   string
	mgmtURL          string
	maskPII          bool
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
		"device name to register (default: hostname)")
	f.StringVar(&o.listen, "listen", "127.0.0.1:0",
		"UDP listen address for the WireGuard data plane (host:port; 0 picks a random port)")
	f.StringVar(&o.endpoint, "endpoint", "",
		"public endpoint to advertise to peers (default: udp4:<--listen>)")
	f.BoolVar(&o.noBrowser, "no-browser", false,
		"don't open the browser; print the URL and code instead")
	f.StringVar(&o.stateDir, "state-dir", defaultInitStateDir(),
		"directory for identity / secrets / cache files")
	// Bypass-mode (headless mock-IdP login via /test/complete-login) is a
	// dev/e2e affordance. The prod CLI build (-tags prod) compiles these flags
	// out so they cannot be passed and bypassMode stays false — real browser
	// (or daemon-driven) Google login only.
	if buildflag.AllowBypassFlags {
		f.BoolVar(&o.bypassMode, "bypass-mode", false,
			"non-interactively complete login against a Control Plane started with --bypass-idp; calls /test/complete-login with --bypass-email instead of opening a browser")
		f.StringVar(&o.bypassEmail, "bypass-email", "",
			"email passed to /test/complete-login when --bypass-mode is set (default: <hostname>@test.waired.local)")
	}
	f.BoolVar(&o.googleSALogin, "google-sa-login", false,
		"complete login by presenting a Google service-account ID token to the Control Plane's /v1/login/oidc-grant endpoint (no browser). For hands-free testing against a production-like CP (e.g. dev.waired.net) that has --enable-oidc-grant on. Mints the token via gcloud unless --oidc-id-token is given.")
	f.StringVar(&o.impersonateSA, "impersonate-sa", "",
		"service account to impersonate when minting the ID token for --google-sa-login (gcloud auth print-identity-token --impersonate-service-account). Requires roles/iam.serviceAccountTokenCreator on it.")
	f.StringVar(&o.oidcIDToken, "oidc-id-token", "",
		"explicit Google ID token to present with --google-sa-login; when empty the token is minted via gcloud + --impersonate-sa. Lets CI mint the token out-of-band (e.g. after a Workload Identity Federation auth step).")
	f.StringVar(&o.oidcAudience, "oidc-audience", "",
		"audience (Google OAuth client_id) for the minted ID token under --google-sa-login; when empty it is discovered from {control}/v1/login/oidc-grant/audience.")
	f.BoolVar(&o.skipDeploy, "skip-deploy", false,
		"skip the deploy phase (hardware profile / runtime check / bundled-model plan)")
	f.BoolVar(&o.skipIntegration, "skip-integration", false,
		"skip the coding-agent integration phase (Claude Code / OpenCode auto-config)")
	f.StringVar(&o.gatewayBaseURL, "gateway-base-url", defaultGatewayURL,
		"Local Gateway base URL the integration phase wires into the agents (Claude proxy / OpenCode plugin)")
	f.BoolVar(&o.nonInteractive, "non-interactive", false,
		"skip all interactive prompts; use hardware-derived defaults for inference choices")
	f.BoolVar(&o.startAgent, "start-agent", true,
		"after a fresh enroll, start the registered waired-agent service (systemctl / launchctl / SCM); pass --start-agent=false to skip and start it yourself")
	f.BoolVar(&o.noWaitModel, "no-wait-model", false,
		"don't wait while the AI model downloads after a fresh setup; let it finish in the background (default: wait in the foreground showing progress)")
	f.BoolVar(&o.resetConfig, "reset-config", false,
		"ignore existing agent.json and re-prompt from hardware-derived defaults")
	f.BoolVar(&infEnabled, "inference-enabled", false,
		"answer \"Run AI models on this computer?\" without prompting: --inference-enabled=true / =false")
	f.BoolVar(&infShare, "share-with-mesh", false,
		"answer \"Let your other devices use this computer's AI?\" without prompting: --share-with-mesh=true / =false. The shorter name (vs --inference-share-with-mesh) is intentional: under 'waired init' the 'inference-' prefix is redundant.")
	f.StringVar(&o.ollamaSource, "ollama-source", "",
		"who provides the Ollama engine: \"bundled\" (Waired installs and manages its own) or \"reuse\" (keep using one you installed yourself); empty prompts (default bundled)")
	f.StringVar(&o.bundledModelID, "inference-bundled-model-id", "",
		"pin the bundled model to pre-pull (manifest model_id); empty auto-selects the largest model that fits this host above the coding-quality floor (#517). Combine with --inference-enabled=true to force-install on an under-spec host.")
	f.StringVar(&o.mgmtURL, "mgmt", defaultMgmtURL,
		"Local Management API base URL; when a waired-agent daemon is reachable there, login is driven through the daemon (Tailscale model) instead of enrolling locally")
	f.BoolVar(&o.maskPII, "mask-pii", os.Getenv("WAIRED_PII_MASK") != "",
		"mask personal information (home directory, username, hostname, account email) in init's output — for screenshots and bug reports. Best-effort; env form: WAIRED_PII_MASK=1 (set by the installers' --mask-pii / -MaskPII). Progress rendering falls back to plain lines while masking.")
	return cmd
}

// runInitBody holds the original init logic verbatim. The leading block
// re-aliases the parsed flags (now fields of o) back to the pointer names
// the body uses, so the ~460-line implementation below is unchanged.
func runInitBody(o *initFlags) error {
	control := &o.control
	deviceName := &o.deviceName
	listen := &o.listen
	endpoint := &o.endpoint
	noBrowser := &o.noBrowser
	stateDir := &o.stateDir
	bypassMode := &o.bypassMode
	bypassEmail := &o.bypassEmail
	googleSALogin := &o.googleSALogin
	impersonateSA := &o.impersonateSA
	oidcIDToken := &o.oidcIDToken
	oidcAudience := &o.oidcAudience
	skipDeploy := &o.skipDeploy
	skipIntegration := &o.skipIntegration
	gatewayBaseURL := &o.gatewayBaseURL
	nonInteractive := &o.nonInteractive
	startAgent := &o.startAgent
	noWaitModel := &o.noWaitModel
	resetConfig := &o.resetConfig
	inferenceEnabled := &o.inferenceEnabled
	inferenceShare := &o.inferenceShare
	ollamaSource := &o.ollamaSource
	bundledModelID := &o.bundledModelID
	mgmtURL := &o.mgmtURL

	// Reject a typo'd --ollama-source up front so it errors instead of being
	// silently dropped (enroll falls through to bundled; renew ignores it, #485).
	if err := validateOllamaSourceFlag(*ollamaSource); err != nil {
		return err
	}

	if *googleSALogin {
		if *bypassMode {
			return errors.New("--google-sa-login and --bypass-mode are mutually exclusive")
		}
		if *oidcIDToken == "" && *impersonateSA == "" {
			return errors.New("--google-sa-login requires either --oidc-id-token or --impersonate-sa")
		}
	}

	// Fall back to the installer-configured control URL (e.g. what
	// `install.sh --control <URL>` wrote to /etc/waired/agent.env) when the
	// operator passed neither --control nor $WAIRED_CONTROL_URL, so the
	// common `sudo waired init` (no flag) just works.
	*control = resolveControlURL(*control, platformDefaultControlURL())
	// Normalize the scheme up front (bare "dev.waired.net" -> https://...,
	// loopback -> http://...). Done before the renew comparison below so a
	// scheme-less flag matches the stored (already-normalized) ControlURL
	// instead of tripping a spurious "already enrolled to X" error.
	if *control != "" {
		norm, err := normalizeControlURL(*control)
		if err != nil {
			return err
		}
		*control = norm
	}

	// `waired init` doubles as the re-auth entry point (gcloud-init
	// style). When an existing identity is already on disk we:
	//   - Reuse its ControlURL / DeviceName when the operator didn't
	//     pass new ones explicitly.
	//   - Refuse to silently move the device to a *different* CP — the
	//     operator must run `waired logout` first.
	//   - Confirm before re-running the Google sign-in.
	//   - Skip the deploy + integration phases (auth-only refresh).
	existing, idErr := identity.Load(*stateDir)
	if idErr != nil {
		return fmt.Errorf("load existing identity: %w", idErr)
	}
	renewing := existing != nil
	if renewing {
		if existing.ControlURL != "" && *control != "" && existing.ControlURL != *control {
			return fmt.Errorf(
				"already enrolled to %s — run `waired logout` first to switch control planes (requested %s)",
				existing.ControlURL, *control)
		}
		if *control == "" {
			*control = existing.ControlURL
		}
		if *deviceName == "" {
			*deviceName = existing.DeviceName
			if *deviceName == "" {
				*deviceName = existing.DeviceID
			}
		}
		// Renew is auth-only; whatever hardware / integration state is
		// already on disk stays untouched.
		*skipDeploy = true
		*skipIntegration = true
		if !confirmRenew(os.Stdin, os.Stdout, existing, *bypassMode || *googleSALogin, *nonInteractive) {
			fmt.Println("Nothing changed.")
			return nil
		}
	}

	if *control == "" {
		return errors.New("--control or WAIRED_CONTROL_URL is required")
	}

	if *deviceName == "" {
		host, _ := os.Hostname()
		*deviceName = host
	}

	// Friendly intro for a fresh interactive first-run (both the daemon and
	// standalone journeys below). Skipped on re-auth (quieter) and in
	// bypass/CI headless runs. Renders a framed banner on a capable TTY, a
	// single plain line otherwise.
	if !renewing && !*bypassMode {
		welcomeBanner(os.Stdout)
	}

	// Thin-client path: when a waired-agent daemon is already running,
	// drive the daemon-owned login MGMT API rather than enrolling
	// locally. The daemon owns the runtime + state dir and brings the
	// tunnel up live (the Tailscale model). Bypass-idp and re-auth keep
	// the standalone path below: the daemon login does not implement the
	// mock-complete-login shortcut or token-only refresh, and a no-daemon
	// host (headless / CI / pre-service-start) naturally falls through.
	if !*bypassMode && !*googleSALogin && !renewing && daemonReachable(*mgmtURL) {
		fmt.Println("waired-agent is running; signing in via the daemon (no local enrollment).")
		return runInitViaDaemon(*mgmtURL, *control, *deviceName, *noBrowser, *nonInteractive,
			*skipIntegration, *gatewayBaseURL)
	}

	listenAddr, err := chooseListenAddr(*listen)
	if err != nil {
		return err
	}
	if *endpoint == "" {
		*endpoint = "udp4:" + listenAddr.String()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	steps := initStepLabels(renewing)

	var httpClient *http.Client
	var onLogin func(loginURL, userCode string)
	if *bypassMode {
		httpClient = bypassHTTPClient(ctx, *control)
		email := *bypassEmail
		if email == "" {
			host, _ := os.Hostname()
			email = host + "@test.waired.local"
		}
		fmt.Printf("%s %s\n", steps.signIn, bold("Sign in (bypass-idp mode)"))
		onLogin = func(loginURL, _ string) {
			// Headless completion: retry transient failures, and on a
			// permanent one cancel the init ctx so the poll loop dies
			// immediately instead of burning its 10-minute budget on a
			// session that will never authorize (#352). The supervisor
			// (systemd / the bootstrap wrapper) restarts us with a
			// fresh session within seconds.
			sessionID := lastPathSegment(loginURL)
			if sessionID == "" {
				fmt.Fprintf(os.Stderr, "bypass-mode: cannot parse session id from %q — aborting init\n", loginURL)
				cancel()
				return
			}
			err := retryHeadlessCompletion(ctx, "bypass-mode", func(ctx context.Context) error {
				return bypassCompleteLogin(ctx, httpClient, *control, sessionID, email)
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "bypass-mode: complete-login failed: %v — aborting init so the supervisor retries with a fresh session\n", err)
				cancel()
				return
			}
			fmt.Printf("Mock-completed login as %s (session=%s)\n", email, sessionID)
		}
	} else if *googleSALogin {
		// dev.waired.net is publicly reachable, so a plain client suffices
		// (no Cloud Run IAM token transport). The CP authenticates us via
		// the Google-signed SA id_token in the grant body, not the HTTP
		// layer.
		httpClient = &http.Client{Timeout: 30 * time.Second}
		fmt.Printf("%s %s\n", steps.signIn, bold("Sign in (Google service account, no browser)"))
		onLogin = func(loginURL, _ string) {
			// Same headless retry + fail-fast semantics as bypass-mode
			// above; the token mint is inside the retried closure so a
			// transient gcloud / metadata hiccup also self-heals.
			sessionID := lastPathSegment(loginURL)
			if sessionID == "" {
				fmt.Fprintf(os.Stderr, "google-sa-login: cannot parse session id from %q — aborting init\n", loginURL)
				cancel()
				return
			}
			err := retryHeadlessCompletion(ctx, "google-sa-login", func(ctx context.Context) error {
				token, err := obtainSAIDToken(ctx, httpClient, *control, *oidcIDToken, *impersonateSA, *oidcAudience)
				if err != nil {
					return fmt.Errorf("obtain id_token: %w", err)
				}
				return oidcGrantCompleteLogin(ctx, httpClient, *control, sessionID, token)
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "google-sa-login: completion failed: %v — aborting init so the supervisor retries with a fresh session\n", err)
				cancel()
				return
			}
			fmt.Printf("Completed login via OIDC grant (session=%s)\n", sessionID)
		}
	} else {
		fmt.Printf("%s %s\n", steps.signIn, bold("Sign in"))
		onLogin = func(loginURL, userCode string) {
			if *noBrowser {
				fmt.Printf("\nOpen this URL to sign in:\n  %s\n\nCode: %s\n\n%s Waiting for sign-in to complete…\n",
					loginURL, userCode, emo("⏳", "..."))
				return
			}
			if err := openBrowser(loginURL); err != nil {
				fmt.Printf("%s Couldn't open a browser automatically. Open this URL to sign in:\n  %s\n",
					emo("⚠️", "!"), loginURL)
			} else {
				fmt.Printf("%s Opened your browser to sign in. If nothing appeared, open this URL:\n  %s\n",
					emo("🌐", ">>"), loginURL)
			}
			fmt.Printf("%s Waiting for sign-in to complete…\n", emo("⏳", "..."))
		}
	}

	// onLoginComplete confirms the browser sign-in succeeded the moment
	// the login session is authorized — before device registration — so
	// the operator gets explicit feedback after the "waiting" line instead
	// of a silent gap.
	onLoginComplete := func(email, _ string) {
		if email != "" {
			fmt.Printf("%s Signed in as %s\n", emo("✅", "[ok]"), email)
		} else {
			fmt.Printf("%s Signed in\n", emo("✅", "[ok]"))
		}
	}

	homeDir, _ := os.UserHomeDir()

	// Coding-agent integration targets the invoking user under sudo —
	// detection and the eventual apply must look at their home, not
	// /root. The consent answer (default Yes) is cached here by the
	// configureInference closure, which asks it alongside the other
	// questions, and is consumed by both the ConfigureIntegration hook
	// and the post-Init sudo hop below.
	integSudoUser, integIsSudo := invokingSudoUser()
	integTargetHome := homeDir
	if integIsSudo {
		if h, herr := sudoUserHome(integSudoUser); herr == nil {
			integTargetHome = h
		}
	}
	integConsent := false
	// ollamaSourceChanged: set by configureInference when an explicit
	// --ollama-source rewrote agent.json on the renew path (#485). Read after
	// setup.Init to fix state-dir ownership + print the restart hint.
	ollamaSourceChanged := false
	// claudeManagedEligible: whether `waired init` will write the system-wide
	// Claude Code managed settings (ANTHROPIC_BASE_URL -> local gateway, no
	// credential) and sweep up any retired MITM proxy artifacts (#488).
	claudeManagedEligible := claudeManagedEligibleFor(elevation.IsElevated(), claudemanaged.Path())

	// Load any existing agent.json so the inference prompt's defaults
	// reflect prior answers. The actual prompt runs in the
	// configureInference hook below — AFTER enroll — so the sign-in /
	// browser step is what the operator sees first. The prompt + agent.json
	// write live in cmd/waired so the setup/ package stays stdin-free.
	if err := os.MkdirAll(*stateDir, 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	cfgRoot := agentconfig.Defaults()
	hasExisting := false
	if !*resetConfig {
		cfgPath := agentconfig.JSONPathFor(*stateDir)
		if _, statErr := os.Stat(cfgPath); statErr == nil {
			if err := cfgRoot.MergeJSON(cfgPath); err != nil {
				fmt.Fprintf(os.Stderr, "warn: existing agent.json unreadable (%v); using defaults\n", err)
			} else {
				hasExisting = true
			}
		}
	}
	// configureInference runs INSIDE setup.Init, after Enroll (sign-in +
	// device registration) succeeds and before Deploy. Running the prompt
	// here — rather than before setup.Init — means the browser/login step
	// is what the operator sees first; the inference questions only appear
	// once they are signed in. It also prints the post-enroll status lines
	// from the just-produced enroll result.
	configureInference := func(ctx context.Context, enroll *setup.EnrollResult) (agentconfig.InferenceConfig, error) {
		fmt.Printf("%s %s\nDevice: %s\nNetwork: %s\nStatus: approved\nOverlay IP: %s\n",
			steps.register, bold("Register this device"), *deviceName, enroll.NetworkName, enroll.OverlayIP)
		if renewing {
			// Auth-only refresh: no inference prompt; reuse the on-disk config.
			// Exception (#485): an explicit --ollama-source is still applied +
			// persisted so the engine source can be switched on an already-enrolled
			// device without hand-editing agent.json (which risks root-owned state,
			// the sibling #484 pitfall).
			if next, changed := renewOllamaSourceChange(cfgRoot.Inference.OllamaSource, *ollamaSource); changed {
				from := effectiveOllamaSource(cfgRoot.Inference.OllamaSource)
				cfgRoot.Inference.OllamaSource = next
				if err := cfgRoot.Save(agentconfig.JSONPathFor(*stateDir)); err != nil {
					fmt.Fprintf(os.Stderr, "warn: failed to write agent.json: %v\n", err)
				} else {
					ollamaSourceChanged = true
					fmt.Printf("Ollama source: %s → %s\n", from, next)
				}
			}
			fmt.Printf("%s %s\n", steps.persist, bold("Refresh this device's sign-in — done"))
			return cfgRoot.Inference, nil
		}
		fmt.Printf("%s %s\n", steps.persist, bold("Save this device's settings — done"))

		prof := hardware.NewProfiler("").Profile(ctx)
		fmt.Printf("%s %s\n", steps.inference, bold("Set up AI on this computer"))
		choice := promptInference(os.Stdin, os.Stdout,
			cfgRoot.Inference, hasExisting, prof,
			*inferenceEnabled, *inferenceShare,
			*nonInteractive)
		cfgRoot.Inference.Enabled = choice.Enabled
		cfgRoot.Inference.ShareWithMesh = choice.ShareWithMesh
		// Ollama engine source (#188). Only relevant when inference is on;
		// default is always bundled. Detect an existing ollama and let the
		// operator opt into reusing it (or honour --ollama-source).
		if choice.Enabled {
			det := setup.DetectOllama(ctx)
			cfgRoot.Inference.OllamaSource = promptOllamaSource(
				os.Stdin, os.Stdout, det, *ollamaSource, *nonInteractive)
			// #517: pick the bundled model from the detected hardware (largest
			// fitting model above the coding-quality floor), disable LOCAL
			// inference when the host is under-spec (still a gateway/relay), and
			// pre-flight free disk. Mutates cfgRoot.Inference; never aborts init.
			applyBundledModelSelection(&cfgRoot, prof, det,
				*stateDir, homeDir, *bundledModelID, *inferenceEnabled,
				*nonInteractive, os.Stdin, os.Stdout)
			// Install the bundled engine NOW — after every answer is in,
			// before Deploy's model pre-pull (which needs an engine). The
			// installers no longer pre-install Ollama; init owns both the
			// decision and the install. Re-check Enabled: an under-spec
			// host may have just been demoted to gateway/relay-only by
			// applyBundledModelSelection.
			if cfgRoot.Inference.Enabled {
				ensureBundledEngine(ctx, os.Stdout, det,
					cfgRoot.Inference.OllamaSource, *stateDir)
			}
		}
		if err := cfgRoot.Save(agentconfig.JSONPathFor(*stateDir)); err != nil {
			// identity-side state is already enrolled; do not abort init for
			// a config-write failure — the operator can re-run or edit the
			// file manually.
			fmt.Fprintf(os.Stderr, "warn: failed to write agent.json: %v\n", err)
		}
		// Coding-agent integration consent (default Yes) — asked here so
		// every question lands up front, before the model download. The
		// answer is consumed by the ConfigureIntegration hook below (and
		// by the sudo hop after Init).
		if !*skipIntegration {
			integConsent = promptIntegrationConsent(os.Stdin, os.Stdout, integrationConsentInput{
				StepLabel:      steps.integration,
				Detections:     detectIntegrationAgents(ctx, integTargetHome),
				NonInteractive: *nonInteractive,
				SudoTarget:     integSudoUser,
				ClaudeManaged:  claudeManagedEligible,
			})
		}
		return cfgRoot.Inference, nil
	}

	res, err := setup.Init(ctx, setup.InitOptions{
		ControlURL:         *control,
		DeviceName:         *deviceName,
		Endpoint:           *endpoint,
		StateDir:           *stateDir,
		HTTPClient:         setup.EnrollHTTPClient{HC: httpClient},
		OnLoginURL:         onLogin,
		OnLoginComplete:    onLoginComplete,
		ClientVersion:      buildinfo.Version,
		Inference:          cfgRoot.Inference,
		ConfigureInference: configureInference,
		HomeDir:            homeDir,
		GatewayBaseURL:     *gatewayBaseURL,
		NonInteractive:     *nonInteractive,
		SkipDeploy:         *skipDeploy,
		SkipIntegration:    *skipIntegration,
		WiredBinary:        wairedBinaryPath(),
		ConfigureIntegration: func(context.Context) (setup.IntegrationDecision, error) {
			if !integConsent {
				return setup.IntegrationDecision{Skip: true}, nil
			}
			if integIsSudo {
				// In-process apply would land in /root; the per-user
				// hop after Init applies it for the invoking user.
				return setup.IntegrationDecision{Skip: true}, nil
			}
			return setup.IntegrationDecision{Force: true}, nil
		},
		AllowMutations:     !*skipDeploy,
		DeployProgressSink: cliPullProgressSink(os.Stdout, isTerminal(os.Stdout)),
	})
	if err != nil {
		// The most common first-run failure is an unreachable Control Plane
		// (e.g. a control host that is down or unreachable, or a typo'd
		// --control). Surface an actionable hint before the raw error
		// so the operator knows it was the sign-in step that failed and how to
		// proceed. Still return the error so the exit code stays non-zero and
		// the installer's "finish later" path fires.
		if _, ok := errors.AsType[net.Error](err); ok {
			fmt.Printf("%s Sign-in failed: couldn't reach the Control Plane at %s.\n"+
				"   It may not be available yet — pass --control <url> for a different one,\n"+
				"   or finish later: %s.\n", emo("⚠️", "!"), *control, elevationHintFor(runtime.GOOS, "waired init"))
		}
		return err
	}

	if res.Deploy != nil {
		fmt.Println()
		fmt.Println(rule())
		fmt.Println(bold("Deploy plan:"))
		if res.Deploy.OllamaInstalled {
			fmt.Printf("  ollama:        %s\n", res.Deploy.OllamaPath)
		} else {
			fmt.Println("  ollama:        not installed yet (Waired adds it when needed)")
		}
		if res.Deploy.BundledModel != "" {
			fmt.Printf("  AI model:      %s (downloaded in the background when needed)\n", res.Deploy.BundledModel)
		}
		if res.Deploy.GatewayPort != 0 {
			fmt.Printf("  gateway:       http://127.0.0.1:%d (started by waired-agent)\n", res.Deploy.GatewayPort)
		}
		for _, n := range res.Deploy.Notes {
			fmt.Printf("  note:          %s\n", n)
		}
	}

	if res.Integration != nil {
		fmt.Println()
		fmt.Println(bold("Coding-agent integration:"))
		for _, ar := range res.Integration.Agents {
			switch {
			case ar.Skipped:
				fmt.Printf("  %-12s skipped (not detected — run `waired link --force` to set up anyway)\n", ar.Agent+":")
			case ar.Err != nil:
				fmt.Printf("  %-12s FAILED: %v\n", ar.Agent+":", ar.Err)
			case ar.Applied:
				fmt.Printf("  %-12s configured\n", ar.Agent+":")
			}
		}
		if integConsent {
			// Print per-agent next-steps (skills + opencode + the proxy
			// pointer). Nothing is written here — Claude routing on Linux is
			// the transparent proxy, set up separately below.
			printSetupHelper("all", helperPrintOptions{
				HomeDir:     homeDir,
				WiredBinary: wairedBinaryPath(),
				Interactive: false,
			}, os.Stdout, os.Stdin)
		}
	}

	// Claude Code managed settings (#488): point ANTHROPIC_BASE_URL at waired's
	// local gateway with NO credential, so the claude.ai subscription and
	// auto-mode are preserved. Also sweep up any retired MITM proxy artifacts a
	// previous install left (a stale api.anthropic.com hosts redirect would
	// break the new gateway's passthrough). Best-effort — enrollment is already
	// durable, so a failure warns but never fails init.
	//
	// Interactive installs no longer flip the route here (waired#772): the
	// early integration consent covered installing artifacts, and the actual
	// routing question is asked after the model download + benchmark below,
	// when the local stack can actually serve. Non-interactive keeps the
	// single-consent immediate flip.
	claudeRouted := false
	if integConsent && claudeManagedEligible && !renewing && *nonInteractive {
		fmt.Printf("\n%s %s\n", emo("🔌", "*"), bold("Configuring Claude Code integration (managed settings)…"))
		legacycleanup.Run(*stateDir, stderrLogger())
		baseURL := fmt.Sprintf("http://127.0.0.1:%d", cfgRoot.Inference.ClaudeGatewayPort)
		if path, err := claudemanaged.Write(baseURL); err != nil {
			fmt.Fprintf(os.Stderr,
				"warn: writing Claude Code managed settings failed (%v); %s\n",
				err, elevationHintFor(runtime.GOOS, "waired claude enable"))
		} else {
			claudeRouted = true
			fmt.Printf("  %s → ANTHROPIC_BASE_URL=%s (no credential; subscription/auto-mode preserved)\n", path, baseURL)
			// Also add the routing statusline segment, with the same
			// ask-before-wrapping flow as `waired claude enable`. This branch
			// is --non-interactive only (installer --yes), which must never
			// block on the y/N read even though install.sh reattaches stdin
			// to /dev/tty — guidance only.
			installStatuslineForInvoker(false, false)
		}
	}

	// Under sudo the per-user integration runs out-of-process as the
	// invoking user (the in-process phase was skipped above). Warn-only:
	// enrollment is already durable, so a hop failure must not fail init.
	if integConsent && integIsSudo && !renewing {
		fmt.Printf("\n%s %s\n", emo("🔌", "*"),
			bold(fmt.Sprintf("Setting up coding-agent integration for user %q…", integSudoUser)))
		if err := runLinkAllAsUser(ctx, integSudoUser,
			linkAllChildArgs(*gatewayBaseURL), os.Stdout, os.Stderr); err != nil {
			fmt.Fprintf(os.Stderr,
				"warn: coding-agent integration for %s failed (%v); re-run later as that user: waired link --force all\n",
				integSudoUser, err)
		}
	}

	// Linux desktop tray (#493): on GNOME the waired-tray SNI icon won't render
	// without an AppIndicator host extension. On a fresh init (root, so the
	// dpkg lock is free) detect GNOME and auto-install + enable one. Best-effort
	// and no-op on non-GNOME / already-present / non-Linux — never fails init.
	if !renewing {
		ensureTrayHostExtension(os.Stdout)
	}

	if res.Enroll != nil {
		fmt.Printf("\n%s %s\nLogged in as: %s\nDevice ID:    %s\n",
			steps.done, bold("Device enrolled"), res.Enroll.AccountEmail, res.Enroll.DeviceID)
		// Both fresh enroll and renew/re-auth (re)write identity / secrets /
		// agent.json under sudo (root-owned); on a renew the 0600 secrets are
		// refreshed too. Hand the whole tree back to the unprivileged service
		// user so the daemon can read its identity and stays enrolled —
		// otherwise it crash-loops on a permission-denied access token (#484).
		// No-op when not root / not installed / on macOS+Windows.
		handStateToServiceUser(*stateDir)
		if renewing {
			if ollamaSourceChanged {
				fmt.Println("Restart waired-agent to apply the new Ollama source (e.g. `sudo systemctl restart waired-agent`).")
			}
			fmt.Println("Tokens and device certificate were refreshed; the running waired-agent will pick them up on the next refresh cycle.")
		} else {
			if *startAgent && !*bypassMode {
				// The unit no longer crash-loops once an identity exists —
				// bring it up so the operator doesn't have to. Best-effort;
				// falls back to a manual hint on any failure.
				startAgentServiceBestEffort(os.Stdout)
			} else {
				fmt.Println("Start the agent: `waired-agent`  (it will pick up the identity from " + *stateDir + ")")
			}
		}
	}

	// postSC is the single stdin scanner for every post-enroll prompt (the
	// benchmark gate, its model-switch questions, and the routing
	// confirmation below) — layering a second bufio reader over os.Stdin
	// would eat buffered input between prompts.
	postSC := bufio.NewScanner(os.Stdin)

	// #133: on a fresh install with inference enabled, offer an end-of-init
	// benchmark (which doubles as the "local inference works" smoke test)
	// and, if the host can't sustain the auto-picked model, offer a lighter
	// one. init started the daemon just above, so offerBenchmark waits for
	// it to come up before probing.
	var bench benchmarkOutcome
	if !renewing && cfgRoot.Inference.Enabled && !*bypassMode {
		// #490: on a fresh bundled enroll the agent pulls the multi-GB model
		// in the background after we start it above, so without this the shell
		// would return on a half-downloaded model. Block in the foreground with
		// percentage progress until it's ready — which also gives the benchmark
		// below a ready engine/model so the #133 auto-fallback actually runs
		// (#489). Skipped in reuse mode (Deploy already pre-pulled), when the
		// agent wasn't started here, or under --no-wait-model (background pull).
		bundled := cfgRoot.Inference.OllamaSource != agentconfig.OllamaSourceReuse
		if *startAgent && bundled && cfgRoot.Inference.PullOnStartup && !*noWaitModel {
			waitForBundledModel(*mgmtURL, os.Stdout, isTerminal(os.Stdout))
		}
		bench = offerBenchmark(*mgmtURL, *nonInteractive, os.Stdout, postSC, isTerminal(os.Stdout))
	}

	// The deferred routing question (waired#772): now that the engine setup,
	// model download, and benchmark are done, the local stack can actually
	// serve — ask whether to route Claude Code inference through it. "No"
	// leaves the integration artifacts installed and Claude traffic on the
	// real Anthropic API. Interactive only; the non-interactive flip already
	// happened above.
	if integConsent && claudeManagedEligible && !renewing && !*nonInteractive {
		baseURL := fmt.Sprintf("http://127.0.0.1:%d", cfgRoot.Inference.ClaudeGatewayPort)
		claudeRouted = promptClaudeRouting(os.Stdout, postSC, baseURL, *stateDir)
		if claudeRouted {
			// Same ask-before-wrapping statusline flow as `waired claude
			// enable`; init keeps the TTY, so prompting is allowed here.
			installStatuslineForInvoker(false, true)
		}
	}

	// waired#749: when the operator consented to integration but this init
	// isn't elevated, the machine-wide managed-settings write above was
	// skipped (claudeManagedEligible=false). Say so honestly with the
	// platform-correct elevation hint instead of the old silent skip, which
	// left the consent copy looking like routing had been enabled. Covers
	// non-elevated Windows and non-sudo Linux/darwin alike; suppressed on an
	// OS with no managed-settings path (claudemanaged.Path()=="").
	if integConsent && !claudeManagedEligible && !renewing && claudemanaged.Path() != "" {
		fmt.Printf("\n%s Claude Code request routing needs elevation — %s to route Claude through Waired.\n",
			emo("🔌", "*"), elevationHintFor(runtime.GOOS, "waired claude enable"))
	}

	// The true end: a framed success summary printed only after the (optional)
	// model download + benchmark have finished, so it — not the mid-flow
	// "Device enrolled" line — is the "everything completed" marker. Fresh
	// standalone enroll only; renew keeps its terser tokens-refreshed ending
	// and bypass/CI runs stay quiet.
	if !renewing && !*bypassMode && res.Enroll != nil {
		printInitSuccessBox(res, cfgRoot.Inference, claudeRouted, *deviceName, bench)
	}
	return nil
}

// printInitSuccessBox renders the final "Waired is ready" summary. It reflects
// the post-selection inference config (applyBundledModelSelection may have
// disabled local inference or chosen the bundled model) and folds in the
// benchmark throughput when one was measured. Best-effort presentation only.
func printInitSuccessBox(res *setup.InitResult, inf agentconfig.InferenceConfig, claudeRouted bool, deviceName string, bench benchmarkOutcome) {
	var lines []string
	add := func(label, val string) { lines = append(lines, fmt.Sprintf("%-9s %s", label, val)) }

	add("Account", res.Enroll.AccountEmail)
	dev := deviceName
	if res.Enroll.DeviceID != "" {
		dev = deviceName + "  " + dim(res.Enroll.DeviceID)
	}
	add("Device", dev)
	if res.Enroll.OverlayIP != "" {
		add("Network", fmt.Sprintf("%s  overlay %s", res.Enroll.NetworkName, res.Enroll.OverlayIP))
	}

	if inf.Enabled {
		model := inf.BundledModelID
		if model == "" {
			model = "(auto)"
		}
		if bench.Measured {
			add("Model", fmt.Sprintf("%s  (%s)", model, green(fmt.Sprintf("%.0f tok/s", bench.Tokps))))
		} else {
			add("Model", model)
		}
		add("Gateway", cyan(fmt.Sprintf("http://127.0.0.1:%d", inf.LocalGatewayPort)))
		if claudeRouted {
			add("Claude", cyan(fmt.Sprintf("http://127.0.0.1:%d", inf.ClaudeGatewayPort))+" (managed settings)")
		}
		lines = append(lines, "")
		lines = append(lines, dim("Point your coding agent at Waired and start building."))
	} else {
		lines = append(lines, dim("Local inference disabled — this device acts as a mesh gateway/relay."))
	}
	lines = append(lines, dim("Check with `waired status`; troubleshoot with `waired doctor`."))

	box(os.Stdout, emo("🎉", "*"), "Waired is ready — everything completed successfully!", lines)
}

// chooseListenAddr expands "127.0.0.1:0" by binding briefly to a real
// port so we can report a stable host:port at enrollment time. The
// agent will re-bind to the same port at startup.
func chooseListenAddr(listen string) (netip.AddrPort, error) {
	host, port, err := splitHostPort(listen)
	if err != nil {
		return netip.AddrPort{}, err
	}
	if port == "0" {
		// Bind to a UDP socket briefly to pick a free port.
		l, err := newReservedUDPPort(host)
		if err != nil {
			return netip.AddrPort{}, err
		}
		port = fmt.Sprintf("%d", l)
	}
	return netip.ParseAddrPort(host + ":" + port)
}

// normalizeControlURL canonicalises a Control Plane URL the operator
// supplied via --control, $WAIRED_CONTROL_URL, or /etc/waired/agent.env.
// A bare host like "dev.waired.net" (no scheme) is the natural thing to
// type, but net/http rejects it ("unsupported protocol scheme \"\"")
// once it reaches the enroll POST, so we prepend a scheme here: https for
// remote hosts, http for loopback (matching the --control example
// http://127.0.0.1:9477). An empty input is returned unchanged so the
// caller's "required" check still fires. Non-http(s) schemes and
// host-less inputs are rejected with a clear message.
func normalizeControlURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	if !strings.Contains(s, "://") {
		host := s
		if i := strings.IndexAny(host, "/?#"); i >= 0 {
			host = host[:i]
		}
		if isLoopbackHost(host) {
			s = "http://" + s
		} else {
			s = "https://" + s
		}
	}
	u, err := url.ParseRequestURI(s)
	if err != nil {
		return "", fmt.Errorf("invalid control URL %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("invalid control URL %q: scheme %q is not http or https", raw, u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("invalid control URL %q: missing host", raw)
	}
	return strings.TrimRight(s, "/"), nil
}

// Service-control seams, overridable in tests so startAgentServiceBestEffort
// can be exercised without a real systemd/launchd/SCM.
var (
	serviceInstalledFn = service.Installed
	serviceStartFn     = service.StartInstalled
	serviceStartHintFn = service.StartHint
)

// startAgentServiceBestEffort brings the agent daemon up right after a
// successful fresh enroll, so the operator does not have to run
// `systemctl start` / `launchctl kickstart` by hand. It is best-effort:
// any failure (or a raw-binary install with no registered service) just
// prints the manual command and never fails init. Callers must gate this
// to the fresh standalone enroll path (not renew, not bypass, not the
// thin-client path where a daemon is already running).
func startAgentServiceBestEffort(out io.Writer) {
	hint := serviceStartHintFn()
	if !serviceInstalledFn() {
		if hint != "" {
			_, _ = fmt.Fprintf(out, "Start the agent: %s\n", hint)
		}
		return
	}
	if err := serviceStartFn(); err != nil {
		if hint != "" {
			_, _ = fmt.Fprintf(out, "warning: could not auto-start waired-agent (%v); start it manually: %s\n", err, hint)
		} else {
			_, _ = fmt.Fprintf(out, "warning: could not auto-start waired-agent: %v\n", err)
		}
		return
	}
	_, _ = fmt.Fprintln(out, "Started waired-agent.")
}

// isLoopbackHost reports whether host (a hostname or host:port, no
// scheme) is loopback. Loopback control planes are the local-dev default
// and speak plain http.
func isLoopbackHost(host string) bool {
	h := host
	if strings.HasPrefix(h, "[") { // [::1]:port
		if i := strings.Index(h, "]"); i >= 0 {
			h = h[1:i]
		}
	} else if i := strings.LastIndex(h, ":"); i >= 0 && !strings.Contains(h[:i], ":") {
		h = h[:i] // host:port (single colon → not a bare IPv6)
	}
	if strings.EqualFold(h, "localhost") {
		return true
	}
	if addr, err := netip.ParseAddr(h); err == nil {
		return addr.IsLoopback()
	}
	return false
}

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
	fmt.Println("Account:    ", id.AccountEmail)
	fmt.Println("Network:    ", id.NetworkName, "("+id.NetworkID+")")
	fmt.Println("Device:     ", id.DeviceID)
	fmt.Println("Overlay IP: ", id.OverlayIP)
	fmt.Println("Endpoint:   ", id.Endpoint)
	fmt.Println("Control:    ", id.ControlURL)
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
	if err := prettyPrint(body); err != nil {
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
	}
	if err := json.Unmarshal(body, &s); err != nil {
		return
	}
	fmt.Println()
	fmt.Println("Inference:")
	fmt.Printf("  state:          %s\n", stateOrDashStr(s.SubsystemState))
	parts := []string{}
	warnings := []string{}
	for name, r := range s.Runtimes {
		if !r.Installed {
			continue
		}
		// Prefer the live (serving) version over the binary probe; in
		// borrowed/adopted modes only the former tells the truth.
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
	for _, w := range warnings {
		fmt.Printf("  ⚠ %s\n", w)
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
			resp, err := httpPost(mgmt+"/waired/v1/ping", body)
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

func httpGet(url string) ([]byte, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, wrapDaemonDialError(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, body)
	}
	return body, nil
}

func httpPost(url string, body []byte) ([]byte, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, wrapDaemonDialError(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, out)
	}
	return out, nil
}

func httpDelete(url string) ([]byte, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, wrapDaemonDialError(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, out)
	}
	return out, nil
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

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Start()
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
	return paths.StateDir(initStateDirMode(runtime.GOOS, os.Geteuid()))
}

// defaultInitStateDir is the --state-dir default for `waired init`. It
// shares initStateDirMode with defaultStateDir: init run as root on
// Linux/macOS targets the SYSTEM state dir — the same path the service
// (systemd unit / macOS LaunchDaemon) bakes into the daemon's command
// line. Without this, `sudo waired init` would write identity to
// /root/.config/waired (or root's ~/Library) and the daemon never sees
// it: the device enrolls at the Control Plane but the local agent stays
// unenrolled. os.Geteuid() returns -1 on Windows, so the guard is a
// no-op there (Windows resolves System via the SCM probe instead).
func defaultInitStateDir() string {
	return paths.StateDir(initStateDirMode(runtime.GOOS, os.Geteuid()))
}

// initStateDirMode is the testable core of defaultInitStateDir: root on
// Linux or macOS → System (the service-owned dir the root daemon reads),
// everything else → Interactive. macOS joined Linux here when its agent
// became a system LaunchDaemon (#520); before that it was a per-user
// LaunchAgent with no root/state split.
func initStateDirMode(goos string, euid int) paths.Mode {
	if (goos == "linux" || goos == "darwin") && euid == 0 {
		return paths.System
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

// splitHostPort + newReservedUDPPort live in helpers.go to keep main.go
// readable; both are tiny.
