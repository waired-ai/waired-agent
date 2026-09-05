package tray

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/platform/appcontrol"
)

// TestShouldNotifyAppControl is the toast cadence. The user cannot act on this
// refusal, so repeating it is pure noise — but a CHANGE in which programs are
// refused is new information and must not wait out the window.
func TestShouldNotifyAppControl(t *testing.T) {
	now := time.Date(2026, 9, 4, 18, 0, 0, 0, time.UTC)
	day := 24 * time.Hour

	for _, tc := range []struct {
		name        string
		subject     string
		lastSubject string
		lastAt      time.Time
		want        bool
	}{
		{"nothing refused", "", "", time.Time{}, false},
		{"first sighting", "waired.exe ", "", time.Time{}, true},
		{"same set, minutes later", "waired.exe ", "waired.exe ", now.Add(-20 * time.Minute), false},
		{"same set, a day later", "waired.exe ", "waired.exe ", now.Add(-25 * time.Hour), true},
		// The case the subject key exists for: the daemon joins the CLI. The
		// machine has just stopped working, and a time-only cadence would
		// have swallowed it for the rest of the day.
		{"a second program joins", "waired.exe waired-agent.exe ", "waired.exe ", now.Add(-1 * time.Minute), true},
		// And the reverse: one lifts while another stays. Still a change, and
		// still worth saying — it is the only signal the user gets that the
		// verdict moves.
		{"one lifts", "waired-agent.exe ", "waired.exe waired-agent.exe ", now.Add(-1 * time.Minute), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldNotifyAppControl(tc.subject, tc.lastSubject, tc.lastAt, now, day)
			if got != tc.want {
				t.Errorf("shouldNotifyAppControl = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAppControlToastSaysTheUsefulThings. A toast cannot be scrolled, so what
// it does say has to be the part that stops the reader hunting for a broken
// setting.
func TestAppControlToastSaysTheUsefulThings(t *testing.T) {
	refused := appcontrol.Result{
		Status:   appcontrol.Refused,
		Refusals: []appcontrol.Refusal{{Program: "waired.exe", Count: 234}},
	}
	msg := appControlToast(refused)
	for _, want := range []string{"waired.exe", "status line", "nothing to repair", "waired doctor"} {
		if !strings.Contains(strings.ToLower(msg), strings.ToLower(want)) {
			t.Errorf("toast does not mention %q:\n%s", want, msg)
		}
	}

	// A refused tray is not a reason to blame Claude Code.
	trayOnly := appcontrol.Result{
		Status:   appcontrol.Refused,
		Refusals: []appcontrol.Refusal{{Program: "waired-tray.exe", Count: 1}},
	}
	if strings.Contains(appControlToast(trayOnly), "status line") {
		t.Errorf("a refused tray must not be reported as breaking Claude Code:\n%s", appControlToast(trayOnly))
	}

	// Nothing refused says nothing at all.
	if got := appControlToast(appcontrol.Result{Status: appcontrol.Clear}); got != "" {
		t.Errorf("Clear produced a toast: %q", got)
	}
	if got := appControlToast(appcontrol.Result{Status: appcontrol.NotApplicable}); got != "" {
		t.Errorf("NotApplicable produced a toast: %q", got)
	}
}

// TestCheckAppControlToastsOnceThenGoesQuiet drives the tray method through
// the seam, so the state handling — not just the pure decision — is covered.
func TestCheckAppControlToastsOnceThenGoesQuiet(t *testing.T) {
	log := resetSeams(t)
	prev := appControlCheck
	t.Cleanup(func() { appControlCheck = prev })
	appControlCheck = func(context.Context) appcontrol.Result {
		log.mu.Lock()
		defer log.mu.Unlock()
		log.appControlChecks++
		return appcontrol.Result{
			Status:   appcontrol.Refused,
			Refusals: []appcontrol.Refusal{{Program: "waired.exe", Count: 3}},
		}
	}

	tr := &tray{}
	now := time.Date(2026, 9, 4, 18, 0, 0, 0, time.UTC)
	tr.checkAppControl(t.Context(), now)
	first := tr.lastNotifiedAppControlAt
	if first.IsZero() {
		t.Fatal("the first sighting did not record a notification")
	}
	tr.checkAppControl(t.Context(), now.Add(10*time.Minute))
	if !tr.lastNotifiedAppControlAt.Equal(first) {
		t.Error("a second read ten minutes later re-toasted; the cadence did not hold")
	}
	tr.checkAppControl(t.Context(), now.Add(25*time.Hour))
	if tr.lastNotifiedAppControlAt.Equal(first) {
		t.Error("a day later the reminder did not fire")
	}
}
