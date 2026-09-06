package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/waired-ai/waired-agent/internal/deauth"
	"github.com/waired-ai/waired-agent/internal/identity"
	"github.com/waired-ai/waired-agent/internal/management"
)

// signOutRecorder records sign-out's three side effects in the order they
// happen. The order is the behaviour under test, and none of it needs a
// control plane or a real state dir to observe.
type signOutRecorder struct {
	steps      []string
	deauthMode deauth.Mode
	deauthErr  error
	wipedDir   string
	wipeErr    error
}

func (r *signOutRecorder) deauth(_ context.Context, stateDir string, mode deauth.Mode) (deauth.Outcome, error) {
	r.steps = append(r.steps, "deauth")
	r.deauthMode = mode
	if r.deauthErr != nil {
		return deauth.OutcomeSkipped, r.deauthErr
	}
	return deauth.OutcomeDone, nil
}

func (r *signOutRecorder) deactivate() { r.steps = append(r.steps, "deactivate") }

func (r *signOutRecorder) wipe(stateDir string) error {
	r.steps = append(r.steps, "wipe")
	r.wipedDir = stateDir
	return r.wipeErr
}

func newTestLogoutController(t *testing.T, sb *switchboard, r *signOutRecorder, stateDir string) *loginController {
	t.Helper()
	return newLoginController(sb, loginControllerConfig{
		StateDir:          stateDir,
		DefaultControlURL: "https://cp.example",
		RootCtx:           context.Background(),
		Logger:            testLogger(),
		Deactivate:        r.deactivate,
		Deauth:            r.deauth,
		Wipe:              r.wipe,
	})
}

// TestLogoutDeauthsBeforeItDeactivatesBeforeItWipes pins the order.
//
// Product contract, waired-agent#1269: each step depends on the previous one
// not having happened yet. The control-plane call needs the credentials that
// step 3 tears down and step 4 deletes; and the teardown has to precede the
// removal because the token refresher writes rotated tokens into secrets/, the
// reconciler writes, and the runtime-state writer writes — stopping them first
// is the step no external process can perform, and it is why sign-out belongs
// to the daemon at all.
//
// A later refactor swapping two of these would still pass every other test in
// this package.
func TestLogoutDeauthsBeforeItDeactivatesBeforeItWipes(t *testing.T) {
	r := &signOutRecorder{}
	lc := newTestLogoutController(t, &switchboard{}, r, "/var/lib/waired")

	resp, err := lc.Logout(context.Background(), management.LogoutRequest{})
	if err != nil {
		t.Fatalf("Logout: %v", err)
	}
	want := []string{"deauth", "deactivate", "wipe"}
	if !slices.Equal(r.steps, want) {
		t.Errorf("sign-out ran %v, want %v", r.steps, want)
	}
	if resp.Enrolled {
		t.Error("response says still enrolled after a completed sign-out")
	}
	if !resp.Deauthed {
		t.Error("response says the control plane was not told, but the call succeeded")
	}
}

// The wipe must target the daemon's OWN state dir. Recomputing it client-side
// is precisely the divergence that produced this issue one layer up.
func TestLogoutWipesTheDaemonsOwnStateDir(t *testing.T) {
	r := &signOutRecorder{}
	lc := newTestLogoutController(t, &switchboard{}, r, "/opt/waired-instance-2")

	if _, err := lc.Logout(context.Background(), management.LogoutRequest{}); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if r.wipedDir != "/opt/waired-instance-2" {
		t.Errorf("wiped %q, want the daemon's own state dir", r.wipedDir)
	}
}

// A control plane that cannot be reached must never strand this machine signed
// in. The local removal proceeds and the failure is reported — the rule the
// CLI has always had (cmd/waired/logout.go), now with somewhere for the app to
// read it.
func TestLogoutDeauthFailureStillWipesAndIsReported(t *testing.T) {
	r := &signOutRecorder{deauthErr: errors.New("control plane unreachable")}
	lc := newTestLogoutController(t, &switchboard{}, r, "/var/lib/waired")

	resp, err := lc.Logout(context.Background(), management.LogoutRequest{})
	if err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if !slices.Contains(r.steps, "wipe") {
		t.Errorf("steps = %v, want the local removal to have happened anyway", r.steps)
	}
	if resp.Deauthed {
		t.Error("Deauthed = true after the control-plane call failed")
	}
	if resp.DeauthError == "" {
		t.Error("DeauthError is empty; the caller cannot tell the device may still be active server-side")
	}
}

// Revoke is the uninstaller's mode: the device row goes away rather than being
// deauthenticated. A person signing out of the app must not get it, because a
// revoked device cannot be recovered by signing back in.
func TestLogoutRevokeSelectsTheTerminalMode(t *testing.T) {
	r := &signOutRecorder{}
	lc := newTestLogoutController(t, &switchboard{}, r, "/var/lib/waired")
	if _, err := lc.Logout(context.Background(), management.LogoutRequest{Revoke: true}); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if r.deauthMode != deauth.ModeRevoke {
		t.Errorf("deauth mode = %v, want ModeRevoke", r.deauthMode)
	}

	r2 := &signOutRecorder{}
	lc2 := newTestLogoutController(t, &switchboard{}, r2, "/var/lib/waired")
	if _, err := lc2.Logout(context.Background(), management.LogoutRequest{}); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if r2.deauthMode != deauth.ModeLogout {
		t.Errorf("default deauth mode = %v, want ModeLogout (recoverable)", r2.deauthMode)
	}
}

// Tearing the session down underneath an OAuth that is about to activate would
// leave the daemon holding an enrollment nobody asked for. Refuse instead.
func TestLogoutRefusesWhileASignInIsInFlight(t *testing.T) {
	for _, phase := range []management.LoginPhase{
		management.LoginPhaseLoggingIn,
		management.LoginPhaseActivating,
	} {
		t.Run(string(phase), func(t *testing.T) {
			r := &signOutRecorder{}
			lc := newTestLogoutController(t, &switchboard{}, r, "/var/lib/waired")
			lc.session = &loginSession{id: "s1", phase: phase}

			_, err := lc.Logout(context.Background(), management.LogoutRequest{})
			if !errors.Is(err, management.ErrLoginInFlight) {
				t.Fatalf("Logout during %s = %v, want ErrLoginInFlight", phase, err)
			}
			if len(r.steps) != 0 {
				t.Errorf("a refused sign-out still ran %v", r.steps)
			}
		})
	}
}

// TestSignedOutClearsThePersistedIdentityToo pins the half of the teardown
// that is invisible.
//
// switchboard.Identity() answers from `offline` whenever no session is
// published, and that arm exists so a daemon which IS enrolled on disk but
// could not activate does not report "Not signed in" — the misreading that
// produced #318's "logged out after reboot". reset() alone therefore leaves a
// signed-out daemon still naming the account it just removed, and the app goes
// on rendering it. Nothing else in this package asserts the difference.
func TestSignedOutClearsThePersistedIdentityToo(t *testing.T) {
	sb := &switchboard{}
	sb.setOffline(management.IdentityView{Enrolled: true, AccountEmail: "someone@example.test"})

	sb.signedOut()

	got := sb.Identity()
	if got.Enrolled {
		t.Errorf("Identity().Enrolled = true after signedOut; the app would still show the account")
	}
	if got.AccountEmail != "" {
		t.Errorf("Identity().AccountEmail = %q after signedOut, want it empty", got.AccountEmail)
	}
}

// TestLoginStartAfterLogoutDoesNotRestoreIdentity is the pin for
// waired-agent#1269's second half.
//
// Product contract: a completed sign-out stays signed out. Start's repair
// branch writes identity.json back from the live session when it finds the
// file missing (waired-agent#800) — which is exactly the state a sign-out
// leaves behind. What stops it is ordering, not a flag: after the teardown the
// switchboard holds no session, so Start does not take the "already live"
// branch and liveIdentity() is nil in any case.
//
// Both guards are load-bearing and neither was asserted by anything before
// this. A refactor that published a session again, or that made Start call
// restoreIdentityIfMissing unconditionally, would silently resurrect the
// enrollment.
func TestLoginStartAfterLogoutDoesNotRestoreIdentity(t *testing.T) {
	stateDir := t.TempDir()
	// A real sign-out on a real enrolled directory.
	if err := identity.Save(stateDir, &identity.Identity{
		DeviceID:   "dev_test",
		AccountID:  "acct_test",
		ControlURL: "https://cp.example",
	}); err != nil {
		t.Fatalf("seed identity: %v", err)
	}
	sb := &switchboard{}
	sb.setOffline(management.IdentityView{Enrolled: true, AccountEmail: "someone@example.test"})

	r := &signOutRecorder{}
	lc := newLoginController(sb, loginControllerConfig{
		StateDir:          stateDir,
		DefaultControlURL: "https://cp.example",
		RootCtx:           context.Background(),
		Logger:            testLogger(),
		Deauth:            r.deauth,
		// The production teardown, minus the session there is none of here:
		// deactivateSession in main.go is `teardown(); sb.signedOut()`.
		Deactivate: func() {
			r.steps = append(r.steps, "deactivate")
			sb.signedOut()
		},
		// The real removal, so the file genuinely goes away.
		Wipe: identity.RemoveEnrollment,
	})

	if _, err := lc.Logout(context.Background(), management.LogoutRequest{}); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	idPath := filepath.Join(stateDir, "identity.json")
	if _, err := os.Stat(idPath); !os.IsNotExist(err) {
		t.Fatalf("identity.json survived the sign-out (stat err = %v)", err)
	}
	if v := sb.Identity(); v.Enrolled || v.AccountEmail != "" {
		t.Errorf("switchboard still reports %+v after a sign-out, want an unenrolled view", v)
	}

	// The next sign-in attempt. On a signed-out daemon this takes the
	// enrollment path rather than the repair path; the enroll seam is nil
	// here so it fails, and that is fine — what must NOT happen is
	// identity.json coming back.
	_, _ = lc.Start(context.Background(), management.LoginStartRequest{})
	if _, err := os.Stat(idPath); !os.IsNotExist(err) {
		t.Errorf("identity.json was restored after a sign-out (stat err = %v); the sign-out did not stick", err)
	}
}
