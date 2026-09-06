package main

import (
	"context"
	"time"

	"github.com/waired-ai/waired-agent/internal/deauth"
	"github.com/waired-ai/waired-agent/internal/identity"
	"github.com/waired-ai/waired-agent/internal/management"
)

// deauthTimeout bounds the control-plane call. Matched to the CLI's own
// budget in cmd/waired/logout.go: the same call, against the same control
// plane, and a sign-out must not hang on an unreachable one.
const deauthTimeout = 10 * time.Second

// deauthFunc is the control-plane deregistration. A seam so the ORDER this
// file is responsible for can be asserted without a control plane.
type deauthFunc func(ctx context.Context, stateDir string, mode deauth.Mode) (deauth.Outcome, error)

// wipeFunc removes the enrollment from disk. A seam for the same reason.
type wipeFunc func(stateDir string) error

// Logout signs this device out, and does the three steps in the one order
// that is safe. It implements management.LogoutController.
//
// Why the daemon and not the CLI: an elevated `waired logout` deletes the
// files from underneath a running daemon that never notices. Identity is read
// from disk only at boot, on a node-key rotation, and on a re-auth login, so
// the daemon goes on serving the deleted enrollment — and Start's
// restoreIdentityIfMissing writes identity.json back from the live session on
// the next sign-in (waired-agent#800). The app therefore kept showing
// "Connected" after a sign-out that appeared to succeed, which is
// waired-agent#1269.
//
// The order, and why each step is where it is:
//
//  1. Refuse while a sign-in is in flight. Tearing the session down underneath
//     an OAuth that is about to activate would leave an enrollment nobody
//     asked for.
//  2. Deauthenticate with the control plane FIRST, while the credentials are
//     still on disk — the call needs them. Best-effort: a control plane that
//     cannot be reached must not strand this machine signed in, so the failure
//     is reported and the local removal proceeds. Same rule the CLI has
//     always had.
//  3. Tear the session down BEFORE removing the files. The token refresher
//     writes rotated tokens into secrets/, the reconciler writes, and the
//     runtime-state writer writes; stopping them first is the step no external
//     process can perform, and it is the whole reason this belongs here.
//  4. Remove the enrollment, through identity.RemoveEnrollment — the daemon's
//     OWN state dir, never a recomputed one.
//
// After step 3 the switchboard holds no session, so Start's repair branch is
// not reached and liveIdentity() is nil: nothing can write identity.json back.
// That is ordering doing the work, not a flag, and
// TestLoginStartAfterLogoutDoesNotRestoreIdentity pins it.
func (lc *loginController) Logout(ctx context.Context, req management.LogoutRequest) (management.LogoutResponse, error) {
	lc.mu.Lock()
	defer lc.mu.Unlock()

	if s := lc.session; s != nil {
		switch s.phase {
		case management.LoginPhaseLoggingIn, management.LoginPhaseActivating:
			return management.LogoutResponse{}, management.ErrLoginInFlight
		}
	}

	resp := management.LogoutResponse{}
	if !req.SkipDeauth {
		mode := deauth.ModeLogout
		if req.Revoke {
			mode = deauth.ModeRevoke
		}
		deauthCtx, cancel := context.WithTimeout(ctx, deauthTimeout)
		outcome, err := lc.deauthFn()(deauthCtx, lc.stateDir, mode)
		cancel()
		switch {
		case err != nil:
			resp.DeauthError = err.Error()
			lc.log().Warn("sign-out: the control plane was not reached; this device may still be active server-side",
				"mode", mode, "err", err)
		case outcome == deauth.OutcomeDone:
			resp.Deauthed = true
		}
	}

	if lc.deactivate != nil {
		lc.deactivate()
	}

	if err := lc.wipeFn()(lc.stateDir); err != nil {
		// The session is already down at this point, so reporting the error
		// leaves the daemon signed out but the files partly present. That is
		// the honest answer: the caller (and the operator) needs to know the
		// disk was not cleared.
		return resp, err
	}
	lc.session = nil
	lc.log().Info("sign-out: identity removed and the session torn down",
		"state_dir", lc.stateDir, "deauthenticated", resp.Deauthed)
	return resp, nil
}

// deauthFn / wipeFn resolve the seams to their production implementations when
// a controller was built without them (every caller but the tests).
func (lc *loginController) deauthFn() deauthFunc {
	if lc.deauth != nil {
		return lc.deauth
	}
	return deauth.Deregister
}

func (lc *loginController) wipeFn() wipeFunc {
	if lc.wipe != nil {
		return lc.wipe
	}
	return identity.RemoveEnrollment
}
