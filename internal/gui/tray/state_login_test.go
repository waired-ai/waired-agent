package tray

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
)

func TestUpdate_LoginLoggingIn(t *testing.T) {
	got := Update(Snapshot{
		Health:   HealthOnline,
		Identity: &management.IdentityView{Enrolled: false},
		Login: &management.LoginStatus{
			Phase:    management.LoginPhaseLoggingIn,
			UserCode: "WXYZ-1234",
		},
	})
	if got.Kind != MenuNotSignedIn {
		t.Errorf("Kind=%d, want MenuNotSignedIn", got.Kind)
	}
	if !strings.Contains(got.HeaderTitle, "Signing in") {
		t.Errorf("HeaderTitle=%q, want a signing-in label", got.HeaderTitle)
	}
	// Login item is hidden while OAuth is pending so a second click can't
	// start a second session.
	if got.ToggleAction != "" {
		t.Errorf("ToggleAction=%q, want hidden during login", got.ToggleAction)
	}
	if !strings.Contains(got.StatusMsg, "WXYZ-1234") {
		t.Errorf("StatusMsg=%q, want the user code", got.StatusMsg)
	}
}

func TestUpdate_LoginActivating(t *testing.T) {
	got := Update(Snapshot{
		Health:   HealthOnline,
		Identity: &management.IdentityView{Enrolled: false},
		Login:    &management.LoginStatus{Phase: management.LoginPhaseActivating},
	})
	if !strings.Contains(got.HeaderTitle, "Activating") {
		t.Errorf("HeaderTitle=%q, want an activating label", got.HeaderTitle)
	}
	if got.ToggleAction != "" {
		t.Errorf("ToggleAction=%q, want hidden during activation", got.ToggleAction)
	}
}

func TestUpdate_LoginError(t *testing.T) {
	got := Update(Snapshot{
		Health:   HealthOnline,
		Identity: &management.IdentityView{Enrolled: false},
		Login: &management.LoginStatus{
			Phase: management.LoginPhaseError,
			Error: "control plane denied",
		},
	})
	if got.Kind != MenuNotSignedIn {
		t.Errorf("Kind=%d, want MenuNotSignedIn", got.Kind)
	}
	// Retry must remain possible: the login item stays visible on error.
	if got.ToggleAction != "Log in..." {
		t.Errorf("ToggleAction=%q, want %q on error", got.ToggleAction, "Log in...")
	}
	if !strings.Contains(got.StatusMsg, "control plane denied") {
		t.Errorf("StatusMsg=%q, want the error reason", got.StatusMsg)
	}
}

// With no Login tracked, the not-signed-in projection is unchanged.
func TestUpdate_NotSignedIn_NoLogin(t *testing.T) {
	got := Update(Snapshot{
		Health:   HealthOnline,
		Identity: &management.IdentityView{Enrolled: false},
	})
	if got.ToggleAction != "Log in..." || got.HeaderTitle != "○ Not signed in" {
		t.Errorf("unexpected resting not-signed-in model: %+v", got)
	}
}
