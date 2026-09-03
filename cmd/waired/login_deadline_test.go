package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/controlclient"
	"github.com/waired-ai/waired-agent/internal/management"
)

// waired-agent#1175. The terminal used to carry its own twelve minutes
// while the daemon carried ten and the control plane carried ten — three
// clocks, none of them the server's. Raising the server's window would
// have left this one firing first, so it follows the window now.
func TestLoginDeadline(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	window := now.Add(30 * time.Minute)
	pending := management.LoginStatus{
		Phase:     management.LoginPhaseLoggingIn,
		ExpiresAt: window.Format(time.RFC3339),
	}

	t.Run("follows the control plane's window while the sign-in is pending", func(t *testing.T) {
		got := loginDeadline(pending, "", now, time.Time{})
		if want := window.Add(loginDeadlineGrace); !got.Equal(want) {
			t.Errorf("deadline = %v, want %v", got, want)
		}
	})

	t.Run("falls back to its own budget when the daemon reports no window", func(t *testing.T) {
		st := management.LoginStatus{Phase: management.LoginPhaseLoggingIn}
		got := loginDeadline(st, "", now, time.Time{})
		if want := now.Add(loginActivationBudget); !got.Equal(want) {
			t.Errorf("deadline = %v, want %v", got, want)
		}
	})

	// The regression this whole shape exists for: a sign-in completed with
	// a minute of the window left must not leave a minute for activation.
	t.Run("re-arms when the sign-in resolves, however little window is left", func(t *testing.T) {
		late := window.Add(-time.Minute)
		prev := window.Add(loginDeadlineGrace)
		st := management.LoginStatus{Phase: management.LoginPhaseActivating}
		got := loginDeadline(st, management.LoginPhaseLoggingIn, late, prev)
		if want := late.Add(loginActivationBudget); !got.Equal(want) {
			t.Errorf("deadline = %v, want %v (the activation budget, freshly armed)", got, want)
		}
	})

	// ...and exactly once. Re-arming on every tick would make it a rolling
	// budget that never expires, which is a hang, not a deadline.
	t.Run("does not re-arm again while the phase is unchanged", func(t *testing.T) {
		st := management.LoginStatus{Phase: management.LoginPhaseActivating}
		armed := loginDeadline(st, management.LoginPhaseLoggingIn, now, window)
		later := loginDeadline(st, management.LoginPhaseActivating, now.Add(time.Minute), armed)
		if !later.Equal(armed) {
			t.Errorf("deadline moved from %v to %v with no phase change", armed, later)
		}
	})

	// --force-reauth mints a new session mid-run, so the loop goes back to
	// the sign-in phase and must pick up the NEW window.
	t.Run("picks up a fresh window when a new sign-in starts", func(t *testing.T) {
		armed := now.Add(loginActivationBudget)
		fresh := now.Add(45 * time.Minute)
		st := management.LoginStatus{
			Phase:     management.LoginPhaseLoggingIn,
			ExpiresAt: fresh.Format(time.RFC3339),
		}
		got := loginDeadline(st, management.LoginPhaseActive, now, armed)
		if want := fresh.Add(loginDeadlineGrace); !got.Equal(want) {
			t.Errorf("deadline = %v, want %v", got, want)
		}
	})
}

func TestLoginWindowPassed(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	past := management.LoginStatus{ExpiresAt: now.Add(-time.Second).Format(time.RFC3339)}
	future := management.LoginStatus{ExpiresAt: now.Add(time.Second).Format(time.RFC3339)}

	if !loginWindowPassed(past, now) {
		t.Error("a window a second in the past reads as still open")
	}
	if loginWindowPassed(future, now) {
		t.Error("a window a second away reads as closed")
	}
	// No window is not an expired window. A daemon that predates the field
	// must not have its sign-ins called dead.
	if loginWindowPassed(management.LoginStatus{}, now) {
		t.Error("an absent window reads as expired")
	}
}

// Both endings have to be the same sentence. The daemon runs enrollment on
// its own goroutine and reports failures as TEXT, so the error value never
// crosses into this process — which is why this is matched rather than
// unwrapped, exactly as classifyAuthKeyError is beside it.
func TestClassifyLoginExpired(t *testing.T) {
	daemonSaid := "login expired. Run `waired init` again"
	got := classifyLoginExpired(daemonSaid)
	if !errors.Is(got, controlclient.ErrLoginSessionExpired) {
		t.Fatalf("classifyLoginExpired = %v, want the expiry sentinel", got)
	}
	if !strings.Contains(got.Error(), "waired init") {
		t.Errorf("the one sentence does not name the command that starts over: %v", got)
	}
	if classifyLoginExpired("") != nil {
		t.Error("an empty daemon error was read as an expiry")
	}
	if classifyLoginExpired("create login session: status 400: unknown field auth_key") != nil {
		t.Error("an unrelated daemon failure was read as an expiry")
	}
}
