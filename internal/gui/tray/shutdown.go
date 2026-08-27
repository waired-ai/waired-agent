package tray

import (
	"context"
	"log/slog"

	"fyne.io/systray"
)

// shutdownCause names why the tray is going away.
//
// It exists so that "does leaving the desktop also wind this machine
// down?" is answered by one table instead of by a condition spread over
// the call sites. Before waired-agent#1045 there was only one way out —
// the Quit menu item — and the answer could live inline. A tray that
// also exits on SIGTERM has two, and an update that restarts it will
// have a third, whose answer differs.
type shutdownCause int

const (
	// causeQuitMenu: the user picked "Quit" from the menu.
	causeQuitMenu shutdownCause = iota
	// causeSignal: SIGINT or SIGTERM. The desktop session manager sends
	// this at logout (the packaged /etc/xdg/autostart entry makes the
	// tray a child of the session), launchctl bootout sends it, and so
	// does the uninstaller (waired-agent#1031).
	causeSignal
	// causeWindowClose: the GUI backend's own exit path ran — a Windows
	// WM_ENDSESSION at logout or shutdown, or a WM_DESTROY. Windows
	// delivers no signal to a -H windowsgui process, so this is the only
	// route by which a Windows desktop can tell the tray it is going
	// away (waired-agent#1059).
	causeWindowClose
)

func (c shutdownCause) String() string {
	switch c {
	case causeQuitMenu:
		return "quit-menu"
	case causeSignal:
		return "signal"
	case causeWindowClose:
		return "window-close"
	}
	return "unknown"
}

// shutdownPlan is what a shutdown does beyond taking the icon off the
// desktop.
type shutdownPlan struct {
	// WindDown withdraws this machine from the mesh and stops the
	// engine, so its memory comes back and peers stop being routed here.
	WindDown bool
}

// planShutdown says what each way out does.
//
// Both causes wind down, and that is a product contract: #316 ratified
// the wind-down for the Quit menu on the grounds that peers must stop
// being routed to a machine "while nobody is at the keyboard", and the
// owner ruled on 2026-08-27 (waired-agent#1031 / #1045) that a signal
// carries the same meaning — signing out of the desktop is that same
// event arriving by another route. Deciding it here rather than at the
// call sites is what keeps the two from drifting.
//
// Every cause winds down, and an update is deliberately NOT a fourth
// one. The installer's restart (waired-agent#1046) reaches the tray as
// an ordinary signal, and by then it has already restarted the daemon —
// which stops the engine on its own — so the wind-down costs nothing
// there and needs no way to be told apart. A cause that changes the
// answer is what would earn a row here; none does yet.
func planShutdown(c shutdownCause) shutdownPlan {
	switch c {
	case causeQuitMenu, causeSignal, causeWindowClose:
		return shutdownPlan{WindDown: true}
	}
	return shutdownPlan{}
}

// ShutdownBudget is the longest a cancelled tray takes to reach
// systray.Quit. The wind-down is the only bounded wait on the way out
// (quitBudget), and watchShutdown starts from inside onReady, so there
// is nothing to wait for before it.
//
// Exported so cmd/waired-tray's own deadline can be expressed against
// it rather than transcribing a number that then drifts, and so
// packaging/install/uninstall.sh's grace period has one figure to be
// larger than.
const ShutdownBudget = quitBudget

// shutdown ends this tray: it carries out the plan, then leaves the GUI
// event loop.
//
// quit is a parameter rather than a package-level seam because the thing
// it stands for — systray.Quit — cannot be table-tested underneath a
// `var quitFn = systray.Quit` indirection the way the dialog seams in
// tray.go can (CLAUDE.md "a var xFn = realFn seam needs a table test on
// realFn"). Passing it in keeps the two references to systray.Quit at
// the two real call sites and puts the seam below the behaviour.
func (t *tray) shutdown(p shutdownPlan, quit func()) {
	if p.WindDown {
		t.windDown()
	}
	quit()
}

// windDown withdraws this machine from the mesh and stops the engine,
// at most once for the life of the process.
//
// Once, because there are now two ways to arrive here for a single
// departure. Leaving through the menu or a signal runs the wind-down and
// THEN quits the GUI loop — and quitting the GUI loop is itself what
// calls onSystrayExit below. systray's own systrayExitCalled guard only
// stops systray from invoking the callback twice; it says nothing about
// this side reaching the daemon twice.
func (t *tray) windDown() {
	t.windDownOnce.Do(t.onQuit)
}

// onSystrayExit is what systray runs when its event loop ends, however
// it ended. It is the only place a Windows desktop's own shutdown is
// observable to us: os/signal delivers nothing to a -H windowsgui
// process, so a logout or a shutdown arrives as WM_ENDSESSION and
// systray turns that into this call (waired-agent#1059). Before this,
// Windows signed out without stopping the engine or withdrawing from the
// mesh, while Linux and macOS did both.
//
// It runs inside the event loop, which systray documents as deliberate:
// "onExit runs in the event loop to make sure it has time to finish
// before the process terminates".
//
// The menu and signal paths reach it too, after they have already wound
// down — windDown's Once is what makes that a no-op rather than a second
// round trip to a daemon that is already suspended.
func (t *tray) onSystrayExit() {
	if planShutdown(causeWindowClose).WindDown {
		t.windDown()
	}
}

// elevationCtx detaches a privileged child from the tray's own
// lifetime.
//
// The four *ViaElevation helpers run pkexec / osascript / ShellExecute
// through exec.CommandContext with the ctx the click handler was given,
// which is the tray's root context. Nothing ever cancelled that context,
// so the coupling was inert. watchShutdown makes it live: a logout part
// way through "Update Waired" would otherwise kill the elevated
// install.sh mid-swap and leave the host with half a release on disk.
//
// Values (and so any future request-scoped state) carry through;
// cancellation and deadline do not. The child is a separate process, so
// once it is started the exiting tray leaves it reparented and running.
func elevationCtx(ctx context.Context) context.Context {
	return context.WithoutCancel(ctx)
}

// watchShutdown ends the tray when ctx is cancelled — which, for the
// context cmd/waired-tray builds, means a SIGINT or SIGTERM arrived.
//
// MUST be started from inside onReady, never before systray.Run.
// systray.Quit is quitOnce.Do(quit) — one shot for the process lifetime
// — and two of the three backends do not tolerate being called before
// they are up: darwin's quit() messages a nil owner (it is set in
// registerSystray), and Windows posts WM_CLOSE to a zero window handle,
// which lands on the calling thread's queue and is never dispatched.
// Either one spends quitOnce and leaves the app unable to quit for the
// rest of its life — by signal AND by its own menu item. onReady is the
// one place all three backends have reached only after init.
//
// A signal that arrives before onReady is therefore not acted on here at
// all; cmd/waired-tray's own deadline is what ends the process then.
// Recorded as fyne.io/systray v1.12.2 behaviour (systray.go:158,
// systray_darwin.m:476, systray_windows.go:951) — re-check on upgrade.
//
// The Windows backend gives the same rule a second, sharper edge:
// quit() there calls runSystrayExit(), which dereferences a callback
// that only Register sets. Quitting before Register is a nil-pointer
// panic, not a dropped message.
func (t *tray) watchShutdown(ctx context.Context) {
	t.watchShutdownWith(ctx, systray.Quit)
}

// watchShutdownWith is watchShutdown with its way out of the GUI event
// loop passed in, for the same reason shutdown takes one: systray.Quit
// is process-global and one-shot, so a test that reached the real one
// would be testing the library's teardown rather than this decision.
func (t *tray) watchShutdownWith(ctx context.Context, quit func()) {
	<-ctx.Done()
	cause := causeSignal
	slog.Info("waired-tray: shutting down", "cause", cause.String())
	t.shutdown(planShutdown(cause), quit)
}
