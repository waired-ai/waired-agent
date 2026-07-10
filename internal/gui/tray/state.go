// Package tray builds the Waired desktop tray UI on top of fyne.io/systray.
// state.go owns the pure projection from a polling snapshot to a menu
// model so the rendering glue can stay free of branching logic.
package tray

import (
	"fmt"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// Health is the daemon-reachability axis — separate from the tunnel
// phase so that "daemon down" and "daemon up but tunnel paused" stay
// distinct UX states.
type Health int

const (
	HealthOnline  Health = iota // /v1/status reachable
	HealthOffline               // dial refused / timeout
)

// Snapshot bundles the latest poll result. Identity and Status are nil
// when the corresponding endpoint has not yet been queried (cold start)
// or returned an error.
type Snapshot struct {
	Health    Health
	Identity  *management.IdentityView
	Status    *management.Status
	Inference *management.InferenceStatus         // nil for daemons predating the inference toggle API
	Claude    *management.ClaudeIntegrationStatus // nil for daemons predating /waired/v1/integration/claude
	// ClaudeRouting is the unified per-class routing state (#649); nil for
	// daemons predating /waired/v1/integration/claude/route so the "Claude
	// Code" routing submenu stays hidden.
	ClaudeRouting *management.ClaudeRoutingState
	OpenCode      *management.OpenCodeIntegrationStatus // nil for daemons predating /waired/v1/integration/opencode
	OpenClaw      *management.OpenClawIntegrationStatus // nil for daemons predating /waired/v1/integration/openclaw
	Catalog       *management.ModelCatalogResponse      // nil for daemons predating /waired/v1/inference/catalog

	// Observability is nil when /waired/v1/observability/state returned
	// 404 (daemon predates Phase 9) or was otherwise unreachable this
	// poll. Update() treats a nil Observability as "skip the recent-
	// activity submenu and skip the degraded-icon override".
	Observability *management.ObservabilityState

	// Mesh is the most recent inferencemesh snapshot from
	// /waired/v1/inference/mesh. nil when the daemon does not expose
	// the mesh API (Phase 3-) or the poll failed. Used by applyWorker
	// to enumerate pinnable peers with their (node, model) labels —
	// management.PeerStatus carries Hardware but not the served
	// model list, so the dedicated mesh poll is necessary.
	Mesh *inferencemesh.Snapshot

	// RecentFallbacks holds kind=fallback events from /events, newest
	// first. Update() applies the 10-minute cutoff at projection time,
	// so the polling loop can keep more than ten minutes of history
	// here without affecting the rendered UI.
	RecentFallbacks []FallbackEntry

	// Login is the in-flight daemon-driven login status from
	// /waired/v1/login/status, or nil when no login is being tracked
	// (or the daemon predates the login API). Update() uses it only
	// while not-yet-enrolled, to render "Signing in…" / "Activating…"
	// and to surface a sign-in error inline.
	Login *management.LoginStatus

	// Update is the manual-update check result from
	// /waired/v1/update/status, or nil when the daemon predates the
	// update API or the poll failed. Update() renders the "Update
	// available" banner from it (in every reachable state) when
	// Available is true. #293.
	Update *management.UpdateStatus

	// Now is the wall-clock reference used by Update() when computing
	// recent-fallback ages. Zero falls back to time.Now() so production
	// callers do not have to stamp this; tests pin it for stable output.
	Now time.Time
}

// FallbackEntry is the tray's projection of one kind=fallback event.
// It deliberately omits the bookkeeping fields (Seq) since the tray
// only renders human-facing strings.
type FallbackEntry struct {
	TS     time.Time
	From   string
	To     string
	Reason string
	Model  string
}

// MaxCatalogEntries caps how many model rows the tray pre-allocates in
// the Catalog submenu. Bundled manifests are 17 with the qwen3.5
// lineup; 20 leaves headroom for a future "Other quantizations" /
// external manifest before another pre-allocation bump is needed.
// (Families render in manifest order — at 12 the alphabetical tail,
// including qwen3.6-27b, silently fell off the menu.)
const MaxCatalogEntries = 20

// MaxWorkerPinEntries caps the "Pin to peer" submenu pre-allocation.
// Mirrors MaxPeerHardwareRows so the operator sees the same set of
// peers in both submenus on hosts with more than 16 mesh members.
const MaxWorkerPinEntries = 16

// WorkerModeRow is one row inside the "Inference worker" submenu's
// mode group (auto / local-only / peer-preferred). The Selected flag
// drives the leading "●" / "○" glyph in apply().
type WorkerModeRow struct {
	Mode     state.RoutingMode
	Label    string
	Selected bool
}

// ClaudeRouteRow is one selectable route inside the "Claude Code"
// submenu — a main-conversation route (auto/waired/anthropic) or a
// subagent route (same/auto/waired/anthropic). Selected drives the
// leading "●" / "○" glyph in apply(); Class is the value POSTed on
// click. Node selection is deliberately NOT here — it lives in the
// "Inference worker" submenu (#649: `waired worker`).
type ClaudeRouteRow struct {
	Class    state.ClaudeRouteClass
	Label    string
	Selected bool
}

// WorkerPinEntryView is one row inside the "Inference worker ▸ Pin to
// peer" submenu. The label is the operator-visible name (with a model
// suffix when available); Available=false greys out the row so the
// operator can see the peer but not pin to a transiently-inactive
// host. Selected drives the "●" / "○" glyph in apply().
type WorkerPinEntryView struct {
	DeviceID  string
	Label     string
	Available bool
	Selected  bool
}

// MaxRecentActivity caps the "Recent activity" submenu. 5 rows is
// enough for an at-a-glance view; the full history lives in
// `waired doctor` / agent journal.
const MaxRecentActivity = 5

// MaxPeerHardwareRows caps the "Peers" submenu pre-allocation. The
// mesh is typically a handful of devices today; 16 leaves headroom
// before a layout bump is needed. When the actual mesh exceeds this
// cap the surplus is summarised as a single "+N more" row instead
// of being silently truncated.
const MaxPeerHardwareRows = 16

// RecentFallbackWindow is the cutoff used to drive both the
// "Recent activity" submenu visibility and the degraded-icon
// override. Entries older than this are dropped at projection time.
const RecentFallbackWindow = 10 * time.Minute

// RecentActivityRow is one row inside the "Recent activity" submenu.
// All rows are disabled (display-only); click handling lives in a
// future phase if it is ever needed.
type RecentActivityRow struct {
	Label string
}

// PeerHardwareRow is one row inside the "Peers" submenu. Phase 7
// follow-up (C1b) surfaces "alice-laptop — RTX 4090 (24 GB)" so the
// operator can correlate routing decisions with hardware. All rows
// are disabled (display-only) — click handling lives in a future
// phase if ever needed.
type PeerHardwareRow struct {
	Label string
}

// CatalogEntryView is one row inside the "Models" submenu. The Label
// already carries any annotation suffix ("(switching…)", "— needs 24 GB
// VRAM", "· 8 GB", etc.) so the tray's apply() only needs to call
// SetTitle/SetDisabled. Tooltip carries the fuller recommended-spec hint
// (min RAM/VRAM · quality tier · params); it is best-effort since some
// Linux indicators don't render menu-item tooltips.
type CatalogEntryView struct {
	ModelID  string
	Label    string
	Tooltip  string
	Disabled bool
}

// MenuKind selects one of the six reachable UI shapes.
type MenuKind int

const (
	MenuDaemonDown   MenuKind = iota // ⚠ daemon unreachable
	MenuNotSignedIn                  // ○ enrolled=false
	MenuConnected                    // ● tunnel active
	MenuDisconnected                 // ○ tunnel paused
	MenuConnecting                   // ◐ transitioning
	MenuError                        // ⚠ tunnel error
)

// IconState picks one of the four tray-icon SVGs. IconDegraded is the
// "connected with claude-integration warning" badge: same network
// state as IconConnected but with a small yellow ! glyph overlaid in
// the upper-right, so the user notices at a glance that something
// (currently: the wrapper's per-spawn gating) needs attention.
type IconState int

const (
	IconError IconState = iota
	IconConnected
	IconDisconnected
	IconDegraded
)

// MenuModel is the rendered intent — pure data, no widgets. The builder
// in tray.go translates each field into a systray menu item.
type MenuModel struct {
	Kind         MenuKind
	Icon         IconState
	HeaderTitle  string // "● Connected", "○ Not signed in", etc.
	AccountEmail string
	DeviceName   string
	OverlayIP    string
	NetworkName  string
	PeerCount    int
	AdminURL     string // "" hides the Open Admin Console... item
	StatusMsg    string // body for daemon-down / error states
	// ToggleAction is the label the connect-toggle menu item should render:
	// "Disconnect" | "Connect" | "Log in..." | "" (hidden).
	ToggleAction string

	// Update banner (#293). ShowUpdate is true when the daemon reports a
	// newer release; UpdateLabel is the menu text ("⚠ Update available —
	// install vX"), UpdateVersion the bare version, and UpdateMethod the
	// apply mechanism ("apt"|"installer"|"installsh") the click handler
	// uses to phrase its progress note. Clicking runs `waired update`
	// under elevation. All empty / false hide the row (current build, or a
	// daemon predating the update API).
	ShowUpdate    bool
	UpdateLabel   string
	UpdateVersion string
	UpdateMethod  string
	// Update-prompt toggle (#294). Shown beneath the banner when an update
	// is available: lets the operator silence the proactive toast (the
	// passive banner stays). "✓ Notify me about updates" when prompts are
	// on, "Notify me about updates" when off, "" hides it (no update, or a
	// daemon predating the settings API). UpdateNotifyEnabled is the current
	// preference the click handler flips.
	UpdateNotifyAction  string
	UpdateNotifyEnabled bool

	// Inference group — present only on enrolled + Connected/Disconnected
	// states when the daemon supports the inference toggle API. Empty
	// strings hide the corresponding menu item.
	InferenceToggleAction string // "Disable inference engine" | "Enable inference engine" | ""
	InferenceStateLabel   string // "Engine: ready" / "Engine: disabled" / "Engine: loading" / ...
	// EngineToggleAction drives the hard engine power axis (#186):
	// "Stop inference engine" (engine up → free VRAM/RAM) | "Start
	// inference engine" (engine stopped → restart) | "" (hidden: daemon
	// predates engine control, or the engine is reused/not managed).
	EngineToggleAction string
	// EngineToggleEnabled is false when the item should render but be
	// greyed out — currently only the reuse/not-managed case, which keeps
	// the row visible (so the user understands why) instead of hiding it.
	EngineToggleEnabled bool
	ActiveModelLabel    string // "Model: <model_id>" or ""
	// InstallEngineAction is "Install Ollama…" when SubsystemState is
	// "no_engine" (no usable local engine installed), else "". Clicking
	// it runs the auto-installer (#188).
	InstallEngineAction string

	// Share-with-mesh toggle (Phase 6). Sibling of the inference
	// engine toggle: lets the operator stop exposing the local engine
	// to mesh peers without turning the engine off locally. Both
	// fields are empty when the daemon predates the share API (= no
	// share_with_mesh field on /inference/status).
	ShareToggleAction string // "Stop sharing engine to mesh" | "Share engine to mesh" | ""
	ShareStateLabel   string // "Sharing: enabled" | "Sharing: disabled" | ""

	// MeshReachableLabel is a one-line, display-only indicator of whether
	// any mesh peer is advertising a reachable inference engine
	// ("Mesh: peer engine reachable" / "Mesh: no reachable peer engine").
	// Empty hides the row: daemons predating the mesh API leave
	// Snapshot.Mesh nil, so they render the pre-feature menu. Sourced from
	// the peers-only inferencemesh.Snapshot.Reachable aggregate.
	MeshReachableLabel string

	// EngineWarningLabel is a one-line, display-only engine provenance
	// warning ("⚠ engine version 0.24.0 does not match the bundled pin
	// 0.30.7 …", or the port-conflict refusal). Sourced from the ollama
	// RuntimeStatus version_warning / last_error fields; empty (old
	// daemons, healthy engine) hides the row.
	EngineWarningLabel string

	// Claude integration group — populated when the daemon exposes
	// /waired/v1/integration/claude. Empty group fields hide the
	// corresponding menu item; ShowClaude=false hides the entire
	// section (including its separator) so old daemons render exactly
	// the pre-extension menu.
	//
	// Since the transparent proxy became the sole Claude-routing method
	// on Linux (docs/decisions.md), this section reports PROXY status
	// (header = live serving state, ProxyLabel = OS-level install state)
	// instead of the retired shell-alias / IDE-wrapper detection.
	ShowClaude   bool
	ClaudeHeader string // "Claude integration: ● active" / "○ inactive (agent-stopped)"
	// ClaudeProxyLabel summarises the Claude Code managed-settings status
	// (#488): whether ANTHROPIC_BASE_URL is wired to the local gateway. The
	// field name is retained for tray menu-item wiring; there is no per-toggle
	// action row anymore (enable/disable is the root `waired claude` command).
	ClaudeProxyLabel string // "Claude: ✓ routed to local gateway" / "✗ not configured"

	// Claude Code routing submenu (#649/#650) — the per-class route
	// selector nested under a "Claude Code" parent. Populated when the
	// daemon exposes /waired/v1/integration/claude/route; ShowClaudeCode=
	// false hides the whole submenu so older daemons render the pre-feature
	// menu. Node selection is NOT here — that stays in the Inference worker
	// submenu. ClaudeFallbackNote is a disabled row surfaced only when the
	// daemon reports a last fallback (no-silent-breakage). ClaudeEnableNote
	// is a disabled row shown only when managed-settings is not yet routing
	// Claude Code through Waired, carrying the OS-specific enable hint.
	ShowClaudeCode      bool
	ClaudeCodeParent    string           // "Claude Code" — submenu parent label
	ClaudeMainRouteRows []ClaudeRouteRow // 3 rows: auto / waired / anthropic
	ClaudeSubRouteRows  []ClaudeRouteRow // 4 rows: same / auto / waired / anthropic
	ClaudeFallbackNote  string           // "⚠ last fell back → Anthropic (…)" or "" (hidden)
	ClaudeEnableNote    string           // "ⓘ not active yet — run …" or "" (hidden)

	// OpenCode integration group — populated when the daemon exposes
	// /waired/v1/integration/opencode. Mirrors the Claude shape but
	// with a single Config row (the waired.js plugin is one file) plus a
	// Reconfigure trigger that re-runs `waired link opencode` after
	// confirmation. ShowOpenCode=false hides the entire section.
	ShowOpenCode             bool
	OpenCodeHeader           string // "OpenCode integration: ● configured" / "⚠ stale (...)" / "○ not configured"
	OpenCodeConfigLabel      string // "Config: ✓ ~/.config/opencode/plugin/waired.js" / "✗ not configured" / "⚠ stale (<currentValue>)"
	OpenCodeReconfigureLabel string // "Reconfigure…" — clicking spawns the confirmation dialog

	// OpenClaw integration group — same shape as the OpenCode group,
	// populated when the daemon exposes /waired/v1/integration/openclaw.
	// ShowOpenClaw=false hides the entire section.
	ShowOpenClaw             bool
	OpenClawHeader           string // "OpenClaw integration: ● configured" / "⚠ stale (...)" / "○ not configured"
	OpenClawConfigLabel      string // "Config: ✓ ~/.openclaw/plugins/waired/index.mjs" / "✗ not configured" / "⚠ stale (<currentValue>)"
	OpenClawReconfigureLabel string // "Reconfigure…" — clicking spawns the confirmation dialog

	// Bundled coding-agent (#429/#486). ShowCodeUI is true whenever local
	// inference is available; clicking the item shells out to `waired codeui
	// open`, which runs `opencode serve` AS the user on the real project,
	// behind an authenticating proxy, and opens the browser. The tray no
	// longer talks to a daemon codeui endpoint (the agent runs user-side).
	ShowCodeUI  bool
	CodeUILabel string // "Open Coding Agent…"

	// Catalog submenu — populated when the daemon exposes
	// /waired/v1/inference/catalog. ShowCatalog=false hides the entire
	// section so old daemons render exactly the pre-extension menu.
	ShowCatalog        bool
	CatalogActiveLabel string             // "Active: Qwen3 8B Instruct" — visible at the top level
	CatalogParentLabel string             // "Models" — parent of the submenu
	CatalogEntries     []CatalogEntryView // ≤ MaxCatalogEntries rows; rest of the pre-allocated slots stay hidden

	// Benchmark step-down recommendation (#133). ShowRecommend is true
	// when the daemon reports a non-dismissed lighter-model suggestion;
	// clicking the row re-opens the confirmation popup. Hidden otherwise
	// (and on older daemons that don't report it).
	ShowRecommend  bool
	RecommendLabel string // "⚠ Lighter model recommended — switch to …"

	// Recent-activity submenu — populated when the daemon exposes
	// /waired/v1/observability/events AND at least one kind=fallback
	// event fell within RecentFallbackWindow. ShowRecentActivity=false
	// hides the parent menu item entirely; older daemons therefore
	// render exactly the pre-Phase-9 menu.
	ShowRecentActivity     bool
	RecentActivityParent   string              // "Recent activity"
	RecentActivityEntries  []RecentActivityRow // ≤ MaxRecentActivity rows
	HasRecentFallbackBadge bool                // exposed for unit tests / future surfaces

	// Peer-hardware submenu — populated when the daemon's Status.Peers
	// carries at least one peer with non-nil Hardware (Phase 7+ mesh).
	// ShowPeerHardware=false keeps the "Peers: N" top-level row alone,
	// matching the pre-Phase-7 menu shape so old daemons still render
	// cleanly.
	ShowPeerHardware     bool
	PeerHardwareParent   string            // "Peers" (parent submenu label, e.g. "Peers (3)")
	PeerHardwareEntries  []PeerHardwareRow // ≤ MaxPeerHardwareRows rows
	PeerHardwareOverflow int               // count of peers beyond the cap, surfaced as a "+N more" row

	// Worker (Tailscale-exit-node-style manual routing) submenu —
	// populated when the daemon exposes /waired/v1/worker (visible as
	// Snapshot.Inference.Worker on the GET /v1/inference/status hot
	// path). ShowWorker=false hides the entire section so old daemons
	// render exactly the pre-worker-pin menu.
	ShowWorker         bool
	WorkerActiveLabel  string               // "Worker: linux-gpu (pinned)" — top-level summary
	WorkerParentLabel  string               // "Inference worker" — parent of the submenu
	WorkerModes        []WorkerModeRow      // 3 fixed rows: auto / local-only / peer-preferred
	WorkerPinEntries   []WorkerPinEntryView // ≤ MaxWorkerPinEntries peer rows
	WorkerShowClearPin bool                 // true when mode==pinned so "(clear pin)" appears
}

// Update is the pure transition. No I/O, no goroutines — safe to call
// from the polling goroutine directly.
func Update(snap Snapshot) MenuModel {
	if snap.Health == HealthOffline {
		return MenuModel{
			Kind:        MenuDaemonDown,
			Icon:        IconError,
			HeaderTitle: "⚠ Waired agent is not running",
			StatusMsg:   startAgentHint(),
		}
	}

	// Daemon up but identity not yet known (e.g. /identity returned nothing
	// because the daemon predates this PR, or transient error). Render the
	// not-signed-in state — safer to under-promise than to claim Connected
	// without the email.
	if snap.Identity == nil || !snap.Identity.Enrolled {
		m := MenuModel{
			Kind:         MenuNotSignedIn,
			Icon:         IconDisconnected,
			HeaderTitle:  "○ Not signed in",
			ToggleAction: "Log in...",
		}
		// Reflect an in-flight daemon-driven login. While OAuth /
		// activation is pending the login menu item is hidden (empty
		// ToggleAction) so a second click cannot start a second session;
		// an error keeps "Log in..." visible so the operator can retry.
		if snap.Login != nil {
			switch snap.Login.Phase {
			case management.LoginPhaseLoggingIn:
				m.HeaderTitle = "◐ Signing in…"
				m.ToggleAction = ""
				if snap.Login.UserCode != "" {
					m.StatusMsg = "Code: " + snap.Login.UserCode
				}
			case management.LoginPhaseActivating:
				m.HeaderTitle = "◐ Activating…"
				m.ToggleAction = ""
			case management.LoginPhaseError:
				if snap.Login.Error != "" {
					m.StatusMsg = "Sign-in failed: " + snap.Login.Error
				}
			}
		}
		// An update can be offered before sign-in too — the check is
		// identity-independent.
		applyUpdate(&m, snap.Update)
		return m
	}

	m := MenuModel{
		AccountEmail: snap.Identity.AccountEmail,
		DeviceName:   identityDeviceName(snap.Identity),
		OverlayIP:    snap.Identity.OverlayIP,
		NetworkName:  snap.Identity.NetworkName,
		AdminURL:     adminURL(snap.Identity.ControlURL),
	}
	phase := ""
	if snap.Status != nil {
		phase = snap.Status.Phase
		m.PeerCount = snap.Status.PeerCount
		if m.DeviceName == "" {
			m.DeviceName = snap.Status.DeviceName
		}
	}

	switch phase {
	case "paused":
		m.Kind = MenuDisconnected
		m.Icon = IconDisconnected
		m.HeaderTitle = "○ Disconnected"
		m.ToggleAction = "Connect"
	case "starting", "stopping":
		m.Kind = MenuConnecting
		m.Icon = IconDisconnected
		m.HeaderTitle = "◐ Connecting…"
	case "error":
		m.Kind = MenuError
		m.Icon = IconError
		m.HeaderTitle = "⚠ Tunnel error"
		m.StatusMsg = checkLogsHint()
	default: // "active" — empty string only retained for back-compat with daemons predating the pause/resume API
		m.Kind = MenuConnected
		m.Icon = IconConnected
		m.HeaderTitle = "● Connected"
		m.ToggleAction = "Disconnect"
	}

	// Inference group: only surface on Connected / Disconnected so the
	// toggle is unreachable while the network state itself is in
	// transition or unknown. Daemons predating the inference toggle API
	// leave Snapshot.Inference nil — render nothing.
	if (m.Kind == MenuConnected || m.Kind == MenuDisconnected) && snap.Inference != nil {
		applyInference(&m, snap.Inference)
	}

	// Claude integration: the status endpoint works regardless of
	// pause/resume (it reports the wrapper-side view), so render
	// whenever it's available.
	if snap.Claude != nil {
		applyClaude(&m, snap.Claude)
	}

	// Claude Code per-class routing submenu (#649/#650) — independent
	// best-effort fetch; nil on daemons predating the route endpoint.
	// Gated on Connected/Disconnected like the worker submenu so route
	// switches don't race the intercept while the tunnel transitions.
	if (m.Kind == MenuConnected || m.Kind == MenuDisconnected) && snap.ClaudeRouting != nil {
		applyClaudeRouting(&m, snap.ClaudeRouting, snap.Claude)
	}

	// OpenCode integration: same lifecycle as Claude — surface
	// regardless of pause/resume. Drift between the on-disk
	// provider.waired.options.baseURL and the gateway is the only
	// failure mode we report; opencode itself surfaces unreachable
	// gateway connections directly to the user.
	if snap.OpenCode != nil {
		applyOpenCode(&m, snap.OpenCode)
	}
	// OpenClaw integration: same lifecycle as OpenCode.
	if snap.OpenClaw != nil {
		applyOpenClaw(&m, snap.OpenClaw)
	}

	// Bundled coding-agent: surface "Open Coding Agent…" whenever local
	// inference is available (the agent routes to the local gateway). The
	// click handler shells out to `waired codeui open` — install/start and
	// any error surfacing happen there, user-side.
	if snap.Inference != nil {
		m.ShowCodeUI = true
		m.CodeUILabel = "Open Coding Agent…"
	}

	// Model catalog: surface only on Connected/Disconnected so the
	// click-to-switch action isn't reachable while the network is
	// transitioning. Old daemons leave Snapshot.Catalog nil — render
	// nothing.
	if (m.Kind == MenuConnected || m.Kind == MenuDisconnected) && snap.Catalog != nil {
		applyCatalog(&m, snap.Catalog)
	}

	// Recent activity submenu + degraded-icon override. Independent of
	// Catalog / Inference visibility — fallback signal stays useful in
	// MenuDisconnected too, so the user can correlate "I just paused"
	// with the trailing activity. Hidden entirely when the daemon
	// predates Phase 9 (Observability==nil + RecentFallbacks empty).
	applyObservability(&m, snap)

	// Peer hardware submenu (Phase 7 follow-up C1b). Only meaningful
	// when at least one peer published Hardware, so old daemons /
	// CPU-only meshes render exactly the pre-Phase-7 menu.
	if snap.Status != nil {
		applyPeerHardware(&m, snap.Status.Peers)
	}

	// Worker (manual routing) submenu. Same connected/disconnected
	// gating as the Catalog submenu — switching routing while the
	// tunnel is transitioning would race against the Selector hot
	// path. Old daemons leave Snapshot.Inference.Worker nil so the
	// section stays hidden.
	if (m.Kind == MenuConnected || m.Kind == MenuDisconnected) &&
		snap.Inference != nil && snap.Inference.Worker != nil {
		applyWorker(&m, snap.Inference.Worker, snap.Mesh)
	}

	// Mesh-reachable indicator (#212). Same connected/disconnected gating
	// as the worker submenu — both read the peers-only mesh aggregate.
	// Old daemons leave Snapshot.Mesh nil so the row stays hidden.
	if m.Kind == MenuConnected || m.Kind == MenuDisconnected {
		applyMeshReachable(&m, snap.Mesh)
	}

	// Manual-update banner (#293). Independent of tunnel phase — an
	// available update stays worth surfacing whether connected or paused.
	applyUpdate(&m, snap.Update)
	return m
}

// applyUpdate surfaces the manual-update banner (#293) when the daemon
// reports a newer published release. The row is display + click: clicking
// it runs `waired update` under elevation (the daemon never installs).
// Hidden when the daemon predates the update API (snap.Update==nil), the
// check errored, or this host is current — so a host on the latest build
// sees nothing.
func applyUpdate(m *MenuModel, st *management.UpdateStatus) {
	if st == nil || !st.Available || st.LatestVersion == "" {
		return
	}
	m.ShowUpdate = true
	m.UpdateVersion = st.LatestVersion
	m.UpdateMethod = st.ApplyMethod
	m.UpdateLabel = "⚠ Update available — install " + st.LatestVersion
	// Update-prompt toggle (#294): silence the proactive toast without
	// hiding the banner. Surfaced beneath the banner — the moment the toggle
	// is meaningful in the tray (`waired update --notify` covers the
	// pre-emptive case). The checkmark conveys the current on/off state.
	m.UpdateNotifyEnabled = st.NotifyEnabled
	if st.NotifyEnabled {
		m.UpdateNotifyAction = "✓ Notify me about updates"
	} else {
		m.UpdateNotifyAction = "Notify me about updates"
	}
}

// applyPeerHardware projects management.Status.Peers[] onto the
// "Peers" submenu: one row per peer, formatted "<name> — <gpu>
// (<vram>)". Peers with no Hardware are still rendered (with a
// "(hardware unknown)" hint) so the operator can see which peer is
// missing the push rather than getting an apparently-empty submenu.
// When NO peer published Hardware at all, the submenu stays hidden
// entirely so old daemons keep the pre-Phase-7 "Peers: N" only.
func applyPeerHardware(m *MenuModel, peers []management.PeerStatus) {
	if len(peers) == 0 {
		return
	}
	hasAny := false
	for _, p := range peers {
		if p.Hardware != nil {
			hasAny = true
			break
		}
	}
	if !hasAny {
		return
	}
	rows := make([]PeerHardwareRow, 0, min(len(peers), MaxPeerHardwareRows))
	overflow := 0
	for _, p := range peers {
		if len(rows) >= MaxPeerHardwareRows {
			overflow++
			continue
		}
		rows = append(rows, PeerHardwareRow{Label: formatPeerHardwareLabel(p)})
	}
	m.ShowPeerHardware = true
	m.PeerHardwareParent = fmt.Sprintf("Peers (%d)", len(peers))
	m.PeerHardwareEntries = rows
	m.PeerHardwareOverflow = overflow
}

// formatPeerHardwareLabel builds one row's label. The order of
// preference for the leading identifier is DeviceName → DeviceID →
// "unknown". Hardware tail covers the four shapes:
//
//   - GPU + VRAM: "RTX 4090 (24 GB)"
//   - GPU only:   "RTX 4090"
//   - RAM only:   "CPU only (32 GB RAM)"
//   - nothing:    "(hardware unknown)"
func formatPeerHardwareLabel(p management.PeerStatus) string {
	name := p.DeviceName
	if name == "" {
		name = p.DeviceID
	}
	if name == "" {
		name = "unknown"
	}
	return fmt.Sprintf("%s — %s", name, formatHardwareTail(p.Hardware))
}

func formatHardwareTail(hw *management.PeerHardware) string {
	if hw == nil {
		return "(hardware unknown)"
	}
	if hw.GPUModel != "" {
		if hw.VRAMTotalMB > 0 {
			return fmt.Sprintf("%s (%d GB)", shortGPUModel(hw.GPUModel), vramMBToGB(hw.VRAMTotalMB))
		}
		return shortGPUModel(hw.GPUModel)
	}
	if hw.RAMTotalGB > 0 {
		return fmt.Sprintf("CPU only (%d GB RAM)", hw.RAMTotalGB)
	}
	return "(hardware unknown)"
}

// shortGPUModel drops the "NVIDIA GeForce " prefix that nvidia-smi
// reports so the menu row stays under typical AppIndicator width.
// Non-NVIDIA names (AMD, future Intel/Apple) are left untouched.
func shortGPUModel(model string) string {
	if trimmed, ok := strings.CutPrefix(model, "NVIDIA GeForce "); ok {
		return trimmed
	}
	return model
}

// vramMBToGB rounds MB to the nearest GB. 24576 MB → 24 GB,
// 11264 MB → 11 GB, 23900 MB (a 24 GB device after the driver's
// ~640 MB reservation) → 23 GB. The rounding is intentional — a
// half-GB difference is below the operator's decision threshold.
func vramMBToGB(mb int) int {
	return (mb + 512) / 1024
}

// applyObservability projects the Phase 9 inputs onto the MenuModel:
// it builds the RecentActivity submenu rows from RecentFallbacks
// (subject to the 10-minute cutoff and MaxRecentActivity cap) and,
// when at least one entry survived, promotes IconConnected to
// IconDegraded so the user notices something without opening the
// menu.
func applyObservability(m *MenuModel, snap Snapshot) {
	now := snap.Now
	if now.IsZero() {
		now = time.Now()
	}
	cutoff := now.Add(-RecentFallbackWindow)

	rows := make([]RecentActivityRow, 0, MaxRecentActivity)
	for _, f := range snap.RecentFallbacks {
		if f.TS.Before(cutoff) {
			continue
		}
		rows = append(rows, RecentActivityRow{Label: formatFallbackRow(f, now)})
		if len(rows) >= MaxRecentActivity {
			break
		}
	}

	if len(rows) == 0 {
		// No recent fallback signal — leave the submenu hidden and the
		// icon at whatever the network-state branch picked.
		return
	}

	m.ShowRecentActivity = true
	m.RecentActivityParent = "Recent activity"
	m.RecentActivityEntries = rows
	m.HasRecentFallbackBadge = true
	if m.Icon == IconConnected {
		m.Icon = IconDegraded
	}
}

// formatFallbackRow renders one row of the Recent activity submenu.
// Format: "<model> — <from> → <to> (<reason>, <age>)".
// Long peer IDs are kept as-is — the submenu has enough horizontal
// room and truncation hurts diagnostics more than menu width.
func formatFallbackRow(f FallbackEntry, now time.Time) string {
	from := f.From
	if from == "" {
		from = "—"
	}
	to := f.To
	if to == "" {
		to = "—"
	}
	reason := f.Reason
	if reason == "" {
		reason = "unspecified"
	}
	return fmt.Sprintf("%s — %s → %s (%s, %s ago)",
		shortModel(f.Model), from, to, reason, humanAge(now.Sub(f.TS)))
}

// shortModel drops a registry/family prefix when present so the
// submenu row stays readable. "qwen3:8b-q4_K_M" stays; "ollama/qwen3:8b"
// shrinks to "qwen3:8b".
func shortModel(model string) string {
	if model == "" {
		return "model:?"
	}
	if i := strings.LastIndex(model, "/"); i >= 0 && i+1 < len(model) {
		return model[i+1:]
	}
	return model
}

// humanAge formats a duration as a sub-minute "<1m" or a minute
// count "Nm". The submenu cutoff is 10 minutes, so anything longer is
// clamped to "10m" at the call site by the cutoff filter.
func humanAge(d time.Duration) string {
	if d < time.Minute {
		return "<1m"
	}
	return fmt.Sprintf("%dm", int(d/time.Minute))
}

// applyCatalog projects the catalog response into the tray's MenuModel
// fields. The label format mirrors the table in the plan:
//
//	● Qwen3 8B Instruct                       (active row)
//	Qwen3 4B Instruct (switching…)            (preferred but not yet active — restart in flight)
//	Qwen3 14B Instruct (downloading…)         (pull running)
//	Qwen3 14B Instruct (downloads on select)  (not yet on disk; click triggers pull)
//	Qwen3 32B Instruct — needs 24 GB VRAM     (over capacity, click disabled)
//	Qwen3 4B Instruct                         (default fit + downloaded)
func applyCatalog(m *MenuModel, c *management.ModelCatalogResponse) {
	m.ShowCatalog = true
	m.CatalogParentLabel = "Models"
	if c.Active != nil {
		name := c.Active.DisplayName
		if name == "" {
			name = c.Active.ModelID
		}
		m.CatalogActiveLabel = "Active: " + name
	} else {
		m.CatalogActiveLabel = "Active: (none)"
	}

	n := len(c.Families)
	if n > MaxCatalogEntries {
		n = MaxCatalogEntries
	}
	entries := make([]CatalogEntryView, 0, n)
	for i := 0; i < n; i++ {
		entries = append(entries, formatCatalogEntry(c.Families[i], c.Engine))
	}
	m.CatalogEntries = entries

	// Benchmark-driven switch suggestions. Surface only when the daemon
	// reports a non-dismissed recommendation; clicking re-opens the
	// popup. At most one direction is live at a time (the daemon makes
	// them mutually exclusive), with lighter taking precedence here for
	// safety should that invariant ever slip.
	if rec := c.BenchmarkRecommendation; rec != nil && !rec.Dismissed && rec.ToModelID != "" {
		m.ShowRecommend = true
		m.RecommendLabel = "⚠ Lighter model recommended — switch to " + rec.ToModelID
	} else if rec := c.BenchmarkUpgrade; rec != nil && !rec.Dismissed && rec.ToModelID != "" {
		m.ShowRecommend = true
		m.RecommendLabel = "⬆ Better model available — switch to " + rec.ToModelID
	}
}

// applyMeshReachable surfaces the peers-only inference-mesh aggregate as
// a single display-only status row. nil mesh (daemon predates the mesh
// API, or the poll 404'd) leaves the label empty so old daemons render
// the pre-feature menu. When the mesh IS known we show both the
// reachable and the "nothing reachable" states — unlike submenus that
// hide when empty — so the operator can tell "mesh known, no peer engine"
// apart from "daemon too old to know".
func applyMeshReachable(m *MenuModel, mesh *inferencemesh.Snapshot) {
	if mesh == nil {
		return
	}
	if mesh.Reachable {
		m.MeshReachableLabel = "Mesh: peer engine reachable"
	} else {
		m.MeshReachableLabel = "Mesh: no reachable peer engine"
	}
}

// applyWorker projects the daemon's WorkerResponse + mesh snapshot
// into the "Inference worker" submenu. Three groups:
//
//  1. Top-level summary: "Worker: <peer> (pinned)" / "Worker: auto" so
//     the operator sees current state without expanding.
//  2. Mode rows (auto / local-only / peer-preferred) — fixed slots,
//     selected leading glyph follows w.Mode.
//  3. Pin rows — one per inference-capable peer (Tailscale exit-node
//     filter: peer must advertise an inference engine, even if
//     transiently unavailable). Stale / unreachable peers render
//     "(unavailable)" but stay selectable, matching Tailscale exit-
//     node UX where the pin survives the down period.
//
// Always set ShowWorker=true since the daemon advertised the API by
// populating w; the caller already gated on tunnel phase.
func applyWorker(m *MenuModel, w *management.WorkerResponse, mesh *inferencemesh.Snapshot) {
	if w == nil {
		return
	}
	m.ShowWorker = true
	m.WorkerParentLabel = "Inference worker"
	m.WorkerActiveLabel = "Worker: " + workerSummaryLabel(*w)
	m.WorkerModes = []WorkerModeRow{
		{Mode: state.RoutingModeAuto, Label: "Auto", Selected: w.Mode == state.RoutingModeAuto || w.Mode == ""},
		{Mode: state.RoutingModeLocalOnly, Label: "Local only", Selected: w.Mode == state.RoutingModeLocalOnly},
		{Mode: state.RoutingModePeerPreferred, Label: "Peer preferred", Selected: w.Mode == state.RoutingModePeerPreferred},
	}
	m.WorkerShowClearPin = w.Mode == state.RoutingModePinned

	// Pin entries — filter mesh to inference-capable peers
	// (signer.InferenceState advertised and Type != "none"). Order
	// mirrors the snapshot's insertion order so the menu stays stable
	// poll-over-poll.
	pins := make([]WorkerPinEntryView, 0, MaxWorkerPinEntries)
	if mesh != nil {
		for _, p := range mesh.Peers {
			if !peerIsInferenceCandidate(p) {
				continue
			}
			if len(pins) >= MaxWorkerPinEntries {
				break
			}
			pins = append(pins, WorkerPinEntryView{
				DeviceID:  p.DeviceID,
				Label:     pinEntryLabel(p),
				Available: peerIsServing(p),
				Selected:  w.Mode == state.RoutingModePinned && w.PinnedPeerDeviceID == p.DeviceID,
			})
		}
	}
	// If the pin is set but its peer dropped out of the snapshot
	// entirely (Mesh==nil OR peer absent), keep a row for it labelled
	// as absent so the operator can see what they pinned to.
	if w.Mode == state.RoutingModePinned && w.PinnedPeerDeviceID != "" && !pinPresent(pins, w.PinnedPeerDeviceID) {
		if len(pins) < MaxWorkerPinEntries {
			label := w.PinnedPeerName
			if label == "" {
				label = w.PinnedPeerDeviceID
			}
			pins = append(pins, WorkerPinEntryView{
				DeviceID:  w.PinnedPeerDeviceID,
				Label:     label + " (absent)",
				Available: false,
				Selected:  true,
			})
		}
	}
	m.WorkerPinEntries = pins
}

func workerSummaryLabel(w management.WorkerResponse) string {
	switch w.Mode {
	case "", state.RoutingModeAuto:
		return "auto"
	case state.RoutingModeLocalOnly:
		return "local only"
	case state.RoutingModePeerPreferred:
		return "peer preferred"
	case state.RoutingModePinned:
		name := w.PinnedPeerName
		if name == "" {
			name = w.PinnedPeerDeviceID
		}
		suffix := ""
		switch w.PinnedPeerStatus {
		case "ok":
			// no suffix — the active row already conveys it
		case "unavailable":
			suffix = " (unavailable)"
		case "absent":
			suffix = " (absent)"
		}
		return name + " (pinned)" + suffix
	default:
		return string(w.Mode)
	}
}

// peerIsInferenceCandidate reports whether a peer should appear in the
// "Pin to peer" submenu at all. Mirrors the Tailscale exit-node
// filter: only include nodes that advertise inference capability,
// even if currently inactive. A nil InferenceState means the peer
// has not pushed any engine info and would never be a usable target.
func peerIsInferenceCandidate(p inferencemesh.PeerView) bool {
	if p.InferenceState == nil {
		return false
	}
	t := p.InferenceState.Type
	return t != "" && t != "none"
}

// peerIsServing reports whether the candidate is currently usable
// (active engine + reachable + serving at least one model). Used to
// drive the "(unavailable)" label and the row's enabled state.
func peerIsServing(p inferencemesh.PeerView) bool {
	if p.Stale {
		return false
	}
	if p.InferenceState == nil || !p.InferenceState.Reachable {
		return false
	}
	return len(p.InferenceState.Models) > 0
}

// pinEntryLabel builds "<name> (<model>)" when serving, or "<name>
// (unavailable)" when the peer is an inference candidate but not
// currently active. Falls back to DeviceID when the name is empty.
func pinEntryLabel(p inferencemesh.PeerView) string {
	name := p.DeviceName
	if name == "" {
		name = p.DeviceID
	}
	if !peerIsServing(p) {
		return name + " (unavailable)"
	}
	return name + " (" + p.InferenceState.Models[0] + ")"
}

func pinPresent(pins []WorkerPinEntryView, deviceID string) bool {
	for _, p := range pins {
		if p.DeviceID == deviceID {
			return true
		}
	}
	return false
}

func formatCatalogEntry(f management.CatalogFamily, engine string) CatalogEntryView {
	name := f.DisplayName
	if name == "" {
		name = f.ModelID
	}
	e := CatalogEntryView{ModelID: f.ModelID}
	// Compact recommended-memory hint appended to fitting/downloadable
	// rows. Over-capacity rows already spell out the requirement in their
	// deficit label, so the suffix would be redundant there.
	suffix := catalogSpecSuffix(engine, f.Recommended)
	switch {
	case f.Active:
		e.Label = "● " + name + suffix
	case f.Preferred:
		// Preference recorded but not yet reflected in the running
		// agent's Active selection. The catalog endpoint surfaces
		// preferred=true on the row the user just clicked; the agent
		// flips to it after restart.
		e.Label = name + " (switching…)" + suffix
	case f.Downloading:
		e.Label = name + " (downloading…)" + suffix
	case !f.Fits:
		if f.DeficitLabel != "" {
			e.Label = name + " — " + f.DeficitLabel
		} else {
			e.Label = name + " — incompatible"
		}
		e.Disabled = true
	case !f.Downloaded:
		e.Label = name + " (downloads on select)" + suffix
	default:
		e.Label = name + suffix
	}
	e.Tooltip = catalogSpecTooltip(engine, f.Recommended)
	return e
}

// engineVLLM mirrors catalog.RuntimeVLLM — the value the management
// catalog endpoint reports in ModelCatalogResponse.Engine. Kept as a
// local literal so the tray view-model layer needs no catalog import.
const engineVLLM = "vllm"

// catalogSpecSuffix returns a compact "· N GB" recommended-memory hint
// for menu labels — RAM on ollama, VRAM on vllm (the host serves one
// engine at a time, so the unit is implied). Empty when no recommended
// spec is available.
func catalogSpecSuffix(engine string, rec *management.CatalogSpec) string {
	if gb := recommendedSpecGB(engine, rec); gb > 0 {
		return fmt.Sprintf(" · %d GB", gb)
	}
	return ""
}

// catalogSpecTooltip is the fuller per-row hint: min RAM/VRAM, quality
// tier, and parameter counts. Best-effort — some Linux indicators drop
// menu-item tooltips. Empty when no spec is available.
func catalogSpecTooltip(engine string, rec *management.CatalogSpec) string {
	if rec == nil {
		return ""
	}
	var parts []string
	if gb := recommendedSpecGB(engine, rec); gb > 0 {
		unit := "RAM"
		if engine == engineVLLM {
			unit = "VRAM"
		}
		parts = append(parts, fmt.Sprintf("min %d GB %s", gb, unit))
	}
	if rec.QualityTier > 0 {
		parts = append(parts, fmt.Sprintf("quality tier %d", rec.QualityTier))
	}
	if p := formatTrayParams(rec.ParamCount, rec.ActiveParams); p != "" {
		parts = append(parts, p+" params")
	}
	if len(parts) == 0 {
		return ""
	}
	return "Recommended: " + strings.Join(parts, " · ")
}

// recommendedSpecGB returns the engine-appropriate recommended memory in
// whole GB: min VRAM (rounded) on vllm, min RAM on ollama. 0 when unknown.
func recommendedSpecGB(engine string, rec *management.CatalogSpec) int {
	if rec == nil {
		return 0
	}
	if engine == engineVLLM {
		if rec.MinVRAMMB > 0 {
			return (rec.MinVRAMMB + 512) / 1024
		}
		return 0
	}
	return rec.MinRAMGB
}

// formatTrayParams humanizes the total parameter count, appending the
// MoE active count when it differs (e.g. "30B (3.3B active)").
func formatTrayParams(total, active int64) string {
	if total <= 0 {
		return ""
	}
	s := humanizeParamCount(total)
	if active > 0 && active != total {
		s += fmt.Sprintf(" (%s active)", humanizeParamCount(active))
	}
	return s
}

func humanizeParamCount(n int64) string {
	const billion = 1_000_000_000
	const million = 1_000_000
	switch {
	case n >= billion:
		v := float64(n) / billion
		if v >= 100 || v == float64(int64(v)) {
			return fmt.Sprintf("%.0fB", v)
		}
		return fmt.Sprintf("%.1fB", v)
	case n >= million:
		return fmt.Sprintf("%.0fM", float64(n)/million)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// applyClaude fills the Claude-integration section. The header reports the
// live serving state (proxy routing claude to local inference vs. falling
// back to real Anthropic); the proxy row reports the OS-level install
// status. When serving is unreachable while the network is up the icon
// swaps to the degraded variant so the user notices — but note the
// transparent proxy always fails open, so "inactive" means "falling back",
// not "claude is broken".
func applyClaude(m *MenuModel, st *management.ClaudeIntegrationStatus) {
	m.ShowClaude = true
	if st.Wrapper.Reachable {
		m.ClaudeHeader = "Claude integration: ● active"
	} else {
		reason := st.Wrapper.Reason
		if reason == "" {
			reason = "unknown"
		}
		m.ClaudeHeader = "Claude integration: ○ inactive (" + reason + ")"
		if m.Kind == MenuConnected {
			m.Icon = IconDegraded
		}
	}
	m.ClaudeProxyLabel = renderManagedSettingsLabel(st.ManagedSettings)
}

// renderManagedSettingsLabel summarises the Claude Code managed-settings status
// for the tray (#488): whether ANTHROPIC_BASE_URL is wired to the local gateway.
// There is no per-toggle action row — enable/disable is the root
// `waired claude enable|disable` command.
func renderManagedSettingsLabel(ms management.ClaudeManagedSettingsView) string {
	if !ms.Supported {
		return "Claude: ⚠ managed settings unsupported on this OS"
	}
	switch {
	case ms.Configured:
		return "Claude: ✓ routed to local gateway"
	case ms.Present && ms.BaseURL != "":
		return "Claude: ⚠ ANTHROPIC_BASE_URL set elsewhere (" + ms.BaseURL + ")"
	default:
		return "Claude: ✗ not configured (" + claudeEnableHint() + ")"
	}
}

// applyClaudeRouting projects the unified per-class routing state (#649)
// into the "Claude Code" submenu: a main-conversation route group
// (auto/waired/anthropic) and a subagent group (same/auto/waired/anthropic),
// each with the current selection flagged. Node selection is intentionally
// absent — that lives in the Inference worker submenu. claude carries the
// managed-settings view (may be nil) so the submenu can note when routing is
// not active yet with the OS-correct enable hint.
func applyClaudeRouting(m *MenuModel, st *management.ClaudeRoutingState, claude *management.ClaudeIntegrationStatus) {
	m.ShowClaudeCode = true
	m.ClaudeCodeParent = "Claude Code"

	mainRoute := st.Policy.Main
	if mainRoute == "" || mainRoute == state.ClaudeRouteSame {
		mainRoute = state.ClaudeRouteAuto
	}
	m.ClaudeMainRouteRows = []ClaudeRouteRow{
		{Class: state.ClaudeRouteAuto, Label: "Auto (Waired-first)", Selected: mainRoute == state.ClaudeRouteAuto},
		{Class: state.ClaudeRouteWaired, Label: "Waired only", Selected: mainRoute == state.ClaudeRouteWaired},
		{Class: state.ClaudeRouteAnthropic, Label: "Anthropic", Selected: mainRoute == state.ClaudeRouteAnthropic},
	}

	subRoute := st.Policy.Sub
	if subRoute == "" {
		subRoute = state.ClaudeRouteSame
	}
	m.ClaudeSubRouteRows = []ClaudeRouteRow{
		{Class: state.ClaudeRouteSame, Label: "Same as main", Selected: subRoute == state.ClaudeRouteSame},
		{Class: state.ClaudeRouteAuto, Label: "Auto (Waired-first)", Selected: subRoute == state.ClaudeRouteAuto},
		{Class: state.ClaudeRouteWaired, Label: "Waired only", Selected: subRoute == state.ClaudeRouteWaired},
		{Class: state.ClaudeRouteAnthropic, Label: "Anthropic", Selected: subRoute == state.ClaudeRouteAnthropic},
	}

	// No-silent-breakage: surface the last fallback as a disabled note so a
	// degrade Claude Code took is visible in the tray too (it is also shown
	// in the Claude Code statusline + as a Recent-activity row). The icon
	// promotion is left to applyObservability, which already flips
	// IconDegraded on recent fallback events — duplicating it here would
	// double-count and flicker.
	if st.LastFallback != nil {
		m.ClaudeFallbackNote = claudeFallbackNote(st.LastFallback)
	}

	// When Claude Code is not yet routed through Waired, the route choice is
	// persisted but inert — say so with the OS-correct enable hint.
	if claude != nil && claude.ManagedSettings.Supported && !claude.ManagedSettings.Configured {
		m.ClaudeEnableNote = "ⓘ not active yet — " + claudeEnableHint()
	}
}

// claudeFallbackNote renders the last-fallback disabled row. Direction
// "anthropic" means an auto request was rescued by the real API (Waired
// failed); "local" means an anthropic/peer request was served locally
// instead (upstream/peer unavailable) — the two read very differently to a
// user worried about where their prompts went.
func claudeFallbackNote(ev *management.ClaudeRoutingFallbackEvent) string {
	if ev == nil {
		return ""
	}
	switch ev.Direction {
	case "local":
		return "⚠ last served locally (Anthropic/peer unavailable)"
	default: // "anthropic" (or unset legacy)
		return "⚠ last fell back → Anthropic (Waired unavailable)"
	}
}

// applyOpenCode fills the OpenCode-integration section. Stale config
// while the network is connected swaps the icon to the degraded
// variant — same treatment as a missing Claude wrapper, so the user
// notices integration drift even before they expand the menu.
func applyOpenCode(m *MenuModel, st *management.OpenCodeIntegrationStatus) {
	m.ShowOpenCode = true
	m.OpenCodeReconfigureLabel = "Reconfigure…"

	cfg := st.Config
	switch {
	case cfg.Note != "":
		m.OpenCodeHeader = "OpenCode integration: ⚠ unreadable (" + cfg.Note + ")"
		m.OpenCodeConfigLabel = "Config: ⚠ " + cfg.Path
		if m.Kind == MenuConnected {
			m.Icon = IconDegraded
		}
	case !cfg.Configured:
		m.OpenCodeHeader = "OpenCode integration: ○ not configured"
		m.OpenCodeConfigLabel = "Config: ✗ not configured"
	case cfg.Stale:
		shown := cfg.CurrentValue
		if shown == "" {
			shown = "drifted"
		}
		m.OpenCodeHeader = "OpenCode integration: ⚠ stale (" + shown + ")"
		m.OpenCodeConfigLabel = "Config: ⚠ stale (" + shown + ")"
		if m.Kind == MenuConnected {
			m.Icon = IconDegraded
		}
	default:
		m.OpenCodeHeader = "OpenCode integration: ● configured"
		m.OpenCodeConfigLabel = "Config: ✓ " + cfg.Path
	}
}

// applyOpenClaw fills the OpenClaw-integration section. Mirrors
// applyOpenCode: stale config while connected degrades the icon so the
// user notices integration drift before expanding the menu.
func applyOpenClaw(m *MenuModel, st *management.OpenClawIntegrationStatus) {
	m.ShowOpenClaw = true
	m.OpenClawReconfigureLabel = "Reconfigure…"

	cfg := st.Config
	switch {
	case cfg.Note != "":
		m.OpenClawHeader = "OpenClaw integration: ⚠ unreadable (" + cfg.Note + ")"
		m.OpenClawConfigLabel = "Config: ⚠ " + cfg.Path
		if m.Kind == MenuConnected {
			m.Icon = IconDegraded
		}
	case !cfg.Configured:
		m.OpenClawHeader = "OpenClaw integration: ○ not configured"
		m.OpenClawConfigLabel = "Config: ✗ not configured"
	case cfg.Stale:
		shown := cfg.CurrentValue
		if shown == "" {
			shown = "drifted"
		}
		m.OpenClawHeader = "OpenClaw integration: ⚠ stale (" + shown + ")"
		m.OpenClawConfigLabel = "Config: ⚠ stale (" + shown + ")"
		if m.Kind == MenuConnected {
			m.Icon = IconDegraded
		}
	default:
		m.OpenClawHeader = "OpenClaw integration: ● configured"
		m.OpenClawConfigLabel = "Config: ✓ " + cfg.Path
	}
}

// applyInference fills the inference group fields. SubsystemState comes
// from the agent (engine health) and is independent of DesiredState
// (operator's enable/disable intent) — the agent reports SubsystemState=
// "disabled" when the operator has the engine turned off.
func applyInference(m *MenuModel, inf *management.InferenceStatus) {
	m.InferenceStateLabel = "Engine: " + humanInferenceState(inf.SubsystemState)
	// Engine provenance (display-only): suffix non-spawned ownership to
	// the state label and surface the agent-computed version warning /
	// failure detail. Old daemons leave these fields empty.
	if ol, ok := inf.Runtimes["ollama"]; ok {
		if ol.Mode != "" && ol.Mode != "spawned" {
			m.InferenceStateLabel += " (" + ol.Mode + ")"
		}
		switch {
		case ol.VersionWarning != "":
			m.EngineWarningLabel = "⚠ " + ol.VersionWarning
		case ol.LastError != "":
			m.EngineWarningLabel = "⚠ " + ol.LastError
		}
	}
	if inf.Active != nil && inf.Active.ModelID != "" {
		m.ActiveModelLabel = "Model: " + inf.Active.ModelID
	}
	// Toggle action mirrors DesiredState (= what the operator most
	// recently asked for). When no engine is registered there's no
	// engine to toggle — hide the action so we don't bait clicks.
	if inf.SubsystemState != "no_engine" {
		switch inf.DesiredState {
		case "enabled":
			m.InferenceToggleAction = "Disable inference engine"
		case "disabled":
			m.InferenceToggleAction = "Enable inference engine"
		}
	}

	// Share toggle (Phase 6). Hidden when:
	// - the daemon predates the share API (= empty share_with_mesh), or
	// - no engine is registered (SubsystemState=="no_engine"): there's
	//   nothing to share, so the row would be confusing.
	// Visible alongside the inference toggle in every other state so
	// the privacy knob is discoverable even when the engine is
	// soft-disabled (the operator may want to flip share before
	// re-enabling the engine).
	if inf.SubsystemState == "no_engine" {
		// No usable engine installed: offer the auto-installer instead of
		// the (meaningless) enable/disable + share toggles (#188).
		m.InstallEngineAction = "Install Ollama…"
		return
	}
	switch inf.ShareWithMesh {
	case "shared":
		m.ShareToggleAction = "Stop sharing engine to mesh"
		m.ShareStateLabel = "Sharing: enabled"
	case "not_shared":
		m.ShareToggleAction = "Share engine to mesh"
		m.ShareStateLabel = "Sharing: disabled"
	}

	// Hard engine power axis (#186). Reached only when a usable engine
	// exists (the no_engine branch returned above). Empty EnginePower
	// means the daemon predates engine control → leave the row hidden.
	switch {
	case inf.EnginePower == "":
		// hidden
	case !inf.EngineManaged:
		// Reuse mode: the engine is the user's own process, so waired
		// can't free it. Show the row disabled so the absence is
		// explained rather than mysterious.
		m.EngineToggleAction = "Engine reused — not managed"
		m.EngineToggleEnabled = false
	case inf.EnginePower == "stopped":
		m.EngineToggleAction = "Start inference engine"
		m.EngineToggleEnabled = true
	default: // running / starting
		m.EngineToggleAction = "Stop inference engine"
		m.EngineToggleEnabled = true
	}
}

// humanInferenceState maps the wire SubsystemState (snake_case) to a
// short label suitable for menu rendering.
func humanInferenceState(s string) string {
	switch s {
	case "no_engine":
		return "no engine"
	case "awaiting_model":
		return "awaiting model"
	case "pull_failed":
		return "pull failed"
	case "stopped":
		return "stopped (memory freed)"
	}
	return s // ready / loading / starting / disabled / degraded / initializing
}

// identityDeviceName returns the user-facing device name. We currently
// reuse DeviceID until the Identity carries a separate human name field;
// keeping a helper isolates that future swap.
func identityDeviceName(id *management.IdentityView) string {
	if id == nil {
		return ""
	}
	if id.DeviceName != "" {
		return id.DeviceName
	}
	return id.DeviceID
}

// adminURL appends "/admin" to the control plane URL, defending against
// trailing slashes. Returns "" when ControlURL is empty so the menu can
// hide the Open Admin Console item.
func adminURL(controlURL string) string {
	controlURL = strings.TrimSpace(controlURL)
	if controlURL == "" {
		return ""
	}
	return strings.TrimRight(controlURL, "/") + "/admin"
}
