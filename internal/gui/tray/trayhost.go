package tray

import (
	"context"
	"log/slog"
	"time"

	"github.com/waired-ai/waired-agent/internal/platform/notification"
	"github.com/waired-ai/waired-agent/internal/platform/trayhost"
)

// Tray-host self-check (#295).
//
// GNOME ships no StatusNotifierItem host, so on GNOME this process publishes a
// tray item that nothing draws — the icon is silently absent and the user has
// no surface to complain through, because the surface IS the icon. Nothing
// installs the AppIndicator host extension any more either: apt has no
// conditional-dependency form, so `waired-tray` cannot depend on a GNOME
// extension without dragging GNOME Shell onto servers (see
// internal/platform/trayhost.PlanRepair).
//
// This process is the right place to close that gap. It only ever runs inside a
// desktop session, so "is there a session?" needs no heuristic; it runs as the
// desktop user, so enabling an extension — a per-user dconf write — costs no
// privilege; and it starts on every login, so a host that breaks later (the
// user disables the extension, a GNOME major upgrade invalidates it against
// metadata.json's shell-version, a second user logs in) is re-checked rather
// than assumed fixed by whatever install.sh did once.

// trayHostRenotifyInterval bounds how often an unrepairable missing host
// re-toasts within one session. The state is per-process, like the update
// toast's, so in practice this fires once per login plus a daily re-reminder in
// a session left running — never per poll.
const trayHostRenotifyInterval = 24 * time.Hour

// Seams over the trayhost package, so unit tests never probe the real session
// bus or run gnome-extensions against the developer's own desktop. These are
// deliberately not in tray.go's dialog-seam block: that block is derived by
// scripts/ci/tray-dialog-seam-guard.sh, which matches bare exported names in
// this package, and these are cross-package. seams_test.go stubs them anyway.
var (
	trayHostCheck  = trayhost.Check
	trayHostPlan   = trayhost.Plan
	trayHostEnable = trayhost.Enable

	// trayHostMenuLabels answers "who will draw this menu, and how does it
	// read a label" (waired-agent#1100). Same seam block for the same
	// reason: it makes a D-Bus call, and no unit test should depend on the
	// developer's own desktop.
	trayHostMenuLabels = trayhost.MenuLabels
)

// checkTrayHost verifies that this session can actually draw our icon, repairs
// the case that is free to repair, and reports the rest.
//
// Enabling an already-installed extension happens without asking. Installing
// waired-tray is the request to show a tray icon, enabling the host extension
// is what makes that request true, it needs no privilege, and it is one toggle
// away in GNOME's Extensions app if the user disagrees. Installing a package,
// which needs root, is never done from here — it is offered through
// `waired doctor --fix`, where the user is already at a terminal.
func (t *tray) checkTrayHost(ctx context.Context) {
	res := trayHostCheck()
	action := trayHostPlan(res)

	if action == trayhost.RepairEnableOnly {
		if err := trayHostEnable(ctx); err != nil {
			// The free repair failed — a version-invalidated extension, a
			// locked-down dconf, no gnome-extensions on PATH. The icon is
			// still missing, so this degrades to the case where only the user
			// can act: report it as RepairManual rather than staying silent
			// (RepairEnableOnly never toasts, since normally we just fix it).
			slog.Debug("tray host: enabling the AppIndicator extension failed", "err", err)
			t.maybeNotifyTrayHost(trayhost.RepairManual, res.Hint, time.Now())
			return
		}
		slog.Info("tray host: enabled the AppIndicator extension for this user")
		if res.Wayland {
			notify("Waired's icon is turned on. Log out and back in to see it.", notification.Info)
		} else {
			notify("Waired's icon is turned on. It should appear in a moment.", notification.Info)
		}
		return
	}

	t.maybeNotifyTrayHost(action, res.Hint, time.Now())
}

// maybeNotifyTrayHost toasts about a missing host subject to the cadence in
// shouldNotifyTrayHost, recording the time only when it actually fires.
func (t *tray) maybeNotifyTrayHost(action trayhost.RepairAction, hint string, now time.Time) {
	t.mu.Lock()
	fire := shouldNotifyTrayHost(action, t.lastNotifiedTrayHostAt, now, trayHostRenotifyInterval)
	if fire {
		t.lastNotifiedTrayHostAt = now
	}
	t.mu.Unlock()
	if !fire {
		return
	}
	slog.Warn("tray host: no SNI host for the tray icon", "action", action.String(), "hint", hint)
	notify("Waired's icon can't be shown on this desktop. Run `waired doctor --fix` in a terminal to fix it.",
		notification.Warning)
}

// shouldNotifyTrayHost is the pure toast decision. It fires only when there is
// something the user can act on — a repair that needs root (RepairInstallThenEnable)
// or one only they can perform (RepairManual) — and then at most once per
// renotify.
//
// RepairNone is deliberately silent, and that covers three different things: the
// icon already renders, this is not a desktop at all, and the desktop cannot
// render SNI icons however hard we try (MATE). The last one is a permanent
// property of the host: toasting it every login would be pure noise, and
// `waired doctor` already reports it. RepairEnableOnly is silent here too
// because checkTrayHost repairs it instead of complaining about it.
func shouldNotifyTrayHost(action trayhost.RepairAction, lastAt, now time.Time, renotify time.Duration) bool {
	switch action {
	case trayhost.RepairInstallThenEnable, trayhost.RepairManual:
	default:
		return false
	}
	if lastAt.IsZero() {
		return true // first sighting this session → say so now
	}
	return now.Sub(lastAt) >= renotify
}
