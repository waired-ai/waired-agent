package tray

import (
	"context"
	"log/slog"
	"time"

	"github.com/waired-ai/waired-agent/internal/platform/appcontrol"
	"github.com/waired-ai/waired-agent/internal/platform/notification"
)

// Application Control self-check (waired-agent#1217).
//
// On a Windows host where Smart App Control has turned against waired.exe,
// Claude Code's Waired status line and hooks go on invoking it and Windows
// goes on refusing — 234 launches in 91 minutes, on the host this came from —
// and nothing told the user anything. Windows raised its own notifications,
// which is how it was noticed at all; Waired said nothing.
//
// The surface that could speak is this one. The verdict is per file, and
// waired-tray.exe and waired-agent.exe are different files: both ran normally
// throughout that window. `waired doctor` cannot report it while it is
// happening, because `waired doctor` is the refused file.
//
// There is no repair to offer, and that is the honest message. The file is
// unsigned, Windows changed its mind about it, and it will change back —
// measured at 91 minutes on one host, about three hours and once about 142
// hours on others
// (docs/knowledges/20260829/1740-sac-verdict-is-per-file-and-moves.md). The
// permanent answer is a signing certificate (waired-ai/waired#759).

const (
	// appControlPollInterval is how often the log is re-read. A refusal window
	// opens while a session is already running — the one that prompted this
	// started 17:12 and ended 18:43 on a machine nobody touched — so a check
	// only at startup would miss the case entirely. The read is one wevtutil
	// query against a channel that is readable unelevated.
	appControlPollInterval = 15 * time.Minute

	// appControlRenotifyInterval bounds re-toasting while the same refusal
	// persists. Same shape as the tray-host and update toasts: once on the
	// first sighting, then at most a daily reminder. The user cannot act on
	// it, so saying it four times an hour would only repeat what Windows is
	// already telling them.
	appControlRenotifyInterval = 24 * time.Hour
)

// Seam over the appcontrol package, so unit tests never read the developer's
// own event log. Not in tray.go's dialog-seam block: that block is derived by
// scripts/ci/tray-dialog-seam-guard.sh, which matches bare exported names in
// this package, and this is cross-package.
var appControlCheck = appcontrol.Check

// watchAppControl reports refused programs for as long as the tray runs.
func (t *tray) watchAppControl(ctx context.Context) {
	tick := time.NewTicker(appControlPollInterval)
	defer tick.Stop()
	for {
		t.checkAppControl(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// checkAppControl reads once and toasts subject to the cadence in
// shouldNotifyAppControl.
func (t *tray) checkAppControl(ctx context.Context, now time.Time) {
	res := appControlCheck(ctx)
	if res.Status != appcontrol.Refused {
		return
	}

	// Key the cadence on WHICH programs are refused, not just on time: a
	// second program going the same way is new information and should be said
	// now, not in twenty-three hours.
	var subject string
	for _, r := range res.Refusals {
		subject += r.Program + " "
	}

	t.mu.Lock()
	fire := shouldNotifyAppControl(subject, t.lastNotifiedAppControlSubject,
		t.lastNotifiedAppControlAt, now, appControlRenotifyInterval)
	if fire {
		t.lastNotifiedAppControlSubject = subject
		t.lastNotifiedAppControlAt = now
	}
	t.mu.Unlock()
	if !fire {
		return
	}

	// The full sentence goes to the log, where there is room for it and where
	// a support request can quote it. The toast is the short form.
	slog.Warn("application control: Windows is refusing a Waired program",
		"cause", res.Cause(), "hint", res.Hint())
	notify(appControlToast(res), notification.Warning)
}

// appControlToast is the short form. A toast is read in a second and cannot be
// scrolled, so it names the file, says the one thing that stops the reader
// looking for a broken setting, and points at where the rest is.
func appControlToast(res appcontrol.Result) string {
	if res.Status != appcontrol.Refused || len(res.Refusals) == 0 {
		return ""
	}
	names := res.Refusals[0].Program
	for _, r := range res.Refusals[1:] {
		names += ", " + r.Program
	}
	msg := "Windows is refusing to run " + names + " on this computer."
	if res.Refused("waired.exe") {
		msg += " That is why the waired command and Claude Code's Waired status line do nothing."
	}
	return msg + " Nothing here is broken and there is nothing to repair — Windows does not trust the file today, and often does tomorrow. Run `waired doctor` for the details."
}

// shouldNotifyAppControl is the pure toast decision: say it once per set of
// refused programs, then at most once per renotify while that set is unchanged.
//
// A CHANGED set always fires, even inside the renotify window. The alternative
// — one timer for the whole subject — would swallow "and now the daemon too",
// which is the moment the machine stops working rather than just going quiet.
func shouldNotifyAppControl(subject, lastSubject string, lastAt, now time.Time, renotify time.Duration) bool {
	if subject == "" {
		return false
	}
	if subject != lastSubject {
		return true
	}
	if lastAt.IsZero() {
		return true
	}
	return now.Sub(lastAt) >= renotify
}
