package tray

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"time"

	"fyne.io/systray"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/notice"
	"github.com/waired-ai/waired-agent/internal/observability"
	"github.com/waired-ai/waired-agent/internal/platform/autostart"
	"github.com/waired-ai/waired-agent/internal/platform/elevation"
	"github.com/waired-ai/waired-agent/internal/platform/notification"
	"github.com/waired-ai/waired-agent/internal/platform/service"
	"github.com/waired-ai/waired-agent/internal/platform/trayhost"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// notify shows a transient OS-level toast (best-effort; silent
// fallback on backends without a notifier). The title is always
// "Waired" so the user-visible source is consistent.
var notifier = notification.New()

func notify(body string, level notification.Level) {
	_ = notifier.Notify("Waired", body, level)
}

// Seams over the per-OS dialog and host-integration helpers. Every handler
// below calls the lowercase name; nothing in the handler layer calls the
// exported one directly (scripts/ci/tray-dialog-seam-guard.sh enforces that).
//
// The reason is that on darwin these are not no-ops. ShowAbout / ShowError /
// ShowConfirm / ConfirmYesNo / ConfirmWithLabels shell out to `osascript
// display dialog`, which shows a real modal and does not return until a human
// clicks it: a unit test that reaches one hangs a headless runner to the job
// timeout. That is #152, and it is why tray-darwin.yml could vet this package
// but not test it. The *ViaElevation four are worse still — they raise a real
// administrator-password prompt. OpenBrowser and CopyToClipboard do return
// promptly, but they launch a browser and overwrite the pasteboard, which a
// test has no business doing either.
//
// seams_test.go's TestMain replaces all of them with recording no-ops, so the
// suite is hermetic by construction rather than by each test remembering to
// opt in. The layer BELOW each seam stays under test per OS: the real helpers
// and their pure argv / AppleScript builders are exercised by
// dialog_darwin_test.go, dialog_linux_test.go and actions_windows_test.go.
var (
	showAbout                 = ShowAbout
	showError                 = ShowError
	showConfirm               = ShowConfirm
	showStatus                = ShowStatus
	confirmYesNo              = ConfirmYesNo
	confirmWithLabels         = ConfirmWithLabels
	copyToClipboard           = CopyToClipboard
	openBrowser               = OpenBrowser
	loginViaElevation         = LoginViaElevation
	logoutViaElevation        = LogoutViaElevation
	installOllamaViaElevation = InstallOllamaViaElevation
	updateViaElevation        = UpdateViaElevation
	startAgentViaElevation    = StartAgentServiceViaElevation
	linkIntegrationAsUser     = LinkIntegrationAsUser
	serviceInstalled          = service.Installed
)

// iconConnected / iconDisconnected / iconError / iconDegraded are
// defined in icons_unix.go and icons_windows.go: Unix (linux/darwin)
// uses PNG, which fyne.io/systray accepts natively, while Windows
// uses ICO, which is the only format the Win32 tray icon API parses
// reliably (per fyne.io/systray SetIcon godoc).

// Options configures the tray. ControlURL is optional; when empty the
// tray reads it from /v1/identity once enrolled, but a first-time
// "Sign in…" action requires either ControlURL or
// $WAIRED_CONTROL_URL to be set.
type Options struct {
	MgmtURL    string
	ControlURL string
	// StateDir is the directory elevated actions are told to work on. It is
	// only consulted when StateDirPinned is set; otherwise the app asks the
	// daemon and falls back to the local answer, per action — see
	// elevationStateDir in statedir.go.
	StateDir string
	// StateDirPinned records that --state-dir was passed on the command
	// line. An operator who named a directory gets exactly that; anyone else
	// gets the daemon's own answer. Same rule --log-level already follows.
	StateDirPinned bool
	Version        string
	BuildSHA       string
	PollEvery      time.Duration // default 5s
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
		// The tray autostarts at login; the agent service does not
		// necessarily answer yet (Windows registers it delayed-auto-start).
		// Until this lapses an unreachable daemon reads as "starting", not
		// as a failure (#315).
		startingUntil: time.Now().Add(startGraceFor(runtime.GOOS)),
		// Sampled once: whether there is a registered service for the
		// daemon-down menu's start row to act on. Install/uninstall during a
		// tray session is rare enough to not be worth re-probing every poll,
		// and the probe is a privileged-looking SCM/systemd call.
		serviceRegistered: serviceInstalled(),
	}
	// Present as a menu-bar-only accessory (no Dock icon / Cmd-Tab
	// entry). No-op off darwin; on darwin this is the analogue of the
	// Windows tray's `-H windowsgui` linker flag. Must run before the
	// AppKit run loop starts so the Dock icon never flashes.
	setActivationPolicyAccessory()
	// systray.Run can panic on the way OUT rather than return, on a
	// session with no D-Bus: fyne.io/systray v1.12.2's unix nativeEnd
	// closes instance.conn unconditionally, and nativeStart leaves that
	// nil when dbus.SessionBus() failed — it logs and returns early.
	// Nothing ever reached nativeEnd before watchShutdown existed
	// (systray.Quit was only ever called from the menu, which needs a
	// bus to have been drawn at all), so the shape was unreachable. A
	// tray with no session bus never drew an icon and has nothing to
	// unwind, so swallow it and let the process exit 0 rather than dump
	// a runtime stack into the user's journal (waired-agent#1045).
	// Record of fyne.io/systray v1.12.2 (systray_unix.go:181-196) —
	// re-check on upgrade. Scope is the native loop only: onReady runs
	// on a goroutine systray owns, so its panics never come through here.
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("waired-tray: systray event loop unwound with a panic", "recovered", r)
		}
	}()
	systray.Run(t.onReady(ctx), t.onSystrayExit)
}

type tray struct {
	opts Options
	cli  *Client

	// windDownOnce guards the mesh withdrawal + engine stop. Two paths
	// reach it for one departure (see shutdown.go windDown), and on
	// Windows the second is the ONLY one — a logout arrives as
	// WM_ENDSESSION, not as a signal.
	windDownOnce sync.Once

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
	miHeader *systray.MenuItem
	miEmail  *systray.MenuItem
	miStatus *systray.MenuItem // renders MenuModel.StatusMsg (daemon-down explanation / login code / error); hidden when empty (waired#808)
	// Daemon-down actions (#315/#317): elevate-and-start, and copy the
	// command. Static titles, so no SetTitle ever reaches them.
	miStartAgent     *systray.MenuItem
	miStartAgentCopy *systray.MenuItem
	miUpdateNotify   *systray.MenuItem // "✓ Notify me about updates"; in Settings (#294)
	miToggle         *systray.MenuItem
	// miInference is the "Inference ▸" submenu parent (waired#809). The
	// engine/share/recommend rows below are its children instead of
	// top-level rows, so the top level stays short. Shown when
	// ShowInferenceMenu is set. Routing lives under miRouting since #327.
	miInference       *systray.MenuItem
	miInferenceToggle *systray.MenuItem
	miInferenceState  *systray.MenuItem
	miEngineToggle    *systray.MenuItem
	miInstallEngine   *systray.MenuItem
	miShareToggle     *systray.MenuItem
	miShareState      *systray.MenuItem
	miEngineWarning   *systray.MenuItem
	miActiveModel     *systray.MenuItem
	// Model residency (waired-agent#861): the release valve and the
	// preset rows for "how long does the model stay in memory". Flat
	// level-2 children of miInference with a disabled caption row, the
	// same shape the worker rows use — the Windows systray backend does
	// not render a third nesting level.
	miUnloadModel     *systray.MenuItem
	miResidencyHeader *systray.MenuItem
	miResidency       []*systray.MenuItem // residencyPresetSlots entries
	lastResidencyRows []ResidencyRow      // duration lookup for click dispatch
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
	// the "Claude Code" parent, holding the Claude status header, the
	// managed-settings row, and a disabled enable note shown conditionally.
	// The per-class route selectors that used to sit here are gone with the
	// routes themselves
	// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md).
	miClaudeCode       *systray.MenuItem
	miClaudeEnableNote *systray.MenuItem

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
	// The top-level status block (waired-agent#1032): this computer's
	// engine, the other computers, and whether Claude Code is pointed
	// here. Display-only; miStatusEngine also carries the active model
	// name, which used to be a row of its own ("Active: <model>").
	miStatusEngine     *systray.MenuItem
	miStatusPeers      *systray.MenuItem
	miStatusClaude     *systray.MenuItem
	miCatalog          *systray.MenuItem // "Models" — submenu parent
	miCatalogNote      *systray.MenuItem
	miCatalogNoteSep   *systray.MenuItem
	miCatalogEntries   []*systray.MenuItem
	lastCatalogEntries []CatalogEntryView // ModelID lookup for click dispatch

	// Daemon-published notices (waired-agent#1205). miNotices are the
	// pre-allocated top-block rows; lastNotices resolves a clicked slot
	// back to what it was showing, the same way lastCatalogEntries does.
	miNotices   []*systray.MenuItem
	lastNotices []NoticeRow

	// The benchmark step-down/step-up suggestions (#133). The row that
	// showed them moved into the notice rows above, so the same message
	// also reaches `waired doctor` and `waired status`. What stays here
	// is the state a click acts on: lastRecommendation is the live
	// recommendation the popup accepts or declines, and lastRecPopupKey
	// de-dupes the once-per-recommendation proactive dialog. Both are
	// still fed by the catalog poll.
	lastRecommendation *management.BenchmarkRecommendation
	lastRecPopupKey    string

	// "Inference routing ▸" submenu (#327) — Tailscale-exit-node-style
	// manual routing, split off "Inference" so engine control and request
	// routing stop sharing one list. miRouting is the top-level parent;
	// miWorkerActive ("Worker: linux-gpu (pinned)") and miMeshReachable
	// are its display-only first rows. miWorkerModesHeader /
	// miWorkerPinsHeader are disabled section labels that separate the
	// automatic modes from the concrete per-peer pins — the systray
	// Windows backend renders no third nesting level, so labelled groups
	// in one flat list are the available separation. Mode rows are fixed
	// slots (auto / local-only / peer-preferred / peer-only); pin rows are
	// MaxWorkerPinEntries dynamic slots driven by the mesh snapshot.
	// miWorkerClearPin only shows when mode==pinned.
	miRouting            *systray.MenuItem
	miWorkerActive       *systray.MenuItem
	miMeshReachable      *systray.MenuItem
	miWorkerModesHeader  *systray.MenuItem
	miWorkerPinsHeader   *systray.MenuItem
	miWorkerModes        []*systray.MenuItem // workerModeSlots entries: auto / local-only / peer-preferred / peer-only
	miWorkerPreferHeader *systray.MenuItem
	miWorkerPrefers      []*systray.MenuItem // workerPreferSlots entries: speed / size
	miWorkerMinSizeHdr   *systray.MenuItem
	miWorkerMinSizes     []*systray.MenuItem // workerMinSizeSlots entries: any / small / medium / large
	miWorkerPinEntries   []*systray.MenuItem
	miWorkerClearPin     *systray.MenuItem
	lastWorkerModes      []WorkerModeRow      // Mode lookup for click dispatch
	lastWorkerPrefers    []WorkerPreferRow    // Prefer lookup for click dispatch
	lastWorkerMinSizes   []WorkerMinSizeRow   // Size lookup for click dispatch
	lastWorkerPinEntries []WorkerPinEntryView // DeviceID lookup for pin click dispatch

	// Public computers submenu (waired#833). miPublicShare is a NEW
	// top-level parent (the Windows systray backend won't render three
	// nesting levels, so every row below is a FLAT level-2 child).
	// miPublicUseHeader is a grey section label for the consumer group,
	// followed by exactly three mode rows (off/auto/explicit).
	// miPublicMore opens the served "Privacy & safety…" link.
	// lastPublicUseModes backs the mode-row click dispatch under t.mu.
	//
	// There is no provider row here since waired#1297: whether this
	// computer is offered to other people is set in the console. What
	// this app still owns is the switch under "Sharing" above, which
	// stops all of it.
	miPublicShare      *systray.MenuItem
	miPublicUseHeader  *systray.MenuItem
	miPublicUseModes   []*systray.MenuItem // 3 entries: off / auto / explicit
	miPublicMore       *systray.MenuItem
	lastPublicUseModes []PublicUseModeRow

	miAdmin *systray.MenuItem
	// miSettings is the "Settings ▸" submenu parent (waired#809): the
	// OpenClaw integration rows, Recent activity, autostart toggle,
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
	// MaxPeerRows slots as children of miPeers + one extra
	// overflow slot for "+N more". Hidden when no peer has published
	// Hardware (old daemons / CPU-only meshes), in which case miPeers
	// stays a bare "Peers: N" label.
	miPeerEntries  []*systray.MenuItem
	miPeerOverflow *systray.MenuItem

	// miStatusPage is the "Status…" row: the discoverable way into the
	// report that every state row also opens. Fixed title, never hidden,
	// so it never enters the row diff.
	miStatusPage *systray.MenuItem

	// statusInFlight guards the report dialog (mu-protected). It blocks
	// on the user, and some two dozen rows open it, so without this a
	// handful of clicks would stack a handful of message boxes.
	statusInFlight bool

	mu   sync.Mutex
	last MenuModel
	// lastSnap is the poll that produced last. The menu keeps only what
	// a row can show; the status report needs the rest — per-peer
	// hardware, transport, map age — so the raw snapshot is kept beside
	// the model rather than widening MenuModel with fields no row reads
	// (and no debug dump wants).
	lastSnap Snapshot

	// Row-diff state (applyMu-protected). applyRows runs on the poll
	// goroutine and on every click handler that calls pollOnce, so the pass
	// is serialised: rowStates is a map, and concurrent writes panic.
	// rowStates records what the last pass left on each pre-allocated item —
	// the guarded setTitle / setTooltip / setEnabled consult it so a hidden
	// row is never mutated (see rows.go). rowForce is set only by
	// paintCreationBaseline.
	applyMu   sync.Mutex
	rowStates map[menuRow]rowState
	rowForce  bool

	// Model-switch grace state (mu-protected, waired#808). lastOnline is
	// the most recent model rendered while the daemon was reachable;
	// switchingUntil, when in the future, marks the window during which an
	// unreachable daemon is shown as "Switching model…" (keeping the last
	// online menu) instead of the red agent-down state — covering the
	// supervised restart a restart-fallback model switch triggers.
	lastOnline     MenuModel
	switchingUntil time.Time

	// Start grace (mu-protected, #315). The tray autostarts at login, but the
	// agent service is registered delayed-auto-start, so for a couple of
	// minutes after every boot the daemon is legitimately not up yet. Until
	// startingUntil passes, an unreachable daemon renders as "starting…"
	// rather than the red failure state. Seeded at tray start and cleared by
	// the first successful poll.
	startingUntil time.Time
	// startInFlight guards the elevation prompt: one at a time.
	startInFlight bool
	// serviceRegistered is service.Installed() sampled at start: whether
	// there is a service for the start row to act on at all. A raw-binary dev
	// run has none, and offering a button that cannot work is worse than
	// offering nothing.
	serviceRegistered bool

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

	// Tray-host toast state (mu-protected). Same cadence shape as the update
	// toast: fire on the first sighting, then re-remind at most once per
	// trayHostRenotifyInterval while the host is still missing (#295).
	lastNotifiedTrayHostAt time.Time

	// Application Control toast state (mu-protected). Keyed on WHICH programs
	// Windows is refusing, not only on time: a second program going the same
	// way is new information and must not wait out the renotify window
	// (waired-agent#1217).
	lastNotifiedAppControlSubject string
	lastNotifiedAppControlAt      time.Time

	// dialect is how a menu label has to be written for whatever will draw
	// it (waired-agent#1100). NOT mu-protected, and deliberately so: it is
	// written once in onReady before any goroutine that paints a row is
	// started, and only read afterwards. The zero value is
	// MenuDialectSpec, so a *tray built by a test — or the window before
	// onReady resolves it — writes the markup the specification asks for,
	// which is the right answer everywhere except the one renderer we
	// recognise.
	dialect trayhost.MenuDialect
}

// onReady builds the whole menu once. Rows are pre-allocated here and the
// diff in apply() only ever flips their visibility, title and enabled state
// — systray cannot insert an item later, so creation order is render order.
//
// A submenu PARENT is never Hide()n at creation, and that is load-bearing.
// On the Windows backend the first AddSubMenuItem is what CREATES the
// submenu: addOrUpdateMenuItem finds no t.menus[parent] and calls
// convertToSubMenu, which is SetMenuItemInfo(t.menuOf[parent], parent,
// MIIM_SUBMENU). Hide() is RemoveMenu, so on an already-hidden parent that
// call fails, convertToSubMenu returns the error, and the child is never
// inserted — nor is any later one, because t.menus[parent] stays unset. The
// submenu then materialises only once some Show() happens to land after the
// parent's own, and endRowPass walks t.rowStates, a Go map: the rows lost
// are whichever ones the random iteration order reached first. Measured on
// pc-dell-premium (0.0.3-rc4): three consecutive tray restarts rendered
// "This device" with 8, 5 and 4 of its 8 rows, the device's own name and
// address among the missing (waired-agent#1063). Parents therefore keep
// their children, and paintCreationBaseline hides them from the zero
// MenuModel — which is the drift it was added to prevent in the first place.
//
// Disable() means ONE thing here: this row is unavailable. That is the
// only thing it means to the person reading the menu — Windows' own
// guidance is "refer to unavailable menu items as unavailable, not as
// dimmed, disabled, or grayed", GNOME's is "make a menu item insensitive
// when its command is unavailable", and grey has meant exactly that since
// the first Macintosh. So a row that says a state is fine — "● Engine:
// ready", "Worker: sv-evox2", a peer that is serving — must never be grey:
// greyed good news reads as broken, which is what the owner reported on
// 2026-08-28 against the rows waired-agent#1032 had just added.
//
// The rows that stay grey are the two kinds where grey is true: section
// headers, which name a group rather than a state, and actions that really
// cannot be taken right now (Unload model with nothing loaded). Each one
// carries a `// grey: <why>` comment, and scripts/ci/tray-grey-row-guard.sh
// fails the build for a Disable() without one.
//
// Everything else got a click instead, because there is no third option:
// on all three backends an ENABLED row dismisses the menu when clicked
// (Windows TrackPopupMenu closes the popup before it posts WM_COMMAND,
// AppKit fires the action after the menu closes, and on Linux the shell
// closes the menu before sending Event) while a DISABLED row swallows the
// click and keeps the menu open. "Looks live, does nothing, stays open"
// is not reachable — so every state row opens the status report instead
// (onShowStatus).
func (t *tray) onReady(ctx context.Context) func() {
	return func() {
		// First, before any menu item is built: from here on a signal
		// can end this process. onReady is the earliest point all three
		// systray backends are up, and systray.Quit is one-shot for the
		// process lifetime, so quitting any earlier would spend it on a
		// backend that cannot act (see watchShutdown). Top rather than
		// beside the other goroutines below: onReady builds some sixty
		// menu items, and on darwin every one of them is a
		// waitUntilDone:YES round trip to the main thread — a window in
		// which the signal would be dropped (waired-agent#1045).
		go t.watchShutdown(ctx)

		// How the process that draws this menu reads a label. Resolved
		// here, before the first row exists, because two titles are
		// written at creation and never pass through the row diff. It is
		// written once and only read afterwards — every goroutine that
		// paints a row is started at the bottom of this function — so it
		// needs no lock (waired-agent#1100).
		t.dialect = trayHostMenuLabels()
		slog.Debug("tray: menu labels", "dialect", t.dialect.String())

		systray.SetTitle("Waired")
		systray.SetTooltip("Waired")
		systray.SetIcon(iconErrorIcon) // start grey until first poll proves daemon up

		t.miHeader = systray.AddMenuItem("Connecting…", "")
		t.miHeader.Disable() // grey: the menu's own title line — the owner scoped this pass to the rows under it
		// The signed-in account. Not grey: it says who you are signed in
		// as, which is a state like any other, and clicking it opens that
		// account's page in the console (owner request, 2026-08-28).
		t.miEmail = systray.AddMenuItem("", "Open this account's page in the Waired console")
		// Status / hint line (waired#808): renders MenuModel.StatusMsg —
		// the daemon-down "Start-Service…" hint, the login user-code, or an
		// error reason. Hidden by default so the initial (false,false)
		// visibility diff is a no-op and a healthy menu never shows a blank
		// row.
		t.miStatus = systray.AddMenuItem("", "")
		t.miStatus.Hide()
		// Agent-start rows (#315/#317). The daemon-down menu used to end at
		// the status line above, which rendered a raw shell command into a
		// disabled row — a command the user was expected to retype into an
		// admin shell. These two do it for them: the first elevates and starts
		// the service, the second copies the command for anyone who would
		// rather run it themselves.
		//
		// Both carry their final title from creation and are never re-titled,
		// which is what keeps them out of the SetTitle-on-hidden trap the row
		// diff exists to police (rows.go). Position is load-bearing: systray
		// cannot insert items at runtime, so creation order IS render order,
		// and the start action belongs with the status it answers — above the
		// update banner, not below Quit.
		t.miStartAgent = systray.AddMenuItem(startAgentActionLabel, "Start the background service (asks for administrator access)")
		t.miStartAgent.Hide()
		t.miStartAgentCopy = systray.AddMenuItem(startAgentCopyLabel, "Copy the command that starts the background service")
		t.miStartAgentCopy.Hide()
		// Daemon-published notices (waired-agent#1205). The top block,
		// under the sign-in rows: these are the things Waired wants to
		// tell you, rather than states you asked about. The manual-update
		// banner (#293) used to be the row above them and is now one of
		// them, published by the daemon so the two CLIs show the same
		// release (waired-agent#1229). Position is load-bearing — systray
		// cannot insert items at runtime, so creation order IS render
		// order.
		//
		// Exactly notice.MaxActive slots, which is also what the daemon
		// clamps to, so there is no overflow row to render: a slot
		// nobody can reach would be a row nobody could ever see. All
		// hidden by default, so a host with nothing to be told shows
		// nothing. NOT disabled: every one carries a click (its own
		// action, or the status report), and grey means unavailable.
		t.miNotices = make([]*systray.MenuItem, notice.MaxActive)
		for i := range t.miNotices {
			t.miNotices[i] = systray.AddMenuItem("", "What Waired wants to tell you about this computer")
			t.miNotices[i].Hide()
		}
		systray.AddSeparator()
		t.miToggle = systray.AddMenuItem("", "")
		systray.AddSeparator()
		// --- Status block (top-level): can this computer answer, can the
		// other computers, is Claude Code pointed here. Three rows, above
		// the submenus that hold the detail and the controls, so the answer
		// to each is one glance rather than one click (waired-agent#1032 +
		// owner request, 2026-08-28). Not Disable()d — see the grey note
		// above onReady; a click opens the full report.
		t.miStatusEngine = systray.AddMenuItem("", "This computer's inference engine")
		t.miStatusEngine.Hide()
		t.miStatusPeers = systray.AddMenuItem("", "Your other computers that can answer this one's requests")
		t.miStatusPeers.Hide()
		t.miStatusClaude = systray.AddMenuItem("", "Whether Claude Code sends its requests to Waired")
		t.miStatusClaude.Hide()
		// --- Models (top-level): switching models is a primary action, so
		// the catalog stays out of a submenu (waired#809). The model name
		// itself moved up into the Engine status row above.
		t.miCatalog = systray.AddMenuItem("Models", "Choose a different model")
		// Not hidden here — see the submenu-parent note above onReady.
		// Context row above the models, for a host with no AI engine
		// (#852). Display-only and created BEFORE the entry slots so it
		// keeps the top of the submenu; hidden on every other host, and
		// the separator collapses with it.
		t.miCatalogNote = t.miCatalog.AddSubMenuItem("", "")
		t.miCatalogNote.Disable() // grey: explains why the list below is empty; it asserts nothing about a state
		t.miCatalogNote.Hide()
		t.miCatalogNoteSep = t.miCatalog.AddSubMenuItem("──────────", "")
		t.miCatalogNoteSep.Disable() // grey: a drawn separator, not a row
		t.miCatalogNoteSep.Hide()
		t.miCatalogEntries = make([]*systray.MenuItem, MaxCatalogEntries)
		for i := 0; i < MaxCatalogEntries; i++ {
			t.miCatalogEntries[i] = t.miCatalog.AddSubMenuItem("", "Switch the model this computer runs")
			t.miCatalogEntries[i].Hide()
		}

		// --- Inference submenu (waired#809): the engine power / share /
		// model status rows for THIS computer. Shown when
		// ShowInferenceMenu is set (the daemon exposes the inference
		// API); apply() fills the rows. Each row keeps its prior
		// Disable()/Hide() baseline so the first paint's (false,false)
		// visibility diffs stay no-ops.
		//
		// Routing — "which computer answers my requests" — is NOT here
		// since #327; it has its own top-level parent below. The review
		// found the two indistinguishable when engine controls, status
		// captions and routing radios shared one flat list.
		t.miInference = systray.AddMenuItem("Inference", "The local inference engine's state and controls")
		// Not hidden here — see the submenu-parent note above onReady.
		t.miInferenceToggle = t.miInference.AddSubMenuItem("", tipInferenceToggle)
		t.miInferenceState = t.miInference.AddSubMenuItem("", "")
		t.miEngineToggle = t.miInference.AddSubMenuItem("", "Stop the engine to free memory, or start it again")
		t.miEngineToggle.Disable() // grey: unavailable until the daemon reports an engine that can be started
		t.miEngineToggle.Hide()
		t.miInstallEngine = t.miInference.AddSubMenuItem("", "Download and install the local inference engine")
		t.miInstallEngine.Hide()
		t.miShareToggle = t.miInference.AddSubMenuItem("", "")
		t.miShareState = t.miInference.AddSubMenuItem("", "")
		// Why the engine is not serving: the reason it stopped, or the
		// version note when it is running. The row is one clamped line
		// (state.go's firstLine); Status… has it in full.
		t.miEngineWarning = t.miInference.AddSubMenuItem("", "Why the inference engine isn't serving. Status… has the full text")
		t.miEngineWarning.Hide()
		t.miActiveModel = t.miInference.AddSubMenuItem("", "")
		t.miActiveModel.Hide()
		t.miUnloadModel = t.miInference.AddSubMenuItem("", "Free the model's memory; the engine keeps running and the next request loads it again")
		t.miUnloadModel.Disable() // grey: unavailable until there is a loaded model whose memory can be given back
		t.miUnloadModel.Hide()
		t.miResidencyHeader = t.miInference.AddSubMenuItem("", "How long the engine keeps the model in memory after the last request")
		t.miResidencyHeader.Disable() // grey: section header for the presets under it
		t.miResidencyHeader.Hide()
		t.miResidency = make([]*systray.MenuItem, residencyPresetSlots)
		for i := 0; i < residencyPresetSlots; i++ {
			t.miResidency[i] = t.miInference.AddSubMenuItem("", "Set how long the model stays loaded after the last request")
			t.miResidency[i].Hide()
		}
		// --- Inference routing submenu (#327): a NEW top-level parent
		// holding the answer to "where do my requests run" — the current
		// worker, whether any peer engine is reachable, the automatic
		// modes, and the concrete per-peer pins.
		//
		// All rows are FLAT level-2 children with disabled header rows
		// marking the two groups, because fyne.io/systray's Windows
		// backend does not render a third nesting level (same limit that
		// flattened these rows under "Inference" in waired#809, and the
		// reason the pins cannot live one submenu deeper as the review
		// suggested). Click dispatch is unchanged — it keys off the item
		// pointers, not the parent.
		t.miRouting = systray.AddMenuItem("Inference routing", "Which computer answers this one's inference requests")
		// Not hidden here — see the submenu-parent note above onReady.
		t.miWorkerActive = t.miRouting.AddSubMenuItem("", "")
		t.miWorkerActive.Hide()
		t.miMeshReachable = t.miRouting.AddSubMenuItem("", "")
		t.miMeshReachable.Hide()
		t.miWorkerModesHeader = t.miRouting.AddSubMenuItem("", "Waired picks the computer for you, following this rule")
		t.miWorkerModesHeader.Disable() // grey: section header for the mode rows under it
		t.miWorkerModesHeader.Hide()
		t.miWorkerModes = make([]*systray.MenuItem, workerModeSlots)
		for i := 0; i < workerModeSlots; i++ {
			t.miWorkerModes[i] = t.miRouting.AddSubMenuItem("", "Set the routing mode")
			t.miWorkerModes[i].Hide()
		}
		// waired-agent#1128. Flat level-2 children like everything else
		// here: fyne.io/systray's Windows backend renders no third
		// nesting level.
		t.miWorkerPreferHeader = t.miRouting.AddSubMenuItem("",
			"Which computer to prefer when more than one could take the turn")
		t.miWorkerPreferHeader.Disable() // grey: section header for the rows under it
		t.miWorkerPreferHeader.Hide()
		t.miWorkerPrefers = make([]*systray.MenuItem, workerPreferSlots)
		for i := 0; i < workerPreferSlots; i++ {
			t.miWorkerPrefers[i] = t.miRouting.AddSubMenuItem("", "Choose what to optimise for")
			t.miWorkerPrefers[i].Hide()
		}
		t.miWorkerMinSizeHdr = t.miRouting.AddSubMenuItem("",
			"Skip computers running a model smaller than this")
		t.miWorkerMinSizeHdr.Disable() // grey: section header for the rows under it
		t.miWorkerMinSizeHdr.Hide()
		t.miWorkerMinSizes = make([]*systray.MenuItem, workerMinSizeSlots)
		for i := 0; i < workerMinSizeSlots; i++ {
			t.miWorkerMinSizes[i] = t.miRouting.AddSubMenuItem("", "Set the smallest model to route to")
			t.miWorkerMinSizes[i].Hide()
		}
		t.miWorkerPinsHeader = t.miRouting.AddSubMenuItem("", "Always use one specific computer, instead of the rule above")
		t.miWorkerPinsHeader.Disable() // grey: section header for the pin rows under it
		t.miWorkerPinsHeader.Hide()
		t.miWorkerPinEntries = make([]*systray.MenuItem, MaxWorkerPinEntries)
		for i := 0; i < MaxWorkerPinEntries; i++ {
			t.miWorkerPinEntries[i] = t.miRouting.AddSubMenuItem("", "Send every request to this computer")
			t.miWorkerPinEntries[i].Hide()
		}
		t.miWorkerClearPin = t.miRouting.AddSubMenuItem("(clear pin)", "Go back to automatic routing")
		t.miWorkerClearPin.Hide()

		// --- Public computers submenu (waired#833): a NEW top-level
		// "Public computers" parent. All rows are FLAT level-2 children (the Windows
		// systray backend won't render three nesting levels — same limit as
		// the worker rows above). Hidden until the daemon proves it exposes
		// the public endpoints (apply() tracks ShowPublicShareMenu); each row
		// keeps its Disable()/Hide() baseline so the first paint's
		// (false,false) visibility diffs stay no-ops.
		t.miPublicShare = systray.AddMenuItem("Public computers", "Choose whether to use other people's public computers")
		// Not hidden here — see the submenu-parent note above onReady.
		t.miPublicUseHeader = t.miPublicShare.AddSubMenuItem("", "")
		t.miPublicUseHeader.Disable() // grey: section header for the consumer rows under it
		t.miPublicUseHeader.Hide()
		t.miPublicUseModes = make([]*systray.MenuItem, 3)
		for i := 0; i < 3; i++ {
			t.miPublicUseModes[i] = t.miPublicShare.AddSubMenuItem("", "Choose how this computer uses other people's public computers")
			t.miPublicUseModes[i].Hide()
		}
		// escapeMenuLabel, not the bare literal: this is the one static
		// title carrying an `&`, and Win32 menus read that as the
		// mnemonic prefix — it drew as "Privacy  safety…" until
		// waired-agent#1096. Creation-time titles do not pass through
		// the row diff, so the escape has to happen here.
		t.miPublicMore = t.miPublicShare.AddSubMenuItem(
			escapeMenuLabel(runtime.GOOS, t.dialect, "Privacy and safety…"),
			"Open the Public Share privacy and safety notes")
		t.miPublicMore.Hide()

		// --- Claude Code submenu (waired#809): the Claude status header and
		// the managed-settings row, so no Claude detail sits at the top
		// level. The parent is shown when ShowClaude || ShowClaudeCode (see
		// apply()).
		t.miClaudeCode = systray.AddMenuItem("Claude Code", "Claude Code integration status")
		// Not hidden here — see the submenu-parent note above onReady.
		t.miClaudeHeader = t.miClaudeCode.AddSubMenuItem("", "")
		t.miClaudeProxy = t.miClaudeCode.AddSubMenuItem("", "Claude Code managed-settings status (waired claude enable / disable / status)")
		t.miClaudeEnableNote = t.miClaudeCode.AddSubMenuItem("", "Claude Code isn't yet routed through Waired")
		t.miClaudeEnableNote.Hide()

		systray.AddSeparator()

		// --- This device submenu (waired#809): name / IP / network / peers
		// move under one parent. Shown only when enrolled — apply() tracks
		// the parent in the device-visibility group, so it starts hidden.
		t.miDeviceLabel = systray.AddMenuItem("This device", "This computer's name, address and the other computers on your network")
		// Not hidden here — see the submenu-parent note above onReady.
		t.miDeviceName = t.miDeviceLabel.AddSubMenuItem("", "")
		t.miOverlayIP = t.miDeviceLabel.AddSubMenuItem("", "Click to copy")
		t.miNetwork = t.miDeviceLabel.AddSubMenuItem("", "")
		// Peers: the "Peers: N" label plus per-peer hardware rows are all
		// flat children of This device (no third nesting level, per the
		// Windows-backend limit above). miPeers is a count, not a header, so
		// it opens the status report like every other row that names a state.
		t.miPeers = t.miDeviceLabel.AddSubMenuItem("", "")
		t.miPeerEntries = make([]*systray.MenuItem, MaxPeerRows)
		for i := range MaxPeerRows {
			t.miPeerEntries[i] = t.miDeviceLabel.AddSubMenuItem("", "")
			t.miPeerEntries[i].Hide()
		}
		t.miPeerOverflow = t.miDeviceLabel.AddSubMenuItem("", "")
		t.miPeerOverflow.Hide()

		// Status… (owner request, 2026-08-28). Every row that names a state
		// opens this report; this row is the one that says so, for a reader
		// who would not think to click a line that looks like a label. Its
		// title is final at creation and it is never hidden, so it needs no
		// MenuModel field and never enters the row diff.
		t.miStatusPage = systray.AddMenuItem(statusPageLabel, "Show everything Waired knows right now, and copy it")

		t.miAdmin = systray.AddMenuItem("Open Waired console…", "Open the Waired console in your browser")

		// --- Settings submenu (waired#809): the OpenCode / OpenClaw
		// integration rows, Recent activity, the startup toggle, About, and
		// Log out move off the top level. The parent is always visible (About
		// and the startup toggle are always available); each row keeps its
		// own Show/Hide.
		t.miSettings = systray.AddMenuItem("Settings", "Integrations, startup and account")
		t.miOpenCodeHeader = t.miSettings.AddSubMenuItem("", "")
		t.miOpenCodeHeader.Disable() // grey: section header for the OpenCode rows under it
		t.miOpenCodeConfig = t.miSettings.AddSubMenuItem("", "")
		t.miOpenCodeReconfigure = t.miSettings.AddSubMenuItem("", "Run `waired link opencode` again after a confirmation")
		t.miOpenClawHeader = t.miSettings.AddSubMenuItem("", "")
		t.miOpenClawHeader.Disable() // grey: section header for the OpenClaw rows under it
		t.miOpenClawConfig = t.miSettings.AddSubMenuItem("", "")
		t.miOpenClawReconfigure = t.miSettings.AddSubMenuItem("", "Run `waired link openclaw` again after a confirmation")
		// Recent activity: a grey "Recent activity" section label plus its
		// rows, flat under Settings (no third nesting level, per the
		// Windows-backend limit above). The label is a header and stays grey;
		// the rows under it each report an event, so they do not.
		t.miRecent = t.miSettings.AddSubMenuItem("Recent activity", "Inference fallbacks observed in the last 10 minutes")
		t.miRecent.Disable() // grey: section header for the activity rows under it
		t.miRecent.Hide()
		t.miRecentEntries = make([]*systray.MenuItem, MaxRecentActivity)
		for i := 0; i < MaxRecentActivity; i++ {
			t.miRecentEntries[i] = t.miSettings.AddSubMenuItem("", "")
			t.miRecentEntries[i].Hide()
		}
		t.miAbout = t.miSettings.AddSubMenuItem("About Waired", "")
		t.miUpdateNotify = t.miSettings.AddSubMenuItem("", "Show a notification when a Waired update is available")
		t.miUpdateNotify.Hide()
		t.miAutostart = t.miSettings.AddSubMenuItem("Start Waired on login", "Start the Waired app when you sign in")
		t.refreshAutostartLabel()
		t.ensureAutostartOnFirstLaunch()
		t.miLogout = t.miSettings.AddSubMenuItem("Sign out…", "Sign this computer out of your network")

		systray.AddSeparator()
		t.miQuit = systray.AddMenuItem("Quit", "Quit the Waired app")

		// Hide everything the zero MenuModel leaves out. This — not the
		// per-item Hide() calls above — is what makes the creation state
		// correct: see paintCreationBaseline. The per-item calls are kept
		// because they document each row's intent where it is created, and
		// this pass re-asserts them for the rows nobody remembered.
		t.paintCreationBaseline()

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
		for i := 0; i < len(t.miWorkerPrefers); i++ {
			idx := i
			go t.dispatchWorkerPreferClicks(ctx, idx)
		}
		for i := 0; i < len(t.miWorkerMinSizes); i++ {
			idx := i
			go t.dispatchWorkerMinSizeClicks(ctx, idx)
		}
		for i := 0; i < MaxWorkerPinEntries; i++ {
			idx := i
			go t.dispatchWorkerPinClicks(ctx, idx)
		}
		for i := 0; i < residencyPresetSlots; i++ {
			idx := i
			go t.dispatchResidencyClicks(ctx, idx)
		}
		go t.dispatchWorkerClearPinClicks(ctx)
		for i := range t.miNotices {
			idx := i
			go t.dispatchNoticeClicks(ctx, idx)
		}
		// Public-use mode rows (off / auto / explicit): one goroutine per
		// fixed slot, same pattern as the worker mode rows (waired#833).
		for i := 0; i < len(t.miPublicUseModes); i++ {
			idx := i
			go t.dispatchPublicUseModeClicks(ctx, idx)
		}

		// Every row that names a state opens the status report. One
		// goroutine each, the same shape as the slot dispatchers above:
		// two dozen goroutines parked on ClickedCh cost nothing next to
		// two dozen extra cases in the handleClicks select.
		for _, mi := range t.statusReportRows() {
			row := mi
			go t.dispatchStatusClicks(ctx, row)
		}

		// Lift any share suspension a previous Quit left behind, before
		// the first poll renders the share row (#316). Off the GUI
		// thread: it is an IPC round trip.
		go t.resumeSharingOnStart(ctx)

		go t.handleClicks(ctx)
		go t.pollLoop(ctx)
		// Tray-host self-check (#295). This process is the one that knows
		// whether it has a host to draw on, and it only ever runs inside a
		// desktop session — which makes it the natural place for the check
		// that no package metadata can express. Off the GUI thread: it makes
		// a D-Bus round trip and may shell out to gnome-extensions.
		go t.checkTrayHost(ctx)
		// Which of Waired's programs Windows is refusing to run
		// (waired-agent#1217). Here rather than in the daemon or the CLI
		// because the verdict is per file: when waired.exe is the refused
		// one, this process is still running and `waired doctor` cannot be
		// started at all. Off the GUI thread: it shells out to wevtutil, and
		// it keeps polling because a refusal window opens mid-session.
		go t.watchAppControl(ctx)
	}
}

// statusReportRows is every row whose click opens the status report:
// each one names a state, and none of them can be acted on in place.
//
// Called once, after onReady has built the menu. The list is the
// executable form of the grey rule above onReady — a row here is a row
// that must not be Disable()d, and the guard script polices the other
// half of that from the Disable() side.
func (t *tray) statusReportRows() []*systray.MenuItem {
	rows := []*systray.MenuItem{
		t.miStatusPage,
		t.miStatus,
		t.miStatusEngine,
		t.miStatusPeers,
		t.miStatusClaude,
		t.miInferenceState,
		t.miShareState,
		t.miEngineWarning,
		t.miActiveModel,
		t.miWorkerActive,
		t.miMeshReachable,
		t.miClaudeHeader,
		t.miClaudeProxy,
		t.miClaudeEnableNote,
		t.miDeviceName,
		t.miNetwork,
		t.miPeers,
		t.miPeerOverflow,
		t.miOpenCodeConfig,
		t.miOpenClawConfig,
	}
	rows = append(rows, t.miPeerEntries...)
	rows = append(rows, t.miRecentEntries...)
	return rows
}

func (t *tray) dispatchStatusClicks(ctx context.Context, mi *systray.MenuItem) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-mi.ClickedCh:
			// Off the dispatch goroutine: the dialog blocks until the
			// user answers it, and this row must be able to take the
			// next click once they do.
			go t.onShowStatus()
		}
	}
}

// onShowStatus renders the status report, shows it, and puts the fuller
// version on the clipboard if the user asks for it.
//
// The report is built from the last poll rather than a fresh one: the
// rows the user just clicked were painted from that same poll, so a
// re-fetch here would answer a question about a menu state that has
// already gone. The report says which poll it is showing.
func (t *tray) onShowStatus() {
	t.mu.Lock()
	if t.statusInFlight {
		t.mu.Unlock()
		return
	}
	t.statusInFlight = true
	m := t.last
	snap := t.lastSnap
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		t.statusInFlight = false
		t.mu.Unlock()
	}()

	slog.Debug("tray: menu action", "action", "show-status")
	body, details := statusReport(m, snap, t.opts.Version, t.opts.BuildSHA, time.Now())
	if !showStatus(body) {
		return
	}
	if err := copyToClipboard(details); err != nil {
		showError(err.Error())
		return
	}
	notify(statusCopiedToast, notification.Info)
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
// posts the preference to the agent. Applying is asynchronous either
// way — an in-process swap reloads the engine, the restart fallback
// takes the agent down and back — so the immediate poll repaints the
// row with "(switching…)" until the new active selection comes back
// from the catalog endpoint.
func (t *tray) onSelectCatalogEntry(ctx context.Context, idx int) {
	t.mu.Lock()
	var modelID, name, unfit string
	var kind UnfitKind
	if idx < len(t.lastCatalogEntries) {
		modelID = t.lastCatalogEntries[idx].ModelID
		name = t.lastCatalogEntries[idx].Name
		unfit = t.lastCatalogEntries[idx].UnfitReason
		kind = t.lastCatalogEntries[idx].UnfitKind
	}
	engineMissing := t.last.CatalogEngineMissing
	t.mu.Unlock()
	if modelID == "" {
		// A slot past the end of the projection: the row is hidden, so
		// there is no click to answer and nothing to tell anyone about.
		return
	}
	slog.Debug("tray: menu action", "action", "select-model", "model", modelID)
	// No engine here is a fact about the HOST, not a verdict about the
	// row — every row answers it the same way — so it is asked before
	// the per-row question and does not become another UnfitKind (#852).
	if engineMissing {
		t.offerEngineInstall(ctx, switchModelName(name, modelID), name, modelID)
		return
	}
	if unfit != "" && !t.confirmUnfitSwitch(switchModelName(name, modelID), modelID, kind, unfit) {
		return
	}
	resp, err := t.cli.SetPreferredModel(ctx, modelID)
	if err != nil {
		showError(modelSwitchErrorText(err, switchModelName(name, modelID)))
		return
	}
	t.onModelSwitchAccepted(resp, name)
	go t.pollOnce(ctx)
}

// offerEngineInstall answers a model click on a host with no AI engine
// by saying so and offering to install one (owner ruling, 2026-08-19).
//
// It is an offer, not a block. The row stays clickable — #842 removed
// the ability to grey a model out on any surface — and this is the
// question that removal left unasked: there is no engine here, so
// recording the choice alone would change nothing a person could see.
//
// The install runs BEFORE the preference is written, and the preference
// only when the install succeeded. Writing it first would post a switch
// to a daemon with nothing to switch, which on Windows reaches the
// restart fallback and the service that does not come back (#855);
// after a successful install the swap is the ordinary one.
func (t *tray) offerEngineInstall(ctx context.Context, displayName, name, modelID string) {
	title, body := engineInstallPrompt(displayName)
	confirmed, ok := confirmWithLabels(title, body, "Install", "Not now")
	if !ok {
		// Not a dialog-less yes. Hand over the terminal equivalent, the
		// same way an unaskable unfit switch does.
		slog.Warn("tray: cannot ask about installing the inference engine", "model", modelID)
		_ = copyToClipboard(elevation.EngineInstallCommand())
		notify(engineInstallNoDialogText(runtime.GOOS), notification.Warning)
		return
	}
	if !confirmed {
		return
	}
	if err := installOllamaViaElevation(elevationCtx(ctx), t.elevationStateDir(ctx)); err != nil {
		showError(fmt.Sprintf("Couldn't install Ollama: %v", err))
		return
	}
	resp, err := t.cli.SetPreferredModel(ctx, modelID)
	if err != nil {
		showError(modelSwitchErrorText(err, displayName))
		return
	}
	t.onModelSwitchAccepted(resp, name)
	go t.pollOnce(ctx)
}

// engineInstallPrompt words that offer. A pure function for the same
// reason unfitSwitchPrompt is one: the dialog seam cannot be driven in a
// test, and the wording is the point.
//
// Both halves of the truth, in this order: there is no engine here, and
// the requests go to the other computers anyway. Saying only the first
// reads as "this computer is broken", which it is not — an engine-less
// host is a supported state that stays enrolled and routes to the mesh
// (waired-agent#387, #841; waired#1067 decision 5).
func engineInstallPrompt(displayName string) (title, body string) {
	return "There is no inference engine on this computer",
		"This computer has no inference engine, so it can't run " +
			displayName + " itself. Your requests go to your other computers instead.\n\n" +
			"Install Ollama now and make " + displayName +
			" the model this computer runs?"
}

// engineInstallNoDialogText hands the terminal equivalent to a desktop
// with no dialog backend. What is quoted is exactly what can be pasted;
// where the shell has to be elevated, that is said outside the quotes
// (#852 — the quoted string used to carry "(from an elevated prompt)"
// inside it, and the clipboard got that too).
func engineInstallNoDialogText(goos string) string {
	if note := elevation.EngineInstallElevationNoteFor(goos); note != "" {
		return `Can't ask here. Run "` + elevation.EngineInstallCommandFor(goos) +
			`" ` + note + ` to install the inference engine.`
	}
	return `Can't ask here. Run "` + elevation.EngineInstallCommandFor(goos) +
		`" in a terminal to install the inference engine.`
}

// confirmUnfitSwitch is the tray's half of the warn-and-ask ruling of
// 2026-08-08: a model this computer is not expected to run is still the
// operator's to choose, so the row stays clickable and the click asks
// with the shortfall, default No (waired-ai/waired#1067 — the decision
// record supersedes the refusal rule of waired-ai/waired#1056). Until
// waired-agent#831 the tray instead greyed the row and returned from
// the click in silence, which is why "switching the model from the tray
// does nothing" while the same switch through the setup wizard worked.
//
// confirmWithLabels rather than showConfirm: it is the one dialog helper
// whose negative button is the default on all three backends, and its
// second return value separates "the user said no" from "there is no
// desktop dialog to ask with". The latter is not consent, so it does not
// switch — but it does not go quiet either, which is the other half of
// the same defect. The fallback is the CLI, which runs this identical
// gate (cmd/waired/models_fit.go), and it is handed over the same way
// the slow-host prompt hands over `waired runtimes benchmark`.
//
// Blocking here is safe: each catalog slot owns a goroutine
// (dispatchCatalogClicks), so a modal the user leaves focused stalls
// only repeat clicks on that one row, never the menu.
func (t *tray) confirmUnfitSwitch(name, modelID string, kind UnfitKind, reason string) bool {
	title, body := unfitSwitchPrompt(name, kind, reason)
	confirmed, ok := confirmWithLabels(title, body, "Switch anyway", "Cancel")
	if !ok {
		slog.Warn("tray: cannot ask about a model this computer is not expected to run",
			"model", modelID, "kind", string(kind), "reason", reason)
		_ = copyToClipboard(unfitSwitchCommand(modelID))
		notify(unfitSwitchNoDialogText(modelID), notification.Warning)
		return false
	}
	return confirmed
}

// unfitSwitchPrompt words the question confirmUnfitSwitch asks. Split
// out as a pure function for the same reason modelSwitchAcceptedText is:
// apply() and the dialog seams cannot be driven in a test, and the
// wording is the point.
//
// Three shapes, because the verdicts fail differently. A shortfall is
// about this computer's memory and reads as a sentence with the deficit
// in it — the same sentence `waired models pull` prints
// (cmd/waired/models_fit.go). "No build here" is not a quantity, so it
// gets its own line rather than being forced after a colon.
//
// The third says only what the row said. A verdict hostfit does not
// price — today the engine-version floor — has a true reason and no
// cause this layer can state, and the previous version of this function
// asserted memory for it: "Qwen3.8 27B does not fit in this computer's
// memory: needs ollama ≥ 0.32.13" (waired-agent#850, seen on a host with
// 63 GB free). Repeating the row the user just clicked cannot be wrong
// and cannot go stale.
func unfitSwitchPrompt(name string, kind UnfitKind, reason string) (title, body string) {
	const tail = "Selecting it is expected to fail. Switch to it anyway?"
	switch kind {
	case UnfitMemory:
		return "This model doesn't fit this computer",
			name + " doesn't fit in this computer's memory: " + reason + ".\n\n" +
				"Loading it is expected to fail. Switch to it anyway?"
	case UnfitNoBuild:
		return "This model doesn't run on this computer",
			name + " has " + reason + ".\n\n" + tail
	}
	return "This model doesn't run on this computer",
		name + " — " + reason + "\n\n" + tail
}

// unfitSwitchCommand is the terminal equivalent of the click, for a
// desktop with no dialog backend. `models use` runs the same gate and
// asks the same question on stdin.
func unfitSwitchCommand(modelID string) string {
	return "waired models use " + modelID
}

func unfitSwitchNoDialogText(modelID string) string {
	return `Can't ask here. Run "` + unfitSwitchCommand(modelID) +
		`" in a terminal to switch anyway.`
}

// onModelSwitchAccepted gives the user feedback for an accepted model
// switch and, when the daemon reports it will restart (the restart
// fallback — an in-process swap per waired#812 does not), arms the
// grace window so the imminent daemon-down poll renders as "Switching
// model…" instead of the red agent-down state (waired#808).
//
// The three arms are the three things that actually happen next, and
// they take different amounts of time. Collapsing the download case
// into the plain one is what waired#808 is about on this path: the
// swap layer owns the pull and answers 202 before it has fetched
// anything, so "Model switched." fired at the head of a multi-GB
// download — while the old model was still the one answering.
func (t *tray) onModelSwitchAccepted(resp *management.PreferredModelResponse, name string) {
	var echoed string
	if resp != nil {
		echoed = resp.ModelID
	}
	notify(modelSwitchAcceptedText(resp, switchModelName(name, echoed)), notification.Info)
	if resp != nil && resp.WillRestart {
		t.armSwitching()
	}
}

// modelSwitchAcceptedText is the notification body for an accepted
// switch, split out as a pure function because apply()/notify() cannot
// be driven in a test (systray) but this wording is the point of
// waired#808.
func modelSwitchAcceptedText(resp *management.PreferredModelResponse, name string) string {
	switch {
	case resp != nil && resp.WillRestart:
		return "Switching model. The background service will restart briefly."
	case resp != nil && resp.Downloading:
		return fmt.Sprintf("Downloading %s. Your current model keeps answering until it is ready.", name)
	default:
		return fmt.Sprintf("Switching to %s. It will be answering in a few seconds.", name)
	}
}

// modelSwitchErrorText turns a failed switch into a sentence. The 409
// arm is the one that matters: the daemon declines when it cannot fetch
// the weights, and it deliberately keeps the recorded preference so the
// choice applies by itself once pulls work again
// (internal/management/inference_preferred_model.go). Without this the
// dialog showed the raw transport error, JSON body and all.
func modelSwitchErrorText(err error, name string) string {
	if errors.Is(err, ErrModelSwitchUnavailable) {
		return fmt.Sprintf("Can't switch to %s right now. This computer couldn't download the model. "+
			"Your choice is saved and applies once downloads work again.", name)
	}
	return fmt.Sprintf("Couldn't switch the model: %v", err)
}

// switchModelName prefers the row's display name and falls back to the
// model_id, so a sentence never renders "Switching to ." if a slot is
// projected without one.
func switchModelName(name, modelID string) string {
	if name != "" {
		return name
	}
	if modelID != "" {
		return modelID
	}
	return "the new model"
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
func (t *tray) dispatchWorkerPreferClicks(ctx context.Context, idx int) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.miWorkerPrefers[idx].ClickedCh:
			t.onSelectWorkerPrefer(ctx, idx)
		}
	}
}

func (t *tray) dispatchWorkerMinSizeClicks(ctx context.Context, idx int) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.miWorkerMinSizes[idx].ClickedCh:
			t.onSelectWorkerMinSize(ctx, idx)
		}
	}
}

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
	slog.Debug("tray: menu action", "action", "worker-mode", "mode", string(mode))
	if _, err := t.cli.SetWorker(ctx, management.WorkerRequest{Mode: mode}); err != nil {
		showError(fmt.Sprintf("Couldn't set the routing mode: %v", err))
		return
	}
	go t.pollOnce(ctx)
}

// dispatchResidencyClicks handles clicks on the model-residency preset
// rows. One goroutine per fixed slot, the dispatchWorkerModeClicks
// pattern.
func (t *tray) dispatchResidencyClicks(ctx context.Context, idx int) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.miResidency[idx].ClickedCh:
			go t.onSelectResidency(ctx, idx)
		}
	}
}

// onSelectResidency applies a residency preset (waired-agent#861). The
// daemon owns both halves of the change — the running engine and
// agent.json — so this is a single POST.
func (t *tray) onSelectResidency(ctx context.Context, idx int) {
	t.mu.Lock()
	var row ResidencyRow
	var ok bool
	if idx < len(t.lastResidencyRows) {
		row, ok = t.lastResidencyRows[idx], true
	}
	t.mu.Unlock()
	if !ok {
		return
	}
	slog.Debug("tray: menu action", "action", "residency", "idle", row.Idle.String())
	if _, err := t.cli.SetResidency(ctx, row.Idle); err != nil {
		showError(fmt.Sprintf("Couldn't set the keep-alive: %v", err))
		return
	}
	go t.pollOnce(ctx)
}

// onUnloadModel frees the model's memory without stopping the engine
// (waired-agent#861). Nothing loaded is a success, not an error: the
// memory the operator asked for is already back.
func (t *tray) onUnloadModel(ctx context.Context) {
	slog.Debug("tray: menu action", "action", "unload-model")
	resp, err := t.cli.UnloadModel(ctx)
	if err != nil {
		showError(fmt.Sprintf("Couldn't unload the model: %v", err))
		return
	}
	if resp != nil && !resp.Unloaded {
		showError("No model was loaded, so there was nothing to unload.")
	}
	t.pollOnce(ctx)
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
	// Log the action, not the peer device ID (avoid emitting identifiers).
	slog.Debug("tray: menu action", "action", "worker-pin")
	if _, err := t.cli.SetWorker(ctx, management.WorkerRequest{
		Mode:               state.RoutingModePinned,
		PinnedPeerDeviceID: entry.DeviceID,
	}); err != nil {
		showError(fmt.Sprintf("Couldn't pin the computer: %v", err))
		return
	}
	go t.pollOnce(ctx)
}

func (t *tray) onWorkerClearPin(ctx context.Context) {
	slog.Debug("tray: menu action", "action", "worker-clear-pin")
	if _, err := t.cli.SetWorker(ctx, management.WorkerRequest{Mode: state.RoutingModeAuto}); err != nil {
		showError(fmt.Sprintf("Couldn't clear the pin: %v", err))
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
		// Both start rows dispatch on a goroutine: every backend blocks while
		// the OS consent UI is up (UAC dialog / polkit agent / macOS auth
		// sheet), and blocking here would freeze every other menu click.
		case <-t.miStartAgent.ClickedCh:
			go t.onStartAgent(ctx)
		case <-t.miStartAgentCopy.ClickedCh:
			go t.onCopyStartCommand()
		case <-t.miUpdateNotify.ClickedCh:
			go t.onUpdateNotifyToggle(ctx)
		case <-t.miInferenceToggle.ClickedCh:
			t.onInferenceToggle(ctx)
		case <-t.miEngineToggle.ClickedCh:
			// Stopping the engine waits for the process to actually die,
			// which can take seconds. Dispatch so a slow stop cannot
			// freeze every other menu item behind it (#316).
			go t.onEngineToggle(ctx)
		case <-t.miUnloadModel.ClickedCh:
			// An unload waits for the engine to actually release the
			// memory. Dispatch so a slow one cannot freeze every other
			// menu item behind it — the miEngineToggle treatment.
			go t.onUnloadModel(ctx)
		case <-t.miInstallEngine.ClickedCh:
			go t.onInstallEngine(ctx)
		case <-t.miShareToggle.ClickedCh:
			t.onShareToggle(ctx)
		case <-t.miPublicMore.ClickedCh:
			go t.onPublicMore()
		case <-t.miOverlayIP.ClickedCh:
			t.onCopyIP()
		case <-t.miOpenCodeReconfigure.ClickedCh:
			go t.onReconfigureOpenCode(ctx)
		case <-t.miOpenClawReconfigure.ClickedCh:
			go t.onReconfigureOpenClaw(ctx)
		case <-t.miEmail.ClickedCh:
			t.onAccount()
		case <-t.miAdmin.ClickedCh:
			t.onAdmin()
		case <-t.miAbout.ClickedCh:
			showAbout(t.opts.Version, t.opts.BuildSHA)
		case <-t.miAutostart.ClickedCh:
			t.onToggleAutostart()
		case <-t.miLogout.ClickedCh:
			t.onLogout(ctx)
		case <-t.miQuit.ClickedCh:
			t.shutdown(planShutdown(causeQuitMenu), systray.Quit)
			return
		}
	}
}

// quitBudget bounds how long Quit may wait on the daemon. It is generous
// compared to the pre-#316 2s because the calls it makes are now worth
// waiting for — but it is only a bound on the WAIT: since the daemon
// commits to the kill, giving up here no longer abandons a live engine.
const quitBudget = 5 * time.Second

// onQuit winds this machine down on tray exit: the engine is hard-stopped
// so its VRAM/RAM comes back (#186), and sharing is suspended so mesh
// peers stop routing work here while nobody is at the keyboard (#316).
// The daemon itself keeps running; the next tray start resumes sharing
// and a later Start brings the engine back.
//
// Both calls are best-effort — an old daemon (404), an unmanaged engine (409), or
// a daemon already down are all expected on the way out. Unlike the
// pre-#316 version, abandoning the stop mid-flight is now safe rather
// than merely hoped-for: the daemon runs the kill to completion under its
// own budget, so a timeout here means "we stopped watching", not "the
// engine survived".
func (t *tray) onQuit() {
	ctx, cancel := context.WithTimeout(context.Background(), quitBudget)
	defer cancel()
	// Suspend first: withdrawing is a local flag flip, so nobody is
	// routed here before the engine they would have been routed to
	// disappears. The reverse order strands in-flight peer requests
	// against a dying engine.
	//
	// Since waired#1297 this covers public guests too, not just the
	// account's own mesh: closing the app stops the lot, which is what a
	// person who closed it expects of a computer they are lending out.
	// It is still the session latch rather than the persisted switch —
	// quitting is an operation, not a policy
	// (docs/decisions/20260801/1035-mesh-share-suspension-is-live-only.md).
	_ = t.cli.SuspendShare(ctx)
	_ = t.cli.StopEngine(ctx)
}

// resumeSharingOnStart lifts any share suspension left by a previous
// Quit. Sharing is suspended for exactly as long as the tray is closed,
// which is the whole point of it being live-only state (#316): the
// operator's persisted choice was never touched, so this only removes the
// override — an operator who turned sharing off stays off.
//
// Best-effort and silent: a daemon that predates the override answers
// 404, and a daemon that is down will be re-polled soon enough.
func (t *tray) resumeSharingOnStart(ctx context.Context) {
	c, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	if err := t.cli.ResumeShare(c); err != nil {
		slog.Debug("tray: resume sharing on start", "err", err)
	}
}

// firstLaunchAutostartApplies reports whether the tray registers its
// own per-user "launch on login" entry when it starts.
//
// Windows and macOS do: neither ships a system-wide autostart location
// a package could write, so without this the tray would need an
// explicit menu click before the next logon started it.
//
// Linux does not: the .deb already installed
// /etc/xdg/autostart/waired-tray.desktop, active for every user out of
// the box (packaging/nfpm/waired-tray.yaml.tmpl). A redundant
// ~/.config/autostart/ entry written from here would break the "Start
// Waired on login" toggle -- Disable removes only the per-user copy,
// leaving the system-wide file in place and the tray still
// auto-starting.
//
// Taking goos as a parameter rather than reading runtime.GOOS is what
// makes the decision table-testable on one host (CLAUDE.md §Cross-OS
// parity). The darwin arm used to be excluded here on the strength of
// a doc comment saying the macOS path was "still stubbed"; the
// LaunchAgent backend has been complete and unit-tested since
// internal/platform/autostart/autostart_darwin.go landed, so the
// comment was stale and the exclusion with it (waired-agent#833).
func firstLaunchAutostartApplies(goos string) bool {
	return goos == "windows" || goos == "darwin"
}

// ensureAutostartOnFirstLaunch registers the per-user "launch on
// login" entry the first time the tray starts, on the platforms
// firstLaunchAutostartApplies names. Users can still opt out via the
// "Start Waired on login" menu toggle.
//
// Errors are logged and swallowed -- failing here doesn't justify
// aborting the tray boot, and the menu toggle remains as a manual
// fallback.
func (t *tray) ensureAutostartOnFirstLaunch() {
	t.ensureAutostartOnFirstLaunchFor(runtime.GOOS)
}

func (t *tray) ensureAutostartOnFirstLaunchFor(goos string) {
	if !firstLaunchAutostartApplies(goos) {
		return
	}
	enabled, err := t.autostartMgr.IsEnabled()
	if err != nil {
		slog.Warn("tray: autostart probe failed on first launch", "err", err)
		return
	}
	// Record that this user has now started the tray, whatever this
	// launch decides. From the next launch on, an absent login item is
	// their choice rather than a default nobody has met yet
	// (waired-agent#1046).
	defer recordAutostartFirstRun()

	plan := planFirstLaunchAutostart(autostartFirstLaunchFacts{
		Applies: true,
		Enabled: enabled,
		HasRun:  autostartHasRun(),
	})
	if plan != "register" {
		slog.Debug("tray: not registering autostart on this launch", "plan", plan, "goos", goos)
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
	slog.Info("tray: registered autostart on first launch", "exe", exe, "goos", goos)
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
		showError(fmt.Sprintf("Couldn't read the start-on-login setting: %v", err))
		return
	}
	slog.Debug("tray: menu action", "action", "toggle-autostart", "was_enabled", enabled)
	if enabled {
		if err := t.autostartMgr.Disable(); err != nil {
			showError(fmt.Sprintf("Couldn't turn off start on login: %v", err))
			return
		}
	} else {
		exe, err := os.Executable()
		if err != nil {
			showError(fmt.Sprintf("Couldn't turn on start on login: can't locate the app: %v", err))
			return
		}
		args := []string{"-mgmt", t.opts.MgmtURL}
		if err := t.autostartMgr.Enable(exe, args); err != nil {
			showError(fmt.Sprintf("Couldn't turn on start on login: %v", err))
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
	// escapeMenuLabel like every other title, even though neither of
	// these two carries an `&` today: this is the only SetTitle outside
	// the row diff, so leaving it unescaped is what would make the rule
	// "every menu label is escaped" untrue (waired-agent#1096).
	if enabled {
		t.miAutostart.SetTitle(escapeMenuLabel(runtime.GOOS, t.dialect, "✓ Start Waired on login"))
	} else {
		t.miAutostart.SetTitle(escapeMenuLabel(runtime.GOOS, t.dialect, "Start Waired on login"))
	}
}

func (t *tray) onToggle(ctx context.Context) {
	t.mu.Lock()
	kind := t.last.Kind
	t.mu.Unlock()
	slog.Debug("tray: menu action", "action", "toggle", "kind", int(kind))
	switch kind {
	case MenuConnected:
		if err := t.cli.Pause(ctx); err != nil {
			showError(fmt.Sprintf("Couldn't pause Waired: %v", err))
		}
	case MenuDisconnected:
		if err := t.cli.Resume(ctx); err != nil {
			showError(fmt.Sprintf("Couldn't resume Waired: %v", err))
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
	slog.Debug("tray: menu action", "action", "login-start")
	st, err := t.cli.LoginStart(ctx, management.LoginStartRequest{ControlURL: t.opts.ControlURL})
	if errors.Is(err, ErrLoginUnsupported) {
		slog.Debug("tray: login: daemon lacks login API, using elevation fallback")
		if err := loginViaElevation(elevationCtx(ctx), t.opts.ControlURL, t.elevationStateDir(ctx)); err != nil {
			showError(err.Error())
		}
		return
	}
	if err != nil {
		showError(fmt.Sprintf("Sign-in failed: %v", err))
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
			if oerr := openBrowser(st.LoginURL); oerr != nil {
				showError(fmt.Sprintf("Couldn't open your browser. Open this link:\n%s", st.LoginURL))
			}
		}
	}

	switch st.Phase {
	case management.LoginPhaseActive:
		slog.Debug("tray: login finished", "phase", string(st.Phase))
		t.mu.Lock()
		t.loginSessionID = ""
		t.mu.Unlock()
	case management.LoginPhaseError:
		slog.Debug("tray: login finished", "phase", string(st.Phase))
		msg := st.Error
		if msg == "" {
			msg = "sign-in failed"
		}
		showError("Sign-in failed: " + msg)
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
	slog.Debug("tray: menu action", "action", "inference-toggle", "want", action)
	switch action {
	case labelPauseInference:
		if err := t.cli.DisableInference(ctx); err != nil {
			showError(fmt.Sprintf("Couldn't pause local inference: %v", err))
		}
	case labelResumeInference, labelEnableInference:
		// Same call for both: the two labels differ only in whether this
		// computer has ever run models here (#465). The daemon starts
		// the engine and fetches a model if they are not there yet.
		if err := t.cli.EnableInference(ctx); err != nil {
			showError(fmt.Sprintf("Couldn't turn on local inference: %v", err))
		}
	}
	go t.pollOnce(ctx)
}

// onEngineToggle drives the hard engine power axis (#186): stop frees
// VRAM/RAM, start restarts the engine. Mirrors onInferenceToggle — reads
// the last-rendered action and relies on the post-click pollOnce to
// refresh labels. The action is empty (item hidden) when the engine is
// not managed by waired, or the daemon predates engine control.
func (t *tray) onEngineToggle(ctx context.Context) {
	t.mu.Lock()
	action := t.last.EngineToggleAction
	t.mu.Unlock()
	slog.Debug("tray: menu action", "action", "engine-toggle", "want", action)
	switch action {
	case labelStopEngine:
		if err := t.cli.StopEngine(ctx); err != nil {
			showError(fmt.Sprintf("Couldn't stop the inference engine: %v", err))
		}
	case labelStartEngine:
		if err := t.cli.StartEngine(ctx); err != nil {
			showError(fmt.Sprintf("Couldn't start the inference engine: %v", err))
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
	slog.Debug("tray: menu action", "action", "install-engine")
	if err := installOllamaViaElevation(elevationCtx(ctx), t.elevationStateDir(ctx)); err != nil {
		showError(fmt.Sprintf("Couldn't install Ollama: %v", err))
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
// onStartAgent elevates and starts the agent service, then waits for the
// daemon to actually answer before claiming anything.
//
// The wait is not optional and is the same on all three OSes for different
// reasons: on Windows ShellExecuteW returns as soon as the elevated process is
// spawned, so its nil says nothing about the service (and #315's failure mode
// is precisely a start that the OS blocks); `systemctl start` returns when the
// unit is active, which is before the management socket listens; `launchctl
// kickstart` returns when launchd has spawned the job. Rather than encode
// three timing models, converge on the one observable that matters — /status
// answering.
func (t *tray) onStartAgent(ctx context.Context) {
	if !t.claimStartAgent() {
		return
	}
	defer t.releaseStartAgent()

	slog.Debug("tray: menu action", "action", "start-agent")
	if err := startAgentViaElevation(elevationCtx(ctx)); err != nil {
		t.offerStartCommand(fmt.Sprintf("Couldn't %v", err))
		return
	}
	// Paint "starting…" immediately: the next poll is up to 5 s away, and the
	// service takes a moment to open its socket.
	t.mu.Lock()
	t.startingUntil = time.Now().Add(startGraceAfterClick)
	t.mu.Unlock()
	t.pollOnce(ctx)

	if !t.awaitDaemonUp(ctx, startWaitTimeout) {
		notify("The background service didn't come up. Run `waired doctor` to see why.", notification.Warning)
	}
	t.pollOnce(ctx)
}

// claimStartAgent takes the single start slot, or reports that there is
// nothing to do. Two guards, both re-read from the latched model rather than
// trusted from the widget (the row may have been hidden between the click and
// this goroutine being scheduled):
//
//   - the model has to still be offering the action, and
//   - no other start may be in flight. A double-click would otherwise stack
//     two UAC dialogs on Windows, or two polkit agents on Linux.
func (t *tray) claimStartAgent() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.last.StartAgentAction == "" || t.startInFlight {
		return false
	}
	t.startInFlight = true
	return true
}

func (t *tray) releaseStartAgent() {
	t.mu.Lock()
	t.startInFlight = false
	t.mu.Unlock()
}

// awaitDaemonUp polls /status until it answers or the deadline lapses.
func (t *tray) awaitDaemonUp(ctx context.Context, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := t.cli.Status(probeCtx)
		cancel()
		if err == nil {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Second):
		}
	}
}

// onCopyStartCommand puts the start command on the clipboard for a user who
// would rather run it themselves.
func (t *tray) onCopyStartCommand() {
	t.mu.Lock()
	cmd := t.last.StartAgentCmd
	t.mu.Unlock()
	if cmd == "" {
		return
	}
	slog.Debug("tray: menu action", "action", "copy-start-command")
	if err := copyToClipboard(cmd); err != nil {
		showError(fmt.Sprintf("Couldn't copy the command: %v", err))
		return
	}
	notify("Copied: "+cmd, notification.Info)
}

// offerStartCommand reports a failed start and leaves the user something to
// act on. The clipboard route matters most where the failure is "no consent
// UI available at all" — a Linux session with no polkit agent, where a modal
// error dialog may be just as unavailable as the prompt was.
func (t *tray) offerStartCommand(msg string) {
	t.mu.Lock()
	cmd := t.last.StartAgentCmd
	t.mu.Unlock()
	if cmd != "" && copyToClipboard(cmd) == nil {
		notify(msg+" The command is on your clipboard: "+cmd, notification.Warning)
		return
	}
	showError(msg)
}

func (t *tray) onUpdate(ctx context.Context) {
	t.mu.Lock()
	show := t.last.UpdateAvailable
	ver := t.last.UpdateVersion
	t.mu.Unlock()
	if !show {
		return
	}
	slog.Debug("tray: menu action", "action", "update", "version", ver)
	if ver != "" {
		notify("Updating Waired to "+ver+"…", notification.Info)
	} else {
		notify("Updating Waired…", notification.Info)
	}
	if err := updateViaElevation(elevationCtx(ctx)); err != nil {
		showError(fmt.Sprintf("Couldn't update Waired: %v", err))
		return
	}
	// The installer restarts the daemon as part of the swap; the next poll
	// repaints the version, and the daemon stops publishing the notice
	// once it's current.
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
			slog.Debug("tray: update endpoints unavailable; skipping henceforth")
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
const (
	// startGraceAfterClick keeps the menu in "starting…" right after the user
	// asked for a start, so the row does not flip back to the red failure
	// state for the second or two the service needs to open its socket.
	startGraceAfterClick = 30 * time.Second
	// startWaitTimeout bounds awaitDaemonUp. Generous: an agent that has to
	// re-read a large state dir, or a Windows service the SCM starts cold,
	// can take a while — and reporting failure early is worse than waiting.
	startWaitTimeout = 45 * time.Second
)

// startGraceFor is how long after tray start an unreachable daemon is treated
// as "still coming up" rather than broken.
//
// Windows registers the service delayed-auto-start (service_windows.go), which
// the SCM honours by waiting ~2 minutes after boot before it even attempts the
// start — while the tray autostarts at logon and starts polling immediately.
// The rc7 reviewer hit exactly this and read it as a failure (#315, root cause
// 2). systemd (WantedBy=multi-user.target) and launchd (RunAtLoad) both start
// the agent as part of boot, so their window is only as long as the daemon
// takes to open its socket.
//
// Untagged and GOOS-taking so all three values are pinned by one table test on
// every leg (CLAUDE.md §Test discipline).
func startGraceFor(goos string) time.Duration {
	if goos == "windows" {
		return 3 * time.Minute
	}
	return 20 * time.Second
}

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
		notify("Waired "+st.LatestVersion+" is available. Open the menu to update.", notification.Info)
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
	slog.Debug("tray: menu action", "action", "update-notify-toggle", "enabled", enabled)
	if _, err := t.cli.UpdateSettings(ctx, !enabled); err != nil {
		showError(fmt.Sprintf("Couldn't change the update notification setting: %v", err))
		return
	}
	go t.pollOnce(ctx)
}

// onShareToggle flips whether this computer lends itself out at all
// (waired#1297), via /waired/v1/sharing. Same pattern as
// onInferenceToggle. No confirmation dialog: the action is reversible in
// one click, and the same one Quit performs.
func (t *tray) onShareToggle(ctx context.Context) {
	t.mu.Lock()
	action := t.last.ShareToggleAction
	t.mu.Unlock()
	slog.Debug("tray: menu action", "action", "share-toggle", "want", action)
	switch action {
	case labelStopSharing:
		if err := t.cli.DisableShare(ctx); err != nil {
			showError(fmt.Sprintf("Couldn't stop sharing: %v", err))
		}
	case labelStartSharing:
		if err := t.cli.EnableShare(ctx); err != nil {
			showError(fmt.Sprintf("Couldn't turn sharing on: %v", err))
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

// publicReciprocityNote is shown after a first accept. Public sharing of
// your own computer is what makes you eligible to use anyone else's
// (spec §4.2), and it is turned on in the console.
const publicReciprocityNote = "To use other people's computers you must share one of yours. Turn on public sharing for a computer in the Waired console."

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
	slog.Debug("tray: menu action", "action", "public-use-mode", "mode", mode, "consented", consented)

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
		showError(fmt.Sprintf("Public computers: %v", err))
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
		showError(fmt.Sprintf("Public computers: %v", err))
		return false
	}
	yes, ok := confirmWithLabels(w.Title, w.Text, w.AcceptLabel, w.CancelLabel)
	if !ok {
		// No dialog backend: consent MUST NOT be recorded without showing
		// the text, so hand off to the CLI (which prints the warning).
		if err := copyToClipboard("waired public use --auto"); err != nil {
			showError("Public use: " + err.Error())
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
			showError(fmt.Sprintf("Public computers: %v", err))
			return false
		}
		// The served text changed between display and accept: re-fetch,
		// re-display exactly once, then give up if it still mismatches.
		w2, werr := t.cli.PublicWarning(ctx)
		if werr != nil {
			showError(fmt.Sprintf("Public computers: %v", werr))
			return false
		}
		yes2, ok2 := confirmWithLabels(w2.Title, w2.Text, w2.AcceptLabel, w2.CancelLabel)
		if !ok2 || !yes2 {
			return false
		}
		if _, rerr := t.cli.AcceptPublicConsent(ctx, w2.Version); rerr != nil {
			showError(fmt.Sprintf("Public computers: %v", rerr))
			return false
		}
	}
	// Reciprocity: using other people's computers requires sharing one of
	// yours (spec §4.2). This used to turn public sharing on from here;
	// the setting is the console's since waired#1297, so say where it is
	// instead. Consent is already recorded either way.
	notify(publicReciprocityNote, notification.Info)
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
	slog.Debug("tray: menu action", "action", "public-more")
	if err := openBrowser(url); err != nil {
		showError(err.Error())
	}
}

func (t *tray) onCopyIP() {
	t.mu.Lock()
	ip := t.last.OverlayIP
	t.mu.Unlock()
	if ip == "" {
		return
	}
	// Log the action, not the overlay IP value.
	slog.Debug("tray: menu action", "action", "copy-ip")
	if err := copyToClipboard(ip); err != nil {
		showError(err.Error())
	}
}

// onAccount opens the console's account page for the signed-in account.
//
// Silent when there is no URL: that is the daemon-down menu, where the
// email is the last one seen rather than a live identity, and the row is
// naming who you were rather than offering to open anything.
func (t *tray) onAccount() {
	t.mu.Lock()
	url := t.last.AccountURL
	t.mu.Unlock()
	if url == "" {
		return
	}
	// Log the action, not the URL (it carries a network identifier).
	slog.Debug("tray: menu action", "action", "open-account")
	if err := openBrowser(url); err != nil {
		showError(err.Error())
	}
}

func (t *tray) onAdmin() {
	t.mu.Lock()
	url := t.last.AdminURL
	t.mu.Unlock()
	if url == "" {
		showError("The console address isn't known yet. Sign in first.")
		return
	}
	// Log the action, not the admin URL (may carry a network identifier).
	slog.Debug("tray: menu action", "action", "open-admin")
	if err := openBrowser(url); err != nil {
		showError(err.Error())
	}
}

// onReconfigureOpenCode walks the user through re-applying the OpenCode
// integration: confirm, then run `waired link opencode` as this user
// (which rewrites ~/.config/opencode/plugin/waired.js and its command
// files to point at the current waired gateway).
// Long-running; callers must dispatch in a goroutine.
func (t *tray) onReconfigureOpenCode(ctx context.Context) {
	t.reconfigureIntegration(ctx, "opencode", "OpenCode",
		"Reconfigure OpenCode integration?",
		"This rewrites the Waired OpenCode plugin "+
			"(~/.config/opencode/plugin/waired.js) to point at the current "+
			"Waired gateway. Continue?")
}

// onReconfigureOpenClaw is the OpenClaw counterpart of
// onReconfigureOpenCode.
func (t *tray) onReconfigureOpenClaw(ctx context.Context) {
	t.reconfigureIntegration(ctx, "openclaw", "OpenClaw",
		"Reconfigure OpenClaw integration?",
		"This rewrites the Waired OpenClaw plugin "+
			"(~/.openclaw/plugins/waired/) and refreshes its openclaw.json keys to "+
			"point at the current Waired gateway. Continue?")
}

// reconfigureIntegration is the shared body of both Reconfigure clicks:
// confirm, run the CLI as this user, re-poll so the row updates.
//
// The CLI does the writing, not the daemon: these files live in the
// desktop user's home, and this process is the one running as that user
// (waired-agent#986, waired#935). Without a dialog the command goes to
// the clipboard instead — the same string the row's tooltip names.
func (t *tray) reconfigureIntegration(ctx context.Context, target, product, title, body string) {
	command := "waired link " + target

	yes, ok := confirmYesNo(title, body)
	if !ok {
		if err := copyToClipboard(command); err != nil {
			showError("Reconfigure: " + err.Error())
			return
		}
		notify("Run `"+command+"` in a terminal to reconfigure.", notification.Info)
		return
	}
	if !yes {
		return
	}

	slog.Debug("tray: menu action", "action", "reconfigure-"+target)
	if err := linkIntegrationAsUser(ctx, target); err != nil {
		notify(product+" Couldn't reconfigure: "+err.Error(), notification.Warning)
		showError(product + " reconfigure: " + err.Error())
		return
	}
	notify(product+" integration reconfigured.", notification.Info)
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

// dispatchNoticeClicks routes clicks on one notice row
// (waired-agent#1205).
//
// A model suggestion opens the accept/decline dialog it always did.
// Everything else — a notice whose action this build does not know, and
// a suggestion whose live details the catalog poll has not got — opens
// the status report, because the notice and the recommendation arrive on
// two independent best-effort polls, and a row that silently does
// nothing when clicked is the worst thing a menu can do.
func (t *tray) dispatchNoticeClicks(ctx context.Context, idx int) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.miNotices[idx].ClickedCh:
			t.mu.Lock()
			var row NoticeRow
			if idx < len(t.lastNotices) {
				row = t.lastNotices[idx]
			}
			haveRec := t.lastRecommendation != nil
			haveUpdate := t.last.UpdateAvailable
			t.mu.Unlock()

			switch noticeClickTarget(row.Action, haveRec, haveUpdate) {
			case noticeClickRecommendation:
				go t.onShowRecommendationPopup(ctx)
			case noticeClickUpdate:
				go t.onUpdate(ctx)
			default:
				go t.onShowStatus()
			}
		}
	}
}

// noticeClickTarget is what a click on a notice row opens.
//
// A pure function of the action and what the tray currently holds,
// because the interesting case is a disagreement between them: the
// notices poll and the polls that fill the local state are independent
// best-effort GETs with independent nil states, so a live notice with no
// live recommendation (or no update status) is reachable and is exactly
// the case a click must still answer. A row that does nothing at all is
// the worst thing a menu can do, so the fallback is the status report,
// which explains the state the row is about.
func noticeClickTarget(a notice.Action, haveRec, haveUpdate bool) noticeClick {
	switch {
	case a == notice.ActionModelSuggestion && haveRec:
		return noticeClickRecommendation
	case a == notice.ActionInstallUpdate && haveUpdate:
		// The same elevated update the banner ran before this row
		// replaced it (waired-agent#1229).
		return noticeClickUpdate
	}
	return noticeClickStatusReport
}

type noticeClick int

const (
	noticeClickStatusReport noticeClick = iota
	noticeClickRecommendation
	noticeClickUpdate
)

// onShowRecommendationPopup presents the lighter-model suggestion in a
// native yes/no dialog. Yes posts the preferred-model switch; No records
// a dismissal so the same pairing does not nag again. When no desktop
// dialog backend is available it falls back to copying the CLI command
// to the clipboard. Long-running (dialog wait) — callers dispatch in a
// goroutine.
//
// The question has to describe what saying yes costs. Since waired#812
// the switch applies in process, so the old "the agent will restart"
// was overstating it; what the upgrade arm does cost is the download,
// which the downgrade arm does not (the lighter model is the one this
// host can already serve).
func (t *tray) onShowRecommendationPopup(ctx context.Context) {
	t.mu.Lock()
	rec := t.lastRecommendation
	t.mu.Unlock()
	if rec == nil || rec.ToModelID == "" {
		return
	}

	title := "Local inference is slow"
	body := fmt.Sprintf(
		"This computer benchmarked at %.0f tok/s, below the %.0f tok/s needed for interactive use.\n\n"+
			"Switch to the lighter model %s? It applies live. Waired keeps answering.",
		rec.MeasuredTokps, rec.FloorTokps, rec.ToModelID)
	if rec.Direction == management.RecommendationUpgrade {
		title = "Better model available"
		body = fmt.Sprintf(
			"This computer benchmarked at %.0f tok/s, enough headroom for a stronger model.\n\n"+
				"Switch to %s (about %.0f tok/s predicted)? It downloads first, and your current model "+
				"keeps answering until it is ready.",
			rec.MeasuredTokps, rec.ToModelID, rec.PredictedTokps)
	}

	yes, ok := confirmYesNo(title, body)
	if !ok {
		// No desktop dialog backend — fall back to the CLI command.
		if err := copyToClipboard("waired runtimes benchmark"); err != nil {
			showError("Recommendation: " + err.Error())
			return
		}
		notify("Run `waired runtimes benchmark` in a terminal to switch models.", notification.Info)
		return
	}
	if !yes {
		if err := t.cli.DismissRecommendation(ctx, rec.FromVariantID, rec.ToVariantID); err != nil &&
			!errors.Is(err, ErrCatalogUnsupported) {
			showError("Couldn't dismiss the recommendation: " + err.Error())
			return
		}
		go t.pollOnce(ctx)
		return
	}
	resp, err := t.cli.SetPreferredModel(ctx, rec.ToModelID)
	if err != nil {
		showError(modelSwitchErrorText(err, switchModelName("", rec.ToModelID)))
		return
	}
	// The recommendation carries a model_id, not a catalog display name,
	// so this arm names the model the same way the dialog just did.
	t.onModelSwitchAccepted(resp, rec.ToModelID)
	go t.pollOnce(ctx)
}

// onLogout signs this device out: the daemon does it, and only an agent too
// old to offer the route falls back to the elevated CLI.
//
// Daemon-first is the same shape startLogin already uses for sign-in, and for
// the same reason — the daemon owns the state dir and the running session, so
// it is the only process that can stop what is writing to those files before
// they go away. An elevated `waired logout` deletes them from underneath a
// daemon that never notices, which is why the app went on showing "Connected"
// after a sign-out that looked like it worked (waired-agent#1269).
//
// The confirmation stays. Sign-out is destructive in the sense that matters to
// a person — signing back in enrols this computer as a new device — and that
// is worth one question whether or not an authorization prompt follows it.
func (t *tray) onLogout(ctx context.Context) {
	if !showConfirm("Sign this computer out of Waired?\nThis computer leaves your network. Signing back in adds it as a new device.") {
		return
	}
	slog.Debug("tray: menu action", "action", "logout")
	go func() {
		resp, err := t.cli.Logout(ctx, management.LogoutRequest{})
		switch {
		case err == nil:
			// The daemon may have cleared this machine without reaching the
			// control plane. Local state is gone either way, so this is a
			// warning about the OTHER side rather than a failed sign-out —
			// and the app had no way to say it before.
			if resp != nil && resp.DeauthError != "" {
				showError("Signed out on this computer, but Waired couldn't tell the " +
					"control plane (" + resp.DeauthError + "). If this device still appears " +
					"in your account, remove it in the Waired console.")
			}
			// Any login this app was tracking is over; leaving the id set
			// would let the next poll fold a stale session back onto the menu.
			t.mu.Lock()
			t.loginSessionID = ""
			t.loginURLOpened = false
			t.mu.Unlock()
		case errors.Is(err, ErrSignInInFlight):
			showError("A sign-in is in progress. Wait for it to finish, then sign out.")
		case errors.Is(err, ErrLogoutUnsupported), isDaemonUnreachable(err):
			// An agent that predates the route, or one that is not answering.
			// The legacy path: an elevated `waired logout` against the state
			// dir the daemon is enrolled in.
			if err := logoutViaElevation(elevationCtx(ctx), t.elevationStateDir(ctx)); err != nil {
				showError(err.Error())
			}
		default:
			showError("Couldn't sign out: " + err.Error())
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
		now := time.Now()
		switching := now.Before(t.switchingUntil)
		lastOnline := t.lastOnline
		facts := daemonDownFacts{
			ServiceInstalled: t.serviceRegistered,
			Starting:         now.Before(t.startingUntil),
			LastEmail:        lastOnline.AccountEmail,
		}
		// The daemon answered nothing this round, so the status report
		// must not keep quoting the last poll that worked as if it were
		// current. HealthOffline is what it has to say.
		t.lastSnap = Snapshot{Health: HealthOffline, Now: now}
		t.mu.Unlock()
		slog.Debug("tray: poll: daemon unreachable",
			"err", statusErr, "switching", switching, "starting", facts.Starting)
		t.apply(offlineModel(lastOnline, switching, facts))
		return
	}
	snap.Health = HealthOnline
	snap.Status = st
	// The daemon answered, so whatever start grace was running is over.
	t.mu.Lock()
	t.startingUntil = time.Time{}
	t.mu.Unlock()

	id, idErr := t.cli.Identity(pollCtx)
	if idErr == nil {
		snap.Identity = id
	} else {
		// A real transport failure, not "the daemon says nobody is enrolled":
		// Client.Identity folds 404 into {Enrolled:false} (mgmt.go). Rendering
		// this as "not signed in" is what pushed a reviewer into a re-login
		// that re-ran setup, on a device whose identity was sitting on disk
		// with months of validity (#317 / #318).
		snap.IdentityErr = true
		slog.Debug("tray: poll: identity unavailable", "err", idErr)
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
	// OpenCode / OpenClaw: the daemon answers where its data-plane
	// gateway is, and this process — which runs as the desktop user the
	// plugin belongs to — reads the plugin file itself (waired-agent#986).
	// 404 on older daemons leaves the field nil and the tray hides the
	// group; so does an unresolvable home.
	home := trayHomeFn()
	if oc, ocErr := t.cli.OpenCodeIntegration(pollCtx); ocErr == nil {
		snap.OpenCode = probeOpenCode(home, oc.ExpectedBaseURL)
	}
	if ow, owErr := t.cli.OpenClawIntegration(pollCtx); owErr == nil {
		snap.OpenClaw = probeOpenClaw(home, ow.ExpectedBaseURL)
	}
	// Catalog: best-effort with 404 → ErrCatalogUnsupported sentinel,
	// leaving snap.Catalog nil so the menu hides the submenu entirely.
	if cat, catErr := t.cli.ModelCatalog(pollCtx); catErr == nil {
		snap.Catalog = cat
		t.maybeShowRecommendation(ctx, liveRecommendation(cat))
	}
	// Notices: best-effort with 404 → ErrNoticesUnsupported, leaving
	// snap.Notices nil so an older daemon renders the menu it always did
	// (waired-agent#1205).
	if ns, nErr := t.cli.Notices(pollCtx); nErr == nil {
		snap.Notices = ns
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
	// Sharing (waired#1297) and public use (waired#833): independent
	// best-effort GETs. Each 404 is swallowed on its own (via the
	// ErrShareUnsupported / ErrPublicUse* sentinels) so the halves gate
	// independently — a daemon exposing only one still renders that half.
	if sh, err := t.cli.Sharing(pollCtx); err == nil {
		snap.Sharing = sh
	}
	if pu, err := t.cli.PublicUse(pollCtx); err == nil {
		snap.PublicUse = pu
	}
	if pw, err := t.cli.PublicWarning(pollCtx); err == nil {
		snap.PublicWarning = pw
	}
	snap.Now = time.Now()
	m := Update(snap)
	// One concise line per online poll: what the fan-out saw (which
	// best-effort endpoints were present) and the resulting header. No
	// identity/PII values — only presence booleans and the rendered state.
	slog.Debug("tray: poll ok",
		"enrolled", snap.Identity != nil && snap.Identity.Enrolled,
		"inference", snap.Inference != nil,
		"claude", snap.Claude != nil,
		"routing", snap.ClaudeRouting != nil,
		"opencode", snap.OpenCode != nil,
		"openclaw", snap.OpenClaw != nil,
		"catalog", snap.Catalog != nil,
		"mesh", snap.Mesh != nil,
		"menu", m.HeaderTitle)
	// The daemon is reachable again: remember this model for the switch
	// grace window and close any open window so a later genuine crash is
	// not masked as "Switching model…" (waired#808).
	t.mu.Lock()
	t.lastOnline = m
	t.lastSnap = snap
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

	// Log only the transitions (icon / kind / header changed) so a steady
	// state is silent while every state flip is recorded. kind/icon are the
	// raw enum values; header is the human-readable label.
	if prev.Kind != m.Kind || prev.Icon != m.Icon || prev.HeaderTitle != m.HeaderTitle {
		slog.Debug("tray: menu transition",
			"from", prev.HeaderTitle, "to", m.HeaderTitle,
			"kind", int(m.Kind), "icon", int(m.Icon))
	}

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
	systray.SetTooltip(trayTooltip(m))

	t.applyRows(prev, m)
}

// paintCreationBaseline asserts the initial menu state, and is the reason no
// item needs a hand-written Hide() at creation any more.
//
// systray creates every item visible, and apply() diffs model-to-model — so a
// row whose zero-model visibility is false was never hidden by anything: the
// first paint's (false,false) diff is a no-op and the row sat there blank,
// forever. That was the two blank rows plus the stray "Sign out…" /
// "Open Admin Console…" in the daemon-down screenshot (#317), and the same
// class waired#808 fixed one row at a time.
//
// Painting the zero model with the diff forced derives the creation state from
// the same predicates apply() uses, so the two cannot drift.
func (t *tray) paintCreationBaseline() {
	t.applyMu.Lock()
	defer t.applyMu.Unlock()
	t.beginRowPass(true)
	defer func() { t.rowForce = false }()
	t.diffRows(MenuModel{}, MenuModel{})
}

// applyRows runs one guarded diff pass. Split out of apply() so the creation
// baseline can reuse the predicates without also re-running the icon, tooltip,
// transition log and debug dump that belong to a real state change.
func (t *tray) applyRows(prev, m MenuModel) {
	t.applyMu.Lock()
	defer t.applyMu.Unlock()
	t.beginRowPass(false)
	t.diffRows(prev, m)
}

func (t *tray) diffRows(prev, m MenuModel) {
	t.setTitle(t.miHeader, prev.HeaderTitle, m.HeaderTitle)
	t.setVisible(t.miEmail, prev.AccountEmail != "", m.AccountEmail != "")
	t.setTitle(t.miEmail, prev.AccountEmail, m.AccountEmail)
	// Status/hint line (waired#808): the daemon-down "Start-Service…" hint,
	// the login user-code, or an error reason. Previously computed into
	// MenuModel.StatusMsg but never rendered.
	t.setVisible(t.miStatus, prev.StatusMsg != "", m.StatusMsg != "")
	t.setTitle(t.miStatus, prev.StatusMsg, m.StatusMsg)
	// Agent-start rows: visibility only. Their titles are static (set at
	// creation), so nothing here can push a title at a hidden row.
	t.setVisible(t.miStartAgent, prev.StartAgentAction != "", m.StartAgentAction != "")
	t.setVisible(t.miStartAgentCopy, prev.StartAgentCopy != "", m.StartAgentCopy != "")

	t.setVisible(t.miUpdateNotify, prev.UpdateNotifyAction != "", m.UpdateNotifyAction != "")
	t.setTitle(t.miUpdateNotify, prev.UpdateNotifyAction, m.UpdateNotifyAction)

	// Daemon-published notices (waired-agent#1205), latched so a click
	// on slot i can resolve what that slot was showing.
	t.applyNoticeRows(prev.Notices, m.Notices)
	t.mu.Lock()
	t.lastNotices = m.Notices
	t.mu.Unlock()

	// Toggle item: title + visibility track ToggleAction.
	t.setVisible(t.miToggle, prev.ToggleAction != "", m.ToggleAction != "")
	t.setTitle(t.miToggle, prev.ToggleAction, m.ToggleAction)

	// Inference group: the "Inference" submenu parent plus its rows —
	// toggle + engine state + share toggle + share state + active model +
	// worker (waired#809). The parent shows when the daemon exposes the
	// inference API (ShowInferenceMenu); each inner item still tracks its
	// own field, so an empty row inside the open submenu stays hidden.
	t.setVisible(t.miInference, prev.ShowInferenceMenu, m.ShowInferenceMenu)
	t.setVisible(t.miInferenceToggle, prev.InferenceToggleAction != "", m.InferenceToggleAction != "")
	t.setTitle(t.miInferenceToggle, prev.InferenceToggleAction, m.InferenceToggleAction)
	t.setVisible(t.miInferenceState, prev.InferenceStateLabel != "", m.InferenceStateLabel != "")
	t.setTitle(t.miInferenceState, prev.InferenceStateLabel, m.InferenceStateLabel)
	// Hard engine power toggle (#186): visibility + title track
	// EngineToggleAction; enablement tracks EngineToggleEnabled (the
	// not-managed case renders the row greyed out).
	t.setVisible(t.miEngineToggle, prev.EngineToggleAction != "", m.EngineToggleAction != "")
	t.setTitle(t.miEngineToggle, prev.EngineToggleAction, m.EngineToggleAction)
	t.setEnabled(t.miEngineToggle, prev.EngineToggleEnabled, m.EngineToggleEnabled)
	// "Install Ollama…" — shown only on no_engine (#188).
	t.setVisible(t.miInstallEngine, prev.InstallEngineAction != "", m.InstallEngineAction != "")
	t.setTitle(t.miInstallEngine, prev.InstallEngineAction, m.InstallEngineAction)
	// Share-with-mesh items (Phase 6). Pre-allocated regardless of
	// daemon support; visibility tracks the MenuModel fields which
	// applyInference leaves empty when the daemon predates the API.
	t.setVisible(t.miShareToggle, prev.ShareToggleAction != "", m.ShareToggleAction != "")
	t.setTitle(t.miShareToggle, prev.ShareToggleAction, m.ShareToggleAction)
	t.setVisible(t.miShareState, prev.ShareStateLabel != "", m.ShareStateLabel != "")
	t.setTitle(t.miShareState, prev.ShareStateLabel, m.ShareStateLabel)
	t.setVisible(t.miEngineWarning, prev.EngineWarningLabel != "", m.EngineWarningLabel != "")
	t.setTitle(t.miEngineWarning, prev.EngineWarningLabel, m.EngineWarningLabel)
	// miActiveModel ("Model: <model_id>") is suppressed when the catalog
	// submenu is showing — the top-level Engine row renders the same intent
	// with the friendlier display_name, and one row per concept is enough.
	prevActiveModelVisible := prev.ActiveModelLabel != "" && !prev.ShowCatalog
	activeModelVisible := m.ActiveModelLabel != "" && !m.ShowCatalog
	t.setVisible(t.miActiveModel, prevActiveModelVisible, activeModelVisible)
	t.setTitle(t.miActiveModel, prev.ActiveModelLabel, m.ActiveModelLabel)

	// Model residency (waired-agent#861). The whole group follows the
	// daemon reporting the setting at all, so a tray against an older
	// daemon renders none of it.
	t.setVisible(t.miUnloadModel, prev.UnloadModelAction != "", m.UnloadModelAction != "")
	t.setTitle(t.miUnloadModel, prev.UnloadModelAction, m.UnloadModelAction)
	t.setEnabled(t.miUnloadModel, prev.UnloadModelEnabled, m.UnloadModelEnabled)
	t.setVisible(t.miResidencyHeader, prev.ResidencyHeader != "", m.ResidencyHeader != "")
	t.setTitle(t.miResidencyHeader, prev.ResidencyHeader, m.ResidencyHeader)
	t.applyResidencyRows(prev.ResidencyRows, m.ResidencyRows)

	// Top-level status block. Each row is independent: a daemon that
	// exposes the inference API but not the mesh one renders the Engine row
	// and no Peers row, rather than an empty block or a lying one.
	t.setVisible(t.miStatusEngine, prev.StatusEngineLabel != "", m.StatusEngineLabel != "")
	t.setTitle(t.miStatusEngine, prev.StatusEngineLabel, m.StatusEngineLabel)
	t.setVisible(t.miStatusPeers, prev.StatusPeersLabel != "", m.StatusPeersLabel != "")
	t.setTitle(t.miStatusPeers, prev.StatusPeersLabel, m.StatusPeersLabel)
	t.setVisible(t.miStatusClaude, prev.StatusClaudeLabel != "", m.StatusClaudeLabel != "")
	t.setTitle(t.miStatusClaude, prev.StatusClaudeLabel, m.StatusClaudeLabel)

	// Catalog group: the "Models" submenu (the leading separator
	// auto-collapses when ShowCatalog is false).
	t.setVisible(t.miCatalog, prev.ShowCatalog, m.ShowCatalog)
	parentLabel := m.CatalogParentLabel
	if parentLabel == "" {
		parentLabel = "Models"
	}
	t.setTitle(t.miCatalog, prev.CatalogParentLabel, parentLabel)
	t.setVisible(t.miCatalogNote, prev.CatalogNoteLabel != "", m.CatalogNoteLabel != "")
	t.setTitle(t.miCatalogNote, prev.CatalogNoteLabel, m.CatalogNoteLabel)
	t.setVisible(t.miCatalogNoteSep, prev.CatalogNoteLabel != "", m.CatalogNoteLabel != "")
	t.applyCatalogEntries(prev.CatalogEntries, m.CatalogEntries)
	t.mu.Lock()
	t.lastCatalogEntries = m.CatalogEntries
	t.mu.Unlock()

	// "Inference routing" submenu (#327). The parent follows
	// ShowRoutingMenu — true when either group below it has content — so
	// a daemon predating the worker API but exposing the mesh still gets
	// a parent worth opening, and one predating both renders none.
	// Inside it: the "Worker: …" summary and the mesh-reachable row, then
	// the two labelled groups (automatic modes, then per-peer pins).
	t.setVisible(t.miRouting, prev.ShowRoutingMenu, m.ShowRoutingMenu)
	routingParent := m.WorkerParentLabel
	if routingParent == "" {
		routingParent = "Inference routing"
	}
	t.setTitle(t.miRouting, prev.WorkerParentLabel, routingParent)
	t.setVisible(t.miWorkerActive, prev.ShowWorker, m.ShowWorker)
	t.setTitle(t.miWorkerActive, prev.WorkerActiveLabel, m.WorkerActiveLabel)
	// Mesh-reachable indicator (#212): display-only, moved here from the
	// engine submenu because it answers "can routing use a peer at all?".
	t.setVisible(t.miMeshReachable, prev.MeshReachableLabel != "", m.MeshReachableLabel != "")
	t.setTitle(t.miMeshReachable, prev.MeshReachableLabel, m.MeshReachableLabel)
	t.setVisible(t.miWorkerModesHeader, prev.WorkerModesHeader != "", m.WorkerModesHeader != "")
	t.setTitle(t.miWorkerModesHeader, prev.WorkerModesHeader, m.WorkerModesHeader)
	t.applyWorkerModes(prev.WorkerModes, m.WorkerModes)
	t.setVisible(t.miWorkerPreferHeader, prev.WorkerPreferHeader != "", m.WorkerPreferHeader != "")
	t.setTitle(t.miWorkerPreferHeader, prev.WorkerPreferHeader, m.WorkerPreferHeader)
	t.applyWorkerPrefers(prev.WorkerPrefers, m.WorkerPrefers)
	t.setVisible(t.miWorkerMinSizeHdr, prev.WorkerMinSizeHeader != "", m.WorkerMinSizeHeader != "")
	t.setTitle(t.miWorkerMinSizeHdr, prev.WorkerMinSizeHeader, m.WorkerMinSizeHeader)
	t.applyWorkerMinSizes(prev.WorkerMinSizes, m.WorkerMinSizes)
	t.setVisible(t.miWorkerPinsHeader, prev.WorkerPinsHeader != "", m.WorkerPinsHeader != "")
	t.setTitle(t.miWorkerPinsHeader, prev.WorkerPinsHeader, m.WorkerPinsHeader)
	t.applyWorkerPins(prev.WorkerPinEntries, m.WorkerPinEntries)
	t.setVisible(t.miWorkerClearPin, prev.WorkerShowClearPin, m.WorkerShowClearPin)
	t.mu.Lock()
	t.lastResidencyRows = m.ResidencyRows
	t.lastWorkerModes = m.WorkerModes
	t.lastWorkerPrefers = m.WorkerPrefers
	t.lastWorkerMinSizes = m.WorkerMinSizes
	t.lastWorkerPinEntries = m.WorkerPinEntries
	t.mu.Unlock()

	// Public share submenu (waired#833): the top-level parent follows
	// ShowPublicShareMenu; the toggle / state / note / use-header / more
	// rows track their own MenuModel fields, and the three mode rows diff
	// via applyPublicUseModes. lastPublicUseModes is latched for the
	// mode-row click dispatch, mirroring the worker rows above.
	t.setVisible(t.miPublicShare, prev.ShowPublicShareMenu, m.ShowPublicShareMenu)
	t.setVisible(t.miPublicUseHeader, prev.PublicUseHeaderLabel != "", m.PublicUseHeaderLabel != "")
	t.setTitle(t.miPublicUseHeader, prev.PublicUseHeaderLabel, m.PublicUseHeaderLabel)
	t.applyPublicUseModes(prev.PublicUseModes, m.PublicUseModes)
	t.setVisible(t.miPublicMore, prev.PublicMoreURL != "", m.PublicMoreURL != "")
	t.mu.Lock()
	t.lastPublicUseModes = m.PublicUseModes
	t.mu.Unlock()

	// "This device" group is shown only when enrolled (i.e. we have a name or IP).
	hasDevice := m.DeviceName != "" || m.OverlayIP != ""
	prevHasDevice := prev.DeviceName != "" || prev.OverlayIP != ""
	for _, mi := range []*systray.MenuItem{t.miDeviceLabel, t.miDeviceName, t.miOverlayIP, t.miNetwork} {
		t.setVisible(mi, prevHasDevice, hasDevice)
	}
	// Peers row (waired#808): gate on peer presence, not just enrollment.
	// Sharing hasDevice's visibility left a blank "Peers" chevron row in
	// the steady peerless state (miPeers is allocated with an empty title,
	// and the first-paint "Peers: 0" == "Peers: 0" diff is a no-op).
	t.setVisible(t.miPeers, peersRowVisible(prev), peersRowVisible(m))
	// Both sides of these three diffs go through the same formatting. They
	// used to compare a raw prev against a formatted next ("  "+name,
	// fmtNetwork(name)), which never matched — so every poll pushed a SetTitle
	// at rows the model had not changed, and (before the row guard above) at
	// rows it had just hidden (#317).
	t.setTitle(t.miDeviceName, indentLabel(prev.DeviceName), indentLabel(m.DeviceName))
	t.setTitle(t.miOverlayIP, indentLabel(prev.OverlayIP), indentLabel(m.OverlayIP))
	t.setTitle(t.miNetwork, fmtNetwork(prev.NetworkName), fmtNetwork(m.NetworkName))
	// Phase 7 follow-up (C1b): when at least one peer has Hardware
	// the "Peers" item gets the submenu form ("Peers (N)") and the
	// child rows render the per-peer GPU labels; otherwise the
	// pre-Phase-7 bare label ("Peers: N") stays.
	t.applyPeersLabel(prev, m)
	t.applyPeerRowEntries(prev.PeerRowEntries, m.PeerRowEntries)
	t.applyPeerRowOverflow(prev.PeerRowOverflow, m.PeerRowOverflow)

	t.setVisible(t.miAdmin, prev.AdminURL != "", m.AdminURL != "")
	t.setVisible(t.miLogout, prev.AccountEmail != "", m.AccountEmail != "")

	// Claude Code submenu parent (waired#809): visible when the daemon
	// exposes *either* the Claude status endpoint (ShowClaude) or the
	// per-class routing endpoint (ShowClaudeCode), since both the status
	// rows and the route selectors now live inside this one parent. Old
	// daemons exposing neither render no "Claude Code" entry at all.
	prevShowClaudeParent := prev.ShowClaude || prev.ShowClaudeCode
	showClaudeParent := m.ShowClaude || m.ShowClaudeCode
	t.setVisible(t.miClaudeCode, prevShowClaudeParent, showClaudeParent)

	// Claude status rows (now children of the parent above). The header
	// reports live serving state; the proxy row reports the OS-level
	// managed-settings status. ProxyLabel="" hides that row.
	t.setVisible(t.miClaudeHeader, prev.ShowClaude, m.ShowClaude)
	t.setTitle(t.miClaudeHeader, prev.ClaudeHeader, m.ClaudeHeader)
	t.setVisible(t.miClaudeProxy, prev.ClaudeProxyLabel != "", m.ClaudeProxyLabel != "")
	t.setTitle(t.miClaudeProxy, "  "+prev.ClaudeProxyLabel, "  "+m.ClaudeProxyLabel)

	// Claude Code per-class routing rows (#649/#650): the two header rows
	// follow ShowClaudeCode; the route slots + conditional notes diff via
	// applyClaudeRoutes / setTitle.
	claudeCodeParent := m.ClaudeCodeParent
	if claudeCodeParent == "" {
		claudeCodeParent = "Claude Code"
	}
	t.setTitle(t.miClaudeCode, prev.ClaudeCodeParent, claudeCodeParent)
	t.setVisible(t.miClaudeEnableNote, prev.ClaudeEnableNote != "", m.ClaudeEnableNote != "")
	t.setTitle(t.miClaudeEnableNote, "  "+prev.ClaudeEnableNote, "  "+m.ClaudeEnableNote)

	// OpenCode integration group — same lifecycle as Claude. Header +
	// Config + Reconfigure share the ShowOpenCode flag; its leading
	// separator auto-collapses if the group above is hidden, so two
	// adjacent rules never render.
	t.setVisible(t.miOpenCodeHeader, prev.ShowOpenCode, m.ShowOpenCode)
	t.setTitle(t.miOpenCodeHeader, prev.OpenCodeHeader, m.OpenCodeHeader)
	t.setVisible(t.miOpenCodeConfig, prev.OpenCodeConfigLabel != "", m.OpenCodeConfigLabel != "")
	t.setTitle(t.miOpenCodeConfig, "  "+prev.OpenCodeConfigLabel, "  "+m.OpenCodeConfigLabel)
	t.setVisible(t.miOpenCodeReconfigure, prev.OpenCodeReconfigureLabel != "", m.OpenCodeReconfigureLabel != "")
	t.setTitle(t.miOpenCodeReconfigure, prev.OpenCodeReconfigureLabel, m.OpenCodeReconfigureLabel)

	// OpenClaw integration group — same lifecycle as the OpenCode group.
	t.setVisible(t.miOpenClawHeader, prev.ShowOpenClaw, m.ShowOpenClaw)
	t.setTitle(t.miOpenClawHeader, prev.OpenClawHeader, m.OpenClawHeader)
	t.setVisible(t.miOpenClawConfig, prev.OpenClawConfigLabel != "", m.OpenClawConfigLabel != "")
	t.setTitle(t.miOpenClawConfig, "  "+prev.OpenClawConfigLabel, "  "+m.OpenClawConfigLabel)
	t.setVisible(t.miOpenClawReconfigure, prev.OpenClawReconfigureLabel != "", m.OpenClawReconfigureLabel != "")
	t.setTitle(t.miOpenClawReconfigure, prev.OpenClawReconfigureLabel, m.OpenClawReconfigureLabel)

	// Recent activity submenu (Phase 9 observability). Hidden when no
	// kind=fallback events landed in RecentFallbackWindow; its leading
	// separator auto-collapses while the parent is hidden, so no stray
	// rule is drawn.
	t.setVisible(t.miRecent, prev.ShowRecentActivity, m.ShowRecentActivity)
	t.applyRecentActivityEntries(prev.RecentActivityEntries, m.RecentActivityEntries)

	// Every row this pass revealed is now carrying its title, so it can go
	// on the menu (#351 — see the rows.go file comment).
	t.endRowPass()
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
		t.setVisible(mi, prevHas, nextHas)
		t.setTitle(mi, prevLabel, nextLabel)
	}
}

// applyNoticeRows diffs the prev / next notice projection against the
// notice.MaxActive pre-allocated slots (waired-agent#1205). Mirrors
// applyRecentActivityEntries, except these rows are not disabled: every
// one carries a click.
func (t *tray) applyNoticeRows(prev, next []NoticeRow) {
	for i, mi := range t.miNotices {
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
		t.setVisible(mi, prevHas, nextHas)
		t.setTitle(mi, prevLabel, nextLabel)
	}
}

// applyPeersLabel picks one of two labels for the top-level "Peers"
// item. With hardware visible, the label uses MenuModel.PeerRowsParent
// ("Peers (N)") so the submenu indicator reads consistently. Without
// hardware, the pre-Phase-7 "Peers: N" form is preserved.
func (t *tray) applyPeersLabel(prev, m MenuModel) {
	prevLabel := peersLabel(prev)
	nextLabel := peersLabel(m)
	t.setTitle(t.miPeers, prevLabel, nextLabel)
}

func peersLabel(m MenuModel) string {
	if m.ShowPeerRows && m.PeerRowsParent != "" {
		return m.PeerRowsParent
	}
	return fmt.Sprintf("Peers: %d", m.PeerCount)
}

// applyPeerRowEntries mirrors applyRecentActivityEntries: it
// walks the pre-allocated submenu children and flips visibility +
// title based on the projection.
func (t *tray) applyPeerRowEntries(prev, next []PeerRow) {
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
		t.setVisible(mi, prevHas, nextHas)
		t.setTitle(mi, prevLabel, nextLabel)
	}
}

// applyPeerRowOverflow renders the "+N more" row when the mesh
// has more peers than fit in the submenu. Hidden when n == 0.
func (t *tray) applyPeerRowOverflow(prev, next int) {
	t.setVisible(t.miPeerOverflow, prev > 0, next > 0)
	if next > 0 {
		t.setTitle(t.miPeerOverflow,
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

// indentLabel renders a "This device" child row. Empty stays empty so the
// hidden state's title is "" on both sides of the diff rather than two spaces.
func indentLabel(s string) string {
	if s == "" {
		return ""
	}
	return "  " + s
}

// applyWorkerModes diffs the worker mode rows (auto / local-only /
// peer-preferred / peer-only) against the pre-allocated submenu slots. Selected
// rows get a "● " prefix so the operator sees the current mode at a
// glance; unselected rows get "○ ". Mirrors applyCatalogEntries'
// diff-only approach so DBus traffic stays minimal.
// onSelectWorkerPrefer sends the ordering preference the operator picked.
// Only that field travels: the request leaves `mode` unsaid, so the
// daemon leaves the routing mode and any pin alone (waired-agent#1128).
func (t *tray) onSelectWorkerPrefer(ctx context.Context, idx int) {
	t.mu.Lock()
	var prefer state.RoutingPrefer
	if idx < len(t.lastWorkerPrefers) {
		prefer = t.lastWorkerPrefers[idx].Prefer
	}
	t.mu.Unlock()
	if prefer == "" {
		return
	}
	slog.Debug("tray: menu action", "action", "worker-prefer", "prefer", string(prefer))
	if _, err := t.cli.SetWorker(ctx, management.WorkerRequest{Prefer: &prefer}); err != nil {
		showError(fmt.Sprintf("Couldn't set the routing preference: %v", err))
		return
	}
	go t.pollOnce(ctx)
}

// onSelectWorkerMinSize sends the routing floor. The "Any size" row sends
// an empty string, which is a VALUE — the daemon distinguishes it from
// the field being absent, and that is how the floor is cleared.
func (t *tray) onSelectWorkerMinSize(ctx context.Context, idx int) {
	t.mu.Lock()
	var size string
	var known bool
	if idx < len(t.lastWorkerMinSizes) {
		size, known = t.lastWorkerMinSizes[idx].Size, true
	}
	t.mu.Unlock()
	if !known {
		return
	}
	slog.Debug("tray: menu action", "action", "worker-min-model-size", "size", size)
	if _, err := t.cli.SetWorker(ctx, management.WorkerRequest{MinModelSize: &size}); err != nil {
		showError(fmt.Sprintf("Couldn't set the smallest model: %v", err))
		return
	}
	go t.pollOnce(ctx)
}

func (t *tray) applyWorkerPrefers(prev, next []WorkerPreferRow) {
	for i, mi := range t.miWorkerPrefers {
		var prevHas, nextHas bool
		var prevLabel, nextLabel string
		if i < len(prev) {
			prevHas = true
			prevLabel = selectedRowLabel(prev[i].Selected, prev[i].Label)
		}
		if i < len(next) {
			nextHas = true
			nextLabel = selectedRowLabel(next[i].Selected, next[i].Label)
		}
		t.setVisible(mi, prevHas, nextHas)
		t.setTitle(mi, prevLabel, nextLabel)
	}
}

func (t *tray) applyWorkerMinSizes(prev, next []WorkerMinSizeRow) {
	for i, mi := range t.miWorkerMinSizes {
		var prevHas, nextHas bool
		var prevLabel, nextLabel string
		if i < len(prev) {
			prevHas = true
			prevLabel = selectedRowLabel(prev[i].Selected, prev[i].Label)
		}
		if i < len(next) {
			nextHas = true
			nextLabel = selectedRowLabel(next[i].Selected, next[i].Label)
		}
		t.setVisible(mi, prevHas, nextHas)
		t.setTitle(mi, prevLabel, nextLabel)
	}
}

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
		t.setVisible(mi, prevHas, nextHas)
		t.setTitle(mi, prevLabel, nextLabel)
	}
}

// applyResidencyRows diffs the residency preset rows against the
// pre-allocated slots, the applyWorkerModes shape.
func (t *tray) applyResidencyRows(prev, next []ResidencyRow) {
	for i, mi := range t.miResidency {
		var prevHas, nextHas bool
		var prevLabel, nextLabel string
		if i < len(prev) {
			prevHas = true
			prevLabel = residencyRowLabel(prev[i])
		}
		if i < len(next) {
			nextHas = true
			nextLabel = residencyRowLabel(next[i])
		}
		t.setVisible(mi, prevHas, nextHas)
		t.setTitle(mi, prevLabel, nextLabel)
	}
}

func residencyRowLabel(r ResidencyRow) string {
	prefix := "○ "
	if r.Selected {
		prefix = "● "
	}
	return prefix + r.Label
}

func workerModeRowLabel(r WorkerModeRow) string {
	return selectedRowLabel(r.Selected, r.Label)
}

// selectedRowLabel bakes the radio glyph into the label, which is how
// every fixed-set group in this menu shows its selection — systray has no
// check state of its own.
func selectedRowLabel(selected bool, label string) string {
	prefix := "○ "
	if selected {
		prefix = "● "
	}
	return prefix + label
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
		t.setVisible(mi, prevHas, nextHas)
		t.setTitle(mi, prevLabel, nextLabel)
	}
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
		t.setVisible(mi, prevHas, nextHas)
		t.setTitle(mi, prevLabel, nextLabel)
		t.setEnabled(mi, !prevDisabled, !nextDisabled)
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
// against the pre-allocated submenu slots. Each slot's title and
// visibility are only mutated when they actually change so the systray
// DBus traffic stays low even though the catalog refreshes on every
// poll tick.
//
// No slot is ever disabled. A model this computer cannot hold is still
// a choice the operator may make (waired-agent#831); the row carries
// the shortfall and the click asks.
func (t *tray) applyCatalogEntries(prev, next []CatalogEntryView) {
	for i, mi := range t.miCatalogEntries {
		var prevHas, nextHas bool
		var prevLabel, nextLabel string
		var prevTooltip, nextTooltip string
		if i < len(prev) {
			prevHas = true
			prevLabel = prev[i].Label
			prevTooltip = prev[i].Tooltip
		}
		if i < len(next) {
			nextHas = true
			nextLabel = next[i].Label
			nextTooltip = next[i].Tooltip
		}
		t.setVisible(mi, prevHas, nextHas)
		t.setTitle(mi, prevLabel, nextLabel)
		t.setTooltip(mi, prevTooltip, nextTooltip)
	}
}
