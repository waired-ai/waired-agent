package tray

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"fyne.io/systray"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/observability"
	"github.com/waired-ai/waired-agent/internal/platform/autostart"
	"github.com/waired-ai/waired-agent/internal/platform/notification"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// notify shows a transient OS-level toast (best-effort; silent
// fallback on backends without a notifier). The title is always
// "Waired" so the user-visible source is consistent.
var notifier = notification.New()

func notify(body string, level notification.Level) {
	_ = notifier.Notify("Waired", body, level)
}

// confirmWithLabels is a seam over the per-OS ConfirmWithLabels dialog so
// the public-consent / kill-switch flows can be exercised without a real
// desktop backend (tests swap it). It is genuinely used by the handlers
// below, so the linter does not flag it as dead.
var confirmWithLabels = ConfirmWithLabels

// iconConnected / iconDisconnected / iconError / iconDegraded are
// defined in icons_unix.go and icons_windows.go: Unix (linux/darwin)
// uses PNG, which fyne.io/systray accepts natively, while Windows
// uses ICO, which is the only format the Win32 tray icon API parses
// reliably (per fyne.io/systray SetIcon godoc).

// Options configures the tray. ControlURL is optional; when empty the
// tray reads it from /v1/identity once enrolled, but a first-time
// "Log in..." action requires either ControlURL or
// $WAIRED_CONTROL_URL to be set.
type Options struct {
	MgmtURL    string
	ControlURL string
	StateDir   string
	Version    string
	BuildSHA   string
	PollEvery  time.Duration // default 5s
}

// Run blocks until the user picks "Quit" (or ctx is cancelled).
// It must be called from the program's main goroutine because the
// underlying systray library has GUI thread-affinity requirements.
func Run(ctx context.Context, opts Options) {
	if opts.PollEvery <= 0 {
		opts.PollEvery = 5 * time.Second
	}
	t := &tray{
		opts:            opts,
		cli:             NewClient(opts.MgmtURL),
		obsSupported:    true, // optimistic; first 404 flips this off
		updateSupported: true, // optimistic; first 404 flips this off (#293)
		autostartMgr:    autostart.New("waired-tray"),
	}
	// Present as a menu-bar-only accessory (no Dock icon / Cmd-Tab
	// entry). No-op off darwin; on darwin this is the analogue of the
	// Windows tray's `-H windowsgui` linker flag. Must run before the
	// AppKit run loop starts so the Dock icon never flashes.
	setActivationPolicyAccessory()
	systray.Run(t.onReady(ctx), func() {})
}

type tray struct {
	opts Options
	cli  *Client

	// Pre-allocated menu items. systray exposes a single linear list
	// of items; we allocate every item we might ever show up front and
	// flip Show/Hide + SetTitle on state changes. New items cannot be
	// inserted between existing ones at runtime, so the order here is
	// the rendered order.
	// Visual dividers between groups are real separators (systray's
	// AddSeparator → type=separator on Linux, native on macOS/Windows),
	// added inline in onReady. They carry no handle and need no
	// Show/Hide bookkeeping: GNOME's PopupSeparatorMenuItem and macOS's
	// NSMenu auto-collapse separators that end up leading, trailing, or
	// adjacent once a neighbouring group is hidden. (They used to be
	// empty-title menu items, which render as blank rows on every
	// backend — see issue #281.)
	miHeader       *systray.MenuItem
	miEmail        *systray.MenuItem
	miStatus       *systray.MenuItem // renders MenuModel.StatusMsg (daemon-down hint / login code / error); hidden when empty (waired#808)
	miUpdate       *systray.MenuItem // "⚠ Update available — install vX"; hidden when current (#293)
	miUpdateNotify *systray.MenuItem // "✓ Notify me about updates"; under the banner, hidden when current (#294)
	miToggle       *systray.MenuItem
	// miInference is the "Inference ▸" submenu parent (waired#809). The
	// engine/share/mesh/worker/recommend rows below are its children instead
	// of top-level rows, so the top level stays short. Shown when
	// ShowInferenceMenu is set.
	miInference       *systray.MenuItem
	miInferenceToggle *systray.MenuItem
	miInferenceState  *systray.MenuItem
	miEngineToggle    *systray.MenuItem
	miInstallEngine   *systray.MenuItem
	miShareToggle     *systray.MenuItem
	miShareState      *systray.MenuItem
	miMeshReachable   *systray.MenuItem
	miEngineWarning   *systray.MenuItem
	miActiveModel     *systray.MenuItem
	// miDeviceLabel is the "This device ▸" submenu parent (waired#809);
	// name / IP / network / peers are its children.
	miDeviceLabel *systray.MenuItem
	miDeviceName  *systray.MenuItem
	miOverlayIP   *systray.MenuItem
	miNetwork     *systray.MenuItem
	miPeers       *systray.MenuItem
	// Claude integration group — pre-allocated even on daemons that
	// do not expose the endpoint; each item Hides itself in apply()
	// when the corresponding model field is empty. Since the transparent
	// proxy became the sole Claude-routing method on Linux, this group
	// reports proxy status (header + one proxy row) — the retired
	// alias/IDE-wrapper rows and the `waired claude` diagnose action are
	// gone.
	miClaudeHeader *systray.MenuItem
	miClaudeProxy  *systray.MenuItem

	// Claude Code per-class routing submenu (#649/#650). miClaudeCode is
	// the "Claude Code" parent; under it two disabled header rows label the
	// "Main conversation" and "Subagents" groups, each followed by fixed
	// route slots (main: auto/waired/anthropic; sub: same/auto/waired/
	// anthropic — systray can't grow a submenu after onReady, so both are
	// pre-allocated). miClaudeFallbackNote / miClaudeEnableNote are disabled
	// rows shown conditionally. last* slices back the click dispatch.
	miClaudeCode         *systray.MenuItem
	miClaudeMainHeader   *systray.MenuItem
	miClaudeMainRoutes   []*systray.MenuItem // 3 slots: auto / waired / anthropic
	miClaudeSubHeader    *systray.MenuItem
	miClaudeSubRoutes    []*systray.MenuItem // 4 slots: same / auto / waired / anthropic
	miClaudeFallbackNote *systray.MenuItem
	miClaudeEnableNote   *systray.MenuItem
	lastClaudeMainRoutes []ClaudeRouteRow // Class lookup for main-route click dispatch
	lastClaudeSubRoutes  []ClaudeRouteRow // Class lookup for sub-route click dispatch

	// OpenCode integration group — symmetric pre-allocation. The
	// Reconfigure click is the only interactive item; the rest are
	// status rows.
	miOpenCodeHeader      *systray.MenuItem
	miOpenCodeConfig      *systray.MenuItem
	miOpenCodeReconfigure *systray.MenuItem

	// OpenClaw integration group — same shape as the OpenCode group.
	miOpenClawHeader      *systray.MenuItem
	miOpenClawConfig      *systray.MenuItem
	miOpenClawReconfigure *systray.MenuItem

	// Catalog (model selector) submenu — Tailscale-style nested menu
	// under a "Models" parent. Items are pre-allocated up to
	// MaxCatalogEntries; the projection slice tells apply() how many
	// to keep visible. Hidden on daemons that do not expose
	// /waired/v1/inference/catalog.
	miCatalogActive    *systray.MenuItem // "Active: Qwen3 8B Instruct" — top-level
	miCatalog          *systray.MenuItem // "Models" — submenu parent
	miCatalogEntries   []*systray.MenuItem
	lastCatalogEntries []CatalogEntryView // ModelID lookup for click dispatch

	// Benchmark step-down recommendation (#133). miRecommend is a
	// top-level "⚠ Lighter model recommended…" row shown only while the
	// daemon reports a non-dismissed suggestion; clicking it re-opens the
	// confirmation popup. lastRecommendation is the live recommendation
	// the click handler / popup act on; lastRecPopupKey de-dupes the
	// once-per-recommendation proactive popup.
	miRecommend        *systray.MenuItem
	lastRecommendation *management.BenchmarkRecommendation
	lastRecPopupKey    string

	// Inference-worker (manual routing) submenu — Tailscale-exit-node-
	// style. miWorkerActive is the top-level "Worker: linux-gpu (pinned)"
	// summary; miWorker is the parent of the "Inference worker" submenu.
	// Mode rows are fixed slots (auto / local-only / peer-preferred);
	// pin rows are MaxWorkerPinEntries dynamic slots driven by the mesh
	// snapshot. miWorkerClearPin only shows when mode==pinned.
	miWorkerActive       *systray.MenuItem
	miWorker             *systray.MenuItem
	miWorkerModes        []*systray.MenuItem // 3 entries: auto / local-only / peer-preferred
	miWorkerPinEntries   []*systray.MenuItem
	miWorkerClearPin     *systray.MenuItem
	lastWorkerModes      []WorkerModeRow      // Mode lookup for click dispatch
	lastWorkerPinEntries []WorkerPinEntryView // DeviceID lookup for pin click dispatch

	// Public share submenu (waired#833). miPublicShare is a NEW top-level
	// parent (the Windows systray backend won't render three nesting
	// levels, so every row below is a FLAT level-2 child). miPublicShareToggle
	// is the provider kill switch; miPublicShareState / miPublicShareNote are
	// display-only (Disable()). miPublicUseHeader is a disabled section label
	// for the consumer group, followed by exactly three mode rows
	// (off/auto/explicit). miPublicMore opens the served "Privacy & safety…"
	// link. lastPublicUseModes backs the mode-row click dispatch under t.mu.
	miPublicShare       *systray.MenuItem
	miPublicShareToggle *systray.MenuItem
	miPublicShareState  *systray.MenuItem
	miPublicShareNote   *systray.MenuItem
	miPublicUseHeader   *systray.MenuItem
	miPublicUseModes    []*systray.MenuItem // 3 entries: off / auto / explicit
	miPublicMore        *systray.MenuItem
	lastPublicUseModes  []PublicUseModeRow

	miCodeUI *systray.MenuItem
	miAdmin  *systray.MenuItem
	// miSettings is the "Settings ▸" submenu parent (waired#809): the
	// OpenCode/OpenClaw integration rows, Recent activity, autostart toggle,
	// About, and Log out live under it instead of at the top level.
	miSettings  *systray.MenuItem
	miAbout     *systray.MenuItem
	miAutostart *systray.MenuItem
	miLogout    *systray.MenuItem
	miQuit      *systray.MenuItem

	// autostartMgr toggles the per-user "run waired-tray on login"
	// registration via internal/platform/autostart. Initialised in
	// onReady, queried on every poll so the menu label tracks the
	// current Run-key / .desktop file presence.
	autostartMgr autostart.Manager

	// Recent activity (Phase 9 observability) submenu — pre-allocated
	// MaxRecentActivity slots under a single parent. Hidden entirely
	// when the daemon predates Phase 9, or when no kind=fallback
	// events fell inside RecentFallbackWindow.
	miRecent        *systray.MenuItem
	miRecentEntries []*systray.MenuItem

	// Peer-hardware submenu (Phase 7 follow-up C1b) — pre-allocated
	// MaxPeerHardwareRows slots as children of miPeers + one extra
	// overflow slot for "+N more". Hidden when no peer has published
	// Hardware (old daemons / CPU-only meshes), in which case miPeers
	// stays a bare "Peers: N" label.
	miPeerEntries  []*systray.MenuItem
	miPeerOverflow *systray.MenuItem

	mu   sync.Mutex
	last MenuModel

	// Model-switch grace state (mu-protected, waired#808). lastOnline is
	// the most recent model rendered while the daemon was reachable;
	// switchingUntil, when in the future, marks the window during which an
	// unreachable daemon is shown as "Switching model…" (keeping the last
	// online menu) instead of the red agent-down state — covering the
	// supervised restart a restart-fallback model switch triggers.
	lastOnline     MenuModel
	switchingUntil time.Time

	// Observability poll state (mu-protected). recentFallbacks is the
	// rolling buffer the projection's RecentFallbackWindow filters at
	// render time. obsCursor is the next_since returned by the previous
	// /events poll. obsSupported flips to false on the first 404 so we
	// stop dialing /events every 5 s on legacy daemons.
	recentFallbacks []FallbackEntry
	obsCursor       uint64
	obsSupported    bool

	// lastPublicNudgeSeq de-dupes the one-shot pre-consent Public Share
	// nudge toast (waired#833). The daemon emits KindPublicShareNudge at
	// most once per process, but the tray still re-reads it whenever the
	// first-poll since=0 window replays the ring, so we key the toast on
	// the event Seq (same shape as lastRecPopupKey) and fire only on a Seq
	// we have not shown before. mu-protected.
	lastPublicNudgeSeq uint64

	// Daemon-driven login state (mu-protected). loginSessionID is the
	// session returned by LoginStart; while non-empty, pollOnce polls
	// LoginStatus and folds it into the snapshot. loginURLOpened guards
	// the one-shot browser open so we don't re-launch a tab every tick.
	loginSessionID string
	loginURLOpened bool

	// Update poll state (mu-protected). updateSupported flips to false on
	// the first 404 so we stop dialing the update API on legacy daemons.
	// updateSeeded gates the one POST /update/check that seeds the daemon's
	// cache (subsequent polls read the cheap /status; the daemon's #294
	// background loop refreshes it thereafter). lastNotifiedUpdateVersion +
	// lastNotifiedUpdateAt drive the toast cadence: once per newly-seen
	// version, then a bounded re-reminder every updateRenotifyInterval while
	// the same version stays pending — never every poll. #293/#294.
	updateSupported           bool
	updateSeeded              bool
	lastNotifiedUpdateVersion string
	lastNotifiedUpdateAt      time.Time
}

func (t *tray) onReady(ctx context.Context) func() {
	return func() {
		systray.SetTitle("Waired")
		systray.SetTooltip("Waired")
		systray.SetIcon(iconErrorIcon) // start grey until first poll proves daemon up

		t.miHeader = systray.AddMenuItem("Connecting…", "")
		t.miHeader.Disable()
		t.miEmail = systray.AddMenuItem("", "")
		t.miEmail.Disable()
		// Status / hint line (waired#808): renders MenuModel.StatusMsg —
		// the daemon-down "Start-Service…" hint, the login user-code, or an
		// error reason. Hidden by default so the initial (false,false)
		// visibility diff is a no-op and a healthy menu never shows a blank
		// row.
		t.miStatus = systray.AddMenuItem("", "")
		t.miStatus.Disable()
		t.miStatus.Hide()
		// Manual-update banner (#293). Prominent near the top, like
		// Tailscale's "Update available". Hidden by default — the initial
		// (false,false) visibility diff is a no-op, so an up-to-date host
		// (or a daemon predating the update API) never shows a blank row.
		t.miUpdate = systray.AddMenuItem("", "Install the available Waired update")
		t.miUpdate.Hide()
		// Update-prompt toggle (#294). Sits directly beneath the banner so a
		// user being prompted can silence it in place. Hidden by default —
		// the initial (false,false) visibility diff is a no-op, so a current
		// host (or a daemon predating the settings API) never shows it.
		t.miUpdateNotify = systray.AddMenuItem("", "Toggle the proactive notification when a Waired update is available")
		t.miUpdateNotify.Hide()
		systray.AddSeparator()
		t.miToggle = systray.AddMenuItem("", "")
		systray.AddSeparator()
		// --- Models (top-level): switching models is a primary action, so
		// the catalog stays out of a submenu. "Active: <model>" sits above
		// the "Models" picker (waired#809).
		t.miCatalogActive = systray.AddMenuItem("", "")
		t.miCatalogActive.Disable()
		t.miCatalogActive.Hide()
		t.miCatalog = systray.AddMenuItem("Models", "Choose a different inference model")
		t.miCatalog.Hide()
		t.miCatalogEntries = make([]*systray.MenuItem, MaxCatalogEntries)
		for i := 0; i < MaxCatalogEntries; i++ {
			t.miCatalogEntries[i] = t.miCatalog.AddSubMenuItem("", "Switch the active inference model")
			t.miCatalogEntries[i].Hide()
		}

		// --- Inference submenu (waired#809): the engine power / share / mesh
		// status rows and the worker-routing submenu move off the top level
		// into one "Inference" parent. Shown when ShowInferenceMenu is set
		// (the daemon exposes the inference API); apply() fills the rows.
		// Each row keeps its prior Disable()/Hide() baseline so the first
		// paint's (false,false) visibility diffs stay no-ops.
		t.miInference = systray.AddMenuItem("Inference", "Local inference engine status and controls")
		t.miInference.Hide()
		t.miInferenceToggle = t.miInference.AddSubMenuItem("", "")
		t.miInferenceState = t.miInference.AddSubMenuItem("", "")
		t.miInferenceState.Disable()
		t.miEngineToggle = t.miInference.AddSubMenuItem("", "Hard-stop the engine to free memory, or restart it")
		t.miEngineToggle.Disable()
		t.miEngineToggle.Hide()
		t.miInstallEngine = t.miInference.AddSubMenuItem("", "Download and install the local inference engine")
		t.miInstallEngine.Hide()
		t.miShareToggle = t.miInference.AddSubMenuItem("", "")
		t.miShareState = t.miInference.AddSubMenuItem("", "")
		t.miShareState.Disable()
		t.miMeshReachable = t.miInference.AddSubMenuItem("", "")
		t.miMeshReachable.Disable()
		t.miMeshReachable.Hide()
		t.miEngineWarning = t.miInference.AddSubMenuItem("", "Engine provenance warning (version mismatch / port conflict)")
		t.miEngineWarning.Disable()
		t.miEngineWarning.Hide()
		t.miActiveModel = t.miInference.AddSubMenuItem("", "")
		t.miActiveModel.Disable()
		t.miActiveModel.Hide()
		t.miRecommend = t.miInference.AddSubMenuItem("", "This host benchmarks below the interactive floor; a lighter model is recommended")
		t.miRecommend.Hide()
		t.miWorkerActive = t.miInference.AddSubMenuItem("", "")
		t.miWorkerActive.Disable()
		t.miWorkerActive.Hide()
		// Worker routing rows are flattened directly under Inference (not a
		// nested submenu): fyne.io/systray's Windows backend does not render
		// items nested three levels deep, so miWorker is now just a disabled
		// section label and the mode/pin/clear rows are siblings of the
		// engine rows (waired#809). Click dispatch is unchanged — it keys off
		// the item pointers.
		t.miWorker = t.miInference.AddSubMenuItem("Inference worker", "Where outbound inference flows (Tailscale-exit-node style)")
		t.miWorker.Disable()
		t.miWorker.Hide()
		t.miWorkerModes = make([]*systray.MenuItem, 3)
		for i := 0; i < 3; i++ {
			t.miWorkerModes[i] = t.miInference.AddSubMenuItem("", "Set the routing mode")
			t.miWorkerModes[i].Hide()
		}
		t.miWorkerPinEntries = make([]*systray.MenuItem, MaxWorkerPinEntries)
		for i := 0; i < MaxWorkerPinEntries; i++ {
			t.miWorkerPinEntries[i] = t.miInference.AddSubMenuItem("", "Pin outbound inference to this peer")
			t.miWorkerPinEntries[i].Hide()
		}
		t.miWorkerClearPin = t.miInference.AddSubMenuItem("(clear pin)", "Return to auto routing")
		t.miWorkerClearPin.Hide()

		// --- Public share submenu (waired#833): a NEW top-level "Public
		// share" parent. All rows are FLAT level-2 children (the Windows
		// systray backend won't render three nesting levels — same limit as
		// the worker rows above). Hidden until the daemon proves it exposes
		// the public endpoints (apply() tracks ShowPublicShareMenu); each row
		// keeps its Disable()/Hide() baseline so the first paint's
		// (false,false) visibility diffs stay no-ops.
		t.miPublicShare = systray.AddMenuItem("Public share", "Share this computer publicly, and choose whether to use other people's public computers")
		t.miPublicShare.Hide()
		t.miPublicShareToggle = t.miPublicShare.AddSubMenuItem("", "Turn public sharing of this computer on or off")
		t.miPublicShareToggle.Hide()
		t.miPublicShareState = t.miPublicShare.AddSubMenuItem("", "")
		t.miPublicShareState.Disable()
		t.miPublicShareState.Hide()
		t.miPublicShareNote = t.miPublicShare.AddSubMenuItem("", "")
		t.miPublicShareNote.Disable()
		t.miPublicShareNote.Hide()
		t.miPublicUseHeader = t.miPublicShare.AddSubMenuItem("", "")
		t.miPublicUseHeader.Disable()
		t.miPublicUseHeader.Hide()
		t.miPublicUseModes = make([]*systray.MenuItem, 3)
		for i := 0; i < 3; i++ {
			t.miPublicUseModes[i] = t.miPublicShare.AddSubMenuItem("", "Choose how this computer uses other people's public computers")
			t.miPublicUseModes[i].Hide()
		}
		t.miPublicMore = t.miPublicShare.AddSubMenuItem("Privacy & safety…", "Open the Public Share privacy and safety notes")
		t.miPublicMore.Hide()

		// --- Claude Code submenu (waired#809): the Claude status header +
		// managed-settings row fold into the same parent as the per-class
		// route selectors, so no Claude detail sits at the top level. The
		// parent is shown when ShowClaude || ShowClaudeCode (see apply()).
		t.miClaudeCode = systray.AddMenuItem("Claude Code", "Claude Code routing status and per-class route selection")
		t.miClaudeCode.Hide()
		t.miClaudeHeader = t.miClaudeCode.AddSubMenuItem("", "")
		t.miClaudeHeader.Disable()
		t.miClaudeProxy = t.miClaudeCode.AddSubMenuItem("", "Claude Code managed-settings status (waired claude enable / disable / status)")
		t.miClaudeProxy.Disable()
		t.miClaudeMainHeader = t.miClaudeCode.AddSubMenuItem("Main conversation", "The route for Claude Code's main conversation")
		t.miClaudeMainHeader.Disable()
		t.miClaudeMainRoutes = make([]*systray.MenuItem, 3)
		for i := range t.miClaudeMainRoutes {
			t.miClaudeMainRoutes[i] = t.miClaudeCode.AddSubMenuItem("", "Set the main-conversation route")
			t.miClaudeMainRoutes[i].Hide()
		}
		t.miClaudeSubHeader = t.miClaudeCode.AddSubMenuItem("Subagents", "The route for Claude Code's bulk subagents")
		t.miClaudeSubHeader.Disable()
		t.miClaudeSubRoutes = make([]*systray.MenuItem, 4)
		for i := range t.miClaudeSubRoutes {
			t.miClaudeSubRoutes[i] = t.miClaudeCode.AddSubMenuItem("", "Set the subagent route")
			t.miClaudeSubRoutes[i].Hide()
		}
		t.miClaudeFallbackNote = t.miClaudeCode.AddSubMenuItem("", "The last time Claude Code's chosen route could not serve")
		t.miClaudeFallbackNote.Disable()
		t.miClaudeFallbackNote.Hide()
		t.miClaudeEnableNote = t.miClaudeCode.AddSubMenuItem("", "Claude Code is not yet routed through Waired")
		t.miClaudeEnableNote.Disable()
		t.miClaudeEnableNote.Hide()

		systray.AddSeparator()
		// Bundled coding-agent stays a top-level primary action (#429).
		t.miCodeUI = systray.AddMenuItem("Open Coding Agent…", "Open the bundled OpenCode coding agent in your browser")
		t.miCodeUI.Hide()
		systray.AddSeparator()

		// --- This device submenu (waired#809): name / IP / network / peers
		// move under one parent. Shown only when enrolled — apply() tracks
		// the parent in the device-visibility group, so it starts hidden.
		t.miDeviceLabel = systray.AddMenuItem("This device", "This device's name, address, and mesh peers")
		t.miDeviceLabel.Hide()
		t.miDeviceName = t.miDeviceLabel.AddSubMenuItem("", "")
		t.miDeviceName.Disable()
		t.miOverlayIP = t.miDeviceLabel.AddSubMenuItem("", "Click to copy")
		t.miNetwork = t.miDeviceLabel.AddSubMenuItem("", "")
		t.miNetwork.Disable()
		// Peers: the "Peers: N" label plus per-peer hardware rows are all
		// flat children of This device (no third nesting level, per the
		// Windows-backend limit above). miPeers stays a disabled label.
		t.miPeers = t.miDeviceLabel.AddSubMenuItem("", "")
		t.miPeers.Disable()
		t.miPeerEntries = make([]*systray.MenuItem, MaxPeerHardwareRows)
		for i := range MaxPeerHardwareRows {
			t.miPeerEntries[i] = t.miDeviceLabel.AddSubMenuItem("", "")
			t.miPeerEntries[i].Disable()
			t.miPeerEntries[i].Hide()
		}
		t.miPeerOverflow = t.miDeviceLabel.AddSubMenuItem("", "")
		t.miPeerOverflow.Disable()
		t.miPeerOverflow.Hide()

		t.miAdmin = systray.AddMenuItem("Open Admin Console…", "Open the Waired Control Plane admin UI")

		// --- Settings submenu (waired#809): the OpenCode / OpenClaw
		// integration rows, Recent activity, the startup toggle, About, and
		// Log out move off the top level. The parent is always visible (About
		// and the startup toggle are always available); each row keeps its
		// own Show/Hide.
		t.miSettings = systray.AddMenuItem("Settings", "Integrations, startup, and account")
		t.miOpenCodeHeader = t.miSettings.AddSubMenuItem("", "")
		t.miOpenCodeHeader.Disable()
		t.miOpenCodeConfig = t.miSettings.AddSubMenuItem("", "")
		t.miOpenCodeConfig.Disable()
		t.miOpenCodeReconfigure = t.miSettings.AddSubMenuItem("", "Re-apply `waired link opencode` after a confirmation prompt")
		t.miOpenClawHeader = t.miSettings.AddSubMenuItem("", "")
		t.miOpenClawHeader.Disable()
		t.miOpenClawConfig = t.miSettings.AddSubMenuItem("", "")
		t.miOpenClawConfig.Disable()
		t.miOpenClawReconfigure = t.miSettings.AddSubMenuItem("", "Re-apply `waired link openclaw` after a confirmation prompt")
		// Recent activity: a disabled "Recent activity" label plus its rows,
		// flat under Settings (no third nesting level, per the Windows-backend
		// limit above).
		t.miRecent = t.miSettings.AddSubMenuItem("Recent activity", "Inference fallbacks observed in the last 10 minutes")
		t.miRecent.Disable()
		t.miRecent.Hide()
		t.miRecentEntries = make([]*systray.MenuItem, MaxRecentActivity)
		for i := 0; i < MaxRecentActivity; i++ {
			t.miRecentEntries[i] = t.miSettings.AddSubMenuItem("", "")
			t.miRecentEntries[i].Disable()
			t.miRecentEntries[i].Hide()
		}
		t.miAbout = t.miSettings.AddSubMenuItem("About Waired", "")
		t.miAutostart = t.miSettings.AddSubMenuItem("Start Waired on login", "Toggle launching the tray when you sign in")
		t.refreshAutostartLabel()
		t.ensureAutostartOnFirstLaunch()
		t.miLogout = t.miSettings.AddSubMenuItem("Log out…", "Sign this device out and remove its identity")

		systray.AddSeparator()
		t.miQuit = systray.AddMenuItem("Quit", "Exit the Waired tray")

		// Catalog submenu items each have their own ClickedCh; spawning
		// one goroutine per slot avoids inflating the main click select
		// with a dozen extra cases.
		for i := 0; i < MaxCatalogEntries; i++ {
			idx := i
			go t.dispatchCatalogClicks(ctx, idx)
		}
		// Same goroutine-per-slot pattern for worker submenu: 3 mode
		// slots + MaxWorkerPinEntries pin slots + 1 clear-pin slot.
		// 20 goroutines blocked on ClickedCh is negligible compared to
		// the cost of growing the main handleClicks select case-by-case.
		for i := 0; i < len(t.miWorkerModes); i++ {
			idx := i
			go t.dispatchWorkerModeClicks(ctx, idx)
		}
		for i := 0; i < MaxWorkerPinEntries; i++ {
			idx := i
			go t.dispatchWorkerPinClicks(ctx, idx)
		}
		go t.dispatchWorkerClearPinClicks(ctx)
		go t.dispatchRecommendClicks(ctx)
		// Claude Code route selectors: one goroutine per fixed slot (3 main
		// + 4 sub), same pattern as the worker mode rows.
		for i := 0; i < len(t.miClaudeMainRoutes); i++ {
			idx := i
			go t.dispatchClaudeMainRouteClicks(ctx, idx)
		}
		for i := 0; i < len(t.miClaudeSubRoutes); i++ {
			idx := i
			go t.dispatchClaudeSubRouteClicks(ctx, idx)
		}
		// Public-use mode rows (off / auto / explicit): one goroutine per
		// fixed slot, same pattern as the worker mode rows (waired#833).
		for i := 0; i < len(t.miPublicUseModes); i++ {
			idx := i
			go t.dispatchPublicUseModeClicks(ctx, idx)
		}

		go t.handleClicks(ctx)
		go t.pollLoop(ctx)
	}
}

func (t *tray) dispatchCatalogClicks(ctx context.Context, idx int) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.miCatalogEntries[idx].ClickedCh:
			t.onSelectCatalogEntry(ctx, idx)
		}
	}
}

// onSelectCatalogEntry maps a click on the i-th submenu slot back to
// the model_id projected at that slot in the most recent apply(), then
// posts the preference to the agent. The agent restarts asynchronously;
// the immediate poll repaints the row with "(switching…)" until the
// new active selection comes back from the catalog endpoint.
func (t *tray) onSelectCatalogEntry(ctx context.Context, idx int) {
	t.mu.Lock()
	var modelID string
	var disabled bool
	if idx < len(t.lastCatalogEntries) {
		modelID = t.lastCatalogEntries[idx].ModelID
		disabled = t.lastCatalogEntries[idx].Disabled
	}
	t.mu.Unlock()
	if modelID == "" || disabled {
		return
	}
	resp, err := t.cli.SetPreferredModel(ctx, modelID)
	if err != nil {
		ShowError(fmt.Sprintf("Switch model failed: %v", err))
		return
	}
	t.onModelSwitchAccepted(resp)
	go t.pollOnce(ctx)
}

// onModelSwitchAccepted gives the user feedback for an accepted model
// switch and, when the daemon reports it will restart (the restart
// fallback — an in-process swap per waired#812 does not), arms the
// grace window so the imminent daemon-down poll renders as "Switching
// model…" instead of the red agent-down state (waired#808).
func (t *tray) onModelSwitchAccepted(resp *management.PreferredModelResponse) {
	if resp != nil && resp.WillRestart {
		notify("Switching model — the agent will restart briefly.", notification.Info)
		t.armSwitching()
		return
	}
	notify("Model switched.", notification.Info)
}

// armSwitching opens the ~45s model-switch grace window (waired#808).
func (t *tray) armSwitching() {
	t.mu.Lock()
	t.switchingUntil = time.Now().Add(45 * time.Second)
	t.mu.Unlock()
}

// dispatchWorkerModeClicks handles clicks on the auto / local-only /
// peer-preferred rows. Mirrors dispatchCatalogClicks one-goroutine-
// per-slot pattern.
func (t *tray) dispatchWorkerModeClicks(ctx context.Context, idx int) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.miWorkerModes[idx].ClickedCh:
			t.onSelectWorkerMode(ctx, idx)
		}
	}
}

func (t *tray) dispatchWorkerPinClicks(ctx context.Context, idx int) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.miWorkerPinEntries[idx].ClickedCh:
			t.onSelectWorkerPin(ctx, idx)
		}
	}
}

func (t *tray) dispatchWorkerClearPinClicks(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.miWorkerClearPin.ClickedCh:
			t.onWorkerClearPin(ctx)
		}
	}
}

func (t *tray) onSelectWorkerMode(ctx context.Context, idx int) {
	t.mu.Lock()
	var mode state.RoutingMode
	if idx < len(t.lastWorkerModes) {
		mode = t.lastWorkerModes[idx].Mode
	}
	t.mu.Unlock()
	if mode == "" {
		return
	}
	if _, err := t.cli.SetWorker(ctx, management.WorkerRequest{Mode: mode}); err != nil {
		ShowError(fmt.Sprintf("Set worker mode failed: %v", err))
		return
	}
	go t.pollOnce(ctx)
}

func (t *tray) onSelectWorkerPin(ctx context.Context, idx int) {
	t.mu.Lock()
	var entry WorkerPinEntryView
	if idx < len(t.lastWorkerPinEntries) {
		entry = t.lastWorkerPinEntries[idx]
	}
	t.mu.Unlock()
	if entry.DeviceID == "" {
		return
	}
	if _, err := t.cli.SetWorker(ctx, management.WorkerRequest{
		Mode:               state.RoutingModePinned,
		PinnedPeerDeviceID: entry.DeviceID,
	}); err != nil {
		ShowError(fmt.Sprintf("Pin worker failed: %v", err))
		return
	}
	go t.pollOnce(ctx)
}

func (t *tray) onWorkerClearPin(ctx context.Context) {
	if _, err := t.cli.SetWorker(ctx, management.WorkerRequest{Mode: state.RoutingModeAuto}); err != nil {
		ShowError(fmt.Sprintf("Clear pin failed: %v", err))
		return
	}
	go t.pollOnce(ctx)
}

// dispatchClaudeMainRouteClicks / dispatchClaudeSubRouteClicks block on one
// route slot's ClickedCh, mirroring dispatchWorkerModeClicks. The slot index
// maps to lastClaudeMainRoutes / lastClaudeSubRoutes under the lock so a
// concurrent poll rebuild can't tear the lookup.
func (t *tray) dispatchClaudeMainRouteClicks(ctx context.Context, idx int) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.miClaudeMainRoutes[idx].ClickedCh:
			t.onSelectClaudeRoute(ctx, state.ClaudeClassMain, idx)
		}
	}
}

func (t *tray) dispatchClaudeSubRouteClicks(ctx context.Context, idx int) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.miClaudeSubRoutes[idx].ClickedCh:
			t.onSelectClaudeRoute(ctx, state.ClaudeClassSub, idx)
		}
	}
}

// onSelectClaudeRoute POSTs a single class's route change (the other class is
// left untouched via a nil pointer) then triggers a poll so the ●/○ marks and
// any fallback note refresh from the authoritative daemon state.
func (t *tray) onSelectClaudeRoute(ctx context.Context, class string, idx int) {
	t.mu.Lock()
	rows := t.lastClaudeMainRoutes
	if class == state.ClaudeClassSub {
		rows = t.lastClaudeSubRoutes
	}
	var route state.ClaudeRouteClass
	if idx < len(rows) {
		route = rows[idx].Class
	}
	t.mu.Unlock()
	if route == "" {
		return
	}
	var req management.ClaudeRoutingRequest
	if class == state.ClaudeClassSub {
		req.Sub = &route
	} else {
		req.Main = &route
	}
	if _, err := t.cli.SetClaudeRouting(ctx, req); err != nil {
		ShowError(fmt.Sprintf("Set Claude route failed: %v", err))
		return
	}
	go t.pollOnce(ctx)
}

func (t *tray) handleClicks(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.miToggle.ClickedCh:
			t.onToggle(ctx)
		case <-t.miUpdate.ClickedCh:
			go t.onUpdate(ctx)
		case <-t.miUpdateNotify.ClickedCh:
			go t.onUpdateNotifyToggle(ctx)
		case <-t.miInferenceToggle.ClickedCh:
			t.onInferenceToggle(ctx)
		case <-t.miEngineToggle.ClickedCh:
			t.onEngineToggle(ctx)
		case <-t.miInstallEngine.ClickedCh:
			go t.onInstallEngine(ctx)
		case <-t.miShareToggle.ClickedCh:
			t.onShareToggle(ctx)
		case <-t.miPublicShareToggle.ClickedCh:
			// Dialog + HTTP: dispatch so the kill-switch confirmation and
			// the enable side-effect note never stall the click loop.
			go t.onPublicShareToggle(ctx)
		case <-t.miPublicMore.ClickedCh:
			go t.onPublicMore()
		case <-t.miOverlayIP.ClickedCh:
			t.onCopyIP()
		case <-t.miOpenCodeReconfigure.ClickedCh:
			go t.onReconfigureOpenCode(ctx)
		case <-t.miOpenClawReconfigure.ClickedCh:
			go t.onReconfigureOpenClaw(ctx)
		case <-t.miCodeUI.ClickedCh:
			go t.onCodeUI(ctx)
		case <-t.miAdmin.ClickedCh:
			t.onAdmin()
		case <-t.miAbout.ClickedCh:
			ShowAbout(t.opts.Version, t.opts.BuildSHA)
		case <-t.miAutostart.ClickedCh:
			t.onToggleAutostart()
		case <-t.miLogout.ClickedCh:
			t.onLogout(ctx)
		case <-t.miQuit.ClickedCh:
			t.onQuit()
			systray.Quit()
			return
		}
	}
}

// onQuit runs the hard-stop on tray exit (#186): quitting the tray frees
// the engine's VRAM/RAM. Best-effort and bounded — the tray must not hang
// on a slow/unreachable daemon, and the daemon-side SIGTERM→SIGKILL
// continues server-side after this call returns. The daemon itself keeps
// running (the engine is left parked); a later Start brings it back.
func (t *tray) onQuit() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Best-effort: an old daemon (no engine control), reuse mode (409), or
	// a daemon already down are all expected on the way out and ignored.
	_ = t.cli.StopEngine(ctx)
}

// ensureAutostartOnFirstLaunch registers the per-user "launch on
// login" entry the first time the tray starts on platforms that
// don't have a system-wide alternative. Windows ships no XDG-style
// /etc/xdg/autostart equivalent, so without this the tray would
// require an explicit menu click before subsequent logons auto-start
// it -- diverging from the Linux .deb's
// /etc/xdg/autostart/waired-tray.desktop, which is registered by the
// package and active for every user out of the box. Users can still
// opt out via the "Start Waired on login" menu toggle.
//
// Linux and macOS skip this: on Linux the .deb already wrote the
// system-wide .desktop file, and writing a redundant ~/.config/
// autostart/ entry from here would break the menu toggle (Disable
// would remove only the per-user copy, leaving the system-wide file
// in place and the tray still auto-starting). The macOS tray path is
// still stubbed.
//
// Errors are logged and swallowed -- failing here doesn't justify
// aborting the tray boot, and the menu toggle remains as a manual
// fallback.
func (t *tray) ensureAutostartOnFirstLaunch() {
	if runtime.GOOS != "windows" {
		return
	}
	enabled, err := t.autostartMgr.IsEnabled()
	if err != nil {
		slog.Warn("tray: autostart probe failed on first launch", "err", err)
		return
	}
	if enabled {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		slog.Warn("tray: locate self for autostart failed", "err", err)
		return
	}
	args := []string{"-mgmt", t.opts.MgmtURL}
	if err := t.autostartMgr.Enable(exe, args); err != nil {
		slog.Warn("tray: enable autostart on first launch failed", "err", err)
		return
	}
	slog.Info("tray: registered HKCU autostart on first launch", "exe", exe)
	t.refreshAutostartLabel()
}

// onToggleAutostart flips the per-user "launch on login"
// registration. Reads the current state, calls Enable or Disable,
// then refreshes the menu label so the user sees the new state on
// the next menu open. We deliberately do NOT block on the
// systray event loop: registry / file writes are O(ms) and the
// click handler is the systray click loop itself.
func (t *tray) onToggleAutostart() {
	enabled, err := t.autostartMgr.IsEnabled()
	if err != nil {
		ShowError(fmt.Sprintf("Autostart query failed: %v", err))
		return
	}
	if enabled {
		if err := t.autostartMgr.Disable(); err != nil {
			ShowError(fmt.Sprintf("Disable autostart failed: %v", err))
			return
		}
	} else {
		exe, err := os.Executable()
		if err != nil {
			ShowError(fmt.Sprintf("Enable autostart: cannot locate self: %v", err))
			return
		}
		args := []string{"-mgmt", t.opts.MgmtURL}
		if err := t.autostartMgr.Enable(exe, args); err != nil {
			ShowError(fmt.Sprintf("Enable autostart failed: %v", err))
			return
		}
	}
	t.refreshAutostartLabel()
}

// refreshAutostartLabel rewrites the menu item's title to match the
// current registration state. Safe to call from onReady (before the
// first poll) and from the click handler.
func (t *tray) refreshAutostartLabel() {
	if t.miAutostart == nil {
		return
	}
	enabled, _ := t.autostartMgr.IsEnabled()
	if enabled {
		t.miAutostart.SetTitle("✓ Start Waired on login")
	} else {
		t.miAutostart.SetTitle("Start Waired on login")
	}
}

func (t *tray) onToggle(ctx context.Context) {
	t.mu.Lock()
	kind := t.last.Kind
	t.mu.Unlock()
	switch kind {
	case MenuConnected:
		if err := t.cli.Pause(ctx); err != nil {
			ShowError(fmt.Sprintf("Disconnect failed: %v", err))
		}
	case MenuDisconnected:
		if err := t.cli.Resume(ctx); err != nil {
			ShowError(fmt.Sprintf("Connect failed: %v", err))
		}
	case MenuNotSignedIn:
		go t.startLogin(ctx)
	}
	// Refresh promptly so the menu reflects the action without waiting
	// for the next 5 s tick.
	go t.pollOnce(ctx)
}

// startLogin begins a daemon-driven login. On a daemon that exposes the
// login API the daemon owns the session and no polkit dialog appears;
// pollOnce surfaces progress + opens the browser. On an older daemon
// (404 → ErrLoginUnsupported) we fall back to the legacy pkexec
// elevation path so the tray still works against pre-#177 agents.
func (t *tray) startLogin(ctx context.Context) {
	st, err := t.cli.LoginStart(ctx, management.LoginStartRequest{ControlURL: t.opts.ControlURL})
	if errors.Is(err, ErrLoginUnsupported) {
		if err := LoginViaElevation(ctx, t.opts.ControlURL, t.opts.StateDir); err != nil {
			ShowError(err.Error())
		}
		return
	}
	if err != nil {
		ShowError(fmt.Sprintf("Sign-in failed: %v", err))
		return
	}
	t.mu.Lock()
	t.loginSessionID = st.SessionID
	t.loginURLOpened = false
	t.mu.Unlock()
	// Poll promptly so the login URL is picked up and the browser opens
	// within a tick rather than waiting for the 5 s cadence.
	go t.pollOnce(ctx)
}

// pollLogin folds an in-flight daemon-driven login into snap. It opens
// the browser once on the first login URL, and clears the tracked
// session on a terminal phase (active clears silently; error shows a
// dialog). Best-effort: a transient error just leaves the previous
// state for the next tick.
func (t *tray) pollLogin(ctx context.Context, snap *Snapshot) {
	t.mu.Lock()
	sessID := t.loginSessionID
	t.mu.Unlock()
	if sessID == "" {
		return
	}

	st, err := t.cli.LoginStatus(ctx, sessID)
	if err != nil {
		// ErrLoginUnsupported (daemon downgraded?) or a transient error:
		// stop tracking so we don't spin; a fresh click re-starts login.
		if errors.Is(err, ErrLoginUnsupported) {
			t.mu.Lock()
			t.loginSessionID = ""
			t.mu.Unlock()
		}
		return
	}
	snap.Login = st

	if st.LoginURL != "" {
		t.mu.Lock()
		open := !t.loginURLOpened
		if open {
			t.loginURLOpened = true
		}
		t.mu.Unlock()
		if open {
			if oerr := OpenBrowser(st.LoginURL); oerr != nil {
				ShowError(fmt.Sprintf("Could not open browser; visit:\n%s", st.LoginURL))
			}
		}
	}

	switch st.Phase {
	case management.LoginPhaseActive:
		t.mu.Lock()
		t.loginSessionID = ""
		t.mu.Unlock()
	case management.LoginPhaseError:
		msg := st.Error
		if msg == "" {
			msg = "sign-in failed"
		}
		ShowError("Sign-in failed: " + msg)
		t.mu.Lock()
		t.loginSessionID = ""
		t.mu.Unlock()
	}
}

// onInferenceToggle reads the most recent MenuModel to decide which
// direction to flip and calls the corresponding management API. The
// click handler does not poll; it relies on the post-click pollOnce
// to refresh the displayed labels.
func (t *tray) onInferenceToggle(ctx context.Context) {
	t.mu.Lock()
	action := t.last.InferenceToggleAction
	t.mu.Unlock()
	switch action {
	case "Disable inference engine":
		if err := t.cli.DisableInference(ctx); err != nil {
			ShowError(fmt.Sprintf("Disable inference failed: %v", err))
		}
	case "Enable inference engine":
		if err := t.cli.EnableInference(ctx); err != nil {
			ShowError(fmt.Sprintf("Enable inference failed: %v", err))
		}
	}
	go t.pollOnce(ctx)
}

// onEngineToggle drives the hard engine power axis (#186): stop frees
// VRAM/RAM, start restarts the engine. Mirrors onInferenceToggle — reads
// the last-rendered action and relies on the post-click pollOnce to
// refresh labels. The action is empty (item hidden) when the engine is
// reused (not managed) or the daemon predates engine control.
func (t *tray) onEngineToggle(ctx context.Context) {
	t.mu.Lock()
	action := t.last.EngineToggleAction
	t.mu.Unlock()
	switch action {
	case "Stop inference engine":
		if err := t.cli.StopEngine(ctx); err != nil {
			ShowError(fmt.Sprintf("Stop inference engine failed: %v", err))
		}
	case "Start inference engine":
		if err := t.cli.StartEngine(ctx); err != nil {
			ShowError(fmt.Sprintf("Start inference engine failed: %v", err))
		}
	}
	go t.pollOnce(ctx)
}

// onInstallEngine runs the OS-specific Ollama auto-installer under
// elevation (pkexec on Linux, UAC RunAs on Windows). It is dispatched on
// its own goroutine because the install is slow; on success the next
// poll clears the no_engine state and the "Install Ollama…" item hides
// itself. (#188)
func (t *tray) onInstallEngine(ctx context.Context) {
	t.mu.Lock()
	action := t.last.InstallEngineAction
	t.mu.Unlock()
	if action == "" {
		return
	}
	if err := InstallOllamaViaElevation(ctx, t.opts.StateDir); err != nil {
		ShowError(fmt.Sprintf("Install Ollama failed: %v", err))
		return
	}
	t.pollOnce(ctx)
}

// onUpdate handles a click on the "Update available" banner (#293). The
// daemon runs unprivileged and cannot install, so we run `waired update`
// under elevation — UpdateViaElevation wraps the CLI in the platform's GUI
// elevation (pkexec on Linux, UAC on Windows, osascript admin on macOS),
// and the CLI re-runs the official installer. Long-running (download +
// elevation dialog + service restart): callers must dispatch in a
// goroutine so the click select stays responsive.
func (t *tray) onUpdate(ctx context.Context) {
	t.mu.Lock()
	show := t.last.ShowUpdate
	ver := t.last.UpdateVersion
	t.mu.Unlock()
	if !show {
		return
	}
	if ver != "" {
		notify("Updating Waired to "+ver+"…", notification.Info)
	} else {
		notify("Updating Waired…", notification.Info)
	}
	if err := UpdateViaElevation(ctx); err != nil {
		ShowError(fmt.Sprintf("Update failed: %v", err))
		return
	}
	// The installer restarts the daemon as part of the swap; the next poll
	// repaints the version and clears the banner once it's current.
	t.pollOnce(ctx)
}

// pollUpdate folds the manual-update check into the snapshot (#293). It
// POSTs /update/check once to seed the daemon's cache, then reads the cheap
// cached /update/status each poll — never hammering the version feed (the
// daemon caches with a multi-hour TTL). A 404 flips updateSupported off so
// legacy daemons aren't dialed every tick. When a newer version first
// appears it pops a one-shot toast.
func (t *tray) pollUpdate(ctx context.Context, snap *Snapshot) {
	t.mu.Lock()
	supported := t.updateSupported
	seeded := t.updateSeeded
	t.mu.Unlock()
	if !supported {
		return
	}

	var st *management.UpdateStatus
	var err error
	if seeded {
		st, err = t.cli.UpdateStatus(ctx)
	} else {
		// First successful poll seeds the daemon's cache so the banner
		// reflects reality promptly rather than after a later /status.
		st, err = t.cli.UpdateCheck(ctx, false)
	}
	if err != nil {
		if errors.Is(err, ErrUpdateUnsupported) {
			t.mu.Lock()
			t.updateSupported = false
			t.mu.Unlock()
		}
		return
	}
	t.mu.Lock()
	t.updateSeeded = true
	t.mu.Unlock()
	snap.Update = st
	t.maybeNotifyUpdate(st)
}

// updateRenotifyInterval bounds how often an ignored-but-still-pending update
// re-prompts. The first sighting of a version toasts immediately; the same
// version then re-reminds at most once per interval (#294) — "appropriate
// intervals", not every 5s poll and not a single fire-and-forget.
const updateRenotifyInterval = 24 * time.Hour

// maybeNotifyUpdate pops the proactive "update available" toast subject to
// the prompt toggle + the re-reminder cadence (see shouldNotifyUpdate). It
// records the (version, time) only when it actually fires, so disabling then
// re-enabling prompts re-arms the toast correctly.
func (t *tray) maybeNotifyUpdate(st *management.UpdateStatus) {
	now := time.Now()
	t.mu.Lock()
	fire := shouldNotifyUpdate(st, t.lastNotifiedUpdateVersion, t.lastNotifiedUpdateAt, now, updateRenotifyInterval)
	if fire {
		t.lastNotifiedUpdateVersion = st.LatestVersion
		t.lastNotifiedUpdateAt = now
	}
	t.mu.Unlock()
	if fire {
		notify("Waired "+st.LatestVersion+" is available — open the menu to update.", notification.Info)
	}
}

// shouldNotifyUpdate is the pure toast decision. It fires only when an update
// is available AND the operator has prompts enabled, AND either the version
// differs from the last one toasted (newly discovered) or the same version is
// still pending and renotify has elapsed since the last toast (a bounded
// re-reminder). Pure so the cadence is unit-testable without the tray.
func shouldNotifyUpdate(st *management.UpdateStatus, lastVer string, lastAt, now time.Time, renotify time.Duration) bool {
	if st == nil || !st.Available || st.LatestVersion == "" || !st.NotifyEnabled {
		return false
	}
	if st.LatestVersion != lastVer {
		return true // newly-discovered version → prompt now
	}
	return now.Sub(lastAt) >= renotify // same version still ignored → re-remind
}

// onUpdateNotifyToggle flips the proactive-prompt preference via the daemon's
// POST /update/settings (#294). The banner stays either way; this only
// controls whether the tray pushes a toast. Long-ish (one HTTP round-trip),
// so handleClicks dispatches it in a goroutine.
func (t *tray) onUpdateNotifyToggle(ctx context.Context) {
	t.mu.Lock()
	show := t.last.UpdateNotifyAction != ""
	enabled := t.last.UpdateNotifyEnabled
	t.mu.Unlock()
	if !show {
		return
	}
	if _, err := t.cli.UpdateSettings(ctx, !enabled); err != nil {
		ShowError(fmt.Sprintf("Update-notification toggle failed: %v", err))
		return
	}
	go t.pollOnce(ctx)
}

// onShareToggle flips the mesh-share decision via the management API.
// Same pattern as onInferenceToggle but talks to the Phase 6
// /inference/share endpoints. No confirmation dialog: the action is
// reversible in one click and matches `waired pause` / `Disable
// inference engine` UX expectations.
func (t *tray) onShareToggle(ctx context.Context) {
	t.mu.Lock()
	action := t.last.ShareToggleAction
	t.mu.Unlock()
	switch action {
	case "Stop sharing engine to mesh":
		if err := t.cli.DisableShare(ctx); err != nil {
			ShowError(fmt.Sprintf("Stop sharing failed: %v", err))
		}
	case "Share engine to mesh":
		if err := t.cli.EnableShare(ctx); err != nil {
			ShowError(fmt.Sprintf("Share failed: %v", err))
		}
	}
	go t.pollOnce(ctx)
}

// dispatchPublicUseModeClicks blocks on one public-use mode slot's
// ClickedCh, mirroring dispatchWorkerModeClicks. The dispatch runs on its
// own goroutine, so onPublicUseMode may safely block on a consent dialog.
func (t *tray) dispatchPublicUseModeClicks(ctx context.Context, idx int) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.miPublicUseModes[idx].ClickedCh:
			t.onPublicUseMode(ctx, idx)
		}
	}
}

// onPublicShareToggle drives the provider Public Share toggle (waired#833).
// It reads the last-rendered action under the lock and branches on the
// literal:
//   - "Share this computer publicly": enable. This path checks err FIRST
//     and only on success surfaces the SERVED side-effect note (the mesh
//     auto-enable / pending sentence) — there is no consent dialog here.
//   - "Stop sharing this computer publicly": the kill switch. It confirms
//     with the SERVED disable-confirm copy before disabling; with no dialog
//     backend it hands off to the CLI rather than silently stopping other
//     people's in-flight requests.
func (t *tray) onPublicShareToggle(ctx context.Context) {
	t.mu.Lock()
	action := t.last.PublicShareToggleAction
	t.mu.Unlock()
	switch action {
	case "Share this computer publicly":
		resp, err := t.cli.EnablePublicShare(ctx, 0)
		if err != nil {
			ShowError(fmt.Sprintf("Public sharing: %v", err))
			return
		}
		if resp.Note != "" {
			// The served note carries the mesh auto-enable / pending-sync
			// sentence — the only channel that side effect reaches the user.
			notify(resp.Note, notification.Info)
		}
		go t.pollOnce(ctx)
	case "Stop sharing this computer publicly":
		yes, ok := confirmWithLabels(
			management.PublicShareDisableConfirmTitle,
			management.PublicShareDisableConfirmText,
			"Stop sharing", "Cancel")
		if !ok {
			// No desktop dialog backend — do not stop other people's running
			// requests without showing the warning; hand off to the CLI.
			if err := CopyToClipboard("waired public unshare"); err != nil {
				ShowError("Public sharing: " + err.Error())
				return
			}
			notify("Run `waired public unshare` in a terminal to stop public sharing.", notification.Info)
			return
		}
		if !yes {
			return
		}
		resp, err := t.cli.DisablePublicShare(ctx)
		if err != nil {
			ShowError(fmt.Sprintf("Public sharing: %v", err))
			return
		}
		if resp.Note != "" {
			notify(resp.Note, notification.Info)
		}
		go t.pollOnce(ctx)
	}
}

// onPublicUseMode applies a click on the off/auto/explicit mode row idx
// (waired#833). The target mode is resolved from the latched projection
// under the lock (never from the label). Choosing any non-off mode while
// unconsented runs the consent flow first; because the server sets
// mode=auto/main/sub on the FIRST consent, a just-completed first consent
// that already lands the requested "auto" makes the follow-up POST
// redundant, so it is skipped. A late server-side gate
// (ErrPublicConsentRequired) runs consent once and retries.
func (t *tray) onPublicUseMode(ctx context.Context, idx int) {
	t.mu.Lock()
	var mode string
	if idx < len(t.lastPublicUseModes) {
		mode = t.lastPublicUseModes[idx].Mode
	}
	consented := t.last.PublicUseConsented
	t.mu.Unlock()
	if mode == "" {
		return
	}

	consentJustRan := false
	if mode != agentconfig.PublicUseModeOff && !consented {
		if !t.runPublicConsent(ctx) {
			return
		}
		consentJustRan = true
	}
	// First consent already set mode=auto (main+sub on, no tier), so a
	// follow-up POST for the same "auto" would be a no-op — skip it and let
	// the post-click poll repaint the selection.
	if consentJustRan && mode == agentconfig.PublicUseModeAuto {
		go t.pollOnce(ctx)
		return
	}

	_, err := t.cli.SetPublicUse(ctx, management.PublicUseUpdateRequest{Mode: &mode})
	if errors.Is(err, ErrPublicConsentRequired) {
		// Consent lapsed since the last poll; run it once and retry.
		if !t.runPublicConsent(ctx) {
			return
		}
		_, err = t.cli.SetPublicUse(ctx, management.PublicUseUpdateRequest{Mode: &mode})
	}
	if err != nil {
		ShowError(fmt.Sprintf("Public use: %v", err))
		return
	}
	go t.pollOnce(ctx)
}

// runPublicConsent shows the served Public Share warning and records
// consent on acceptance (waired#833). It returns true only when consent
// was actually recorded. The title / body / button labels are ALWAYS the
// server-served copy — never string literals here — so every surface
// renders identical wording and consent is never recorded without the
// user seeing the text.
func (t *tray) runPublicConsent(ctx context.Context) bool {
	w, err := t.cli.PublicWarning(ctx)
	if err != nil {
		ShowError(fmt.Sprintf("Public use: %v", err))
		return false
	}
	yes, ok := confirmWithLabels(w.Title, w.Text, w.AcceptLabel, w.CancelLabel)
	if !ok {
		// No dialog backend: consent MUST NOT be recorded without showing
		// the text, so hand off to the CLI (which prints the warning).
		if err := CopyToClipboard("waired public use --auto"); err != nil {
			ShowError("Public use: " + err.Error())
			return false
		}
		notify("Run `waired public use --auto` in a terminal to read the warning and turn this on.", notification.Info)
		return false
	}
	if !yes {
		return false
	}
	if _, err := t.cli.AcceptPublicConsent(ctx, w.Version); err != nil {
		if !errors.Is(err, ErrPublicWarningVersionMismatch) {
			ShowError(fmt.Sprintf("Public use: %v", err))
			return false
		}
		// The served text changed between display and accept: re-fetch,
		// re-display exactly once, then give up if it still mismatches.
		w2, werr := t.cli.PublicWarning(ctx)
		if werr != nil {
			ShowError(fmt.Sprintf("Public use: %v", werr))
			return false
		}
		yes2, ok2 := confirmWithLabels(w2.Title, w2.Text, w2.AcceptLabel, w2.CancelLabel)
		if !ok2 || !yes2 {
			return false
		}
		if _, rerr := t.cli.AcceptPublicConsent(ctx, w2.Version); rerr != nil {
			ShowError(fmt.Sprintf("Public use: %v", rerr))
			return false
		}
	}
	// Reciprocity (owner-approved): a single accept also enables sharing
	// this computer, since using public computers requires sharing one of
	// yours. Best-effort — check err BEFORE touching resp (no nil-deref).
	if ps, err := t.cli.PublicShareStatus(ctx); err == nil && ps.DesiredState == string(state.PublicShareOff) {
		if resp, err := t.cli.EnablePublicShare(ctx, 0); err == nil && resp.Note != "" {
			notify(resp.Note, notification.Info)
		}
	}
	return true
}

// onPublicMore opens the served "Privacy & safety…" link (waired#833).
// The URL is whatever publicMoreURL extracted from the served warning
// text — never hardcoded. "" (no link served) is a no-op.
func (t *tray) onPublicMore() {
	t.mu.Lock()
	url := t.last.PublicMoreURL
	t.mu.Unlock()
	if url == "" {
		return
	}
	if err := OpenBrowser(url); err != nil {
		ShowError(err.Error())
	}
}

func (t *tray) onCopyIP() {
	t.mu.Lock()
	ip := t.last.OverlayIP
	t.mu.Unlock()
	if ip == "" {
		return
	}
	if err := CopyToClipboard(ip); err != nil {
		ShowError(err.Error())
	}
}

func (t *tray) onAdmin() {
	t.mu.Lock()
	url := t.last.AdminURL
	t.mu.Unlock()
	if url == "" {
		ShowError("Admin URL is unknown — sign in first.")
		return
	}
	if err := OpenBrowser(url); err != nil {
		ShowError(err.Error())
	}
}

// onCodeUI opens the bundled OpenCode coding agent in the browser. Since #486
// the agent runs USER-SIDE (as the tray's own user) via the `waired codeui`
// CLI, not the daemon: it runs `opencode serve` on the real project behind an
// authenticating proxy. If an instance is already running we reuse its URL
// (whatever project it serves); otherwise we start one rooted at the user's
// home (opencode's default landing — no folder picker). Long-running on first
// run (a ~55 MB download), so callers dispatch this in a goroutine.
func (t *tray) onCodeUI(ctx context.Context) {
	bin, err := wairedCLIPath()
	if err != nil {
		ShowError("Coding agent: waired CLI not found (" + err.Error() + ")")
		return
	}
	// Reuse a running instance rather than restarting it onto a new project.
	if url := codeUIRunningURL(ctx, bin); url != "" {
		if oerr := OpenBrowser(url); oerr != nil {
			ShowError(oerr.Error())
		}
		return
	}
	notify("Starting the coding agent… (first run downloads it; this can take a minute)", notification.Info)
	args := []string{"codeui", "open"}
	if home, herr := os.UserHomeDir(); herr == nil && home != "" {
		args = append(args, "--project", home)
	}
	// `codeui open` opens the browser itself (it inherits the tray's session
	// env, so xdg-open/open works). Stream its output to the tray log.
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if rerr := cmd.Run(); rerr != nil {
		notify("Coding agent failed to start: "+rerr.Error(), notification.Warning)
		ShowError("Coding agent failed to start — see the tray log.")
	}
}

// codeUIRunningURL returns the access URL of a running user-side coding agent,
// or "" when none is running. It shells out to `waired codeui url`, which reads
// the per-user runtime.json.
func codeUIRunningURL(ctx context.Context, bin string) string {
	out, err := exec.CommandContext(ctx, bin, "codeui", "url").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// onReconfigureOpenCode walks the user through the
// "rewrite the waired OpenCode plugin" flow:
//
//  1. Pop a confirmation dialog (zenity / kdialog). The rewrite touches a
//     file outside the waired state dir (~/.config/opencode/plugin/), so
//     we pause for the user's consent first.
//  2. On "yes", POST to /waired/v1/integration/opencode/reconfigure.
//     Surface success / failure via desktop notification — the tray
//     is already showing the live status so the user does not need a
//     second source of truth.
//  3. When no dialog backend is available (zenity + kdialog both
//     missing), copy `waired link opencode` to the clipboard and
//     notify the user instead. Better than silently doing nothing or
//     silently rewriting the file without consent.
//
// Long-running (HTTP + dialog wait); callers must dispatch in a
// goroutine — the systray click select must stay responsive.
func (t *tray) onReconfigureOpenCode(ctx context.Context) {
	const title = "Reconfigure OpenCode integration?"
	const body = "This rewrites the waired OpenCode plugin " +
		"(~/.config/opencode/plugin/waired.js) to point at the current " +
		"waired gateway. Proceed?"

	yes, ok := ConfirmYesNo(title, body)
	if !ok {
		// No desktop dialog available — fall back to clipboard.
		if err := CopyToClipboard("waired link opencode"); err != nil {
			ShowError("Reconfigure: " + err.Error())
			return
		}
		notify("Run `waired link opencode` in a terminal to reconfigure.", notification.Info)
		return
	}
	if !yes {
		return
	}

	if err := t.cli.ReconfigureOpenCode(ctx); err != nil {
		notify("OpenCode reconfigure failed: "+err.Error(), notification.Warning)
		ShowError("OpenCode reconfigure: " + err.Error())
		return
	}
	notify("OpenCode integration reconfigured.", notification.Info)
	go t.pollOnce(ctx)
}

// onReconfigureOpenClaw mirrors onReconfigureOpenCode for the OpenClaw
// integration: confirm, then POST the reconfigure (which rewrites the plugin
// under ~/.openclaw/plugins/waired/ and refreshes the openclaw.json keys).
// Long-running; callers must dispatch in a goroutine.
func (t *tray) onReconfigureOpenClaw(ctx context.Context) {
	const title = "Reconfigure OpenClaw integration?"
	const body = "This rewrites the waired OpenClaw plugin " +
		"(~/.openclaw/plugins/waired/) and refreshes its openclaw.json keys to " +
		"point at the current waired gateway. Proceed?"

	yes, ok := ConfirmYesNo(title, body)
	if !ok {
		if err := CopyToClipboard("waired link openclaw"); err != nil {
			ShowError("Reconfigure: " + err.Error())
			return
		}
		notify("Run `waired link openclaw` in a terminal to reconfigure.", notification.Info)
		return
	}
	if !yes {
		return
	}

	if err := t.cli.ReconfigureOpenClaw(ctx); err != nil {
		notify("OpenClaw reconfigure failed: "+err.Error(), notification.Warning)
		ShowError("OpenClaw reconfigure: " + err.Error())
		return
	}
	notify("OpenClaw integration reconfigured.", notification.Info)
	go t.pollOnce(ctx)
}

// maybeShowRecommendation records the live recommendation for the menu
// item / click handler and proactively pops the confirmation dialog once
// per distinct, non-dismissed recommendation. A nil/dismissed rec clears
// the stored state so the row hides and a later re-appearance pops again.
func (t *tray) maybeShowRecommendation(ctx context.Context, rec *management.BenchmarkRecommendation) {
	t.mu.Lock()
	if rec == nil || rec.Dismissed || rec.ToModelID == "" {
		t.lastRecommendation = nil
		t.lastRecPopupKey = ""
		t.mu.Unlock()
		return
	}
	t.lastRecommendation = rec
	key := rec.FromVariantID + "→" + rec.ToVariantID
	fresh := key != t.lastRecPopupKey
	t.lastRecPopupKey = key
	t.mu.Unlock()

	if fresh {
		// Proactive one-shot popup. The persistent menu item keeps the
		// recommendation reachable afterwards without re-popping every 5 s.
		go t.onShowRecommendationPopup(ctx)
	}
}

// liveRecommendation picks the catalog's switch suggestion to surface:
// lighter takes precedence over upgrade (the daemon makes them mutually
// exclusive; precedence here is a safety net).
func liveRecommendation(cat *management.ModelCatalogResponse) *management.BenchmarkRecommendation {
	if rec := cat.BenchmarkRecommendation; rec != nil {
		return rec
	}
	return cat.BenchmarkUpgrade
}

// dispatchRecommendClicks routes clicks on the recommendation row
// (lighter or upgrade) to the confirmation popup (issue #133).
func (t *tray) dispatchRecommendClicks(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.miRecommend.ClickedCh:
			go t.onShowRecommendationPopup(ctx)
		}
	}
}

// onShowRecommendationPopup presents the lighter-model suggestion in a
// native yes/no dialog. Yes posts the preferred-model switch (agent
// restarts); No records a dismissal so the same pairing does not nag
// again. When no desktop dialog backend is available it falls back to
// copying the CLI command to the clipboard. Long-running (dialog wait) —
// callers dispatch in a goroutine.
func (t *tray) onShowRecommendationPopup(ctx context.Context) {
	t.mu.Lock()
	rec := t.lastRecommendation
	t.mu.Unlock()
	if rec == nil || rec.ToModelID == "" {
		return
	}

	title := "Local inference is slow"
	body := fmt.Sprintf(
		"This host benchmarked at %.0f tok/s, below the %.0f tok/s interactive floor.\n\n"+
			"Switch to the lighter model %s? The agent will restart to apply it.",
		rec.MeasuredTokps, rec.FloorTokps, rec.ToModelID)
	if rec.Direction == management.RecommendationUpgrade {
		title = "Better model available"
		body = fmt.Sprintf(
			"This host benchmarked at %.0f tok/s — enough headroom for a higher-quality model.\n\n"+
				"Switch to %s (predicted ~%.0f tok/s)? This downloads a larger model and restarts the agent.",
			rec.MeasuredTokps, rec.ToModelID, rec.PredictedTokps)
	}

	yes, ok := ConfirmYesNo(title, body)
	if !ok {
		// No desktop dialog backend — fall back to the CLI command.
		if err := CopyToClipboard("waired runtimes benchmark"); err != nil {
			ShowError("Recommendation: " + err.Error())
			return
		}
		notify("Run `waired runtimes benchmark` in a terminal to switch models.", notification.Info)
		return
	}
	if !yes {
		if err := t.cli.DismissRecommendation(ctx, rec.FromVariantID, rec.ToVariantID); err != nil &&
			!errors.Is(err, ErrCatalogUnsupported) {
			ShowError("Dismiss recommendation: " + err.Error())
			return
		}
		go t.pollOnce(ctx)
		return
	}
	resp, err := t.cli.SetPreferredModel(ctx, rec.ToModelID)
	if err != nil {
		ShowError(fmt.Sprintf("Switch model failed: %v", err))
		return
	}
	t.onModelSwitchAccepted(resp)
	go t.pollOnce(ctx)
}

func (t *tray) onLogout(ctx context.Context) {
	if !ShowConfirm("Sign this device out of Waired?\nThe identity and secrets will be removed.") {
		return
	}
	go func() {
		if err := LogoutViaElevation(ctx, t.opts.StateDir); err != nil {
			ShowError(err.Error())
		}
		t.pollOnce(ctx)
	}()
}

func (t *tray) pollLoop(ctx context.Context) {
	t.pollOnce(ctx)
	tk := time.NewTicker(t.opts.PollEvery)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			t.pollOnce(ctx)
		}
	}
}

func (t *tray) pollOnce(ctx context.Context) {
	pollCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	st, statusErr := t.cli.Status(pollCtx)
	snap := Snapshot{}
	if statusErr != nil {
		// Unreachable daemon. During the model-switch grace window this is
		// the expected supervised restart, so keep the last online menu and
		// show "Switching model…" rather than the red agent-down state
		// (waired#808). Once the window lapses (a genuinely failed restart)
		// offlineModel falls back to the daemon-down model.
		t.mu.Lock()
		switching := time.Now().Before(t.switchingUntil)
		lastOnline := t.lastOnline
		t.mu.Unlock()
		t.apply(offlineModel(lastOnline, switching))
		return
	}
	snap.Health = HealthOnline
	snap.Status = st

	id, idErr := t.cli.Identity(pollCtx)
	if idErr == nil {
		snap.Identity = id
	}
	// Fold in any in-flight daemon-driven login (opens the browser on the
	// first login URL, surfaces progress/errors). No-op when no login is
	// being tracked.
	t.pollLogin(pollCtx, &snap)
	// Inference is best-effort: 404 (older daemon) is swallowed via the
	// ErrInferenceUnsupported sentinel, leaving snap.Inference nil so the
	// menu hides the inference group entirely.
	if inf, infErr := t.cli.InferenceStatus(pollCtx); infErr == nil {
		snap.Inference = inf
	}
	// Claude integration is best-effort with the same 404-tolerance.
	if cl, clErr := t.cli.ClaudeIntegration(pollCtx); clErr == nil {
		snap.Claude = cl
	}
	// Claude Code per-class routing (#649): best-effort, 404 on older
	// daemons leaves snap.ClaudeRouting nil and hides the routing submenu.
	if cr, crErr := t.cli.ClaudeRouting(pollCtx); crErr == nil {
		snap.ClaudeRouting = cr
	}
	// OpenCode integration: same shape — 404 on older daemons leaves
	// snap.OpenCode nil and the tray hides the group.
	if oc, ocErr := t.cli.OpenCodeIntegration(pollCtx); ocErr == nil {
		snap.OpenCode = oc
	}
	// OpenClaw integration: same shape — 404 on older daemons leaves
	// snap.OpenClaw nil and the tray hides the group.
	if ow, owErr := t.cli.OpenClawIntegration(pollCtx); owErr == nil {
		snap.OpenClaw = ow
	}
	// Catalog: best-effort with 404 → ErrCatalogUnsupported sentinel,
	// leaving snap.Catalog nil so the menu hides the submenu entirely.
	if cat, catErr := t.cli.ModelCatalog(pollCtx); catErr == nil {
		snap.Catalog = cat
		t.maybeShowRecommendation(ctx, liveRecommendation(cat))
	}
	// Mesh snapshot for the inference-worker pin submenu. Best-effort:
	// a 404 leaves snap.Mesh nil so applyWorker still renders the mode
	// rows but produces an empty pin list. The InferenceStatus already
	// carries snap.Inference.Worker for the active state, so a missing
	// mesh poll only loses the alternate-peer rows.
	if mesh, mErr := t.cli.MeshSnapshot(pollCtx); mErr == nil {
		snap.Mesh = mesh
	}
	// Observability (Phase 9). On a daemon that supports it, the state
	// poll succeeds and we then fetch the new fallback events using
	// the cursor we kept from last time. A 404 on either route flips
	// obsSupported off so subsequent polls skip both.
	t.pollObservability(pollCtx, &snap)
	// Manual-update check (#293): best-effort, 404-tolerant like the others.
	t.pollUpdate(pollCtx, &snap)
	// Public share (waired#833): three independent best-effort GETs. Each
	// 404 is swallowed on its own (via the ErrPublicShare*/ErrPublicUse*
	// sentinels) so the two feature halves gate independently — a daemon
	// exposing only one still renders that half.
	if ps, err := t.cli.PublicShareStatus(pollCtx); err == nil {
		snap.PublicShare = ps
	}
	if pu, err := t.cli.PublicUse(pollCtx); err == nil {
		snap.PublicUse = pu
	}
	if pw, err := t.cli.PublicWarning(pollCtx); err == nil {
		snap.PublicWarning = pw
	}
	snap.Now = time.Now()
	m := Update(snap)
	// The daemon is reachable again: remember this model for the switch
	// grace window and close any open window so a later genuine crash is
	// not masked as "Switching model…" (waired#808).
	t.mu.Lock()
	t.lastOnline = m
	t.switchingUntil = time.Time{}
	t.mu.Unlock()
	t.apply(m)
}

// pollObservability fans out the two Phase 9 GETs, updates the cursor
// and rolling fallback buffer, and writes the projection inputs into
// snap. All errors (other than 404 → ErrObservabilityUnsupported) are
// swallowed silently — the tray treats observability as best-effort
// the same way it treats inference / claude / opencode.
//
// Why a single call instead of two ad-hoc GETs inline:
//   - cursor + buffer are tray-private state, so they don't belong in
//     state.go's pure projection;
//   - on a 404 we want to flip *both* /state and /events into "skip"
//     mode and log exactly once on the transition.
func (t *tray) pollObservability(ctx context.Context, snap *Snapshot) {
	t.mu.Lock()
	supported := t.obsSupported
	cursor := t.obsCursor
	t.mu.Unlock()
	if !supported {
		return
	}

	state, err := t.cli.ObservabilityState(ctx)
	if err != nil {
		if errors.Is(err, ErrObservabilityUnsupported) {
			t.markObservabilityUnsupported("/state")
		}
		// Other errors (transient HTTP failures, decode error) are
		// silent: the next poll will retry. Don't strand the cursor —
		// keep it for when /state recovers.
		return
	}
	snap.Observability = state

	resp, err := t.cli.ObservabilityEvents(
		ctx,
		cursor,
		// One shared /events call carries both kinds; a second call would
		// desync obsCursor and the fallback buffer. The loop below routes
		// each event by Kind, so widening the filter leaves fallback
		// handling untouched (waired#833).
		[]observability.Kind{observability.KindFallback, observability.KindPublicShareNudge},
		fallbackEventsBatch(cursor),
	)
	if err != nil {
		if errors.Is(err, ErrObservabilityUnsupported) {
			t.markObservabilityUnsupported("/events")
		}
		return
	}

	t.mu.Lock()
	t.obsCursor = resp.NextSince
	if resp.Gap {
		// The ring rolled over since we last polled. Best we can do is
		// keep what's in the new batch and let older entries age out by
		// RecentFallbackWindow. Dropping the local buffer here would
		// blank the submenu mid-stream which is worse UX than briefly
		// double-counting an event that was already in the buffer.
		slog.Info("tray: observability event ring gap; older entries may be missing",
			"oldest_seq", resp.OldestSeq)
	}
	// The pre-consent Public Share nudge rides the same batch as the
	// fallback events (waired#833). Capture the latest one here and show
	// its toast after the lock is released; fallback routing is unchanged.
	var nudge string
	var nudgeSeq uint64
	for _, ev := range resp.Events {
		if ev.Kind == observability.KindPublicShareNudge && ev.PublicShareNudge != nil {
			nudge = ev.PublicShareNudge.Message
			nudgeSeq = ev.Seq
			continue
		}
		if ev.Kind != observability.KindFallback || ev.Fallback == nil {
			continue
		}
		t.recentFallbacks = append(t.recentFallbacks, FallbackEntry{
			TS:     ev.TS,
			From:   ev.Fallback.From,
			To:     ev.Fallback.To,
			Reason: ev.Fallback.Reason,
			Model:  ev.Fallback.Model,
		})
	}
	t.recentFallbacks = trimRecentFallbacks(t.recentFallbacks, time.Now())
	// Hand a newest-first defensive copy to the snapshot so Update()
	// can read it without holding the lock and the projection's
	// MaxRecentActivity cap drops oldest entries first.
	snap.RecentFallbacks = reverseFallbacks(t.recentFallbacks)
	t.mu.Unlock()

	t.maybeShowPublicNudge(nudge, nudgeSeq)
}

// maybeShowPublicNudge shows the one-shot pre-consent Public Share hint
// as a tray toast (waired#833). The message is rendered VERBATIM — it is
// authored as user-facing copy on the daemon side (observability.
// PublicShareNudgeMessage); the tray never re-words it and never renders
// PublicShareNudgeEvent.Reason, which is a filter tag, not display text.
// Suppressed when the message is empty, when this Seq has already been
// shown (dedupe across polls and the first-poll since=0 replay), or when
// consent already exists (the hint only makes sense pre-consent).
func (t *tray) maybeShowPublicNudge(msg string, seq uint64) {
	if msg == "" {
		return
	}
	t.mu.Lock()
	if seq == t.lastPublicNudgeSeq || t.last.PublicUseConsented {
		t.mu.Unlock()
		return
	}
	t.lastPublicNudgeSeq = seq
	t.mu.Unlock()
	notify(msg, notification.Info)
}

// fallbackEventsBatch chooses an /events limit per poll. On the very
// first poll after startup (cursor==0) we ask for the full ring window
// in one shot so the submenu shows something useful immediately;
// thereafter the cursor delta keeps each batch small so we always cap.
func fallbackEventsBatch(cursor uint64) int {
	if cursor == 0 {
		return 64
	}
	return 32
}

// markObservabilityUnsupported flips obsSupported off and logs the
// transition exactly once. Subsequent polls short-circuit before the
// HTTP round trip so the legacy-agent case stays cheap and quiet.
func (t *tray) markObservabilityUnsupported(reason string) {
	t.mu.Lock()
	wasSupported := t.obsSupported
	t.obsSupported = false
	t.mu.Unlock()
	if wasSupported {
		slog.Info("tray: observability endpoints unavailable; submenu hidden",
			"reason", reason)
	}
}

// trimRecentFallbacks bounds the rolling buffer in two ways:
//   - drop entries older than 2 × RecentFallbackWindow (so newly
//     out-of-window entries get GC'd promptly without surprising the
//     projection cutoff with a stale buffer);
//   - cap the total size at 64 to bound memory under a fallback burst.
//
// The buffer is kept in oldest-first order (matching the order
// /events returns) so successive append calls don't need to merge-sort.
// reverseFallbacks flips it to newest-first when handing to the
// snapshot.
func trimRecentFallbacks(buf []FallbackEntry, now time.Time) []FallbackEntry {
	const maxRecent = 64
	cutoff := now.Add(-2 * RecentFallbackWindow)
	out := buf[:0]
	for _, f := range buf {
		if f.TS.Before(cutoff) {
			continue
		}
		out = append(out, f)
	}
	if len(out) > maxRecent {
		out = out[len(out)-maxRecent:]
	}
	return out
}

// reverseFallbacks returns a newest-first copy of buf without
// mutating the input. The projection consumes the result; the tray's
// in-memory buffer stays oldest-first.
func reverseFallbacks(buf []FallbackEntry) []FallbackEntry {
	if len(buf) == 0 {
		return nil
	}
	out := make([]FallbackEntry, len(buf))
	for i, f := range buf {
		out[len(buf)-1-i] = f
	}
	return out
}

// apply pushes m to the systray menu items, only mutating items whose
// rendering actually changes. Each SetTitle / Show / Hide is a DBus
// call on Linux, so suppressing no-ops keeps the bus traffic low.
func (t *tray) apply(m MenuModel) {
	t.mu.Lock()
	prev := t.last
	t.last = m
	t.mu.Unlock()

	// Best-effort debug dump for the Phase W-3 Windows screenshot
	// loop; no-op unless WAIRED_TRAY_DEBUG is set. Kept here (in the
	// apply path, after the model has been latched) so the JSON on
	// disk matches what the subsequent systray.Set* calls render.
	dumpDebugState(m)

	switch m.Icon {
	case IconConnected:
		systray.SetIcon(iconConnected)
	case IconDisconnected:
		systray.SetIcon(iconDisconnected)
	case IconError:
		systray.SetIcon(iconErrorIcon)
	case IconDegraded:
		systray.SetIcon(iconDegraded)
	case IconBusy:
		systray.SetIcon(iconBusy)
	}
	systray.SetTooltip(m.HeaderTitle)

	setTitleIfChanged(t.miHeader, prev.HeaderTitle, m.HeaderTitle)
	setVisibleIfChanged(t.miEmail, prev.AccountEmail != "", m.AccountEmail != "")
	setTitleIfChanged(t.miEmail, prev.AccountEmail, m.AccountEmail)
	// Status/hint line (waired#808): the daemon-down "Start-Service…" hint,
	// the login user-code, or an error reason. Previously computed into
	// MenuModel.StatusMsg but never rendered.
	setVisibleIfChanged(t.miStatus, prev.StatusMsg != "", m.StatusMsg != "")
	setTitleIfChanged(t.miStatus, prev.StatusMsg, m.StatusMsg)

	// Update banner (#293): visibility + title track ShowUpdate / UpdateLabel.
	setVisibleIfChanged(t.miUpdate, prev.ShowUpdate, m.ShowUpdate)
	setTitleIfChanged(t.miUpdate, prev.UpdateLabel, m.UpdateLabel)
	setVisibleIfChanged(t.miUpdateNotify, prev.UpdateNotifyAction != "", m.UpdateNotifyAction != "")
	setTitleIfChanged(t.miUpdateNotify, prev.UpdateNotifyAction, m.UpdateNotifyAction)

	// Toggle item: title + visibility track ToggleAction.
	setVisibleIfChanged(t.miToggle, prev.ToggleAction != "", m.ToggleAction != "")
	setTitleIfChanged(t.miToggle, prev.ToggleAction, m.ToggleAction)

	// Inference group: the "Inference" submenu parent plus its rows —
	// toggle + engine state + share toggle + share state + active model +
	// worker (waired#809). The parent shows when the daemon exposes the
	// inference API (ShowInferenceMenu); each inner item still tracks its
	// own field, so an empty row inside the open submenu stays hidden.
	setVisibleIfChanged(t.miInference, prev.ShowInferenceMenu, m.ShowInferenceMenu)
	setVisibleIfChanged(t.miInferenceToggle, prev.InferenceToggleAction != "", m.InferenceToggleAction != "")
	setTitleIfChanged(t.miInferenceToggle, prev.InferenceToggleAction, m.InferenceToggleAction)
	setVisibleIfChanged(t.miInferenceState, prev.InferenceStateLabel != "", m.InferenceStateLabel != "")
	setTitleIfChanged(t.miInferenceState, prev.InferenceStateLabel, m.InferenceStateLabel)
	// Hard engine power toggle (#186): visibility + title track
	// EngineToggleAction; enablement tracks EngineToggleEnabled (the
	// reuse/not-managed case renders the row greyed out).
	setVisibleIfChanged(t.miEngineToggle, prev.EngineToggleAction != "", m.EngineToggleAction != "")
	setTitleIfChanged(t.miEngineToggle, prev.EngineToggleAction, m.EngineToggleAction)
	setEnabledIfChanged(t.miEngineToggle, prev.EngineToggleEnabled, m.EngineToggleEnabled)
	// "Install Ollama…" — shown only on no_engine (#188).
	setVisibleIfChanged(t.miInstallEngine, prev.InstallEngineAction != "", m.InstallEngineAction != "")
	setTitleIfChanged(t.miInstallEngine, prev.InstallEngineAction, m.InstallEngineAction)
	// Share-with-mesh items (Phase 6). Pre-allocated regardless of
	// daemon support; visibility tracks the MenuModel fields which
	// applyInference leaves empty when the daemon predates the API.
	setVisibleIfChanged(t.miShareToggle, prev.ShareToggleAction != "", m.ShareToggleAction != "")
	setTitleIfChanged(t.miShareToggle, prev.ShareToggleAction, m.ShareToggleAction)
	setVisibleIfChanged(t.miShareState, prev.ShareStateLabel != "", m.ShareStateLabel != "")
	setTitleIfChanged(t.miShareState, prev.ShareStateLabel, m.ShareStateLabel)
	// Mesh-reachable indicator (#212): display-only, like miShareState.
	setVisibleIfChanged(t.miMeshReachable, prev.MeshReachableLabel != "", m.MeshReachableLabel != "")
	setTitleIfChanged(t.miMeshReachable, prev.MeshReachableLabel, m.MeshReachableLabel)
	setVisibleIfChanged(t.miEngineWarning, prev.EngineWarningLabel != "", m.EngineWarningLabel != "")
	setTitleIfChanged(t.miEngineWarning, prev.EngineWarningLabel, m.EngineWarningLabel)
	// miActiveModel ("Model: <model_id>") is suppressed when the catalog
	// submenu is showing — CatalogActiveLabel renders the same intent
	// with the friendlier display_name, and one row per concept is enough.
	prevActiveModelVisible := prev.ActiveModelLabel != "" && !prev.ShowCatalog
	activeModelVisible := m.ActiveModelLabel != "" && !m.ShowCatalog
	setVisibleIfChanged(t.miActiveModel, prevActiveModelVisible, activeModelVisible)
	setTitleIfChanged(t.miActiveModel, prev.ActiveModelLabel, m.ActiveModelLabel)

	// Catalog group: "Active: …" top-level + "Models" submenu (the
	// leading separator auto-collapses when ShowCatalog is false).
	setVisibleIfChanged(t.miCatalogActive, prev.ShowCatalog, m.ShowCatalog)
	setTitleIfChanged(t.miCatalogActive, prev.CatalogActiveLabel, m.CatalogActiveLabel)
	setVisibleIfChanged(t.miCatalog, prev.ShowCatalog, m.ShowCatalog)
	parentLabel := m.CatalogParentLabel
	if parentLabel == "" {
		parentLabel = "Models"
	}
	setTitleIfChanged(t.miCatalog, prev.CatalogParentLabel, parentLabel)
	t.applyCatalogEntries(prev.CatalogEntries, m.CatalogEntries)
	t.mu.Lock()
	t.lastCatalogEntries = m.CatalogEntries
	t.mu.Unlock()

	// #133 step-down recommendation row.
	setVisibleIfChanged(t.miRecommend, prev.ShowRecommend, m.ShowRecommend)
	setTitleIfChanged(t.miRecommend, prev.RecommendLabel, m.RecommendLabel)

	// Worker (manual routing) group: "Worker: …" summary + "Inference
	// worker" submenu parent. Visibility follows ShowWorker so old
	// daemons render exactly the pre-feature menu; the leading separator
	// auto-collapses when the group is hidden.
	setVisibleIfChanged(t.miWorkerActive, prev.ShowWorker, m.ShowWorker)
	setTitleIfChanged(t.miWorkerActive, prev.WorkerActiveLabel, m.WorkerActiveLabel)
	setVisibleIfChanged(t.miWorker, prev.ShowWorker, m.ShowWorker)
	workerParent := m.WorkerParentLabel
	if workerParent == "" {
		workerParent = "Inference worker"
	}
	setTitleIfChanged(t.miWorker, prev.WorkerParentLabel, workerParent)
	t.applyWorkerModes(prev.WorkerModes, m.WorkerModes)
	t.applyWorkerPins(prev.WorkerPinEntries, m.WorkerPinEntries)
	setVisibleIfChanged(t.miWorkerClearPin, prev.WorkerShowClearPin, m.WorkerShowClearPin)
	t.mu.Lock()
	t.lastWorkerModes = m.WorkerModes
	t.lastWorkerPinEntries = m.WorkerPinEntries
	t.mu.Unlock()

	// Public share submenu (waired#833): the top-level parent follows
	// ShowPublicShareMenu; the toggle / state / note / use-header / more
	// rows track their own MenuModel fields, and the three mode rows diff
	// via applyPublicUseModes. lastPublicUseModes is latched for the
	// mode-row click dispatch, mirroring the worker rows above.
	setVisibleIfChanged(t.miPublicShare, prev.ShowPublicShareMenu, m.ShowPublicShareMenu)
	setVisibleIfChanged(t.miPublicShareToggle, prev.PublicShareToggleAction != "", m.PublicShareToggleAction != "")
	setTitleIfChanged(t.miPublicShareToggle, prev.PublicShareToggleAction, m.PublicShareToggleAction)
	setVisibleIfChanged(t.miPublicShareState, prev.PublicShareStateLabel != "", m.PublicShareStateLabel != "")
	setTitleIfChanged(t.miPublicShareState, prev.PublicShareStateLabel, m.PublicShareStateLabel)
	setVisibleIfChanged(t.miPublicShareNote, prev.PublicShareNote != "", m.PublicShareNote != "")
	setTitleIfChanged(t.miPublicShareNote, prev.PublicShareNote, m.PublicShareNote)
	setVisibleIfChanged(t.miPublicUseHeader, prev.PublicUseHeaderLabel != "", m.PublicUseHeaderLabel != "")
	setTitleIfChanged(t.miPublicUseHeader, prev.PublicUseHeaderLabel, m.PublicUseHeaderLabel)
	t.applyPublicUseModes(prev.PublicUseModes, m.PublicUseModes)
	setVisibleIfChanged(t.miPublicMore, prev.PublicMoreURL != "", m.PublicMoreURL != "")
	t.mu.Lock()
	t.lastPublicUseModes = m.PublicUseModes
	t.mu.Unlock()

	// "This device" group is shown only when enrolled (i.e. we have a name or IP).
	hasDevice := m.DeviceName != "" || m.OverlayIP != ""
	prevHasDevice := prev.DeviceName != "" || prev.OverlayIP != ""
	for _, mi := range []*systray.MenuItem{t.miDeviceLabel, t.miDeviceName, t.miOverlayIP, t.miNetwork} {
		setVisibleIfChanged(mi, prevHasDevice, hasDevice)
	}
	// Peers row (waired#808): gate on peer presence, not just enrollment.
	// Sharing hasDevice's visibility left a blank "Peers" chevron row in
	// the steady peerless state (miPeers is allocated with an empty title,
	// and the first-paint "Peers: 0" == "Peers: 0" diff is a no-op).
	setVisibleIfChanged(t.miPeers, peersRowVisible(prev), peersRowVisible(m))
	setTitleIfChanged(t.miDeviceName, prev.DeviceName, "  "+m.DeviceName)
	if m.OverlayIP != "" {
		setTitleIfChanged(t.miOverlayIP, "  "+prev.OverlayIP, "  "+m.OverlayIP)
	} else {
		setTitleIfChanged(t.miOverlayIP, prev.OverlayIP, "")
	}
	setTitleIfChanged(t.miNetwork, prev.NetworkName, fmtNetwork(m.NetworkName))
	// Phase 7 follow-up (C1b): when at least one peer has Hardware
	// the "Peers" item gets the submenu form ("Peers (N)") and the
	// child rows render the per-peer GPU labels; otherwise the
	// pre-Phase-7 bare label ("Peers: N") stays.
	t.applyPeersLabel(prev, m)
	t.applyPeerHardwareEntries(prev.PeerHardwareEntries, m.PeerHardwareEntries)
	t.applyPeerHardwareOverflow(prev.PeerHardwareOverflow, m.PeerHardwareOverflow)

	setVisibleIfChanged(t.miCodeUI, prev.ShowCodeUI, m.ShowCodeUI)
	setTitleIfChanged(t.miCodeUI, prev.CodeUILabel, m.CodeUILabel)
	setVisibleIfChanged(t.miAdmin, prev.AdminURL != "", m.AdminURL != "")
	setVisibleIfChanged(t.miLogout, prev.AccountEmail != "", m.AccountEmail != "")

	// Claude Code submenu parent (waired#809): visible when the daemon
	// exposes *either* the Claude status endpoint (ShowClaude) or the
	// per-class routing endpoint (ShowClaudeCode), since both the status
	// rows and the route selectors now live inside this one parent. Old
	// daemons exposing neither render no "Claude Code" entry at all.
	prevShowClaudeParent := prev.ShowClaude || prev.ShowClaudeCode
	showClaudeParent := m.ShowClaude || m.ShowClaudeCode
	setVisibleIfChanged(t.miClaudeCode, prevShowClaudeParent, showClaudeParent)

	// Claude status rows (now children of the parent above). The header
	// reports live serving state; the proxy row reports the OS-level
	// managed-settings status. ProxyLabel="" hides that row.
	setVisibleIfChanged(t.miClaudeHeader, prev.ShowClaude, m.ShowClaude)
	setTitleIfChanged(t.miClaudeHeader, prev.ClaudeHeader, m.ClaudeHeader)
	setVisibleIfChanged(t.miClaudeProxy, prev.ClaudeProxyLabel != "", m.ClaudeProxyLabel != "")
	setTitleIfChanged(t.miClaudeProxy, "  "+prev.ClaudeProxyLabel, "  "+m.ClaudeProxyLabel)

	// Claude Code per-class routing rows (#649/#650): the two header rows
	// follow ShowClaudeCode; the route slots + conditional notes diff via
	// applyClaudeRoutes / setTitleIfChanged.
	claudeCodeParent := m.ClaudeCodeParent
	if claudeCodeParent == "" {
		claudeCodeParent = "Claude Code"
	}
	setTitleIfChanged(t.miClaudeCode, prev.ClaudeCodeParent, claudeCodeParent)
	setVisibleIfChanged(t.miClaudeMainHeader, prev.ShowClaudeCode, m.ShowClaudeCode)
	setVisibleIfChanged(t.miClaudeSubHeader, prev.ShowClaudeCode, m.ShowClaudeCode)
	t.applyClaudeRoutes(t.miClaudeMainRoutes, prev.ClaudeMainRouteRows, m.ClaudeMainRouteRows)
	t.applyClaudeRoutes(t.miClaudeSubRoutes, prev.ClaudeSubRouteRows, m.ClaudeSubRouteRows)
	setVisibleIfChanged(t.miClaudeFallbackNote, prev.ClaudeFallbackNote != "", m.ClaudeFallbackNote != "")
	setTitleIfChanged(t.miClaudeFallbackNote, "  "+prev.ClaudeFallbackNote, "  "+m.ClaudeFallbackNote)
	setVisibleIfChanged(t.miClaudeEnableNote, prev.ClaudeEnableNote != "", m.ClaudeEnableNote != "")
	setTitleIfChanged(t.miClaudeEnableNote, "  "+prev.ClaudeEnableNote, "  "+m.ClaudeEnableNote)
	t.mu.Lock()
	t.lastClaudeMainRoutes = m.ClaudeMainRouteRows
	t.lastClaudeSubRoutes = m.ClaudeSubRouteRows
	t.mu.Unlock()

	// OpenCode integration group — same lifecycle as Claude. Header +
	// Config + Reconfigure share the ShowOpenCode flag; its leading
	// separator auto-collapses if the group above is hidden, so two
	// adjacent rules never render.
	setVisibleIfChanged(t.miOpenCodeHeader, prev.ShowOpenCode, m.ShowOpenCode)
	setTitleIfChanged(t.miOpenCodeHeader, prev.OpenCodeHeader, m.OpenCodeHeader)
	setVisibleIfChanged(t.miOpenCodeConfig, prev.OpenCodeConfigLabel != "", m.OpenCodeConfigLabel != "")
	setTitleIfChanged(t.miOpenCodeConfig, "  "+prev.OpenCodeConfigLabel, "  "+m.OpenCodeConfigLabel)
	setVisibleIfChanged(t.miOpenCodeReconfigure, prev.OpenCodeReconfigureLabel != "", m.OpenCodeReconfigureLabel != "")
	setTitleIfChanged(t.miOpenCodeReconfigure, prev.OpenCodeReconfigureLabel, m.OpenCodeReconfigureLabel)

	// OpenClaw integration group — same lifecycle as the OpenCode group.
	setVisibleIfChanged(t.miOpenClawHeader, prev.ShowOpenClaw, m.ShowOpenClaw)
	setTitleIfChanged(t.miOpenClawHeader, prev.OpenClawHeader, m.OpenClawHeader)
	setVisibleIfChanged(t.miOpenClawConfig, prev.OpenClawConfigLabel != "", m.OpenClawConfigLabel != "")
	setTitleIfChanged(t.miOpenClawConfig, "  "+prev.OpenClawConfigLabel, "  "+m.OpenClawConfigLabel)
	setVisibleIfChanged(t.miOpenClawReconfigure, prev.OpenClawReconfigureLabel != "", m.OpenClawReconfigureLabel != "")
	setTitleIfChanged(t.miOpenClawReconfigure, prev.OpenClawReconfigureLabel, m.OpenClawReconfigureLabel)

	// Recent activity submenu (Phase 9 observability). Hidden when no
	// kind=fallback events landed in RecentFallbackWindow; its leading
	// separator auto-collapses while the parent is hidden, so no stray
	// rule is drawn.
	setVisibleIfChanged(t.miRecent, prev.ShowRecentActivity, m.ShowRecentActivity)
	t.applyRecentActivityEntries(prev.RecentActivityEntries, m.RecentActivityEntries)
}

// applyRecentActivityEntries diffs the prev / next projection against
// the MaxRecentActivity pre-allocated slots. Mirrors applyCatalogEntries
// in spirit; rows are always Disabled (display-only) so we only flip
// Show/Hide + SetTitle.
func (t *tray) applyRecentActivityEntries(prev, next []RecentActivityRow) {
	for i, mi := range t.miRecentEntries {
		var prevHas, nextHas bool
		var prevLabel, nextLabel string
		if i < len(prev) {
			prevHas = true
			prevLabel = prev[i].Label
		}
		if i < len(next) {
			nextHas = true
			nextLabel = next[i].Label
		}
		setVisibleIfChanged(mi, prevHas, nextHas)
		setTitleIfChanged(mi, prevLabel, nextLabel)
	}
}

// applyPeersLabel picks one of two labels for the top-level "Peers"
// item. With hardware visible, the label uses MenuModel.PeerHardwareParent
// ("Peers (N)") so the submenu indicator reads consistently. Without
// hardware, the pre-Phase-7 "Peers: N" form is preserved.
func (t *tray) applyPeersLabel(prev, m MenuModel) {
	prevLabel := peersLabel(prev)
	nextLabel := peersLabel(m)
	setTitleIfChanged(t.miPeers, prevLabel, nextLabel)
}

func peersLabel(m MenuModel) string {
	if m.ShowPeerHardware && m.PeerHardwareParent != "" {
		return m.PeerHardwareParent
	}
	return fmt.Sprintf("Peers: %d", m.PeerCount)
}

// applyPeerHardwareEntries mirrors applyRecentActivityEntries: it
// walks the pre-allocated submenu children and flips visibility +
// title based on the projection.
func (t *tray) applyPeerHardwareEntries(prev, next []PeerHardwareRow) {
	for i, mi := range t.miPeerEntries {
		var prevHas, nextHas bool
		var prevLabel, nextLabel string
		if i < len(prev) {
			prevHas = true
			prevLabel = prev[i].Label
		}
		if i < len(next) {
			nextHas = true
			nextLabel = next[i].Label
		}
		setVisibleIfChanged(mi, prevHas, nextHas)
		setTitleIfChanged(mi, prevLabel, nextLabel)
	}
}

// applyPeerHardwareOverflow renders the "+N more" row when the mesh
// has more peers than fit in the submenu. Hidden when n == 0.
func (t *tray) applyPeerHardwareOverflow(prev, next int) {
	setVisibleIfChanged(t.miPeerOverflow, prev > 0, next > 0)
	if next > 0 {
		setTitleIfChanged(t.miPeerOverflow,
			fmt.Sprintf("+%d more", prev),
			fmt.Sprintf("+%d more", next))
	}
}

func fmtNetwork(name string) string {
	if name == "" {
		return ""
	}
	return "Network: " + name
}

// setTitleIfChanged avoids the systray DBus chatter that SetTitle on
// every poll would otherwise produce.
func setTitleIfChanged(mi *systray.MenuItem, prev, next string) {
	if mi == nil || prev == next {
		return
	}
	mi.SetTitle(next)
}

func setTooltipIfChanged(mi *systray.MenuItem, prev, next string) {
	if mi == nil || prev == next {
		return
	}
	mi.SetTooltip(next)
}

func setVisibleIfChanged(mi *systray.MenuItem, prev, next bool) {
	if mi == nil || prev == next {
		return
	}
	if next {
		mi.Show()
	} else {
		mi.Hide()
	}
}

func setEnabledIfChanged(mi *systray.MenuItem, prev, next bool) {
	if mi == nil || prev == next {
		return
	}
	if next {
		mi.Enable()
	} else {
		mi.Disable()
	}
}

// applyWorkerModes diffs the worker mode rows (auto / local-only /
// peer-preferred) against the pre-allocated submenu slots. Selected
// rows get a "● " prefix so the operator sees the current mode at a
// glance; unselected rows get "○ ". Mirrors applyCatalogEntries'
// diff-only approach so DBus traffic stays minimal.
func (t *tray) applyWorkerModes(prev, next []WorkerModeRow) {
	for i, mi := range t.miWorkerModes {
		var prevHas, nextHas bool
		var prevLabel, nextLabel string
		if i < len(prev) {
			prevHas = true
			prevLabel = workerModeRowLabel(prev[i])
		}
		if i < len(next) {
			nextHas = true
			nextLabel = workerModeRowLabel(next[i])
		}
		setVisibleIfChanged(mi, prevHas, nextHas)
		setTitleIfChanged(mi, prevLabel, nextLabel)
	}
}

func workerModeRowLabel(r WorkerModeRow) string {
	prefix := "○ "
	if r.Selected {
		prefix = "● "
	}
	return prefix + r.Label
}

// applyPublicUseModes diffs the three public-use mode rows (off / auto /
// explicit) against their pre-allocated slots, mirroring applyWorkerModes:
// the ●/○ selection glyph comes from publicUseModeRowLabel and only
// changed rows are mutated so the systray DBus traffic stays minimal.
func (t *tray) applyPublicUseModes(prev, next []PublicUseModeRow) {
	for i, mi := range t.miPublicUseModes {
		var prevHas, nextHas bool
		var prevLabel, nextLabel string
		if i < len(prev) {
			prevHas = true
			prevLabel = publicUseModeRowLabel(prev[i])
		}
		if i < len(next) {
			nextHas = true
			nextLabel = publicUseModeRowLabel(next[i])
		}
		setVisibleIfChanged(mi, prevHas, nextHas)
		setTitleIfChanged(mi, prevLabel, nextLabel)
	}
}

// applyClaudeRoutes diffs a route-row group (main or sub) against its
// pre-allocated submenu slots, same "● selected / ○ unselected" glyphs and
// diff-only Show/SetTitle approach as applyWorkerModes.
func (t *tray) applyClaudeRoutes(items []*systray.MenuItem, prev, next []ClaudeRouteRow) {
	for i, mi := range items {
		var prevHas, nextHas bool
		var prevLabel, nextLabel string
		if i < len(prev) {
			prevHas = true
			prevLabel = claudeRouteRowLabel(prev[i])
		}
		if i < len(next) {
			nextHas = true
			nextLabel = claudeRouteRowLabel(next[i])
		}
		setVisibleIfChanged(mi, prevHas, nextHas)
		setTitleIfChanged(mi, prevLabel, nextLabel)
	}
}

func claudeRouteRowLabel(r ClaudeRouteRow) string {
	prefix := "○ "
	if r.Selected {
		prefix = "● "
	}
	return prefix + r.Label
}

// applyWorkerPins diffs the pin candidate rows against the pre-
// allocated MaxWorkerPinEntries slots. Tailscale-style: unavailable
// rows stay selectable but are visually distinguished by their label
// suffix; the click handler also no-ops on unavailable peers (the
// daemon would 503 anyway, but failing fast avoids the round trip).
func (t *tray) applyWorkerPins(prev, next []WorkerPinEntryView) {
	for i, mi := range t.miWorkerPinEntries {
		var prevHas, nextHas bool
		var prevLabel, nextLabel string
		var prevDisabled, nextDisabled bool
		if i < len(prev) {
			prevHas = true
			prevLabel = workerPinRowLabel(prev[i])
			prevDisabled = !prev[i].Available
		}
		if i < len(next) {
			nextHas = true
			nextLabel = workerPinRowLabel(next[i])
			nextDisabled = !next[i].Available
		}
		setVisibleIfChanged(mi, prevHas, nextHas)
		setTitleIfChanged(mi, prevLabel, nextLabel)
		if prevDisabled != nextDisabled {
			if nextDisabled {
				mi.Disable()
			} else {
				mi.Enable()
			}
		}
	}
}

func workerPinRowLabel(r WorkerPinEntryView) string {
	prefix := "○ "
	if r.Selected {
		prefix = "● "
	}
	return prefix + r.Label
}

// applyCatalogEntries diffs the previous and next catalog projection
// against the pre-allocated submenu slots. Each slot's title, enabled
// state, and visibility are only mutated when they actually change so
// the systray DBus traffic stays low even though the catalog refreshes
// on every poll tick.
func (t *tray) applyCatalogEntries(prev, next []CatalogEntryView) {
	for i, mi := range t.miCatalogEntries {
		var prevHas, nextHas bool
		var prevLabel, nextLabel string
		var prevTooltip, nextTooltip string
		var prevDisabled, nextDisabled bool
		if i < len(prev) {
			prevHas = true
			prevLabel = prev[i].Label
			prevTooltip = prev[i].Tooltip
			prevDisabled = prev[i].Disabled
		}
		if i < len(next) {
			nextHas = true
			nextLabel = next[i].Label
			nextTooltip = next[i].Tooltip
			nextDisabled = next[i].Disabled
		}
		setVisibleIfChanged(mi, prevHas, nextHas)
		setTitleIfChanged(mi, prevLabel, nextLabel)
		setTooltipIfChanged(mi, prevTooltip, nextTooltip)
		if prevDisabled != nextDisabled {
			if nextDisabled {
				mi.Disable()
			} else {
				mi.Enable()
			}
		}
	}
}
