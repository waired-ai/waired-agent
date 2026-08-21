// Package tray builds the Waired desktop tray UI on top of fyne.io/systray.
// state.go owns the pure projection from a polling snapshot to a menu
// model so the rendering glue can stay free of branching logic.
package tray

import (
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/platform/service"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
	"github.com/waired-ai/waired-agent/proto/hostfit"
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
	Health Health
	// IdentityErr marks a failed /identity call — a transport error, not the
	// daemon answering "nobody is enrolled" (Client.Identity folds 404 into
	// {Enrolled:false}). Update needs the distinction: a device with an
	// identity on disk must not be shown the signed-out menu because one poll
	// could not reach the endpoint (#317 item 5 / #318).
	IdentityErr bool
	Identity    *management.IdentityView
	Status      *management.Status
	Inference   *management.InferenceStatus         // nil for daemons predating the inference toggle API
	Claude      *management.ClaudeIntegrationStatus // nil for daemons predating /waired/v1/integration/claude
	// ClaudeRouting is the unified per-class routing state (#649); nil for
	// daemons predating /waired/v1/integration/claude/route so the "Claude
	// Code" routing submenu stays hidden.
	ClaudeRouting *management.ClaudeRoutingState
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

	// Public Share (waired#833). All three are best-effort and gate
	// independently: each is nil on a daemon that predates the matching
	// endpoint (a 404 on any one leaves it nil), so an old daemon renders
	// the pre-feature menu unchanged. PublicShare is the provider toggle
	// state; PublicUse is the consumer-side settings + consent status;
	// PublicWarning is the served consent text (its "More:" line feeds the
	// "Privacy & safety…" link).
	PublicShare   *management.PublicShareStateResponse
	PublicUse     *management.PublicUseResponse
	PublicWarning *management.PublicWarningResponse

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
// the Catalog submenu. Bundled manifests are 21 today; 32 leaves headroom
// for a future "Other quantizations" / external manifest before another
// pre-allocation bump is needed.
//
// Families render in manifest (alphabetical) order, so an undersized cap
// silently amputates the tail — twice now: at 12 it took qwen3.6-27b, and
// at 20 it took qwen3.6-35b-a3b off a host that was actively SERVING it
// (waired-agent#319). Two things keep that from recurring:
// TestCatalogCapCoversBundledManifests fails the build the moment the
// bundled count catches up with this constant, and applyCatalog never
// drops the active/preferred row even when a cap does bite.
const MaxCatalogEntries = 32

// MaxWorkerPinEntries caps the pin group's pre-allocation in the
// "Inference routing" submenu. Mirrors MaxPeerHardwareRows so the
// operator sees the same set of peers in both submenus on hosts with
// more than 16 mesh members.
const MaxWorkerPinEntries = 16

// workerModeSlots is how many automatic-mode rows the routing submenu
// pre-allocates: auto / local-only / peer-preferred / peer-only. The
// rows are a fixed set, not a mesh-driven one, so this is the exact
// count applyWorker emits — TestApplyWorkerModeSlotsMatchPreallocation
// fails the build if the two ever disagree, since a mode past the
// pre-allocation would be silently unclickable.
const workerModeSlots = 4

// WorkerModeRow is one row inside the "Inference routing" submenu's
// automatic-selection group (auto / local-only / peer-preferred /
// peer-only). The Selected flag drives the leading "●" / "○" glyph in
// apply().
type WorkerModeRow struct {
	Mode     state.RoutingMode
	Label    string
	Selected bool
}

// residencyPresetSlots is how many model-residency preset rows the
// inference submenu pre-allocates. The set is fixed, not data-driven, so
// this is the exact count residencyRows emits —
// TestResidencyPresetSlotsMatchPreallocation fails the build if the two
// disagree, since a preset past the pre-allocation would be silently
// unclickable (the workerModeSlots arrangement).
const residencyPresetSlots = 4

// ResidencyRow is one row of the model-residency preset group inside the
// "Inference" submenu (waired-agent#861). Selected drives the leading
// "●" / "○" glyph in apply(), like WorkerModeRow.
type ResidencyRow struct {
	Idle     time.Duration
	Label    string
	Selected bool
}

// residencyRows builds the preset group for the setting currently in
// force. "Always" is first because it is the default (owner ruling,
// docs/decisions/20260820/0130-model-residency-is-a-setting.md): the
// reload it avoids costs the next request a weights load and a full
// prompt re-read.
func residencyRows(idle time.Duration) []ResidencyRow {
	presets := []struct {
		d     time.Duration
		label string
	}{
		{0, "Always"},
		{15 * time.Minute, "15 minutes"},
		{time.Hour, "1 hour"},
		{8 * time.Hour, "8 hours"},
	}
	out := make([]ResidencyRow, 0, len(presets))
	for _, p := range presets {
		out = append(out, ResidencyRow{Idle: p.d, Label: p.label, Selected: p.d == idle})
	}
	return out
}

// residencyValueLabel renders a residency for the header caption. A zero
// is spelled out: "0s" reads as "unload immediately", the opposite of
// what it means.
func residencyValueLabel(idle time.Duration) string {
	if idle <= 0 {
		return "always"
	}
	for _, r := range residencyRows(idle) {
		if r.Selected {
			return strings.ToLower(r.Label)
		}
	}
	return idle.String()
}

// ClaudeRouteRow is one selectable route inside the "Claude Code"
// submenu — a main-conversation route (auto/waired/anthropic) or a
// subagent route (same/auto/waired/anthropic). Selected drives the
// leading "●" / "○" glyph in apply(); Class is the value POSTed on
// click. Node selection is deliberately NOT here — it lives in the
// "Inference routing" submenu (#649: `waired worker`).
type ClaudeRouteRow struct {
	Class    state.ClaudeRouteClass
	Label    string
	Selected bool
}

// WorkerPinEntryView is one row inside the "Inference routing"
// submenu's pin group. The label is the operator-visible name (with a model
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
// (min RAM/VRAM · size class · params); it is best-effort since some
// Linux indicators don't render menu-item tooltips.
type CatalogEntryView struct {
	ModelID string
	// Name is the family's display name with no row decoration — the
	// same string Label is built from. Kept alongside Label because the
	// switch-accepted notification names the model in a sentence, and
	// Label carries row state ("● ", " (switching…)", the spec suffix)
	// that reads as noise there (waired#808).
	Name    string
	Label   string
	Tooltip string
	// UnfitReason is why this computer is not expected to run the model,
	// in the same words the row's own label carries, or "" for a row that
	// runs here. It is a WARNING, never a block: the row stays clickable
	// and the click asks (waired-agent#831).
	//
	// There is deliberately no Disabled field. Greying a model out was
	// withdrawn on 2026-08-08 — "no hard blocks on model selection on any
	// product surface" (waired-ai/waired#1067; the decision record is
	// docs/decisions/20260808/2325-capacity-warns-and-asks-not-refuses.md
	// in the private repo, superseding the refusal rule of
	// waired-ai/waired#1056). The tray was the last surface still doing
	// it, so the capability is gone from the type rather than merely
	// unused: a future row cannot re-introduce a block by setting a flag.
	UnfitReason string
	// UnfitKind is what KIND of verdict UnfitReason came from, so the
	// click's question can be worded from the verdict instead of by
	// matching the rendered string (waired-agent#850).
	UnfitKind UnfitKind
}

// UnfitKind classifies an unfit verdict by what the wall actually is.
// Only the two named walls get a sentence naming a cause; everything
// else — including a verdict hostfit does not model — is UnfitOther and
// is repeated back rather than explained.
type UnfitKind string

const (
	// UnfitNone is a row this computer is expected to run.
	UnfitNone UnfitKind = ""
	// UnfitMemory: the model does not fit the memory this host has.
	UnfitMemory UnfitKind = "memory"
	// UnfitNoBuild: this way of running AI has no build of the model.
	UnfitNoBuild UnfitKind = "no-build"
	// UnfitOther: unfit for a reason this layer has no sentence for. An
	// allowlist rather than "anything that is not no-build", so a reason
	// added later lands here and repeats the row instead of inheriting a
	// sentence about memory it knows nothing about.
	//
	// The engine-version floor used to be the example, because its
	// branch left Fit at its zero value and DeficitLabel was the only
	// answer. It is not any more: #853 gives that branch
	// hostfit.ReasonEngineTooOld with the needed and running versions on
	// it. It still lands here, and deliberately — the row already spells
	// out the requirement, so repeating it is the honest wording; the
	// reason is now available should that ever want its own sentence.
	UnfitOther UnfitKind = "other"
)

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

// IconState picks one of the tray-icon variants. IconDegraded is the
// "connected with claude-integration warning" badge: same network
// state as IconConnected but with a small yellow ! glyph overlaid in
// the upper-right, so the user notices at a glance that something
// (currently: the wrapper's per-spawn gating) needs attention.
// IconBusy is the connected mark lit in "active green" while the local
// inference engine is serving at least one request (waired#811); it is
// the lowest-priority overlay — only ever promoted from IconConnected,
// so it never masks an error/degraded/disconnected state.
type IconState int

const (
	IconError IconState = iota
	IconConnected
	IconDisconnected
	IconDegraded
	IconBusy
)

// MenuModel is the rendered intent — pure data, no widgets. The builder
// in tray.go translates each field into a systray menu item.
type MenuModel struct {
	Kind        MenuKind
	Icon        IconState
	HeaderTitle string // "● Connected", "○ Not signed in", etc.
	// DegradedReason is a short summary of *why* the icon was promoted to
	// IconDegraded (e.g. "Claude Code routing inactive"). Derived and folded
	// into HeaderTitle by summariseAggregateHeader at the end of Update, so
	// the single top-level status row states the cause while per-subsystem
	// detail moves into submenus (waired#809). Empty unless degraded.
	DegradedReason string
	AccountEmail   string
	DeviceName     string
	OverlayIP      string
	NetworkName    string
	PeerCount      int
	AdminURL       string // "" hides the Open Admin Console... item
	StatusMsg      string // body for daemon-down / error states
	// ToggleAction is the label the connect-toggle menu item should render:
	// "Disconnect" | "Connect" | "Sign in..." | "" (hidden).
	ToggleAction string

	// Daemon-down actions (#315/#317). StartAgentAction and StartAgentCopy
	// are the two rows' labels — non-empty shows the row — and StartAgentCmd
	// is the command they act on: the elevated start runs it via the OS
	// service manager, and the copy row puts it on the clipboard for a user
	// who would rather run it themselves. All three are empty unless the
	// service is registered and the OS has a service backend, since a button
	// that cannot work is worse than no button.
	StartAgentAction string
	StartAgentCopy   string
	StartAgentCmd    string

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
	//
	// ShowInferenceMenu gates the "Inference ▸" submenu parent that houses
	// this whole group (waired#809): true whenever applyInference ran (the
	// daemon exposes the inference API), so old daemons render no empty
	// parent. The engine/share/mesh/worker/recommend rows live under it.
	ShowInferenceMenu     bool
	InferenceToggleAction string // labelPauseInference | labelResumeInference | ""
	InferenceStateLabel   string // "Engine: ready" / "Engine: disabled" / "Engine: loading" / ...
	// EngineToggleAction drives the hard engine power axis (#186):
	// labelStopEngine (engine up → free VRAM/RAM) | labelStartEngine
	// (engine stopped → restart) | "" (hidden: daemon predates engine
	// control, or the engine is not managed).
	EngineToggleAction string
	// EngineToggleEnabled is false when the item should render but be
	// greyed out — currently only the not-managed case, which keeps
	// the row visible (so the user understands why) instead of hiding it.
	EngineToggleEnabled bool
	ActiveModelLabel    string // "Model: <model_id>" or ""
	// UnloadModelAction is the label of the "free the model's memory
	// without stopping the engine" row (waired-agent#861), or "" when the
	// daemon predates the control. Sibling of EngineToggleAction, which
	// frees the same memory by stopping the engine — and thereby stops
	// answering. UnloadModelEnabled is false when nothing is loaded, so
	// the row explains the action instead of baiting a click that would
	// do nothing (the EngineToggleEnabled treatment).
	UnloadModelAction  string
	UnloadModelEnabled bool
	// ResidencyHeader is the disabled caption above the residency preset
	// rows ("Keep model in memory: always"), or "" when the daemon does
	// not report the setting. It carries the CURRENT value, which is what
	// makes a value set from the CLI or the control plane visible here
	// even when it matches none of the presets below.
	ResidencyHeader string
	// ResidencyRows are the selectable presets. The tray cannot take free
	// text, so it offers a fixed set and leaves arbitrary durations to
	// `waired inference residency`.
	ResidencyRows []ResidencyRow
	// InstallEngineAction is "Install Ollama…" when SubsystemState is
	// "no_engine" (no usable local engine installed), else "". Clicking
	// it runs the auto-installer (#188).
	InstallEngineAction string

	// Share-with-mesh toggle (Phase 6). Sibling of the inference
	// engine toggle: lets the operator stop exposing the local engine
	// to mesh peers without turning the engine off locally. Both
	// fields are empty when the daemon predates the share API (= no
	// share_with_mesh field on /inference/status).
	ShareToggleAction string // labelStopSharing | labelStartSharing | ""
	ShareStateLabel   string // "Sharing: enabled" | "Sharing: disabled" | "Sharing: paused" | ""

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
	// on Linux (waired/docs/decisions/), this section reports PROXY status
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
	// menu. Node selection is NOT here — that stays in the Inference routing
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

	// OpenClaw integration group — populated when the daemon exposes
	// /waired/v1/integration/openclaw. Mirrors the Claude shape but with
	// a single Config row (the plugin is one directory) plus a
	// Reconfigure trigger that re-runs `waired link openclaw` after
	// confirmation. ShowOpenClaw=false hides the entire section.
	ShowOpenClaw             bool
	OpenClawHeader           string // "OpenClaw integration: ● configured" / "⚠ stale (...)" / "○ not configured"
	OpenClawConfigLabel      string // "Config: ✓ ~/.openclaw/plugins/waired/index.mjs" / "✗ not configured" / "⚠ stale (<currentValue>)"
	OpenClawReconfigureLabel string // "Reconfigure…" — clicking spawns the confirmation dialog

	// Catalog submenu — populated when the daemon exposes
	// /waired/v1/inference/catalog. ShowCatalog=false hides the entire
	// section so old daemons render exactly the pre-extension menu.
	ShowCatalog        bool
	CatalogActiveLabel string             // "Active: Qwen3 8B Instruct" — visible at the top level
	CatalogParentLabel string             // "Models" — parent of the submenu
	CatalogEntries     []CatalogEntryView // ≤ MaxCatalogEntries rows; rest of the pre-allocated slots stay hidden

	// CatalogNoteLabel is a display-only line above the model rows, "" on
	// a host that needs no such context. Today it says only one thing:
	// there is no AI engine here (#852). The rows below it keep their
	// ordinary verdicts, because those are true about what this computer
	// WOULD run — what was missing is that it will not run any of them
	// itself, and that the requests go somewhere real instead.
	CatalogNoteLabel string

	// CatalogEngineMissing is that same fact as a predicate, for the
	// click path rather than the display: selecting a model here is
	// answered by offering to install the engine (owner ruling,
	// 2026-08-19) instead of silently recording a preference nothing on
	// this host can act on. False on a daemon predating engine_installed,
	// which keeps the pre-#852 behaviour.
	CatalogEngineMissing bool

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
	// path). ShowWorker=false hides the mode/pin rows so old daemons
	// render exactly the pre-worker-pin menu.
	//
	// Since #327 these rows live under their OWN top-level parent
	// ("Inference routing"), not inside "Inference": engine control and
	// request routing are separate questions, and the review found them
	// indistinguishable when both sat in one flat submenu.
	// ShowRoutingMenu gates that parent — it is true when EITHER the
	// worker rows or the mesh-reachable row has something to show, so a
	// daemon that exposes only one of the two still renders a parent
	// with content behind it.
	ShowRoutingMenu    bool
	ShowWorker         bool
	WorkerActiveLabel  string               // "Worker: linux-gpu (pinned)" — first row of the routing submenu
	WorkerParentLabel  string               // "Inference routing" — the top-level parent
	WorkerModesHeader  string               // "Choose automatically" — disabled header above the mode rows
	WorkerPinsHeader   string               // "Pin to one peer" — disabled header above the pin rows
	WorkerModes        []WorkerModeRow      // 4 fixed rows: auto / local-only / peer-preferred / peer-only
	WorkerPinEntries   []WorkerPinEntryView // ≤ MaxWorkerPinEntries peer rows
	WorkerShowClearPin bool                 // true when mode==pinned so "(clear pin)" appears

	// Public share submenu (waired#833). ShowPublicShareMenu gates the
	// whole "Public share" parent: false on daemons exposing neither the
	// provider toggle nor the consumer settings, so old daemons render the
	// pre-feature menu. The provider group (PublicShareToggleAction /
	// PublicShareStateLabel / PublicShareNote) drives the opt-in kill
	// switch; the consumer group (ShowPublicUse + PublicUseHeaderLabel +
	// PublicUseModes + PublicUseConsented) drives the three "use public
	// computers" mode rows. PublicMoreURL is the "Privacy & safety…" link
	// extracted from the served warning text — never hardcoded.
	ShowPublicShareMenu     bool
	PublicShareToggleAction string // "Share this computer publicly" | "Stop sharing this computer publicly" | ""
	PublicShareStateLabel   string // "Public sharing: on" / "…: off" (+ " (saving…)") | ""
	PublicShareNote         string // served pending-sync note, shown while CPSynced is false
	ShowPublicUse           bool
	PublicUseHeaderLabel    string // "Use public computers" section label
	PublicUseModes          []PublicUseModeRow
	PublicUseConsented      bool
	PublicMoreURL           string // "Privacy & safety…" target, from the served warning's "More:" line
}

// PublicUseModeRow is one row inside the "Public share" submenu's
// consumer-mode group (off / auto / explicit). Mode is the wire value
// POSTed on click; Selected drives the leading "●" / "○" glyph in
// apply(), like WorkerModeRow. The label is tray-authored plain English
// (NOT the served consent copy).
type PublicUseModeRow struct {
	Mode     string
	Label    string
	Selected bool
}

// daemonDownFacts is everything outside the /status poll that shapes the
// agent-is-down menu.
type daemonDownFacts struct {
	// ServiceInstalled is service.Installed(). False on a raw-binary dev run,
	// where there is no service to start and the button would be a dead end.
	ServiceInstalled bool
	// Starting marks the window right after login/boot during which the
	// service is expected to still be coming up (see startGraceFor).
	Starting bool
	// LastEmail is the account from the last snapshot taken while the daemon
	// was reachable. A stopped service does not sign anyone out, so the menu
	// keeps saying who is signed in (#317: the tray "read as logged out").
	LastEmail string
}

// daemonDownModel is the "agent not running" menu shown when the local
// management API is unreachable.
func daemonDownModel(f daemonDownFacts) MenuModel {
	return daemonDownModelFor(runtime.GOOS, f)
}

// daemonDownModelFor is untagged and takes the GOOS so all three platforms'
// copy and affordances are table-tested on the Linux leg — the only leg that
// runs tests. The previous shape (a per-OS startAgentHint() in hint_*.go) is
// exactly how macOS shipped a command for a launchd job that #520 had deleted.
//
// Two states, not one:
//
//   - Starting. The tray autostarts at login while the service is registered
//     delayed-auto-start, so on Windows there is a 2-3 minute window at every
//     boot where the agent is legitimately not up yet (#315, root cause 2).
//     Painting the red "not running" alarm there — and, now, offering a button
//     that pops UAC — trains people to ignore both. The start row still shows,
//     because a user who wants it sooner should not have to wait out a timer.
//   - Down. Past the window, this is a real failure and says so.
func daemonDownModelFor(goos string, f daemonDownFacts) MenuModel {
	cmd := service.StartHintFor(goos)
	m := MenuModel{
		Kind:             MenuDaemonDown,
		Icon:             IconError,
		HeaderTitle:      "⚠ Waired agent is not running",
		StatusMsg:        "The background service is stopped or still starting — it can take about 2 minutes after you sign in.",
		AccountEmail:     f.LastEmail,
		StartAgentAction: startAgentActionLabel,
		StartAgentCopy:   startAgentCopyLabel,
		StartAgentCmd:    cmd,
	}
	if f.Starting {
		m.Icon = IconBusy
		m.HeaderTitle = "◐ Waired agent is starting…"
		m.StatusMsg = "The background service starts a couple of minutes after you sign in. You can start it now if you don't want to wait."
	}
	// Nothing to start: a hand-built binary run from a terminal has no
	// registered service, and an OS with no service backend has no command to
	// offer. Say so rather than presenting an action that cannot work.
	if !f.ServiceInstalled || cmd == "" {
		m.StartAgentAction = ""
		m.StartAgentCopy = ""
		m.StartAgentCmd = ""
		m.StatusMsg = "Waired is not registered as a background service on this computer."
	}
	return m
}

// Labels for the two daemon-down action rows. Static, so the rows carry their
// title from creation and apply() never has to SetTitle them — a row whose
// title is never pushed cannot be resurrected by the Windows systray backend
// (see rows.go).
const (
	startAgentActionLabel = "Start the Waired agent…"
	startAgentCopyLabel   = "Copy start command"
)

// offlineModel decides what the tray renders when /status is unreachable.
// During the model-switch grace window (switching) the daemon is expected
// to be briefly down for a supervised restart, so we keep the last online
// model and only relabel it "Switching model…" with the busy icon —
// rather than alarming the user with the red daemon-down state. Outside
// the window, or before any connected snapshot exists, we fall back to
// the honest daemon-down model (waired#808).
func offlineModel(lastOnline MenuModel, switching bool, f daemonDownFacts) MenuModel {
	if !switching || lastOnline.Kind != MenuConnected {
		return daemonDownModel(f)
	}
	m := lastOnline
	m.Icon = IconBusy
	m.HeaderTitle = "Switching model…"
	m.DegradedReason = ""
	m.StatusMsg = "The agent is restarting to apply the new model."
	return m
}

// peersRowVisible reports whether the "Peers" row should show: only when
// the device group shows AND there is at least one peer (or peer hardware
// to expand). Gating on peer presence — not just enrollment — avoids a
// blank "Peers: 0" chevron row in the steady peerless state (waired#808).
func peersRowVisible(m MenuModel) bool {
	hasDevice := m.DeviceName != "" || m.OverlayIP != ""
	return hasDevice && (m.PeerCount > 0 || m.ShowPeerHardware)
}

// Update is the pure transition. No I/O, no goroutines — safe to call
// from the polling goroutine directly.
func Update(snap Snapshot) MenuModel {
	if snap.Health == HealthOffline {
		return daemonDownModel(daemonDownFacts{})
	}

	// The daemon is up but did not answer /identity. That is a transport
	// failure, and it says nothing about whether this device is enrolled — so
	// do not render the signed-out menu. During the rc7 review this state
	// (paired with an expiring device token, #318) read as "logged out" on a
	// device with months of validity on disk, and pushed the reviewer into a
	// re-login that re-ran the whole setup.
	if snap.IdentityErr && snap.Identity == nil {
		return MenuModel{
			Kind:        MenuConnecting,
			Icon:        IconBusy,
			HeaderTitle: "◐ Reconnecting to the Waired agent…",
			StatusMsg:   "The agent is running but hasn't answered yet. This is usually brief — you are still signed in.",
		}
	}

	// Daemon up but identity not yet known (e.g. /identity returned nothing
	// because the daemon predates this PR). Render the not-signed-in state —
	// safer to under-promise than to claim Connected without the email.
	if snap.Identity == nil || !snap.Identity.Enrolled {
		m := MenuModel{
			Kind:         MenuNotSignedIn,
			Icon:         IconDisconnected,
			HeaderTitle:  "○ Not signed in",
			ToggleAction: "Sign in...",
		}
		// Reflect an in-flight daemon-driven login. While OAuth /
		// activation is pending the login menu item is hidden (empty
		// ToggleAction) so a second click cannot start a second session;
		// an error keeps "Sign in..." visible so the operator can retry.
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

	// OpenClaw integration: same lifecycle as Claude — surface
	// regardless of pause/resume. Drift between the on-disk provider
	// baseURL and the gateway is the only failure mode we report;
	// openclaw itself surfaces unreachable gateway connections directly
	// to the user.
	if snap.OpenClaw != nil {
		applyOpenClaw(&m, snap.OpenClaw)
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

	// Public share submenu (waired#833). Independent of tunnel phase —
	// the provider toggle and the consumer consent/mode settings are
	// management-level knobs, like the Claude integration section. Nil
	// endpoints (old daemon) leave the whole parent hidden.
	applyPublicShare(&m, snap)

	// Inference-activity icon (waired#811). Runs before the aggregate
	// header and only promotes a plain IconConnected to IconBusy, so a
	// degraded/error/disconnected state — which summariseAggregateHeader
	// then narrates in the header — is never masked by the busy hue.
	applyInferenceActivity(&m, snap)

	// "Inference routing" is a parent with no state of its own: it shows
	// when either of the two groups underneath it has something to say
	// (#327). Computed here rather than inside applyWorker /
	// applyMeshReachable because neither of those knows about the other,
	// and a parent that opens onto an empty submenu is worse than no
	// parent at all.
	m.ShowRoutingMenu = m.ShowWorker || m.MeshReachableLabel != ""

	// Aggregate the top-level status line last, once every subsystem has
	// had its say (waired#809): a degraded connected state collapses to a
	// single "⚠ <cause>" header so the top level stays an at-a-glance
	// health summary and the per-subsystem detail lives in submenus.
	summariseAggregateHeader(&m)
	return m
}

// applyInferenceActivity lights the tray icon in "active green" (IconBusy)
// while the local inference engine is serving at least one request
// (waired#811), so the user can see Waired is working right now — and gets
// a hint why the fans spun up. It is intentionally the lowest-priority
// icon override: it only promotes a plain IconConnected, so an
// error/degraded/disconnected icon chosen earlier in Update always wins
// and the busy hue never hides a problem.
//
// The signal is ObservabilityState.Agent.Inflight (the
// waired_inference_inflight gauge), already polled every ~5s by the tray.
// That cadence makes this a coarse "is Waired busy?" indicator, not a
// per-request animation: the badge can lag the true start/stop of activity
// by up to one poll interval, and brief gaps between back-to-back requests
// may momentarily drop it. Daemons predating the observability API leave
// Snapshot.Observability nil and render no busy state.
//
// CapacityUsed/CapacityTotal are carried alongside Inflight and could later
// drive a "3/8 busy" tooltip or a graded badge; unused for now — a single
// binary busy/idle hue is the least noisy first step.
func applyInferenceActivity(m *MenuModel, snap Snapshot) {
	if m.Icon != IconConnected {
		return
	}
	if snap.Observability == nil {
		return
	}
	if snap.Observability.Agent.Inflight > 0 {
		m.Icon = IconBusy
	}
}

// summariseAggregateHeader folds a degraded *connected* state into one
// top-level status line naming the cause (waired#809). The icon is already
// IconDegraded by the time this runs; here we replace the plain
// "● Connected" header with "⚠ <cause>" so the user sees *what* needs
// attention without opening a submenu. No-op for healthy, disconnected,
// error, or not-signed-in states — those already carry a self-explanatory
// header. Cause precedence mirrors the order subsystems are evaluated in
// Update (Claude routing first, then integrations, then inference
// fallback), so the most user-facing breakage wins when several coincide.
func summariseAggregateHeader(m *MenuModel) {
	if m.Icon != IconDegraded || m.Kind != MenuConnected {
		return
	}
	switch {
	case strings.Contains(m.ClaudeHeader, "inactive"):
		m.DegradedReason = "Claude Code routing inactive"
	case strings.Contains(m.OpenClawHeader, "stale"),
		strings.Contains(m.OpenClawHeader, "unreadable"):
		m.DegradedReason = "OpenClaw integration needs attention"
	case m.HasRecentFallbackBadge:
		m.DegradedReason = "Inference fell back recently"
	default:
		m.DegradedReason = "Attention needed"
	}
	m.HeaderTitle = "⚠ " + m.DegradedReason
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
// preference for the leading identifier is DeviceName → DisplayID →
// "unknown".
//
// DisplayID, never DeviceID: a menu row is a surface a stranger's device
// identifier may not reach (public share spec §8.5), and PeerStatus alone
// cannot tell a public machine from one of your own — so the daemon
// resolves the question once and this row renders the answer (#768). A
// daemon predating the field sends none, and the row says "unknown"
// rather than guessing.
// Hardware tail covers the four shapes:
//
//   - GPU + VRAM: "RTX 4090 (24 GB)"
//   - GPU only:   "RTX 4090"
//   - RAM only:   "CPU only (32 GB RAM)"
//   - nothing:    "(hardware unknown)"
func formatPeerHardwareLabel(p management.PeerStatus) string {
	name := p.DeviceName
	if name == "" {
		name = p.DisplayID
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
		// #662: EffectiveVRAMMB, not VRAMTotalMB — a unified-memory host
		// reports its budget as the usable bound and Apple Silicon reports
		// no per-device total at all, so reading the raw field dropped the
		// size from an M-series row entirely.
		if mb := hw.EffectiveVRAMMB(); mb > 0 {
			return fmt.Sprintf("%s (%d GB)", shortGPUModel(hw.GPUModel), vramMBToGB(mb))
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
//	Qwen3 4B Instruct (switching…)            (preferred but not yet active — swap in flight)
//	Qwen3 14B Instruct (downloading…)         (pull running)
//	Qwen3 14B Instruct (downloads on select)  (not yet on disk; click triggers pull)
//	Qwen3 32B Instruct — needs 24 GB VRAM     (over capacity, click warns and asks)
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

	// An engine-less host is a normal state, not a fault: it stays
	// enrolled and its requests go to the other computers in the mesh
	// (#387, #841). Both halves are said here, because "no engine" alone
	// reads as "this computer is broken", and the second half is what
	// waired#1067's decision 5 records as the truth.
	//
	// nil means a daemon that predates engine_installed — unknown, not
	// absent — and the submenu then renders exactly as it did before.
	if c.EngineInstalled != nil && !*c.EngineInstalled {
		m.CatalogEngineMissing = true
		m.CatalogNoteLabel = "No AI engine on this computer — models run on your other computers"
	}

	retained := retainedFamilies(c.Families)
	entries := make([]CatalogEntryView, 0, len(retained))
	for _, f := range retained {
		entries = append(entries, formatCatalogEntry(f, c.Engine, c.Host))
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

// applyPublicShare projects the Public Share endpoints (waired#833) into
// the "Public share" submenu. Two independent groups, each gated by its
// own 404 so they render independently:
//
//  1. Provider toggle (snap.PublicShare): the opt-in kill switch that
//     shares THIS computer to other people. Rendered from DesiredState —
//     what the operator most recently asked for — so the label flips at
//     once even while the control-plane sync is still in flight.
//  2. Consumer settings (snap.PublicUse): the off/auto/explicit mode for
//     USING other people's computers, plus consent status.
//
// The parent shows when EITHER endpoint is present, so a daemon exposing
// only one still renders it. Both nil ⇒ nothing (old daemon renders the
// pre-feature menu unchanged).
func applyPublicShare(m *MenuModel, snap Snapshot) {
	if snap.PublicShare == nil && snap.PublicUse == nil {
		return
	}
	m.ShowPublicShareMenu = true

	if ps := snap.PublicShare; ps != nil && (ps.State != "" || ps.DesiredState != "") {
		// DesiredState is a plain string on the wire; the state constants
		// are the typed PublicShareState, so compare via string(...).
		switch ps.DesiredState {
		case string(state.PublicShareOn):
			m.PublicShareToggleAction = "Stop sharing this computer publicly"
			m.PublicShareStateLabel = "Public sharing: on"
		case string(state.PublicShareOff):
			m.PublicShareToggleAction = "Share this computer publicly"
			m.PublicShareStateLabel = "Public sharing: off"
		}
		// Pending control-plane sync: annotate the label and surface the
		// served pending note verbatim (never tray-authored).
		if ps.CPSynced != nil && !*ps.CPSynced && m.PublicShareStateLabel != "" {
			m.PublicShareStateLabel += " (saving…)"
			m.PublicShareNote = ps.Note
		}
	}

	if pu := snap.PublicUse; pu != nil {
		m.ShowPublicUse = true
		m.PublicUseHeaderLabel = "Use public computers"
		m.PublicUseConsented = pu.Consented
		mode := pu.Mode
		if mode == "" {
			mode = agentconfig.PublicUseModeOff
		}
		// Labels are tray-authored plain English — NOT the served consent
		// copy. The mode VALUE POSTed on click is the wire constant.
		m.PublicUseModes = []PublicUseModeRow{
			{Mode: agentconfig.PublicUseModeOff, Label: "Do not use public computers", Selected: mode == agentconfig.PublicUseModeOff},
			{Mode: agentconfig.PublicUseModeAuto, Label: "Use only when better than my own computers", Selected: mode == agentconfig.PublicUseModeAuto},
			{Mode: agentconfig.PublicUseModeExplicit, Label: "Always allow public computers", Selected: mode == agentconfig.PublicUseModeExplicit},
		}
	}

	if snap.PublicWarning != nil {
		m.PublicMoreURL = publicMoreURL(snap.PublicWarning.Text)
	}
}

// publicMoreURL extracts the "Privacy & safety…" link from the served
// consent warning text: the LAST line beginning "More: ", trimmed, with
// https:// prepended when scheme-less. Returns "" when no such line
// exists. The link is ALWAYS sourced from the served text, never
// hardcoded, so a server-side copy change moves the link with it.
func publicMoreURL(text string) string {
	const prefix = "More: "
	url := ""
	for _, line := range strings.Split(text, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), prefix); ok {
			url = strings.TrimSpace(rest)
		}
	}
	if url == "" {
		return ""
	}
	if !strings.Contains(url, "://") {
		url = "https://" + url
	}
	return url
}

// publicUseModeRowLabel prefixes a mode row with the ● / ○ selection
// glyph, mirroring workerModeRowLabel.
func publicUseModeRowLabel(r PublicUseModeRow) string {
	prefix := "○ "
	if r.Selected {
		prefix = "● "
	}
	return prefix + r.Label
}

// applyWorker projects the daemon's WorkerResponse + mesh snapshot
// into the "Inference routing" submenu — the top-level sibling of
// "Inference", which holds only this computer's engine controls
// (#327: engine settings and "where do my requests run" are different
// questions and the review found them indistinguishable in one flat
// list). Four groups:
//
//  1. Summary: "Worker: <peer> (pinned)" / "Worker: auto" so the
//     operator sees the current answer at the top of the submenu.
//  2. A disabled section header naming what the mode rows are —
//     automatic selection, as opposed to the pins below them.
//  3. Mode rows (auto / local-only / peer-preferred / peer-only) —
//     fixed slots, selected leading glyph follows w.Mode.
//  4. A second header, then pin rows — one per inference-capable peer
//     (Tailscale exit-node filter: peer must advertise an inference
//     engine, even if transiently unavailable). Stale / unreachable
//     peers render "(unavailable)" but stay selectable, matching
//     Tailscale exit-node UX where the pin survives the down period.
//
// The two headers carry the separation the reviewer asked for: the
// systray Windows backend does not render a third nesting level (see
// tray.go's menu construction), so the pins cannot live one submenu
// deeper — labelled groups inside one flat list are the available
// shape.
//
// Always set ShowWorker=true since the daemon advertised the API by
// populating w; the caller already gated on tunnel phase.
func applyWorker(m *MenuModel, w *management.WorkerResponse, mesh *inferencemesh.Snapshot) {
	if w == nil {
		return
	}
	m.ShowWorker = true
	m.WorkerParentLabel = "Inference routing"
	m.WorkerActiveLabel = "Worker: " + workerSummaryLabel(*w)
	m.WorkerModesHeader = "Choose automatically"
	m.WorkerModes = []WorkerModeRow{
		{Mode: state.RoutingModeAuto, Label: "Auto", Selected: w.Mode == state.RoutingModeAuto || w.Mode == ""},
		{Mode: state.RoutingModeLocalOnly, Label: "Local only", Selected: w.Mode == state.RoutingModeLocalOnly},
		{Mode: state.RoutingModePeerPreferred, Label: "Peer preferred", Selected: w.Mode == state.RoutingModePeerPreferred},
		{Mode: state.RoutingModePeerOnly, Label: "Peer only", Selected: w.Mode == state.RoutingModePeerOnly},
	}
	m.WorkerShowClearPin = w.Mode == state.RoutingModePinned

	// Pin entries — filter mesh to inference-capable peers
	// (signer.InferenceState advertised and Type != "none"). The
	// snapshot arrives sorted by device name (#326), which is what
	// keeps a given peer on the same menu row poll-over-poll.
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
			label := pinnedPeerLabel(*w)
			pins = append(pins, WorkerPinEntryView{
				DeviceID:  w.PinnedPeerDeviceID,
				Label:     label + " (absent)",
				Available: false,
				Selected:  true,
			})
		}
	}
	m.WorkerPinEntries = pins
	// The pin header labels the group below it, so it only earns a row
	// when there is a group: a mesh with no inference-capable peer would
	// otherwise render "Pin to one peer" over nothing.
	if len(pins) > 0 {
		m.WorkerPinsHeader = "Pin to one peer"
	}
}

// pinnedPeerLabel names the pinned peer for the two rows that describe
// it without having the mesh entry in hand: the summary row, and the
// "(absent)" row for a pin that has dropped out of the snapshot.
//
// The identifier is the daemon's display identifier — the grant
// pseudonym when the pin is a public machine — never PinnedPeerDeviceID,
// which is the routing key the tray posts back and may be a stranger's
// real device id (#739, public share spec §8.5). The device-id fallback
// is for a daemon predating the field: it reports no display identifier
// for any pin, and dropping the id there would blank the row for every
// own-network pin during a version skew.
//
// Mirrors displayPin in cmd/waired/worker.go, deliberately: the same
// body reaches both, and one machine should read the same way in the
// menu and in the terminal.
func pinnedPeerLabel(w management.WorkerResponse) string {
	if w.PinnedPeerName != "" {
		return w.PinnedPeerName
	}
	if w.PinnedPeerDisplayID != "" {
		return w.PinnedPeerDisplayID
	}
	return w.PinnedPeerDeviceID
}

func workerSummaryLabel(w management.WorkerResponse) string {
	switch w.Mode {
	case "", state.RoutingModeAuto:
		return "auto"
	case state.RoutingModeLocalOnly:
		return "local only"
	case state.RoutingModePeerPreferred:
		return "peer preferred"
	case state.RoutingModePeerOnly:
		return "peer only"
	case state.RoutingModePinned:
		name := pinnedPeerLabel(w)
		// A down pin says what it MEANS, not just that it is down
		// (waired-agent#325): the pin is fail-closed, so nothing runs on
		// this computer in its place. "not served here" is the accurate
		// phrasing for every surface — general inference fails outright,
		// while a Claude turn on the auto route leaves for the Anthropic
		// API; neither is served by the pinned worker.
		//
		// waired#1064 keeps that phrasing and makes the first half
		// specific: "loading" or "pull failed" is what an operator can
		// act on, where "unavailable" only says it is down. A peer that
		// gave no reason still reads exactly as it did before.
		suffix := ""
		switch w.PinnedPeerStatus {
		case "ok":
			// The consequence needs no stating — it is working. Name
			// the model instead, so the summary row answers the same
			// question the pin rows below it do.
			if w.PinnedPeerModel != "" {
				suffix = " — " + w.PinnedPeerModel
			}
		case "unavailable":
			why := inferencemesh.ConditionLabel(w.PinnedPeerCondition)
			if why == "" {
				why = inferencemesh.ConditionUnavailable
			}
			suffix = " — " + why + ", requests are not served here"
		case "absent":
			suffix = " — absent, requests are not served here"
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
// drive the row's enabled state. Delegates so the tray, the management
// API and `waired peers list` cannot drift on the answer.
func peerIsServing(p inferencemesh.PeerView) bool {
	return inferencemesh.PeerServing(p)
}

// pinEntryLabel builds the pin row: "<name> (<model>)" for a peer that
// is serving, "<name> (<model> — <why not>)" for one that named a model
// but is not, and "<name> (<why not>)" for one that named none. Falls
// back to DeviceID when the name is empty.
//
// The model is the catalog id where the peer reports one, so the same
// model reads the same on every row regardless of which engine — and
// therefore which OS — the peer runs (waired#1064). The reason is on the
// row rather than in a tooltip because some Linux indicators do not
// render menu-item tooltips, and inline rather than in a nested item
// because the systray Windows backend renders no third level (see the
// comment above WorkerPinsHeader).
func pinEntryLabel(p inferencemesh.PeerView) string {
	name := p.DeviceName
	if name == "" {
		// A menu row is one of the surfaces a public machine's real
		// device id may not reach (public share spec §8.5), so the
		// fallback is its display identifier — and the plain phrase when
		// even that is missing (#739).
		id, ok := inferencemesh.PeerDisplayID(p)
		if !ok {
			id = inferencemesh.PublicPeerLabel
		}
		name = id
	}
	model := inferencemesh.PeerModel(p)
	if peerIsServing(p) {
		// Serving is the ordinary state and the model alone says it; a
		// reason appears only when there is one worth giving.
		if model == "" {
			return name
		}
		return name + " (" + model + ")"
	}
	cond := inferencemesh.PeerCondition(p)
	why := inferencemesh.ConditionLabel(cond)
	if model == "" || !inferencemesh.ConditionHasFreshModel(cond) {
		return name + " (" + why + ")"
	}
	return name + " (" + model + " — " + why + ")"
}

func pinPresent(pins []WorkerPinEntryView, deviceID string) bool {
	for _, p := range pins {
		if p.DeviceID == deviceID {
			return true
		}
	}
	return false
}

// retainedFamilies trims the served family list to what the pre-allocated
// menu can render, keeping manifest order — but never at the cost of the
// rows that describe the user's own machine. When the cap bites and the
// active (or preferred) family sorts past it, that row displaces the last
// retained one instead of vanishing.
//
// The bug this closes twice over: families render alphabetically, so an
// undersized cap amputates the tail, and on a host serving qwen3.6-35b-a3b
// the amputated row was the one with the "● Active" bullet — the menu
// claimed the running model did not exist (waired-agent#319). The cap now
// has headroom, so this path is dormant on bundled manifests; it exists so
// a future external manifest source cannot resurrect the same class.
func retainedFamilies(families []management.CatalogFamily) []management.CatalogFamily {
	if len(families) <= MaxCatalogEntries {
		return families
	}
	out := append([]management.CatalogFamily(nil), families[:MaxCatalogEntries]...)
	// Each rescued row consumes one slot from the end, so an active AND a
	// preferred family past the cap both survive rather than overwriting
	// each other.
	slot := len(out) - 1
	for _, keep := range []func(management.CatalogFamily) bool{
		func(f management.CatalogFamily) bool { return f.Active },
		func(f management.CatalogFamily) bool { return f.Preferred },
	} {
		if slot < 0 || familyIndex(out, keep) >= 0 {
			continue // no room left, or already inside the retained window
		}
		if i := familyIndex(families[MaxCatalogEntries:], keep); i >= 0 {
			out[slot] = families[MaxCatalogEntries+i]
			slot--
		}
	}
	return out
}

// familyIndex returns the first index in families satisfying match, or -1.
func familyIndex(families []management.CatalogFamily, match func(management.CatalogFamily) bool) int {
	for i, f := range families {
		if match(f) {
			return i
		}
	}
	return -1
}

func formatCatalogEntry(f management.CatalogFamily, engine string, host management.CatalogHost) CatalogEntryView {
	name := f.DisplayName
	if name == "" {
		name = f.ModelID
	}
	e := CatalogEntryView{ModelID: f.ModelID, Name: name}
	// Compact size + tier hint appended to fitting/downloadable rows,
	// then the pick note. Over-capacity rows already spell out the
	// requirement in their blocked text, so the suffix would be redundant
	// there.
	suffix := catalogSpecSuffix(engine, f) + catalogPickNote(f) + catalogSpillNote(host, f)
	switch {
	case f.Active:
		e.Label = "● " + name + suffix
	case f.Preferred:
		// Preference recorded but not yet reflected in the running
		// agent's Active selection. The catalog endpoint surfaces
		// preferred=true on the row the user just clicked; Active
		// follows once the swap applies — in process since waired#812,
		// or after the restart fallback.
		e.Label = name + " (switching…)" + suffix
	case f.Downloading:
		e.Label = name + " (downloading…)" + suffix
	case !f.Fits:
		// The row says why, and the click asks with the same sentence —
		// one string so the menu and the dialog cannot drift.
		e.UnfitReason = catalogBlockedText(f)
		e.UnfitKind = catalogUnfitKind(f)
		e.Label = name + " — " + e.UnfitReason
	case !f.Downloaded:
		e.Label = name + " (downloads on select)" + suffix
	default:
		e.Label = name + suffix
	}
	e.Tooltip = catalogSpecTooltip(engine, f, host)
	return e
}

// catalogSpillMB is how much of a full coding session this computer
// cannot keep on the graphics card: what the model needs to serve the
// coding window, less the memory the engine may address there.
//
// Both figures come from the catalog response, so this is arithmetic
// rather than a second opinion. 0 for a row that fits on the card, and
// equally for a host with no card and for a projection that priced
// nothing — all three are "nothing to say" rather than "nothing spills",
// which is why the callers print nothing instead of "0 GB".
func catalogSpillMB(host management.CatalogHost, f management.CatalogFamily) int {
	if f.Fit == nil || host.GPUBudgetMB <= 0 || f.Fit.RequiredWindowResidentMB <= 0 {
		return 0
	}
	if over := f.Fit.RequiredWindowResidentMB - host.GPUBudgetMB; over > 0 {
		return over
	}
	return 0
}

// catalogSpillNote is the label's short mark for that shortfall, in the
// same shape as the pick note beside it.
//
// It is a FACT about memory and deliberately not a speed. A row can be
// fitting AND recommended and still leave gigabytes of a session in
// system RAM — on the rc8 Windows host the recommended model needed
// 10719 MB against an 8188 MB budget, and no surface said so until
// 6.6 GB had been downloaded (waired-agent#632). Predicting what that
// costs in tokens per second is a MEASURED input this catalog does not
// carry (waired-agent#466, and docs/decisions/20260804/1937-… decision 4
// for why a predicted one may not exclude).
func catalogSpillNote(host management.CatalogHost, f management.CatalogFamily) string {
	mb := catalogSpillMB(host, f)
	if mb <= 0 {
		return ""
	}
	return " · " + formatSpillGB(mb) + " of context cache in system RAM"
}

// formatSpillGB writes a shortfall in GB, with one decimal below 10 GB
// so a 2531 MB gap does not round to a flat "3 GB" the operator cannot
// reconcile with the two figures it came from. Matches
// `waired models ls --detail` word for word.
func formatSpillGB(mb int) string {
	gb := float64(mb) / 1024
	if gb < 10 {
		return fmt.Sprintf("%.1f GB", gb)
	}
	return fmt.Sprintf("%.0f GB", gb)
}

// catalogPickNote marks the row this computer would choose for itself,
// and the rows it would not — the half of the catalog that had no way to
// reach a user at all (waired-ai/waired#988 shipped the rule; nothing
// rendered it, so `waired models ls` printed "fits" for a model whose
// weights spill and the tray drew it as an ordinary row).
//
// Deliberately short. The full sentence lives in the tooltip: a menu the
// operator scans has room for a mark, not for a paragraph, and the two
// notes have to stay distinguishable at a glance.
func catalogPickNote(f management.CatalogFamily) string {
	switch {
	case f.RecommendedPick:
		return " — recommended"
	case f.Fit != nil && f.Fit.NotRecommended:
		return " — not recommended here"
	}
	return ""
}

// catalogBlockedText is the reason a row is greyed.
//
// Resolved from the machine code first and from DeficitLabel second. The
// fallback is not a transition state: DeficitLabel is composed by the
// same binary and already reads in the operator's terms ("needs 24 GB
// VRAM (have 8 GB)"), so re-deriving those sentences from codes here
// would be two implementations of one string inside one process. The
// codes earn their place where they say something the label cannot —
// no_variant_for_engine, whose label is "no variant supports vllm": two
// words of ours and an engine name, for a person who has never heard of
// either.
// catalogNoBuildText is the blocked text for a family this way of
// running AI has no build of — the one unfit verdict that is not a
// quantity.
const catalogNoBuildText = "not available on this computer"

func catalogBlockedText(f management.CatalogFamily) string {
	if f.Fit != nil && f.Fit.Reason == hostfit.ReasonNoVariantForEngine {
		return catalogNoBuildText
	}
	if f.DeficitLabel != "" {
		return f.DeficitLabel
	}
	return "incompatible"
}

// catalogUnfitKind reads the verdict behind catalogBlockedText's
// sentence, so the click's dialog can be worded without matching that
// sentence back (waired-agent#850 — matching it put an engine-version
// wall, whose text is authored in the router, into the arm that talks
// about memory).
//
// The memory arm is an allowlist of hostfit's three capacity refusals.
// Everything else is UnfitOther, including a nil Fit: a reason added
// later has to be classified here on purpose rather than inherit a
// sentence by falling through.
func catalogUnfitKind(f management.CatalogFamily) UnfitKind {
	if f.Fits {
		return UnfitNone
	}
	if f.Fit == nil {
		return UnfitOther
	}
	switch f.Fit.Reason {
	case hostfit.ReasonNoVariantForEngine:
		return UnfitNoBuild
	case hostfit.ReasonInsufficientMemory,
		hostfit.ReasonInsufficientRAM,
		hostfit.ReasonInsufficientVRAM:
		return UnfitMemory
	}
	return UnfitOther
}

// engineVLLM mirrors catalog.RuntimeVLLM — the value the management
// catalog endpoint reports in ModelCatalogResponse.Engine. Kept as a
// local literal so the tray view-model layer needs no catalog import.
const engineVLLM = "vllm"

// catalogSpecSuffix returns the compact "· N GB VRAM · medium" hint for
// menu labels.
//
// The size is what the model needs to serve the ~200k coding window
// here — see catalogSizeGB. It used to be min_ram_gb: a threshold
// authored for a host that loads into system RAM, printed beside a
// graphics card that has to hold the thing (waired-agent#321). That
// figure is still the last fallback, and it is the RIGHT answer on a
// computer the projection cannot price.
//
// The size class rides along because the review that asked for this
// asked for a quality value on every picker. It used to be the raw
// quality tier, on the argument that a coarse scale would have to be
// kept in step with the catalog forever — true of a scale somebody
// authors, and #537 replaced it with one nothing authors: hostfit
// derives small/medium/large from the weight annotation the manifest
// already carries. The number itself is arithmetic over two catalog
// fields (#518) and printing it claimed a measurement.
func catalogSpecSuffix(engine string, f management.CatalogFamily) string {
	gb, unit := catalogSizeGB(engine, f)
	var out string
	if gb > 0 {
		out = fmt.Sprintf(" · %d GB %s", gb, unit)
	}
	if f.ModelSize != "" {
		out += " · " + f.ModelSize
	}
	return out
}

// catalogSizeGB is the memory figure to print and the noun for it.
//
// The window-inclusive requirement first: weights, engine overhead and
// the KV cache for the whole ~200k coding window. It is what a person
// reading a size beside a model is actually asking about, and it is
// ~2.6 GB larger than the fit-time figure on qwen3.5-4b — the gap that
// let a host be shown "5 GB", pull the model, and then be unable to hold
// a coding session in it (waired-ai/waired#1056 defect 2).
//
// Its noun is plain "memory", because the sum it is compared against is
// RAM plus dedicated VRAM. The resident figure below it keeps "VRAM":
// that one really is a graphics-memory question. The unit is not
// cosmetic — calling a system-RAM threshold "VRAM" on a machine with no
// card would send the operator shopping for hardware the number has
// nothing to do with.
func catalogSizeGB(engine string, f management.CatalogFamily) (int, string) {
	if f.Fit != nil && f.Fit.RequiredWindowResidentMB > 0 {
		return (f.Fit.RequiredWindowResidentMB + 1023) / 1024, "memory"
	}
	if f.Fit != nil && f.Fit.RequiredResidentMB > 0 {
		return (f.Fit.RequiredResidentMB + 1023) / 1024, "VRAM"
	}
	gb := recommendedSpecGB(engine, f.Recommended)
	if engine == engineVLLM {
		return gb, "VRAM"
	}
	return gb, "RAM"
}

// catalogSizeNote spells out what the label's one-word size class means,
// in the only terms that make it comparable across computers: the card
// it runs on.
//
// It does not repeat the memory figure beside it. That one is about THIS
// computer and counts its RAM too; this one is a property of the model,
// the same on every machine. Two numbers under one row would read as one
// quantity stated twice.
func catalogSizeNote(size string) string {
	switch size {
	case hostfit.ModelSizeSmall:
		return "small — fits an 8 GB graphics card"
	case hostfit.ModelSizeMedium:
		return "medium — fits a 32 GB graphics card"
	case hostfit.ModelSizeLarge:
		return "large — needs more than a 32 GB graphics card"
	}
	return ""
}

// catalogSpecTooltip is the fuller per-row hint: the memory figure, the
// size class, parameter counts, and the sentence behind whichever pick
// note the label carries. Best-effort — some Linux indicators drop menu
// item tooltips. Empty when there is nothing to say.
func catalogSpecTooltip(engine string, f management.CatalogFamily, host management.CatalogHost) string {
	var parts []string
	if gb, unit := catalogSizeGB(engine, f); gb > 0 {
		parts = append(parts, fmt.Sprintf("needs %d GB %s", gb, unit))
	}
	if note := catalogSizeNote(f.ModelSize); note != "" {
		parts = append(parts, note)
	}
	if rec := f.Recommended; rec != nil {
		if p := formatTrayParams(rec.ParamCount, rec.ActiveParams); p != "" {
			parts = append(parts, p+" params")
		}
	}
	sentences := catalogPickTooltip(f)
	// The label's spill mark is a quantity; this is what it means. Same
	// sentence the docs use for the same arithmetic
	// (docs-site reference/model-catalog), so a person who read one meets
	// the other.
	if mb := catalogSpillMB(host, f); mb > 0 {
		spill := "About " + formatSpillGB(mb) + " of a long coding session will not fit " +
			"on the graphics card and is read from system memory, which is slower."
		if sentences == "" {
			sentences = spill
		} else {
			sentences += " " + spill
		}
	}
	if len(parts) == 0 {
		return sentences
	}
	out := strings.Join(parts, " · ")
	if sentences != "" {
		out += ". " + sentences
	}
	return out
}

// catalogPickTooltip is the full sentence the label's short mark stands
// in for. The wording of the demotion is the same one the public docs
// use for the same rule, so a person who read the docs meets the same
// explanation here (docs-site reference/model-catalog). Subjectless
// "Not recommended" since waired#1146 (owner-approved 2026-08-12), with
// two tails only: "here" when the body already names this computer,
// "for this computer" when it does not — the same sentences the setup
// wizard renders, byte for byte.
func catalogPickTooltip(f management.CatalogFamily) string {
	switch {
	case f.RecommendedPick:
		// Word for word the sentence the setup wizard puts under its own
		// Recommended badge (web/admin EnginePicker). One rule, one
		// explanation, whichever surface the operator meets it on.
		return "Chosen from this computer’s RAM and graphics memory combined."
	case f.Fit != nil && f.Fit.NotRecommended:
		switch f.Fit.NotRecommendedReason {
		case hostfit.ReasonWeightsSpill:
			return "It fits, but not entirely on the graphics card — the rest is fetched from " +
				"system RAM on every reply, so replies are slower. Not recommended for this computer."
		case hostfit.ReasonTooSlow:
			return "It fits, but this computer would be slow with it. Not recommended here."
		case hostfit.ReasonWindowTooSmall:
			// The one reason that is not about this computer: no machine
			// makes this model hold a coding session (#465 item 5).
			return "It fits, but it cannot hold a long coding session — a coding agent " +
				"has to compact much earlier with it. Not recommended on any computer."
		case hostfit.ReasonWindowExceedsMemory:
			return "It runs and answers well, but this computer cannot hold a full " +
				"coding session in it. Not recommended here."
		default:
			return "Not recommended for this computer."
		}
	}
	return ""
}

// recommendedSpecGB returns the engine-appropriate recommended memory in
// whole GB: min VRAM on vllm, min RAM on ollama. 0 when unknown.
//
// VRAM rounds UP, matching `waired models ls --detail`
// (formatRecommendedResource) and the deficit labels the same row can carry
// (router.mbToGBCeil). This used to round to nearest here alone, so a
// 24000 MB variant advertised "min 23 GB" in the tray while the CLI and the
// deficit label both said 24 — one requirement, two numbers
// (waired-agent#319).
//
// The remaining asymmetry is deliberate: a requirement rounds up and an
// available budget rounds down (family_picker's "have N GB"), so neither
// figure can flatter the host into a model that will not load.
func recommendedSpecGB(engine string, rec *management.CatalogSpec) int {
	if rec == nil {
		return 0
	}
	if engine == engineVLLM {
		if rec.MinVRAMMB > 0 {
			return (rec.MinVRAMMB + 1023) / 1024
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
// absent — that lives in the Inference routing submenu. claude carries the
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
// failed); "local" means an anthropic-routed request was served locally
// instead (upstream unavailable) — the two read very differently to a
// user worried about where their prompts went. A pinned worker never
// produces the "local" direction any more: the pin is fail-closed
// (waired-agent#325), so the only remaining producer is the unreachable
// Anthropic upstream.
func claudeFallbackNote(ev *management.ClaudeRoutingFallbackEvent) string {
	if ev == nil {
		return ""
	}
	switch ev.Direction {
	case "local":
		return "⚠ last served locally (Anthropic unavailable)"
	default: // "anthropic" (or unset legacy)
		return "⚠ last fell back → Anthropic (Waired unavailable)"
	}
}

// applyOpenClaw fills the OpenClaw-integration section. Stale config
// while the network is connected swaps the icon to the degraded
// variant — same treatment as a missing Claude wrapper, so the user
// notices integration drift even before they expand the menu.
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

// Menu action labels. These are the strings the user sees AND the keys
// the click handlers switch on (tray.go), so they live in one place:
// renaming a label in only one of the two silently turns the menu item
// into a no-op.
//
// The two inference axes are deliberately worded apart (#316). The soft
// gate merely stops accepting requests — the engine keeps running and the
// model stays in VRAM — so it is "pause", not "disable the engine": the
// rc7 review found a tester who clicked the old "Disable inference
// engine", was told it succeeded, and watched llama-server hold 15GB
// anyway. Freeing memory is the power axis, which says "stop".
//
// "local" is load-bearing in the pause labels: `waired pause` is already
// documented as pausing routing for the whole machine, so an unqualified
// "Pause inference" would read as that much broader control.
const (
	// Soft gate: POST /inference/{enable,disable}. No process is touched.
	labelPauseInference  = "Pause local inference"
	labelResumeInference = "Resume local inference"
	// labelEnableInference is the same action as Resume, worded for a
	// computer that has never run models here: no engine installed, and
	// local inference off by default because the machine is below the
	// recommended spec (#465).
	labelEnableInference = "Run AI models on this computer"
	// tipInferenceToggle spells out what the labels cannot: this axis
	// does not give the memory back. Worded as what the toggle does and
	// does not do, not as a claim about the current state — the earlier
	// "The model stays loaded in memory." asserted residency, which
	// nothing here knew and which is false on any host whose keep-alive
	// has lapsed (waired-agent#879).
	tipInferenceToggle = "Stops new requests on this computer. Does not unload the model."
	// Release valve for model residency (waired-agent#861): frees the
	// model's memory while the engine keeps answering. Distinct from the
	// power axis below, which frees the same memory by stopping the
	// engine — and stops answering with it.
	labelUnloadModel = "Unload model (free memory)"
	// What the same row says when the engine reports nothing loaded. It
	// stays visible and greyed rather than disappearing, so the control
	// is discoverable and its unavailability is explained — the
	// labelEngineNotManaged treatment. Never used for an UNOBSERVED
	// residency: "not loaded" would be asserting something the daemon did
	// not say (waired-agent#879).
	labelModelNotLoaded = "Model not loaded"
	// Hard power axis (#186): stops/starts the engine process itself.
	labelStopEngine  = "Stop inference engine"
	labelStartEngine = "Start inference engine"
	// The engine is one waired serves through but holds no process
	// handle for — an adopted orphan of a previous run (#489).
	labelEngineNotManaged = "Engine not managed"
	// Mesh sharing.
	labelStopSharing  = "Stop sharing engine to mesh"
	labelStartSharing = "Share engine to mesh"
)

// applyInference fills the inference group fields. SubsystemState comes
// from the agent (engine health) and is independent of DesiredState
// (operator's enable/disable intent) — the agent reports SubsystemState=
// "disabled" when the operator has the engine turned off.
func applyInference(m *MenuModel, inf *management.InferenceStatus) {
	// The presence of the inference API is what surfaces the "Inference ▸"
	// submenu parent (waired#809); the rows below fill it in.
	m.ShowInferenceMenu = true
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
		// waired-agent#879: whether the weights are actually in (V)RAM.
		// Without it this row reads the same on a host that answers in
		// half a second and one that will spend 17-56 s reloading first
		// (waired-agent#861). Suffixed only when a daemon that reports
		// residency said so — old daemons leave the row unchanged.
		if ol, ok := inf.Runtimes["ollama"]; ok && ol.ModelResident != nil {
			if *ol.ModelResident {
				m.ActiveModelLabel += " (loaded)"
			} else {
				m.ActiveModelLabel += " (not loaded)"
			}
		}
	}
	// Model residency (waired-agent#861): the setting, and the release
	// valve for it. Both are gated on the daemon reporting the setting at
	// all, so a tray talking to an older daemon renders neither.
	// nil Residency = a daemon with no residency controller. Supported=false
	// = a daemon whose SERVING ENGINE has no residency axis at all: it holds
	// the model for the life of the process, so there is nothing to set and
	// nothing to unload, and offering either would bait a click that cannot
	// work (waired-agent#943). A nil Supported is an older daemon making no
	// claim, so it draws exactly what it drew before.
	if inf.Residency != nil && (inf.Residency.Supported == nil || *inf.Residency.Supported) {
		idle, err := management.ParseResidency(inf.Residency.IdleTimeout)
		if err != nil || inf.Residency.HoldsIndefinitely {
			idle = 0
		}
		m.ResidencyHeader = "Keep model in memory: " + residencyValueLabel(idle)
		m.ResidencyRows = residencyRows(idle)
		m.UnloadModelAction = labelUnloadModel
		m.UnloadModelEnabled = true
		if ol, ok := inf.Runtimes["ollama"]; ok && ol.ModelResident != nil && !*ol.ModelResident {
			m.UnloadModelAction = labelModelNotLoaded
			m.UnloadModelEnabled = false
		}
	}
	// Toggle action mirrors DesiredState (= what the operator most
	// recently asked for).
	//
	// no_engine hides the action only while local inference is already
	// ON: there is no engine to pause, so the row would bait a click
	// that does nothing. When it is OFF, no_engine is the state of a
	// computer that has never set local inference up — a host below the
	// recommended spec starts exactly here — and turning it on is what
	// installs the engine and fetches a model. Hiding the row there left
	// the app with no way in at all, which is #465's dead end surviving
	// in the one surface a desktop user actually looks at.
	switch inf.DesiredState {
	case "enabled":
		if inf.SubsystemState != "no_engine" {
			m.InferenceToggleAction = labelPauseInference
		}
	case "disabled":
		// "Resume" would claim it ran here before. On a computer with no
		// engine it never did.
		m.InferenceToggleAction = labelResumeInference
		if inf.SubsystemState == "no_engine" {
			m.InferenceToggleAction = labelEnableInference
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
	switch {
	case inf.ShareWithMesh == "shared" && inf.ShareSuspended:
		// The session override is on: sharing is withheld right now even
		// though the operator's choice is still "shared" (#316). Normally
		// invisible — the tray lifts the suspension when it starts — so
		// seeing this means the lift did not land. Offer the action that
		// clears it rather than one that would appear to do nothing.
		m.ShareToggleAction = labelStartSharing
		m.ShareStateLabel = "Sharing: paused"
	case inf.ShareWithMesh == "shared":
		m.ShareToggleAction = labelStopSharing
		m.ShareStateLabel = "Sharing: enabled"
	case inf.ShareWithMesh == "not_shared":
		m.ShareToggleAction = labelStartSharing
		m.ShareStateLabel = "Sharing: disabled"
	}

	// Hard engine power axis (#186). Reached only when a usable engine
	// exists (the no_engine branch returned above). Empty EnginePower
	// means the daemon predates engine control → leave the row hidden.
	switch {
	case inf.EnginePower == "":
		// hidden
	case !inf.EngineManaged:
		// Adopted (#489): waired serves through the engine but holds no
		// process handle, so it can't free its memory. Show the row
		// disabled so the absence is explained rather than mysterious.
		m.EngineToggleAction = labelEngineNotManaged
		m.EngineToggleEnabled = false
	case inf.EnginePower == "stopped":
		m.EngineToggleAction = labelStartEngine
		m.EngineToggleEnabled = true
	default: // running / starting
		m.EngineToggleAction = labelStopEngine
		m.EngineToggleEnabled = true
	}
}

// humanInferenceState maps the wire SubsystemState (snake_case) to a
// short label suitable for menu rendering.
func humanInferenceState(s string) string {
	// One mapping, shared with the peer rows so this machine reads the
	// same locally as it does from someone else's menu (waired#1064).
	// The one local-only word is "stopped": on its own line here, "you
	// stopped the engine to free memory" is worth spelling out, but as a
	// suffix inside a peer row's parentheses it would nest a second pair.
	if s == "stopped" {
		return "stopped (memory freed)"
	}
	return inferencemesh.ConditionLabel(s)
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
