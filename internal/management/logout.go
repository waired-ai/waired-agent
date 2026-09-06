package management

import (
	"context"
	"errors"
)

// Sign-out, as a daemon operation.
//
// It used to be the one enrollment verb that lived outside the daemon: sign-in
// is POST /login/start and the daemon writes the state dir, while sign-out was
// an elevated `waired logout` that deleted the files from underneath a daemon
// that never noticed. The daemon reads identity from disk only at boot, on a
// node-key rotation, and on a re-auth login, so it went on serving the deleted
// identity — and loginController.restoreIdentityIfMissing would write
// identity.json back from the live session on the next sign-in
// (waired-agent#800). Sign-out therefore did not stick, and the app showed
// "Connected" throughout. That is waired-agent#1269's second half.
//
// Ownership also fixes an ordering the CLI cannot reach at all: the token
// refresher writes rotated tokens into secrets/, and the reconciler and the
// runtime-state writer write too. Only the process that owns those goroutines
// can stop them before the files go away.
//
// This route is a mutating verb, so the write guard already confines it to the
// local IPC socket / named pipe — it is unreachable from the loopback TCP port
// and therefore from a browser. It is not a widening of who may sign this
// machine out: POST /login/start sits on the same socket and takes an auth key,
// so a local process could already re-enroll this device unattended. A
// ModeLogout sign-out is strictly weaker — the control plane keeps the device
// row and `waired init` recovers it.

// ErrLoginInFlight is returned by LogoutController.Logout when a sign-in is
// part-way through. Tearing the session down underneath an OAuth that is about
// to activate would leave the daemon with an enrollment nobody asked for, so
// the caller is told to wait rather than being served a half-answer.
var ErrLoginInFlight = errors.New("management: a sign-in is in progress")

// LogoutRequest is the POST /waired/v1/logout body. An empty body is valid and
// means an ordinary, recoverable sign-out.
type LogoutRequest struct {
	// Revoke asks the control plane to remove the device row entirely rather
	// than deauthenticate it. The uninstaller wants this; a person signing out
	// of the app does not, because a revoked device cannot be recovered by
	// signing back in.
	Revoke bool `json:"revoke,omitempty"`
	// SkipDeauth signs out without calling the control plane at all. It
	// carries `waired logout --local` through, so an operator who asked for a
	// purely local sign-out still gets the running daemon told about it.
	SkipDeauth bool `json:"skip_deauth,omitempty"`
}

// LogoutResponse reports what happened. Enrolled is the answer that matters —
// false means this daemon is no longer serving an identity.
type LogoutResponse struct {
	// Enrolled is false on a completed sign-out.
	Enrolled bool `json:"enrolled"`
	// Deauthed is true when the control plane confirmed the deauthentication.
	Deauthed bool `json:"deauthed"`
	// DeauthError describes a control-plane call that did not land. Local
	// state is still removed — sign-out must always clear this machine — so
	// this is a warning, not a failure. It travels because the CLI already
	// prints "the device may still be active server-side" and the app had no
	// way to say the same thing.
	DeauthError string `json:"deauth_error,omitempty"`
}

// LogoutController is implemented by the agent. One method, and it does the
// whole job in one order: deauthenticate with the control plane while the
// credentials are still on disk, tear the live session down, then remove the
// enrollment.
type LogoutController interface {
	Logout(ctx context.Context, req LogoutRequest) (LogoutResponse, error)
}
