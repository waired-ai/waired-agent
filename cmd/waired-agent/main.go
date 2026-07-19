// Command waired-agent is the Waired daemon. It loads the identity left
// behind by `waired init` from --state-dir, brings up a userspace
// WireGuard engine with the device's Node Key, subscribes to the Control
// Plane's Network Map stream, and keeps the WG peer set in sync with
// what the CP says the network looks like.
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/controlclient"
	"github.com/waired-ai/waired-agent/internal/devicekeys"
	"github.com/waired-ai/waired-agent/internal/gateway"
	"github.com/waired-ai/waired-agent/internal/gcptoken"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/identity"
	"github.com/waired-ai/waired-agent/internal/inference"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/integration/openclaw"
	"github.com/waired-ai/waired-agent/internal/integration/opencode"
	"github.com/waired-ai/waired-agent/internal/management"
	disco "github.com/waired-ai/waired-agent/internal/network/disco"
	"github.com/waired-ai/waired-agent/internal/network/netif"
	"github.com/waired-ai/waired-agent/internal/network/wgnet"
	"github.com/waired-ai/waired-agent/internal/observability"
	"github.com/waired-ai/waired-agent/internal/platform/logsink"
	"github.com/waired-ai/waired-agent/internal/platform/paths"
	"github.com/waired-ai/waired-agent/internal/platform/service"
	relayclient "github.com/waired-ai/waired-agent/internal/relay/client"
	"github.com/waired-ai/waired-agent/internal/router"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/internal/runtime/peer"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
	"github.com/waired-ai/waired-agent/internal/setup"
	"github.com/waired-ai/waired-agent/internal/testharness"
	"github.com/waired-ai/waired-agent/proto/signer"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const inferenceServicePort uint16 = 9474

// restartRequestedExitCode is the agent's "restart me" exit status.
// The packaged systemd unit pairs it with RestartForceExitStatus=17
// (and SuccessExitStatus=17) so a preferred-model switch restart
// works under Restart=on-failure while a plain exit 0 or
// `systemctl stop` still stays down (issue #347).
const restartRequestedExitCode = 17

// restartRequested is set by the management API's RestartScheduler
// just before it SIGTERMs the process, turning the subsequent clean
// shutdown into an exit-17 "restart me" signal for systemd.
var restartRequested atomic.Bool

func main() {
	args := os.Args[1:]
	// platform/service.Dispatch handles `install` / `uninstall` /
	// `start` / `stop` subcommands across OSes, and on Windows also
	// enters the SCM dispatcher when invoked by the service control
	// manager. It returns handled=true after taking the appropriate
	// action (this function then exits with its rc) or handled=false to
	// let normal foreground startup proceed.
	if handled, rc := service.Dispatch(args, run); handled {
		os.Exit(rc)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), shutdownSignals()...)
	defer cancel()
	if err := run(ctx, args); err != nil {
		fmt.Fprintln(os.Stderr, "waired-agent:", err)
		os.Exit(1)
	}
	if restartRequested.Load() {
		os.Exit(restartRequestedExitCode)
	}
}

func run(ctx context.Context, args []string) error {
	// Derive a cancelable context so a fatal listener error — the
	// transparent Claude proxy intercept listener dying while the agent
	// process otherwise lives — can trigger full teardown. The agent then
	// exits non-zero, which makes systemd restart it (under both
	// Restart=always and Restart=on-failure) and run the hosts-revert
	// ExecStopPost, so api.anthropic.com never stays pointed at a dead
	// listener (the silent-breakage failure mode the proxy must avoid).
	ctx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	var fatalMu sync.Mutex
	var fatalErr error
	fatal := func(err error) {
		fatalMu.Lock()
		if fatalErr == nil {
			fatalErr = err
		}
		fatalMu.Unlock()
		runCancel()
	}
	// proxyH bridges the boot-level intercept listener (started before
	// enrollment) to the session-scoped inference handler + degraded
	// signal, which the activate closure wires in once they exist.
	proxyH := &proxyHandle{}

	fs := flag.NewFlagSet("waired-agent", flag.ContinueOnError)
	stateDir := fs.String("state-dir", paths.StateDir(paths.AutoDetect),
		"directory holding identity.json + secrets/* + cache/* (created by `waired init`)")
	mgmtAddr := fs.String("mgmt", management.DefaultListen,
		"loopback bind for the Local Management API")
	mgmtHardening := fs.Bool("mgmt-hardening", true,
		"enforce Host/Origin allow-listing + Content-Type: application/json on the Local Management API (defends against DNS-rebinding / cross-site requests from a web page; disable only for local debugging)")
	mgmtSocket := fs.String("mgmt-socket", "",
		"override the local IPC write endpoint (unix-domain socket path on Linux/macOS, named pipe on Windows); empty auto-derives from --state-dir. Mutating requests use this endpoint; the loopback TCP port serves reads (waired#838)")
	mgmtSocketWritesOnly := fs.Bool("mgmt-socket-writes-only", true,
		"refuse mutating requests on the loopback TCP port, requiring the local IPC socket instead (waired#838). The CLI and tray send writes over the socket; disable only for local debugging. Automatically inert while the socket is not bound, so a bind failure never blocks control of the agent")
	controlURL := fs.String("control", os.Getenv("WAIRED_CONTROL_URL"),
		"control plane base URL used for daemon-driven login (POST /waired/v1/login/start); a login request may override it")
	loginListen := fs.String("login-listen", "127.0.0.1:0",
		"UDP listen address advertised at enrollment time for daemon-driven login (host:port; 0 picks a random port). The local-candidate loop corrects the advertised endpoint after the engine binds.")
	forceRelay := fs.Bool("force-relay", false,
		"route every peer through the Network Map's home relay (relay-only mode; no direct UDP)")
	fallbackAfter := fs.Duration("fallback-after", 60*time.Second,
		"safety-net: when probe-driven path-selection is silent (no recent disco RTT samples or misses) AND WireGuard hasn't completed a handshake within this duration on the direct UDP path, switch the peer to its home relay")
	downgradeRTTRatio := fs.Float64("downgrade-rtt-ratio", defaultDowngradeRTTRatio,
		"direct EWMA RTT > N × relay EWMA RTT triggers downgrade to relay (probe-driven; Tailscale-style)")
	upgradeRTTRatio := fs.Float64("upgrade-rtt-ratio", defaultUpgradeRTTRatio,
		"direct EWMA RTT < N × relay EWMA RTT triggers upgrade back to direct; N > 1 means a working direct path is preferred even at RTT parity, vetoed only when much slower than relay (asymmetric dead-band against downgrade ratio)")
	downgradeMisses := fs.Int("downgrade-misses", defaultDowngradeMisses,
		"consecutive missed direct probes (no pong inside reaper window) that trigger downgrade independent of RTT comparison")
	upgradePongStreak := fs.Int("upgrade-pong-streak", defaultUpgradePongStreak,
		"required consecutive direct probe pongs before an upgrade-to-direct can fire")
	ewmaAlpha := fs.Float64("ewma-alpha", defaultEWMAAlpha,
		"per-sample weight for the RTT EWMA: new_avg = alpha*sample + (1-alpha)*prev")
	minRTTSamples := fs.Int("min-rtt-samples", defaultMinRTTSamples,
		"minimum direct + relay RTT samples each before the RTT-ratio criterion is consulted")
	minDwellTime := fs.Duration("min-dwell-time", defaultMinDwellTime,
		"minimum time after a path switch before the reverse switch can fire (flap suppression)")
	callMeMaybeInterval := fs.Duration("call-me-maybe-interval", defaultCallMeMaybeInterval,
		"per-peer base cadence for emitting call_me_maybe over the relay (relay-state rescue + direct-stuck bootstrap). Linearly backed off after fail-streak ≥ 3 (cap = call-me-maybe-backoff-max)")
	callMeMaybeBackoffMax := fs.Duration("call-me-maybe-backoff-max", defaultCallMeMaybeBackoffMax,
		"upper bound for the call_me_maybe cadence after fail-streak backoff; bounds relay bandwidth on symmetric-NAT pairs CMM cannot fix")
	bypassCPIAM := fs.Bool("bypass-cp-iam", false,
		"when the Control Plane is fronted by a Cloud Run / IAP IAM gate, inject a GCE identity token into Authorization for every CP request and present the device access token via X-Waired-Agent-Bearer instead")
	punchEnabled := fs.Bool("punch-enabled", true,
		"enable NAT-traversal disco subsystem (relay STUN observe + peer probe/pong). Disable for relay-only operation or debugging.")
	ipv6Enabled := fs.Bool("ipv6-enabled", true,
		"enumerate the host's globally-routable IPv6 addresses and advertise them as KindIPv6 candidates so peers can reach this agent directly over v6 (in addition to the NAT-mapped v4 candidate the relay STUN echo observes). Set false to suppress all v6 candidate emission.")
	ipv6IncludeULA := fs.Bool("ipv6-include-ula", false,
		"emit IPv6 ULA (fc00::/7) addresses as KindIPv6 candidates. Off by default — ULAs are useful inside a campus but unreachable from public-internet peers, where they waste probe cycles.")
	includeIPv4LAN := fs.Bool("include-ipv4-lan", true,
		"emit RFC1918 IPv4 LAN addresses as KindLocal candidates so peers on the same LAN can dial this agent directly without relay. Receivers in a different LAN are protected by the per-peer subnet-overlap filter in pushDiscoSnapshot, so this does not inflate cross-LAN probe traffic. Set false to disable v4 LAN candidate emission (e.g. for relay-only deployments).")
	localCandidateInterval := fs.Duration("local-candidate-interval", 5*time.Minute,
		"how often to re-enumerate local interfaces and re-advertise candidates (KindIPv6 / KindLocal). Diffs against the last advertisement; only changes hit CP.")
	disableInference := fs.Bool("disable-inference", false,
		"skip wiring up the inference subsystem (Local Gateway + ollama lifecycle + bundled-model pre-pull)")
	benchCacheInvalidate := fs.Bool("bench-cache-invalidate", false,
		"delete the boot benchmark cache (~/.cache/waired/bench.json) before measuring so the next start re-runs the token/s benchmark from scratch. Diagnostic flag; the cache key already changes on driver/variant updates.")
	devForcePhase := fs.String("dev-force-phase", "",
		"DEV ONLY: override Status.Phase value reported to the management API (one of: starting, stopping, error). Used to drive tray UI verification of states the daemon never holds long enough to capture in normal operation.")

	// Inference config: defaults → agent.json → env → flags.
	//
	// agent.json must be read BEFORE fs.Parse (the JSON values become
	// the flag defaults via RegisterInferenceFlags). That ordering
	// means we cannot rely on the parsed *stateDir; instead we peek
	// args directly so any --state-dir on the command line is honored.
	// Without this, an SCM-mode agent launched with
	//   waired-agent.exe -state-dir <somewhere>
	// would read agent.json from the AutoDetect default (e.g.
	// %ProgramData%\waired) and miss the file that `waired init
	// --state-dir <somewhere>` actually wrote — see issue #113.
	cfgRoot := agentconfig.Defaults()
	agentJSONPath := filepath.Join(peekStateDir(args), "agent.json")
	if err := cfgRoot.MergeJSON(agentJSONPath); err != nil {
		return fmt.Errorf("agent config: %w", err)
	}
	if err := cfgRoot.MergeEnv(os.Environ()); err != nil {
		return fmt.Errorf("agent config (env): %w", err)
	}
	cfgRoot.RegisterInferenceFlags(fs)

	if err := fs.Parse(args); err != nil {
		return err
	}

	// waired#756: on a fresh daemon-mediated install the local init path's
	// hardware-aware bundled-model selection never ran (setup.Enroll has no
	// ConfigureInference hook), so an under-spec host would boot inference
	// enabled and pull the full default model. Run it here — gated to a
	// pristine fresh install with no operator inference preference — and
	// persist the verdict to agent.json. Must precede the Inference.Enabled
	// gate below so an under-spec disable feeds it.
	maybeSelectBundledModelForFreshInstall(&cfgRoot, *disableInference, agentJSONPath, filepath.Dir(agentJSONPath), fs)

	// Phase 6: Inference.Enabled is the install-time choice for whether
	// this node runs a local engine at all. When false, force the
	// --disable-inference path so chooseEngine bails, the probe loop
	// short-circuits, and the overlay listener serves only the ping
	// endpoint. The flag wins if explicitly set on the command line
	// (operator override for diagnostics); otherwise the config field
	// drives the boot decision.
	if !cfgRoot.Inference.Enabled && !*disableInference {
		*disableInference = true
	}

	level := slog.LevelInfo
	if os.Getenv("WAIRED_DEBUG") != "" {
		level = slog.LevelDebug
	}
	primary := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	// Wrap with the OS-native secondary sink so Warn/Error records
	// survive stderr being closed (e.g. Windows SCM dispatcher).
	logger := slog.New(logsink.New(primary, service.ServiceName))
	slog.SetDefault(logger)

	// Phase 9 telemetry composite. Owned at boot (not inside the
	// session) so /metrics and /observability/* scrape the same
	// ring/registry across the daemon's lifetime, including a future
	// logout/re-login. The switchboard delegates ObservabilityState()
	// to the live session; Ring + MetricsHandler are agent-wide.
	obsRegistry := prometheus.NewRegistry()
	obsMetrics := observability.NewMetrics(obsRegistry)
	obsRing := observability.NewRing(observability.DefaultRingCapacity)
	obsRecorder := observability.NewRecorder(obsRing, obsMetrics, logger)
	promHandler := promhttp.HandlerFor(obsRegistry, promhttp.HandlerOpts{Registry: obsRegistry})

	// Switchboard + management server: built ONCE here and started
	// immediately, BEFORE any identity exists. The server is wired to
	// the switchboard, which returns unenrolled views (Identity
	// {Enrolled:false}, empty Status) until a session is published. A
	// login that completes at runtime activates the daemon live (see
	// the activate closure below) by publishing a session — the
	// already-registered routes pick it up with no restart and no
	// route re-registration. This is the Tailscale model and makes
	// identity-less boot the natural resting state (#177, #180).
	// Boot-level Claude route controller (#580): the in-session escape
	// hatch (auto/local/anthropic) + post-dispatch fallback policy. Created
	// here (process-lifetime, not session-scoped) so `waired claude
	// route|fallback` and the /waired-route slash command work even before
	// enrollment or while degraded. Seeded from the persisted preference so
	// a prior choice survives a restart.
	// One-shot migration of the pre-unification split state (desired-claude-route
	// #580 + desired-claude-node #645/#665) to the unified desired-claude-routing.
	if migrated, mErr := state.MigrateDesiredClaudeRouting(*stateDir); mErr != nil {
		logger.Warn("claude routing: migrating legacy state failed; using persisted/default", "err", mErr)
	} else if migrated {
		logger.Info("claude routing: migrated legacy route+node state to unified policy")
	}
	claudeRoutingPol, crErr := state.ReadDesiredClaudeRouting(*stateDir)
	if crErr != nil {
		logger.Warn("claude routing: reading persisted policy failed; using defaults", "err", crErr)
		claudeRoutingPol = state.DefaultClaudeRoutingPolicy()
	}
	claudeRouting := newClaudeRoutingController(*stateDir, claudeRoutingPol, logger).
		WithObservability(obsRing)

	// supervisedRestart marks the restart intent before the SIGTERM so the
	// otherwise-clean shutdown exits 17 and systemd/SCM/launchd brings the
	// agent back up (issue #347). Since #812 this is the FALLBACK for the
	// preferred-model switch (cross-engine / wedged engine / unenrolled); the
	// common case swaps the model in process with no restart. Shared by the
	// management RestartScheduler and the provider's wedged-engine self-heal.
	supervisedRestart := func() {
		restartRequested.Store(true)
		management.DefaultRestartScheduler()
	}

	sb := &switchboard{}
	mgmtSrv := management.New(sb, sb).
		WithIdentity(sb).
		WithPause(sb).
		WithInferenceControl(sbInfControl{sb}).
		WithWorkerControl(sbWorkerControl{sb}).
		WithClaudeRouting(claudeRouting).
		WithInferenceMesh(sb).
		WithObservability(management.ObservabilityConfig{
			Ring:           obsRing,
			MetricsHandler: promHandler,
			State:          sb,
		})
	// Browser-facing hardening (waired-ai/waired#836): the loopback bind
	// alone does not stop a web page the user visits from reaching :9476 via
	// DNS-rebinding or a cross-site simple request. On by default; the flag
	// exists only as a local-debug escape hatch.
	if *mgmtHardening {
		mgmtSrv = mgmtSrv.WithBrowserHardening()
	}
	mgmtSrv = mgmtSrv.WithSocketWritesOnly(*mgmtSocketWritesOnly)
	// Public Share consumer settings + consent (waired#826): consuming
	// public nodes is a routing concern, so the endpoints stay available
	// even when local inference serving is disabled.
	mgmtSrv = mgmtSrv.WithPublicUse(&management.PublicUseConfig{
		Path: agentconfig.DefaultPublicUsePath(),
	})
	// Inference routes are gated on the install-time --disable-inference
	// choice (a boot decision, not a session one), mirroring the old
	// inline wiring: an inference-disabled agent leaves these routes
	// unregistered so the tray hides the inference group on 404.
	if !*disableInference {
		mgmtSrv = mgmtSrv.WithInference(sbInfProvider{sb}).
			WithShareControl(sbShareControl{sb}).
			WithEngineControl(sbEngineControl{sb}).
			WithCatalog(&management.CatalogConfig{
				PreferencePath: agentconfig.DefaultPreferencePath(),
				// #812 in-process swap seam; delegates to the live session's
				// controller (errNotEnrolled → the handler restart-falls-back).
				ApplyModelSwitch: sbModelSwapControl{sb}.ApplyModelSwitch,
				// Fallback when the in-process swap can't apply (cross-engine
				// target / wedged engine / unenrolled).
				RestartScheduler: supervisedRestart,
			})
	}
	// The bundled OpenCode coding-agent web UI (#429/#486) now runs entirely
	// on the user side (`waired codeui open` / the tray), AS the invoking user
	// on their real project, behind an authenticating proxy. The daemon no
	// longer vendors or supervises `opencode serve`; it keeps only the no-token
	// :9479 data-plane gateway the user-side instance talks to.
	//
	// Integration endpoints are $HOME / state-dir based, not identity
	// based, so they are wired at boot and stay available unenrolled.
	homeDir, herr := os.UserHomeDir()
	if herr == nil {
		mgmtSrv = mgmtSrv.WithClaudeIntegration(management.ClaudeIntegrationConfig{
			StateDir:   *stateDir,
			HomeDir:    homeDir,
			BinaryPath: resolveOwnBinaryPath(),
		})
		gatewayBaseURL := fmt.Sprintf("http://127.0.0.1:%d", cfgRoot.Inference.LocalGatewayPort)
		// OpenCode's plugin points at the no-token data-plane gateway, a
		// distinct port from LocalGatewayPort; derive it the same way the
		// adapter does so detection and Apply agree.
		expectedBaseURL := opencode.DataPlaneBaseURL(gatewayBaseURL) + "/v1"
		mgmtSrv = mgmtSrv.WithOpenCodeIntegration(management.OpenCodeIntegrationConfig{
			HomeDir:         homeDir,
			ExpectedBaseURL: expectedBaseURL,
			Reconfigure: func(rctx context.Context) error {
				_, err := setup.IntegrationOne(rctx, integration.AgentOpenCode, setup.IntegrationOptions{
					HomeDir:        homeDir,
					StateDir:       *stateDir,
					GatewayBaseURL: gatewayBaseURL,
					NonInteractive: true,
				})
				return err
			},
		})
		// OpenClaw's plugin points at the same no-token data-plane gateway as
		// OpenCode (the loopback port shared by both integrations).
		expectedOpenClawURL := openclaw.DataPlaneBaseURL(gatewayBaseURL) + "/v1"
		mgmtSrv = mgmtSrv.WithOpenClawIntegration(management.OpenClawIntegrationConfig{
			HomeDir:         homeDir,
			ExpectedBaseURL: expectedOpenClawURL,
			Reconfigure: func(rctx context.Context) error {
				_, err := setup.IntegrationOne(rctx, integration.AgentOpenClaw, setup.IntegrationOptions{
					HomeDir:        homeDir,
					StateDir:       *stateDir,
					GatewayBaseURL: gatewayBaseURL,
					NonInteractive: true,
				})
				return err
			},
		})
	} else {
		logger.Warn("claude/opencode/openclaw integration endpoints disabled: cannot resolve $HOME", "err", herr)
	}
	// Claude Code routing is configured once at `waired init` / `waired claude
	// enable` via system-wide managed settings (ANTHROPIC_BASE_URL -> the local
	// gateway; #488) — there is no per-toggle runtime control to wire here.

	// activate brings up the identity-dependent runtime (WG engine,
	// token refresher, reconciler, disco, inference) and publishes it
	// into the switchboard. Runs once at boot when an identity already
	// exists, and again at runtime when a daemon-driven login completes
	// (wired in a later commit). It captures the parsed flags + config +
	// observability handles by closure so the body stays the verbatim
	// former inline run() body. Single-value error returns are
	// preserved; resources built on the way to a successful publish are
	// released by guarded defers if an early error fires (published
	// stays false).
	//
	// reactivate is forward-declared so the Node Key rotation loop (built
	// inside activate) can request a session rebuild on its own key; it is
	// assigned just after activate is defined.
	var reactivate func()
	activate := func(parent context.Context) error {
		id, err := identity.Load(*stateDir)
		if err != nil {
			return fmt.Errorf("load identity: %w", err)
		}
		if id == nil {
			return fmt.Errorf("activate: no identity at %s", *stateDir)
		}

		paths, err := identity.PathsFor(*stateDir)
		if err != nil {
			return err
		}
		mk, err := devicekeys.LoadOrCreateMachineKey(paths.MachineKey)
		if err != nil {
			return fmt.Errorf("load machine key: %w", err)
		}
		nk, err := devicekeys.LoadOrCreateNodeKey(paths.NodeKey)
		if err != nil {
			return fmt.Errorf("load node key: %w", err)
		}
		accessToken, err := identity.LoadAccessToken(*stateDir)
		if err != nil {
			return fmt.Errorf("load access token: %w", err)
		}
		if accessToken == "" {
			return fmt.Errorf("no access token at %s; rerun `waired init`", paths.AccessToken)
		}
		refreshToken, err := identity.LoadRefreshToken(*stateDir)
		if err != nil {
			return fmt.Errorf("load refresh token: %w", err)
		}
		tokenMeta, err := identity.LoadTokenMeta(*stateDir)
		if err != nil {
			return fmt.Errorf("load token meta: %w", err)
		}
		// Same bypass-CP-IAM treatment as every other CP client: the
		// refresh request authenticates at the app layer via the refresh
		// token + machine signature in the body, but still has to clear
		// the Cloud Run / IAP ingress gate, which wants a Google ID
		// token in Authorization. Without this the very first refresh
		// (~2 min before the access token expires) 403s at ingress and
		// the agent's CP session dies at the token TTL.
		var refresherHTTP *http.Client
		if *bypassCPIAM {
			refresherHTTP = bypassCPHTTPClient(parent, id.ControlURL, logger)
		}
		tokens := newTokenRefresher(tokenRefresherConfig{
			StateDir:       *stateDir,
			ControlURL:     id.ControlURL,
			DeviceID:       id.DeviceID,
			NetworkID:      id.NetworkID,
			MachineKey:     mk,
			HTTPClient:     refresherHTTP,
			InitialAccess:  accessToken,
			InitialRefresh: refreshToken,
			InitialMeta:    tokenMeta,
			Logger:         logger,
		})

		overlayIP, err := netip.ParseAddr(id.OverlayIP)
		if err != nil {
			return fmt.Errorf("invalid overlay_ip %q: %w", id.OverlayIP, err)
		}
		listenPort, err := udpListenPortFromEndpoint(id.Endpoint)
		if err != nil {
			return fmt.Errorf("parse endpoint %q: %w", id.Endpoint, err)
		}

		deviceCert, err := loadDeviceCertificate(paths.DeviceCertificate)
		if err != nil {
			return fmt.Errorf("load device certificate: %w", err)
		}
		// Provider must exist before the relay factory closure captures it,
		// because RelayTLSFingerprint() reads the URL→pin map that
		// provider.replacePeers refreshes on every Apply(nm). The factory
		// is invoked lazily by the engine when a relay endpoint is parsed,
		// at which point the network map (and therefore the pin) is
		// guaranteed to have been populated at least once.
		provider := &agentProvider{id: id, engine: nil, wgListenPort: listenPort} // engine wired below
		relayFactory := newRelayClientFactory(logger, id, mk, nk.PublicBase64(), tokens.Get, deviceCert, provider)

		// published flips true only once the fully-built session is handed
		// to the switchboard. Until then, the guarded defers below release
		// every resource on any early-error return (preserving the cleanup
		// the old monolithic run() did via unconditional defers); on
		// success the session owns them and teardown() releases them.
		published := false

		engine, err := wgnet.NewEngine(wgnet.Config{
			SelfName:           id.DeviceID,
			SelfOverlayIP:      overlayIP,
			SelfPrivateKey:     nk.Private[:],
			ListenPort:         listenPort,
			Peers:              nil,
			Logger:             logger,
			SelfDeviceID:       id.DeviceID,
			SelfNetworkID:      id.NetworkID,
			SelfNodePub:        nk.PublicBase64(),
			RelayClientFactory: relayFactory,
		})
		if err != nil {
			return err
		}
		defer func() {
			if !published {
				engine.Close()
			}
		}()

		overlayLn, err := engine.ListenOverlayTCP(inferenceServicePort)
		if err != nil {
			return fmt.Errorf("listen overlay: %w", err)
		}

		ctx, cancel := context.WithCancel(parent)
		defer func() {
			if !published {
				cancel()
			}
		}()

		// Periodic agent-stats publisher. Started AS EARLY AS POSSIBLE —
		// before reconciler / disco / inference subsystem boot — so the
		// testnet CI verify path sees a "this agent is alive" Cloud
		// Logging record within seconds of the binary starting, not
		// minutes after enrollment + map-loop convergence finish. The
		// initial Status() snapshot is sparse (PeerCount=0, no NAT info)
		// but the kickoff record is sufficient to confirm agent ↔
		// Cloud Logging works; subsequent stats samples populate as
		// disco / reconciler come online and refresh agentProvider.
		//
		// cloudLogSink is the lazy-bind handoff for the testharness
		// Reporter (B-4): runStatsPublisher's first action is to construct
		// the cloudLogger (or nil on non-GCE) and Store it here, after
		// which cloudLoggerReporter.ReportScenario can publish through
		// the same client without coordinating startup order.
		cloudLogSink := new(atomic.Pointer[cloudLogger])
		go runStatsPublisher(ctx, provider, statsIntervalFromEnv(), cloudLogSink)

		infClient := inference.NewClient(engine, 15*time.Second)

		provider.engine = engine

		// agentPinger backs the switchboard's PingPeer delegation once this
		// session is published. The observability composite + management
		// server are owned by run() (built at boot, wired to the
		// switchboard); the obsRing / obsRecorder handles below are captured
		// from that boot scope by this closure.
		pinger := &agentPinger{client: infClient, provider: provider}

		// Pause/resume bookkeeping. desired-phase persists across daemon
		// restarts so an explicit `waired pause` is honoured even after a
		// crash. The state writer publishes the current phase + heartbeat
		// to <state>/runtime/state, which the shell-rc precmd hook reads
		// to decide whether ANTHROPIC_BASE_URL etc. should be exported.
		pm, stateWriter, err := newPauseInfra(*stateDir, cfgRoot.Inference.LocalGatewayPort, logger)
		if err != nil {
			return fmt.Errorf("pause infra: %w", err)
		}
		defer func() {
			if !published {
				if err := stateWriter.Remove(); err != nil {
					logger.Warn("remove runtime/state file on cleanup", "err", err)
				}
			}
		}()
		if *devForcePhase != "" {
			switch *devForcePhase {
			case "starting", "stopping", "error":
				pm.forcePhase = state.Phase(*devForcePhase)
				logger.Warn("--dev-force-phase active; Status.Phase is overridden", "phase", *devForcePhase)
			default:
				return fmt.Errorf("--dev-force-phase: invalid value %q (allowed: starting, stopping, error)", *devForcePhase)
			}
		}

		// Inference soft-toggle: orthogonal to pause/resume. The on-disk
		// desired-inference file persists across daemon restarts so an
		// explicit Disable from the tray survives a crash.
		infInitial, err := state.ReadDesiredInferenceState(*stateDir)
		if err != nil {
			return fmt.Errorf("read desired-inference: %w", err)
		}
		infCtl := newInferenceController(*stateDir, infInitial, logger)

		// Phase 6: mesh-share toggle. agentconfig.ShareWithMesh is the
		// install-time default; the persisted desired-share file (set by
		// the CLI/tray runtime toggle) overrides it on boot. Empty
		// desired-share file = "operator has never touched the toggle, use
		// the agentconfig default". The controller is **only** wired when
		// inference is enabled: an agent booted with Inference.Enabled=false
		// has no engine to share, so a share controller would only confuse
		// the tray (the management API surface and the user-visible toggle
		// stay omitempty in that case).
		var shareCtl *shareController
		if !*disableInference {
			shareInitial := state.ShareMeshShared
			if !cfgRoot.Inference.ShareWithMesh {
				shareInitial = state.ShareMeshNotShared
			}
			if persisted, err := state.ReadDesiredShareMesh(*stateDir); err != nil {
				return fmt.Errorf("read desired-share: %w", err)
			} else if persisted != "" {
				shareInitial = persisted
			}
			shareCtl = newShareController(*stateDir, shareInitial, logger)
		}

		// Tailscale-exit-node-style manual routing controller. The
		// agentconfig.Routing default ("auto" unless the operator set a
		// different value in agent.json) supplies the boot fallback; the
		// persisted desired-worker file (written by `waired worker set`
		// and the tray) overrides it on boot. Same precedence as
		// share/desired-share / inference/desired-inference. Wired
		// unconditionally — routing belongs to the *outbound* side of the
		// agent and remains meaningful even when Inference.Enabled=false
		// (a local-only-disabled agent can still pin to a peer).
		workerInitial := cfgRoot.Routing.AsPreference()
		if persisted, err := state.ReadDesiredWorker(*stateDir); err != nil {
			return fmt.Errorf("read desired-worker: %w", err)
		} else if !persisted.IsZero() {
			workerInitial = persisted
		}
		workerCtl := newWorkerController(*stateDir, workerInitial, logger)
		if obsRing != nil {
			workerCtl.WithObservability(obsRing)
		}

		// Phase 3 inference mesh aggregator. Fed by two writers:
		// (a) runLocalInferenceProbe → UpdateLocal on each tick
		// (b) runNetworkMapLoop → Update on every frame
		// Read by GET /waired/v1/inference/mesh on the management API.
		// The 15 s staleness threshold matches the 5 s probe / push
		// cadence × 3 — see docs/decisions.md.
		meshAgg := inferencemesh.New(id.DeviceID, 15*time.Second, time.Now)

		// Phase 4 peer-overlay auth lookup. Fed alongside the aggregator
		// from runNetworkMapLoop so the inference listener can resolve a
		// WG-source overlay IP to (DeviceID, MachinePublicKey) on every
		// inbound peer-engine request.
		peerDir := newPeerDirectory()

		// peerAdapterFactory is the loopback gateway's hook for routing
		// "remote:<deviceID>" Selections through a peer's overlay-side
		// inference listener. Construction is per-Selection so the
		// adapter always observes the latest mesh snapshot.
		peerAdapterFactory := func(deviceID string) (infruntime.Adapter, error) {
			snap := meshAgg.Snapshot()
			for _, p := range snap.Peers {
				if p.DeviceID != deviceID {
					continue
				}
				ip, err := netip.ParseAddr(p.OverlayIP)
				if err != nil {
					return nil, fmt.Errorf("peer adapter factory: parse overlay IP %q: %w", p.OverlayIP, err)
				}
				peerDeviceID := deviceID
				snap := snap
				return peer.NewAdapter(peer.Config{
					SelfDeviceID:  id.DeviceID,
					SelfPrivKey:   mk.Private,
					PeerDeviceID:  peerDeviceID,
					PeerOverlayIP: ip,
					Dialer:        engine,
					HealthFn: func() infruntime.Health {
						// Re-snapshot each time the gateway calls Health
						// so a peer that flapped between Selection and
						// dispatch surfaces a fresh state.
						for _, pp := range snap.Peers {
							if pp.DeviceID != peerDeviceID {
								continue
							}
							if pp.InferenceState == nil || !pp.InferenceState.Reachable || pp.Stale {
								return infruntime.Health{State: infruntime.StateFailed, LastErr: "peer engine unreachable or stale"}
							}
							return infruntime.Health{State: infruntime.StateReady}
						}
						return infruntime.Health{State: infruntime.StateFailed, LastErr: "peer dropped from mesh"}
					},
				})
			}
			return nil, fmt.Errorf("peer %q not in current mesh snapshot", deviceID)
		}

		// Push client used by the local probe to feed the CP's
		// inference-status endpoint. Same bypass-CP-IAM treatment as the
		// network map loop so Cloud Run / IAP-gated deployments work.
		infPushClient := controlclient.NewWithBearer(id.ControlURL, tokens.Get)
		if *bypassCPIAM {
			infPushClient.HTTP = bypassCPHTTPClient(ctx, id.ControlURL, logger)
			infPushClient.UseCustomAuthHeader = true
		}

		rec := newReconciler(engine, provider, logger, id, reconcilerConfig{
			ForceRelay:            *forceRelay,
			FallbackAfter:         *fallbackAfter,
			DowngradeRTTRatio:     *downgradeRTTRatio,
			UpgradeRTTRatio:       *upgradeRTTRatio,
			DowngradeMisses:       *downgradeMisses,
			UpgradePongStreak:     *upgradePongStreak,
			EWMAAlpha:             *ewmaAlpha,
			MinRTTSamples:         *minRTTSamples,
			MinDwellTime:          *minDwellTime,
			CallMeMaybeInterval:   *callMeMaybeInterval,
			CallMeMaybeBackoffMax: *callMeMaybeBackoffMax,
		})
		provider.reconciler = rec

		// Optional disco subsystem. Wires the WG UDP socket's
		// magic-prefix classifier to the STUN observer + peer prober,
		// pushes events back to the reconciler so direct paths get
		// upgraded once a punch succeeds, and forwards observed addrs
		// to CP via POST /v1/devices/self/endpoints.
		//
		// Constructed BEFORE the testharness dispatcher so the dispatcher
		// can subscribe to disco's OnCallMeMaybe hook in the
		// //go:build testharness build — see internal/testharness/
		// dispatcher_testharness.go for the rationale (a static iptables
		// snapshot misses CMM-learned alternate endpoints once the mesh
		// grows past 6 nodes).
		var discoSvc *disco.Service
		if *punchEnabled && !*forceRelay {
			bind := engine.Bind()
			if bind == nil {
				logger.Warn("disco: MultiplexBind not available; punch disabled")
			} else {
				ds, err := disco.New(disco.Config{
					SelfDeviceID:    id.DeviceID,
					SelfNodeKeyPriv: nk.Private,
					SelfNodeKeyPub:  nk.Public,
					Bind:            bind,
					Logger:          logger.With("component", "disco"),
				})
				if err != nil {
					return fmt.Errorf("disco service: %w", err)
				}
				discoSvc = ds
				rec.AttachDisco(discoSvc)
				provider.disco = discoSvc
			}
		}

		// Test-harness dispatcher. NoopDispatcher in production (default
		// build); the //go:build testharness build wires the active
		// dispatcher that reads ActiveTestScenario from each Network Map
		// frame and applies the corresponding iptables-based scenario.
		// Stop is best-effort on shutdown (5 s timeout) so a stale chain
		// is not left behind when the agent exits cleanly.
		dispatcher := newTestHarnessDispatcher(
			logger.With("component", "testharness"),
			cloudLoggerReporter{cl: cloudLogSink},
			id.DeviceID,
			discoSvc,
		)
		defer func() {
			if !published {
				sctx, c := context.WithTimeout(context.Background(), 5*time.Second)
				defer c()
				_ = dispatcher.Stop(sctx)
			}
		}()

		var wg sync.WaitGroup

		// Bring up the inference subsystem before the management server
		// starts serving so the inference routes are available from the
		// first request. Startup is non-blocking — the gateway / engine /
		// pre-pull goroutines run independently and surface their state
		// through /waired/v1/inference/status.

		// Phase 7 routing primitives. All three are agent-wide
		// singletons whose lifetime matches main()'s; the loopback
		// Selector reads them and the probe loop pushes their snapshot
		// to mesh peers. discoSvc.RTTSnapshot is wired in below when the
		// disco service is enabled; the LocalRTT closure stays nil
		// otherwise, which the Selector treats as "no RTT data" and
		// falls back to deviceID tie-break.
		stickyStore := router.NewStickyStore(0, nil) // 0 → DefaultStickyTTL
		inFlightTracker := router.NewInFlightTracker()
		errorWindow := router.NewErrorWindow(nil)
		var rttSnapshotFn func() map[string]uint32
		if discoSvc != nil {
			rttSnapshotFn = discoSvc.RTTSnapshot
		}
		// Phase 8: disco-based reachability snapshot, used by the
		// Selector as a hard-exclusion signal so peers with no recent
		// pong (NAT-asymmetry / WG-keepalive failure / ISP routing
		// blackhole) drop out of the candidate set before the probe layer
		// fans out. 5 s freshness matches roughly 3× the disco prober
		// re-emit interval — that's enough margin to absorb a single
		// missed pong without flipping the peer to "unreachable".
		var reachableSnapshotFn func() map[string]bool
		if discoSvc != nil {
			reachableSnapshotFn = func() map[string]bool {
				return discoSvc.ReachableSnapshot(time.Now(), 5*time.Second)
			}
		}

		var overlayHandlerSet *gateway.HandlerSet
		var claudeHandlerSet *gateway.HandlerSet
		var inferenceSub *inferenceSubsystem
		// infProvider feeds the switchboard's InferenceProvider delegation
		// once this session is published. The matching inference + catalog
		// routes are registered at boot in run() (gated on the same
		// --disable-inference choice), so nothing is wired into mgmtSrv here.
		var infProvider management.InferenceProvider
		// engCtl backs the switchboard's EngineController delegation (#186
		// hard engine power axis). Needs the concrete ollama adapter the
		// subsystem owns, so it is built here and stored in the session;
		// the engine routes are registered at boot like the inference ones.
		var engCtl *engineController
		// swapCtl backs the switchboard's #812 in-process model-swap
		// delegation; like engCtl it needs the concrete provider the subsystem
		// owns and is stored in the session.
		var swapCtl *modelSwapController
		if !*disableInference {
			sub, ip, err := startInferenceSubsystem(ctx, &wg, logger, *stateDir, cfgRoot.Inference, inferenceSubsystemDeps{
				IsPaused:             pm.IsPaused,
				IsInferenceDisabled:  infCtl.IsDisabled,
				InferenceState:       infCtl.State,
				MeshSnapshotFn:       meshAgg.Snapshot,
				PeerAdapterFactory:   peerAdapterFactory,
				Sticky:               stickyStore,
				LocalInFlight:        inFlightTracker,
				LocalRTT:             rttSnapshotFn,
				LocalErrors:          errorWindow.Snapshot,
				LocalReachable:       reachableSnapshotFn,
				Recorder:             obsRecorder,
				Routing:              workerCtl.Routing,
				OnClaudeNodeFallback: claudeRouting.RecordNodeFallback,
			})
			if err != nil {
				return fmt.Errorf("inference subsystem: %w", err)
			}
			infProvider = ip
			engCtl = newEngineController(ctx, sub.ollama, logger)
			swapCtl = newModelSwapController(ctx, sub.provider, logger)
			// Wedged-engine self-heal for the #812 in-process switch: if the
			// engine won't come back after a switch bounce, the reconcile falls
			// back to the supervised restart (the only restart #812 keeps).
			sub.provider.restartOnWedge = supervisedRestart
			overlayHandlerSet = sub.overlayHandlerSet
			claudeHandlerSet = sub.claudeHandlerSet
			inferenceSub = sub
		}

		// Wire the transparent proxy's local-inference path now that the
		// gateway HandlerSet exists. Use the BARE mesh-capable Claude
		// HandlerSet (#601) — the loopback gateway.Server wraps
		// requireToken, which would 401 the Anthropic OAuth token Claude
		// Code actually sends, and the :9474 overlay set is local-only by
		// design. Only when AllowAnthropicAPI is on: otherwise the
		// /anthropic routes aren't registered and a local request would
		// 404 instead of failing open to real Anthropic. When inference is
		// disabled (claudeHandlerSet nil) the handler stays unset, so the
		// proxy fails open (passthrough) for every request.
		if claudeHandlerSet != nil && cfgRoot.Inference.AllowAnthropicAPI {
			proxyH.SetLocalInference(claudeHandlerSet.Handler())
		}
		proxyH.SetDegraded(func() bool {
			if pm.IsPaused() || infCtl.IsDisabled() {
				return true
			}
			s := stateWriter.Snapshot()
			return !s.InferenceReachableLocal && !s.InferenceReachableInMesh
		})

		wg.Add(7) // overlay + map loop + fallback loop + state heartbeat + inference probe + local-candidate advertise + connectivity push (management server runs under run()'s srvWG, not the session)
		if discoSvc != nil {
			wg.Add(2) // disco service + event drain
		}

		go func() {
			defer wg.Done()
			runStateHeartbeat(ctx, stateWriter, logger)
		}()
		go func() {
			defer wg.Done()
			engineKind, enginePort := probeTargetForActive(cfgRoot.Inference)

			// Phase 7: read the hardware profile once at boot. The
			// summary is broadcast on every InferenceState push but
			// never mutates over the agent's lifetime, so a single
			// snapshot is enough; profiler.Profile caches internally
			// anyway. The full profile is also retained (firstGPU below)
			// so the boot benchmark cache can key on driver_version.
			var hwSummary *signer.HardwareSummary
			var firstGPU hardware.GPU
			if !*disableInference {
				prof := hardware.NewProfiler("").Profile(ctx)
				if len(prof.GPUs) > 0 {
					firstGPU = prof.GPUs[0]
				}
				gpus := prof.GPUSummary()
				if len(gpus) > 0 || prof.RAMTotalGB > 0 {
					summary := &signer.HardwareSummary{RAMTotalGB: prof.RAMTotalGB}
					for _, g := range gpus {
						summary.GPUs = append(summary.GPUs, signer.HardwareGPUSummary{
							Model:       g.Model,
							VRAMTotalMB: g.VRAMTotalMB,
							ComputeCap:  g.ComputeCap,
						})
					}
					hwSummary = summary
				}
			}

			// Phase 7: run the boot-time token/s benchmark to derive
			// Capacity. Synchronous on the probe goroutine so the
			// first probe tick already advertises the correct cap;
			// failures (CUDA OOM, slow load) fall back to Capacity=1
			// inside RunBootBenchmark rather than blocking startup.
			//
			// Phase 7 follow-up (C2): a SHA-keyed cache on disk lets
			// subsequent boots return the previous result instantly
			// (typical save: 5-30 s per boot). The cache file lives at
			// ~/.cache/waired/bench.json; empty path = caching disabled.
			capacity := 0
			if !*disableInference {
				var cache *benchCache
				if cachePath := defaultWairedCachePath(); cachePath != "" {
					cache = newBenchCache(cachePath, logger)
					if *benchCacheInvalidate {
						if err := cache.Invalidate(); err != nil {
							logger.Warn("bench cache invalidate failed", "err", err)
						} else {
							logger.Info("bench cache invalidated by --bench-cache-invalidate")
						}
					}
				}
				bench := RunBootBenchmark(ctx, BenchDeps{
					EngineKind:    engineKind,
					EnginePort:    enginePort,
					EngineModel:   engineModelForActive(cfgRoot.Inference),
					VariantID:     variantIDForActive(),
					GPUModel:      firstGPU.Model,
					VRAMTotalMB:   firstGPU.VRAMTotalMB,
					DriverVersion: firstGPU.DriverVersion,
					VariantSHA:    variantSHAForActive(),
					Cache:         cache,
					Logger:        logger,
				})
				capacity = bench.Capacity
				// Feed the result to the provider so the management API
				// can derive the #133 lighter-model recommendation.
				if inferenceSub != nil && inferenceSub.provider != nil {
					inferenceSub.provider.SetLastBench(bench)

					// #624 depth-aware long-context sweep: background,
					// ollama only (needs the native /api/generate
					// counters), never after a failed boot bench (the
					// engine is unhealthy; a 25-minute retry helps
					// nobody). It waits for the #621 tuning verification
					// to settle so a multi-minute prefill never races
					// the one-shot degrade restart, then measures at the
					// APPLIED window. Cache-hit boots return instantly.
					if engineKind == signer.InferenceTypeOllama && !bench.Failed &&
						inferenceSub.provider.ollama != nil {
						prov := inferenceSub.provider
						depthDeps := DepthBenchDeps{
							EnginePort:    enginePort,
							EngineModel:   engineModelForActive(cfgRoot.Inference),
							VariantID:     variantIDForActive(),
							GPUModel:      firstGPU.Model,
							VRAMTotalMB:   firstGPU.VRAMTotalMB,
							DriverVersion: firstGPU.DriverVersion,
							VariantSHA:    variantSHAForActive(),
							Cache:         cache,
							Logger:        logger,
							Nonce:         fmt.Sprintf("boot%d", time.Now().Unix()),
						}
						go func() {
							tuning := waitForAppliedTuning(ctx, prov.ollama, 5*time.Second, depthBenchTuningWait)
							depthDeps.ContextLength = tuning.ContextLength
							depthDeps.KVCacheType = tuning.KVCacheType
							depthDeps.NumBatch = tuning.NumBatch
							if depthDeps.ContextLength == 0 {
								logger.Info("long-context benchmark skipped: no applied context window (untuned engine)")
								return
							}
							res := RunDepthBenchmark(ctx, depthDeps)
							if len(res.Stages) > 0 {
								prov.SetLastDepthBench(res)
							}
						}()
					}
				}
			}

			deps := inferenceProbeDeps{
				StateWriter: stateWriter,
				Aggregator:  meshAgg,
				PushClient:  infPushClient,
				DeviceID:    id.DeviceID,
				MachineKey:  mk.Private,
				EngineKind:  engineKind,
				EnginePort:  enginePort,
				Disabled:    *disableInference,
				Logger:      logger,
				Hardware:    hwSummary,
				Capacity:    capacity,
				ActiveTag:   activeEngineTagForActive(),
			}
			// Advertise the engine's VRAM-safe parallelism ceiling (advisory)
			// so the admin Device detail page can show it and warn before an
			// operator raises the per-node concurrency past it.
			if inferenceSub != nil && inferenceSub.provider != nil && inferenceSub.provider.ollama != nil {
				prov := inferenceSub.provider
				deps.RecommendedMaxParallel = func() int {
					return prov.ollama.AppliedTuning().RecommendedMaxParallel
				}
			}
			if shareCtl != nil {
				deps.IsShared = shareCtl.IsShared
			}
			runLocalInferenceProbe(ctx, deps)
		}()

		// Connectivity push (#252): report the direct/relay path mix to CP
		// for the admin Device detail page. Reads reconciler.Snapshot()
		// read-only; reuses the inference push client (same bearer / bypass
		// treatment) and the device's machine key.
		go func() {
			defer wg.Done()
			runConnectivityPush(ctx, connectivityPushDeps{
				PushClient: infPushClient,
				DeviceID:   id.DeviceID,
				MachineKey: mk.Private,
				Snapshot:   rec.Snapshot,
				Logger:     logger,
			})
		}()

		// Construct the overlay-side inference server. Mounts the gateway
		// HandlerSet built by startInferenceSubsystem behind the peer-auth
		// chain (wgPeerOnly + verifyPeerSignature + paused/inference
		// gates). When inference is disabled (--disable-inference or no
		// engine viable), overlayHandlerSet is nil and we degrade to the
		// Phase 1 ping-only server so peers can still reachability-probe.
		var infSrv *inference.Server
		if overlayHandlerSet != nil {
			cfg := inference.Config{
				DeviceName:     id.DeviceID,
				GatewayHandler: overlayHandlerSet,
				PeerLookup:     peerDir,
				// Bounded (per-device + global): Public Share grants put
				// foreign device IDs into this cache, so growth must cap
				// (spec §8.5).
				NonceCache:          inference.NewBoundedNonceCache(0, 0),
				IsPaused:            pm.IsPaused,
				IsInferenceDisabled: infCtl.IsDisabled,
				Recorder:            obsRecorder,
			}
			if shareCtl != nil {
				cfg.IsShareDenied = shareCtl.IsShareDenied
			}
			// Phase 8: /waired/v1/inference/healthz reports the local
			// engine + active model so remote probe coordinators can
			// distinguish "engine is loading" from "engine is up but at
			// capacity" without blowing an inference attempt on a stale
			// snapshot.
			if inferenceSub != nil {
				cfg.EngineReadyFn = inferenceSub.EngineReady
			}
			infSrv = inference.NewServerWithConfig(cfg)
		} else {
			infSrv = inference.NewServer(id.DeviceID)
		}

		// Phase 9: wire the management-side observability endpoints now
		// that all subsystems the state provider reads from (inference
		// server, share controller, mesh aggregator, pause manager) are
		// in place. The poller goroutine started below mirrors live state
		// transitions into the composite Recorder so engine_state_change
		// events and the boolean gauges stay current without each
		// transition site having to call into Recorder directly.
		obsStateProvider := &observabilityState{
			startedAt:    time.Now(),
			id:           id,
			isPaused:     pm.IsPaused,
			isShareDeny:  shareDenyFn(shareCtl),
			engineReady:  engineReadyAccessor(inferenceSub),
			engineInfo:   engineInfoAccessor(inferenceSub),
			inflight:     infSrv.InflightCount,
			meshSnapshot: meshAgg.Snapshot,
		}
		// obsStateProvider feeds the switchboard's ObservabilityState()
		// delegation once this session is published; the Ring +
		// MetricsHandler were registered on the management server at boot.

		wg.Add(1)
		go func() {
			defer wg.Done()
			runObservabilityPoller(ctx, obsRecorder, obsStateProvider, 5*time.Second)
		}()

		go func() {
			defer wg.Done()
			if err := infSrv.ServeOverlay(ctx, overlayLn); err != nil {
				logger.Error("overlay server stopped", "err", err)
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			tokens.Run(ctx)
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			var rotatorHTTP *http.Client
			if *bypassCPIAM {
				rotatorHTTP = bypassCPHTTPClient(ctx, id.ControlURL, logger)
			}
			rotator := newNodeKeyRotator(nodeKeyRotatorConfig{
				StateDir:            *stateDir,
				ControlURL:          id.ControlURL,
				DeviceID:            id.DeviceID,
				NetworkID:           id.NetworkID,
				MachineKey:          mk,
				CurrentNodeKey:      nk,
				HTTPClient:          rotatorHTTP,
				BearerFn:            tokens.Get,
				UseCustomAuthHeader: *bypassCPIAM,
				// Detached: reactivate tears down THIS session (cancelling
				// the rotator's ctx), so it must not run on this goroutine.
				TriggerReactivate: func() { go reactivate() },
				Logger:            logger,
			})
			rotator.Run(ctx)
		}()
		go func() {
			defer wg.Done()
			applyConcurrency := func(capacity, parallel int) {
				// Admission gate (non-disruptive) reads Capacity; the ollama
				// engine parallelism (restart-on-change) reads DesiredParallel,
				// which is non-zero only under an explicit admin override — so a
				// default host's benchmark capacity never restarts its engine.
				infSrv.SetCapacity(capacity)
				if inferenceSub != nil && inferenceSub.provider != nil {
					inferenceSub.provider.ApplyConcurrency(ctx, parallel)
				}
			}
			runNetworkMapLoop(ctx, logger, id, tokens.Get, rec, meshAgg, peerDir, dispatcher, applyConcurrency, *bypassCPIAM)
		}()
		go func() {
			defer wg.Done()
			runFallbackLoop(ctx, rec, *fallbackAfter)
		}()

		if discoSvc != nil {
			go func() {
				defer wg.Done()
				if err := discoSvc.Run(ctx); err != nil {
					logger.Error("disco service stopped", "err", err)
				}
			}()
			go func() {
				defer wg.Done()
				runDiscoEventLoop(ctx, logger, id, tokens.Get, mk.Private, discoSvc, rec, *bypassCPIAM)
			}()
		}

		go func() {
			defer wg.Done()
			runLocalCandidateLoop(ctx, logger, id, tokens.Get, mk.Private, localCandidateOptions{
				listenPort:     uint16(listenPort),
				ipv6Enabled:    *ipv6Enabled,
				includeULA:     *ipv6IncludeULA,
				includeIPv4LAN: *includeIPv4LAN,
				interval:       *localCandidateInterval,
				bypassCPIAM:    *bypassCPIAM,
			})
		}()

		s := &session{
			provider:      provider,
			pinger:        pinger,
			pause:         pm,
			infControl:    infCtl,
			shareControl:  shareCtl,
			workerControl: workerCtl,
			meshAgg:       meshAgg,
			infProvider:   infProvider,
			engControl:    engCtl,
			swapControl:   swapCtl,
			obsState:      obsStateProvider,
			engine:        engine,
			stateWriter:   stateWriter,
			dispatcher:    dispatcher,
			cancel:        cancel,
			wg:            &wg,
			logger:        logger,
		}
		if !sb.publish(s) {
			// A session is already live (boot/login race). Abandon this
			// build; the guarded defers above (published still false)
			// cancel the context, stop the dispatcher, close the engine,
			// and remove the runtime/state file.
			return errors.New("activate: a session is already active")
		}
		published = true

		logger.Info("waired-agent ready",
			"device_id", id.DeviceID,
			"network_id", id.NetworkID,
			"overlay_ip", id.OverlayIP,
			"listen_port", listenPort,
			"control_url", id.ControlURL,
			"mgmt", *mgmtAddr,
			"force_relay", *forceRelay,
			"fallback_after", fallbackAfter.String(),
		)
		return nil
	}

	// reactivate rebuilds the live session from the (now rotated) node key
	// on disk: it tears down the current session and re-runs activate,
	// which re-loads node.key and reconstructs the engine / multiplex-bind
	// / relay factory / disco around it (#228). Serialised by its own mutex
	// so a rotation cannot race a second rotation; the once-per-~150d
	// cadence makes a race with a concurrent login implausible. Runs on a
	// detached goroutine (the rotator triggers it via `go reactivate()`)
	// because teardown cancels the rotator's own context.
	var reactivateMu sync.Mutex
	reactivate = func() {
		reactivateMu.Lock()
		defer reactivateMu.Unlock()
		if s := sb.current(); s != nil {
			s.teardown()
		}
		sb.reset()
		if err := activate(ctx); err != nil {
			logger.Error("re-activate after node-key rotation failed; device unenrolled until restart", "err", err)
		}
	}

	// Daemon-driven login (Tailscale model): the login controller runs
	// enrollment in-process and, on success, calls activate to bring the
	// runtime up live. Wired before the management server starts serving
	// so a login request can never arrive before the controller exists.
	loginCtl := newLoginController(sb, loginControllerConfig{
		StateDir:          *stateDir,
		DefaultControlURL: *controlURL,
		Endpoint:          "udp4:" + *loginListen,
		RootCtx:           ctx,
		Activate:          activate,
		Logger:            logger,
	})
	mgmtSrv = mgmtSrv.WithLogin(loginCtl)

	// Update check/status/settings (#293/#294). Unconditional — version
	// checks must work even before enrollment. Read-only; the CLI/tray drive
	// the actual apply under elevation.
	updateCtl := newUpdateController(*stateDir)
	mgmtSrv = mgmtSrv.WithUpdateController(updateCtl)

	// Resolve the local IPC write endpoint (waired#838). Empty --mgmt-socket
	// auto-derives from the resolved state dir: a system install
	// (--state-dir == the OS system dir) binds the System runtime socket
	// (/run/waired, /var/run/waired, or the machine-wide named pipe); a
	// dev / interactive run binds a per-user runtime socket.
	mgmtSocketEndpoint := *mgmtSocket
	if mgmtSocketEndpoint == "" {
		socketMode := paths.AutoDetect
		if *stateDir == paths.StateDir(paths.System) {
			socketMode = paths.System
		}
		mgmtSocketEndpoint = paths.MgmtEndpoint(socketMode)
	}

	var srvWG sync.WaitGroup
	srvWG.Add(1)
	go func() {
		defer srvWG.Done()
		if err := mgmtSrv.Serve(ctx, *mgmtAddr); err != nil {
			logger.Error("management server stopped", "err", err)
		}
	}()
	// Local IPC write channel (waired#838): a unix socket (Linux/macOS) /
	// named pipe (Windows) that browsers and network peers cannot open.
	// Fail-open: a bind failure logs but does not crash the agent — the TCP
	// listener's writeGuard keys on the socket actually being up.
	if mgmtSocketEndpoint != "" {
		srvWG.Add(1)
		go func() {
			defer srvWG.Done()
			if err := mgmtSrv.ServeLocal(ctx, mgmtSocketEndpoint); err != nil {
				logger.Error("management local IPC socket stopped", "err", err, "endpoint", mgmtSocketEndpoint)
			}
		}()
	}

	// Background version-check loop (#294): refreshes the cached update
	// status on a 6h cadence so a release published after boot surfaces on
	// /update/status (and the tray prompt) even on headless agents that
	// never seed a check. Identity-independent, like the controller itself.
	srvWG.Add(1)
	go func() {
		defer srvWG.Done()
		runUpdateCheckLoop(ctx, updateCtl, updateCheckInterval, logger)
	}()

	// Claude Code loopback gateway: the plain-HTTP successor to the retired
	// :443 MITM proxy. Claude Code's managed-settings ANTHROPIC_BASE_URL points
	// at 127.0.0.1:ClaudeGatewayPort. Built at boot so it serves (failing open
	// to real Anthropic) even before enrollment; SetLocalInference/SetDegraded
	// above wire the local path in at activation. Gated on AllowAnthropicAPI +
	// a nonzero port. Non-fatal by design: the WG/mesh data plane is independent
	// of it, and a loopback bind failure must not crash-loop the whole agent.
	if cfgRoot.Inference.AllowAnthropicAPI && cfgRoot.Inference.ClaudeGatewayPort > 0 {
		if claudeSrv, claudeLn, cerr := buildClaudeListener(cfgRoot.Inference.ClaudeGatewayPort, proxyH, claudeRouting, cfgRoot.Inference.ClaudeModelRouteDirectives, logger); cerr != nil {
			logger.Error("claude loopback gateway: bind failed; Claude integration not serving", "err", cerr)
		} else if claudeSrv != nil {
			logger.Info("claude loopback gateway listening", "addr", claudeLn.Addr().String())
			srvWG.Add(1)
			go func() {
				defer srvWG.Done()
				// A Serve error means the loopback listener died while the
				// process lives. It is now the SOLE path for Claude Code (no
				// network-layer fallback), so a dead port would silently break
				// Claude — fatal, to force a clean rebind via systemd restart.
				// (A startup bind failure is handled non-fatally above.)
				if err := claudeSrv.Serve(ctx, claudeLn); err != nil {
					logger.Error("claude loopback gateway stopped; triggering agent restart", "err", err)
					fatal(fmt.Errorf("claude loopback listener: %w", err))
				}
			}()
		}
	}

	// Activate immediately if an identity already exists; otherwise stay
	// up unenrolled and await a login. A failed boot activation logs and
	// continues unenrolled rather than killing the daemon — that is what
	// makes identity-less boot the natural resting state (#180) and lets
	// a subsequent login recover without a manual restart.
	if id, lerr := identity.Load(*stateDir); lerr != nil {
		return fmt.Errorf("load identity: %w", lerr)
	} else if id != nil {
		if aerr := activate(ctx); aerr != nil {
			logger.Error("activate session at boot", "err", aerr)
		}
	} else {
		logger.Info("waired-agent ready (unenrolled); awaiting login", "mgmt", *mgmtAddr)
	}

	<-ctx.Done()
	logger.Info("shutdown signal received")
	sb.current().teardown() // nil-safe when no session was ever published
	srvWG.Wait()
	// Return the fatal listener error (if any) so the process exits
	// non-zero and systemd restarts it. A normal SIGTERM leaves fatalErr
	// nil and exits 0.
	fatalMu.Lock()
	defer fatalMu.Unlock()
	return fatalErr
}

// newPauseInfra resolves the persisted desired phase, loads the
// gateway auth token (the same one the integration package writes into
// env.sh), and returns a pause manager + state writer ready to use.
// The state writer's initial heartbeat is implicitly written by the
// first runStateHeartbeat tick after start, so shells see the agent
// as "active" within a couple of seconds.
func newPauseInfra(stateDir string, gatewayPort int, logger *slog.Logger) (*pauseManager, *state.Writer, error) {
	desired, err := state.ReadDesiredPhase(stateDir)
	if err != nil {
		return nil, nil, fmt.Errorf("read desired-phase: %w", err)
	}
	paths, err := integration.PathsFor(stateDir)
	if err != nil {
		return nil, nil, fmt.Errorf("integration paths: %w", err)
	}
	tok, err := integration.LoadOrCreateGatewayToken(paths.GatewayToken)
	if err != nil {
		return nil, nil, fmt.Errorf("gateway token: %w", err)
	}
	gatewayURL := fmt.Sprintf("http://127.0.0.1:%d", gatewayPort)
	initial := state.State{
		Phase:        desired,
		GatewayURL:   gatewayURL,
		GatewayToken: tok,
	}
	writer := state.NewWriter(stateDir, initial)
	if err := writer.Set(initial); err != nil {
		return nil, nil, fmt.Errorf("seed state file: %w", err)
	}
	pm := newPauseManager(stateDir, writer, desired, logger)
	return pm, writer, nil
}

// runNetworkMapLoop drives a long-running subscriber that reconnects with
// linear backoff (1s -> 5s cap) on disconnect. Each frame is fed to the
// reconciler, which owns wgnet.Engine.UpdatePeers.
//
// When bypassCPIAM is set, the client's HTTP transport injects a GCE
// identity token into Authorization (so the Cloud Run / IAP IAM gate
// is happy) and the device access token rides X-Waired-Agent-Bearer.
func runNetworkMapLoop(ctx context.Context, logger *slog.Logger, id *identity.Identity, bearer func() string, rec *reconciler, meshAgg *inferencemesh.Aggregator, peerDir *peerDirectory, dispatcher testharness.Dispatcher, applyConcurrency func(capacity, parallel int), bypassCPIAM bool) {
	cli := controlclient.NewWithBearer(id.ControlURL, bearer)
	if bypassCPIAM {
		cli.HTTP = bypassCPHTTPClient(ctx, id.ControlURL, logger)
		cli.UseCustomAuthHeader = true
	}
	backoff := time.Second
	const backoffMax = 5 * time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		frames, errs := cli.SubscribeNetworkMap(ctx)
		streamCtx, streamCancel := context.WithCancel(ctx)

		streaming(streamCtx, logger, rec, meshAgg, peerDir, dispatcher, applyConcurrency, frames, errs)
		streamCancel()

		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > backoffMax {
			backoff = backoffMax
		}
	}
}

// applyConcurrency, when non-nil, is invoked on every frame that carries a Self
// InferenceState with (Capacity, DesiredParallel). Capacity is the admission
// ceiling (benchmark-derived unless an admin override is set) and drives the
// overlay listener's gate live; DesiredParallel is non-zero ONLY when an admin
// set inference_max_clients and drives the ollama engine parallelism
// (restart-on-change) — kept distinct so a default host's benchmark capacity
// never restarts its engine. See the applyConcurrency closure at the call site.
func streaming(ctx context.Context, logger *slog.Logger, rec *reconciler, meshAgg *inferencemesh.Aggregator, peerDir *peerDirectory, dispatcher testharness.Dispatcher, applyConcurrency func(capacity, parallel int), frames <-chan *signer.NetworkMap, errs <-chan error) {
	for {
		select {
		case <-ctx.Done():
			return
		case nm, ok := <-frames:
			if !ok {
				return
			}
			if err := rec.Apply(nm); err != nil {
				logger.Error("apply network map", "err", err)
			}
			if meshAgg != nil {
				meshAgg.Update(nm)
			}
			if peerDir != nil {
				peerDir.Update(nm)
			}
			// Apply the CP's effective per-device settings: the admission cap
			// (Capacity) and, only when an admin override set it, the engine
			// parallelism target (DesiredParallel). nil InferenceState (engine
			// not yet probed) leaves both untouched.
			if applyConcurrency != nil && nm.Self.InferenceState != nil {
				applyConcurrency(nm.Self.InferenceState.Capacity, nm.Self.InferenceState.DesiredParallel)
			}
			if dispatcher != nil {
				if s := nm.ActiveTestScenario; s != nil {
					logger.Info("test-harness: network-map frame carries scenario, handing off to dispatcher",
						"scenario", s.ScenarioID, "peer", s.PeerDeviceID, "nonce", s.ExpectedNonce)
				}
				// Apply is non-blocking and never returns an error (the
				// active dispatcher hands the map to its worker; the noop
				// dispatcher is a no-op), so we don't back-pressure this
				// stream loop on a slow scenario apply (#303).
				_ = dispatcher.Apply(ctx, nm)
			}
		case err, ok := <-errs:
			if !ok || err == nil {
				return
			}
			if !errors.Is(err, context.Canceled) {
				logger.Warn("network-map stream ended", "err", err)
			}
			return
		}
	}
}

// bypassCPHTTPClient builds an HTTP client whose transport injects a
// Google identity token (audience = controlURL) into Authorization.
// Used when the CP is fronted by a Cloud Run / IAP IAM gate; the
// device's access token rides X-Waired-Agent-Bearer in this mode.
func bypassCPHTTPClient(ctx context.Context, controlURL string, logger *slog.Logger) *http.Client {
	tr := gcptoken.New(controlURL, nil)
	if !tr.Probe(ctx) {
		logger.Warn("bypass-cp-iam: GCE metadata unreachable, falling back to plain transport (CP must be publicly callable)")
		return &http.Client{Transport: http.DefaultTransport}
	}
	return &http.Client{Transport: tr}
}

// runFallbackLoop ticks the reconciler so it can detect peers stuck on
// the direct UDP path and flip them to relay. The cadence is bounded
// from below by 1 second to avoid spinning.
func runFallbackLoop(ctx context.Context, rec *reconciler, fallbackAfter time.Duration) {
	tick := fallbackAfter / 6
	if tick < time.Second {
		tick = time.Second
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			rec.Tick(ctx)
		}
	}
}

// runDiscoEventLoop drains disco.Service.Events() and dispatches:
//   - EventPongFromPeer → reconciler (upgrade peer to direct UDP)
//   - EventObservedAddr → CP advertise via POST /v1/devices/self/endpoints
//     (so peers can probe back at the freshly-observed addr).
//
// CP advertises are coalesced internally: rapid observed-addr changes
// rate-limit at the CP layer (1 update / 5s, burst 3); the agent does
// no extra throttling.
func runDiscoEventLoop(ctx context.Context, logger *slog.Logger, id *identity.Identity, bearer func() string, mkPriv ed25519.PrivateKey, svc *disco.Service, rec *reconciler, bypassCPIAM bool) {
	cli := controlclient.NewWithBearer(id.ControlURL, bearer)
	if bypassCPIAM {
		cli.HTTP = bypassCPHTTPClient(ctx, id.ControlURL, logger)
		cli.UseCustomAuthHeader = true
	}
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-svc.Events():
			if !ok {
				return
			}
			// All reconciler-bound events go through OnDiscoEvent. The
			// reconciler's switch knows what it can use; we forward
			// everything so we don't have to update both files when a
			// new event type is added (the original v0 dispatch only
			// listed EventPongFromPeer, which silently dropped the
			// EventProbeRTTSampled / EventProbeMissed events PR1
			// introduced — the agent's per-peer RTT EWMAs and miss
			// streaks therefore never moved off zero in production).
			rec.OnDiscoEvent(ev)
			switch e := ev.(type) {
			case disco.EventObservedAddr:
				if !e.Addr.IsValid() {
					continue
				}
				// Relay sockets are dual-stack so an IPv4 source comes
				// back as an IPv4-in-v6 mapped address. Normalize to
				// IPv4 form so the candidate is reachable from the
				// agent's IPv4 WG socket.
				ap := e.Addr
				if a := ap.Addr(); a.Is4In6() {
					ap = netip.AddrPortFrom(a.Unmap(), ap.Port())
				}
				kind := "observed"
				addr := "udp4:" + ap.String()
				if ap.Addr().Is6() {
					kind = "ipv6"
					addr = "udp6:" + ap.String()
				}
				cands := []controlclient.CandidateAdvertise{
					{Addr: addr, Kind: kind, Priority: 1},
				}
				advCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				if err := cli.AdvertiseEndpoints(advCtx, id.DeviceID, cands, mkPriv); err != nil {
					logger.Warn("advertise observed addr", "err", err, "addr", addr)
				} else {
					logger.Info("advertised observed addr", "addr", addr, "nat_type", e.NATType.String())
				}
				cancel()
			case disco.EventNATTypeDetected:
				logger.Info("nat type detected", "kind", e.Kind.String())
			}
		}
	}
}

// localCandidateOptions is the small bag of inputs runLocalCandidateLoop
// needs. Bundled into a struct so the goroutine spawn site stays
// readable as flags accumulate.
type localCandidateOptions struct {
	listenPort     uint16
	ipv6Enabled    bool
	includeULA     bool
	includeIPv4LAN bool
	interval       time.Duration
	bypassCPIAM    bool
}

// runLocalCandidateLoop periodically enumerates the host's reachable
// IPv6 GUA (and optionally ULA + IPv4 LAN) addresses and pushes them to
// the Control Plane as AdvertiseEndpoints candidates. Without this
// loop, peers only see the agent's NAT-mapped v4 candidate from the
// relay STUN echo, leaving v6-direct paths unreachable.
//
// The loop:
//  1. Runs once at startup (so v6 candidates are advertised before the
//     first relay STUN observation completes).
//  2. Re-runs every opts.interval, skipping the POST when the candidate
//     set is identical to the previous tick. CP rate-limits at 1 update
//     / 5s burst 3 server-side; the local diff check keeps that budget
//     for actual changes.
//
// netif.LocalCandidates includes a 200ms Dial("udp6", ...) probe to
// learn the kernel-preferred GUA — that's bounded so a misconfigured
// resolver never stalls agent startup.
func runLocalCandidateLoop(ctx context.Context, logger *slog.Logger, id *identity.Identity, bearer func() string, mkPriv ed25519.PrivateKey, opts localCandidateOptions) {
	cli := controlclient.NewWithBearer(id.ControlURL, bearer)
	if opts.bypassCPIAM {
		cli.HTTP = bypassCPHTTPClient(ctx, id.ControlURL, logger)
		cli.UseCustomAuthHeader = true
	}
	netifOpts := netif.Options{
		ListenPort:  opts.listenPort,
		IncludeIPv6: opts.ipv6Enabled,
		IncludeULA:  opts.includeULA,
		// IPv4 LAN candidates let peers on the same RFC1918 segment
		// dial this agent directly instead of bouncing through the
		// relay. Receivers in different LANs filter these out via
		// pushDiscoSnapshot's subnet-overlap check (see
		// internal/network/netif.AddrInAnyPrefix) so this does not
		// inflate cross-LAN probe traffic. Flag-gated for relay-only
		// deployments.
		IncludeIPv4LAN: opts.includeIPv4LAN,
	}
	var lastKey string
	advertise := func() {
		cands := netif.LocalCandidates(netifOpts)
		if len(cands) == 0 {
			return
		}
		key := candidatesKey(cands)
		if key == lastKey {
			return
		}
		advCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := cli.AdvertiseEndpoints(advCtx, id.DeviceID, cands, mkPriv); err != nil {
			logger.Warn("advertise local candidates", "err", err, "count", len(cands))
			return
		}
		lastKey = key
		addrs := make([]string, 0, len(cands))
		for _, c := range cands {
			addrs = append(addrs, c.Addr)
		}
		logger.Info("advertised local candidates", "count", len(cands), "addrs", addrs)
	}
	advertise()
	interval := opts.interval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			advertise()
		}
	}
}

// candidatesKey is a deterministic fingerprint of a candidate set used
// to skip no-op CP updates. enumerate() already produces stable
// ordering so a string join is sufficient.
func candidatesKey(cands []controlclient.CandidateAdvertise) string {
	var b strings.Builder
	for _, c := range cands {
		b.WriteString(c.Kind)
		b.WriteByte('|')
		b.WriteString(c.Addr)
		b.WriteByte('\n')
	}
	return b.String()
}

// loadDeviceCertificate parses cache/device_certificate.json into a
// signer.DeviceCertificate. Returns the zero value when the file is
// missing — agents enrolled before step8 won't have one cached, and the
// relay path simply stays unused in that case.
func loadDeviceCertificate(path string) (signer.DeviceCertificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return signer.DeviceCertificate{}, nil
		}
		return signer.DeviceCertificate{}, err
	}
	if len(data) == 0 {
		return signer.DeviceCertificate{}, nil
	}
	var cert signer.DeviceCertificate
	if err := json.Unmarshal(data, &cert); err != nil {
		return signer.DeviceCertificate{}, err
	}
	return cert, nil
}

// relayPinLookup is the slice of agentProvider that newRelayClientFactory
// depends on for resolving the URL→TLS-fingerprint mapping at dial
// time. Hoisted to an interface so a future test can stub it.
type relayPinLookup interface {
	RelayTLSFingerprint(url string) string
}

// newRelayClientFactory returns a closure suitable for
// wgnet.Config.RelayClientFactory that builds a relay.Client per URL,
// injecting the agent's identity, keys, and (when known) the TLS
// fingerprint pin from the latest network map. pin may be nil — in
// that case relays must terminate behind a publicly-trusted cert.
func newRelayClientFactory(logger *slog.Logger, id *identity.Identity, mk *devicekeys.MachineKey, nodePubB64 string, bearer func() string, cert signer.DeviceCertificate, pin relayPinLookup) wgnet.RelayClientFactory {
	if id == nil || mk == nil || bearer == nil {
		return nil
	}
	machinePub := mk.PublicBase64()
	machinePriv := ed25519.PrivateKey(append([]byte(nil), mk.Private...))
	return func(url string) (*relayclient.Client, error) {
		var fingerprint string
		if pin != nil {
			fingerprint = pin.RelayTLSFingerprint(url)
		}
		// Read the access token fresh per relay-client construction
		// (one per session). Long-lived relay sessions use ticket
		// auth (not the bearer) after handshake, so swapping the
		// bearer mid-session is unnecessary; new sessions just need
		// the current token to fetch a fresh ticket.
		return relayclient.New(relayclient.Config{
			URL:               url,
			Bearer:            bearer(),
			NetworkID:         id.NetworkID,
			DeviceID:          id.DeviceID,
			MachinePublicKey:  machinePub,
			MachinePrivateKey: machinePriv,
			NodePublicKey:     nodePubB64,
			DeviceCertificate: cert,
			TLSFingerprintHex: fingerprint,
			Logger:            logger.With("component", "relay-client"),
		})
	}
}

// udpListenPortFromEndpoint pulls the port out of "udp4:host:port" or
// "host:port" forms produced by the agent at init time.
func udpListenPortFromEndpoint(s string) (int, error) {
	s = strings.TrimPrefix(s, "udp4:")
	s = strings.TrimPrefix(s, "udp6:")
	_, portStr, err := splitHostPort(s)
	if err != nil {
		return 0, err
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		return 0, fmt.Errorf("port %q: %w", portStr, err)
	}
	if port < 1 || port > 65535 {
		return 0, fmt.Errorf("port out of range: %d", port)
	}
	return port, nil
}

// splitHostPort handles bare "host:port" without bringing in net to keep
// the dependency surface tight.
func splitHostPort(s string) (host, port string, err error) {
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return "", "", fmt.Errorf("missing port in %q", s)
	}
	return s[:i], s[i+1:], nil
}

// --- Phase 9 observability plumbing ---

// observabilityState implements management.ObservabilityStateProvider.
// It captures the agent-wide accessors needed to build a state
// snapshot at handler time without taking any lock — the underlying
// closures are responsible for their own synchronization.
//
// All fields except startedAt and id may be nil; nil accessors are
// treated as "unknown" and produce the zero value for the
// corresponding field in the wire response.
type observabilityState struct {
	startedAt    time.Time
	id           *identity.Identity
	isPaused     func() bool
	isShareDeny  func() bool
	engineReady  func() (bool, string)
	engineInfo   func() (mode, version, warning, tuningWarning string)
	inflight     func() int
	meshSnapshot func() inferencemesh.Snapshot
}

// ObservabilityState builds the per-request gauge snapshot the
// management-API state handler returns. last_inference is left nil
// — the handler fills it from the event ring (newest kind=request).
func (o *observabilityState) ObservabilityState() management.ObservabilityState {
	st := management.ObservabilityState{
		Agent: management.AgentState{
			UptimeSeconds: int64(time.Since(o.startedAt).Seconds()),
		},
	}
	if o.id != nil {
		st.Agent.DeviceID = o.id.DeviceID
	}
	if o.isPaused != nil {
		st.Agent.Paused = o.isPaused()
	}
	if o.isShareDeny != nil {
		st.Agent.ShareEnabled = !o.isShareDeny()
	}
	if o.engineReady != nil {
		ready, model := o.engineReady()
		st.Agent.EngineReady = ready
		st.Agent.ModelID = model
	}
	if o.engineInfo != nil {
		st.Agent.EngineMode, st.Agent.EngineVersion,
			st.Agent.EngineVersionWarning, st.Agent.EngineTuningWarning = o.engineInfo()
	}
	if o.inflight != nil {
		st.Agent.Inflight = o.inflight()
		st.Agent.CapacityUsed = st.Agent.Inflight
	}
	if o.meshSnapshot != nil {
		snap := o.meshSnapshot()
		var enrolled, reachable, ready int
		for _, p := range snap.Peers {
			enrolled++
			if !p.Stale {
				reachable++
				if p.InferenceState != nil && p.InferenceState.Reachable {
					ready++
				}
			}
		}
		st.Mesh.PeersEnrolled = enrolled
		st.Mesh.PeersReachable = reachable
		st.Mesh.PeersReady = ready
	}
	return st
}

// shareDenyFn adapts a *shareController to the bool-returning
// closure the observabilityState reads. nil-safe at both ends.
func shareDenyFn(c *shareController) func() bool {
	if c == nil {
		return nil
	}
	return c.IsShareDenied
}

// engineReadyAccessor wraps the inference subsystem's EngineReady
// method so it can be passed as a closure without leaking the
// concrete type to observabilityState. Returns nil when the
// subsystem itself is nil (i.e. --disable-inference).
func engineReadyAccessor(sub *inferenceSubsystem) func() (bool, string) {
	if sub == nil {
		return func() (bool, string) { return false, "" }
	}
	return sub.EngineReady
}

// engineInfoAccessor exposes the ollama adapter's provenance (mode /
// live version / version warning) to the observability state. nil-safe
// for --disable-inference.
func engineInfoAccessor(sub *inferenceSubsystem) func() (string, string, string, string) {
	if sub == nil {
		return func() (string, string, string, string) { return "", "", "", "" }
	}
	return sub.EngineProvenance
}

// runObservabilityPoller drives the edge-triggered state events on
// the composite Recorder. Every interval it reads the current
// engine / share / paused / mesh state from the supplied accessors
// and pushes them into the Recorder; transitions emit
// engine_state_change events into the ring and update the Prom
// gauges. State that hasn't changed since the last tick is a no-op.
//
// 5 seconds is the default cadence — fast enough that operator
// toggles surface in the tray within a tray-poll cycle, slow enough
// that idle agents don't burn CPU.
func runObservabilityPoller(ctx context.Context, rec *observability.Recorder, st *observabilityState, interval time.Duration) {
	if rec == nil || st == nil {
		return
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	push := func() {
		if st.isPaused != nil {
			rec.SetPaused(st.isPaused(), "")
		}
		if st.isShareDeny != nil {
			rec.SetShareEnabled(!st.isShareDeny(), "")
		}
		if st.engineReady != nil {
			ready, _ := st.engineReady()
			rec.SetEngineReady(ready, "")
		}
		if st.meshSnapshot != nil {
			snap := st.meshSnapshot()
			var enrolled, reachable, ready int
			for _, p := range snap.Peers {
				enrolled++
				if !p.Stale {
					reachable++
					if p.InferenceState != nil && p.InferenceState.Reachable {
						ready++
					}
				}
			}
			rec.SetMeshPeers(enrolled, reachable, ready)
		}
	}
	push()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			push()
		}
	}
}

// --- management API plumbing ---

type agentProvider struct {
	id     *identity.Identity
	engine *wgnet.Engine

	// wgListenPort is the actual UDP port the WireGuard engine bound,
	// derived once from id.Endpoint at startup. Stable for the lifetime
	// of the daemon, so no mutex is needed.
	wgListenPort int

	mu         sync.RWMutex
	peerCount  int
	mapEpoch   int64
	peerByName map[string]*signer.NetworkMapPeer
	// relayTLSPin maps relay URL -> hex SHA-256 fingerprint, refreshed
	// on every Apply(nm). The relay-client factory consults this map
	// before dialing so the TLS-skip-verify-with-fingerprint pin is
	// applied to self-signed relay certs.
	relayTLSPin map[string]string

	// disco is optional. Set once at startup after the disco service
	// is constructed (or left nil when --punch-enabled=false).
	disco *disco.Service

	// reconciler is set once at startup so the management API can
	// surface per-peer path-quality snapshots (current_path, RTT
	// EWMAs, miss streaks). Nil during early init / unit tests that
	// only exercise the provider directly.
	reconciler *reconciler
}

// RelayTLSFingerprint returns the latest network-map fingerprint
// (hex sha256) for the given relay URL, or empty if the URL is
// unknown / fingerprint not advertised. Empty disables pinning and
// falls back to system-trust verification — fine for relays that
// terminate behind a publicly-trusted cert, broken for the
// self-signed deployments waired-relay ships by default.
func (p *agentProvider) RelayTLSFingerprint(url string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.relayTLSPin[url]
}

func (p *agentProvider) Status() management.Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	deviceName := p.id.DeviceName
	if deviceName == "" {
		deviceName = p.id.DeviceID
	}
	st := management.Status{
		NetworkID:  p.id.NetworkID,
		DeviceID:   p.id.DeviceID,
		DeviceName: deviceName,
		OverlayIP:  p.id.OverlayIP,
		ListenPort: p.wgListenPort,
		PeerCount:  p.peerCount,
	}
	if p.disco != nil {
		st.DiscoEnabled = true
		if obs := p.disco.ObservedAddr(); obs.IsValid() {
			st.ObservedAddr = obs.String()
		}
		if obs6 := p.disco.LastObservedV6(); obs6.IsValid() {
			st.ObservedAddrV6 = obs6.String()
		}
		if first := p.disco.FirstObservedV6At(); !first.IsZero() {
			st.FirstObservedV6Unix = first.Unix()
		}
		st.STUNAttemptsV4, st.STUNAttemptsV6, st.STUNResponsesV4, st.STUNResponsesV6 = p.disco.STUNCounters()
		st.NATType = p.disco.NATType().String()
	}
	if p.reconciler != nil {
		snap := p.reconciler.Snapshot()
		st.Peers = make([]management.PeerStatus, 0, len(snap))
		for _, ps := range snap {
			out := management.PeerStatus{
				DeviceID:                ps.DeviceID,
				CurrentPath:             ps.CurrentPath,
				LastSwitchAt:            fmtTime(ps.LastSwitchAt),
				LastSwitchReason:        ps.LastSwitchReason,
				DirectRTTMS:             ps.DirectRTTMS,
				RelayRTTMS:              ps.RelayRTTMS,
				DirectSampleCount:       ps.DirectSampleCount,
				RelaySampleCount:        ps.RelaySampleCount,
				DirectMissStreak:        ps.DirectMissStreak,
				LastDirectEvidence:      fmtTime(ps.LastDirectEvidence),
				HasDiscoHint:            ps.HasDiscoHint,
				ObservedAddr:            ps.ObservedAddr,
				CallMeMaybeSentAt:       fmtTime(ps.CallMeMaybeSentAt),
				CallMeMaybeSentCount:    ps.CallMeMaybeSentCount,
				CallMeMaybeRecvAt:       fmtTime(ps.CallMeMaybeRecvAt),
				CallMeMaybeRecvCount:    ps.CallMeMaybeRecvCount,
				CallMeMaybeFailStreak:   ps.CallMeMaybeFailStreak,
				LastUpgradeRejectReason: ps.LastUpgradeRejectReason,
				RecentDirectPongs:       ps.RecentDirectPongs,
			}
			// Phase 7 follow-up (C1): surface peer device name + hardware
			// from the cached NetworkMap. Same RLock as the surrounding
			// Status() — peerByName is written by replacePeers under
			// Lock and read here under RLock. nil-safe at every level so
			// peers that predate Hardware push (or are CPU-only) emit no
			// hardware field at all rather than a noisy {}.
			if peer, ok := p.peerByName[ps.DeviceID]; ok && peer != nil {
				out.DeviceName = peer.DeviceName
				if peer.InferenceState != nil && peer.InferenceState.Hardware != nil {
					hw := peer.InferenceState.Hardware
					ph := &management.PeerHardware{RAMTotalGB: hw.RAMTotalGB}
					if len(hw.GPUs) > 0 {
						ph.GPUModel = hw.GPUs[0].Model
						ph.VRAMTotalMB = hw.GPUs[0].VRAMTotalMB
						ph.ComputeCap = hw.GPUs[0].ComputeCap
					}
					out.Hardware = ph
				}
			}
			st.Peers = append(st.Peers, out)
		}
	}
	return st
}

// fmtTime formats a time as RFC3339 if non-zero, empty otherwise. Used
// in the management API so JSON consumers see "" instead of the
// reference time for unset fields.
func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// Identity exposes the user-relevant fields from the loaded identity
// for the tray's GET /waired/v1/identity endpoint. The daemon refuses
// to start without an identity (see run() above), so Enrolled is
// always true when this is reachable.
func (p *agentProvider) Identity() management.IdentityView {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.id == nil {
		return management.IdentityView{Enrolled: false}
	}
	deviceName := p.id.DeviceName
	if deviceName == "" {
		deviceName = p.id.DeviceID
	}
	return management.IdentityView{
		Enrolled:     true,
		AccountEmail: p.id.AccountEmail,
		NetworkName:  p.id.NetworkName,
		NetworkID:    p.id.NetworkID,
		DeviceID:     p.id.DeviceID,
		DeviceName:   deviceName,
		OverlayIP:    p.id.OverlayIP,
		ControlURL:   p.id.ControlURL,
	}
}

func (p *agentProvider) replacePeers(nm *signer.NetworkMap) {
	// Collected under p.mu, acted on after release: DropRelay takes the
	// bind's relayMu, and the relay-client factory (which runs under
	// relayMu) reads RelayTLSFingerprint under p.mu — dropping while
	// holding p.mu would invert that lock order and deadlock.
	var repinned []string

	p.mu.Lock()
	p.mapEpoch = nm.MapEpoch
	p.peerCount = len(nm.Peers)
	if p.peerByName == nil {
		p.peerByName = map[string]*signer.NetworkMapPeer{}
	} else {
		for k := range p.peerByName {
			delete(p.peerByName, k)
		}
	}
	for i := range nm.Peers {
		peer := nm.Peers[i]
		// Index by both human-readable name and device_id so management
		// API can resolve either.
		if peer.DeviceName != "" {
			p.peerByName[peer.DeviceName] = &peer
		}
		p.peerByName[peer.DeviceID] = &peer
	}
	// Refresh URL → fingerprint mapping for the relay client factory.
	if p.relayTLSPin == nil {
		p.relayTLSPin = map[string]string{}
	}
	old := p.relayTLSPin
	p.relayTLSPin = make(map[string]string, len(nm.Relays))
	for _, rel := range nm.Relays {
		if rel.URL != "" && rel.TLSFingerprint != "" {
			p.relayTLSPin[rel.URL] = rel.TLSFingerprint
			// A restarted relay presents a fresh self-signed cert under
			// the same URL. Any session still pinned to the old
			// fingerprint is doomed; drop it so the next Send re-dials
			// with the new pin instead of waiting out redial backoff.
			if prev, ok := old[rel.URL]; ok && prev != rel.TLSFingerprint {
				repinned = append(repinned, rel.URL)
			}
		}
	}
	p.mu.Unlock()

	if p.engine != nil {
		if bind := p.engine.Bind(); bind != nil {
			for _, url := range repinned {
				bind.DropRelay(url)
			}
		}
	}
}

type agentPinger struct {
	client   *inference.Client
	provider *agentProvider
}

func (a *agentPinger) PingPeer(ctx context.Context, name string) (management.PingResult, error) {
	a.provider.mu.RLock()
	peer, ok := a.provider.peerByName[name]
	a.provider.mu.RUnlock()
	if !ok {
		return management.PingResult{}, fmt.Errorf("peer %q not in current Network Map", name)
	}
	addr, err := netip.ParseAddr(peer.OverlayIP)
	if err != nil {
		return management.PingResult{}, fmt.Errorf("peer %q has bad overlay_ip: %w", name, err)
	}
	body, latency, err := a.client.Ping(ctx, addr, inferenceServicePort)
	if err != nil {
		return management.PingResult{}, err
	}
	return management.PingResult{
		Peer:           peer.DeviceName,
		OK:             body.OK,
		LatencyMS:      float64(latency.Microseconds()) / 1000.0,
		DeviceFromPeer: body.Device,
		TimeFromPeer:   body.Time,
	}, nil
}

// peekStateDir scans args for a --state-dir / -state-dir value before
// fs.Parse runs. The fallback when no flag is present is
// paths.StateDir(paths.AutoDetect), which itself honors
// $WAIRED_STATE_DIR. Accepted forms:
//
//	-state-dir <path>     --state-dir <path>
//	-state-dir=<path>     --state-dir=<path>
//
// Scanning stops at the literal `--` terminator. Other flags between
// the binary name and --state-dir are skipped over verbatim; we do not
// need full flag-aware parsing because --state-dir takes a single
// string value with no boolean-shorthand confusion.
func peekStateDir(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		name := a
		var inline string
		var hasInline bool
		if eq := strings.IndexByte(a, '='); eq >= 0 {
			name = a[:eq]
			inline = a[eq+1:]
			hasInline = true
		}
		if name != "--state-dir" && name != "-state-dir" {
			continue
		}
		if hasInline {
			return inline
		}
		if i+1 < len(args) {
			return args[i+1]
		}
		break
	}
	return paths.StateDir(paths.AutoDetect)
}
