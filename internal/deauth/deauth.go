// Package deauth holds the single best-effort "tell the Control Plane this
// device is going away" routine, shared by every surface that needs it:
// `waired logout` (CLI), `waired-agent uninstall` (service teardown on
// Windows / macOS / Linux) and — via the CLI — the Debian package prerm.
//
// The contract is deliberately non-fatal. Deregistering from the CP must
// never block or fail an uninstall / logout: if the device isn't enrolled,
// has no usable token, or the CP is unreachable, callers proceed with their
// local teardown regardless and (where appropriate) warn the user.
package deauth

import (
	"context"

	"github.com/waired-ai/waired-agent/internal/controlclient"
	"github.com/waired-ai/waired-agent/internal/identity"
)

// Mode selects the server-side end state.
type Mode int

const (
	// ModeLogout deauthenticates the device (reauth_required): tokens are
	// revoked and peers drop it, but the row is preserved and recoverable
	// via `waired init`. This is `waired logout`'s default.
	ModeLogout Mode = iota
	// ModeRevoke moves the device to the terminal revoked state: removed
	// from the admin device list (tombstone), tokens revoked, dropped from
	// peers. Used at uninstall time, when the software is going away for
	// good.
	ModeRevoke
)

// Outcome reports what Deregister did, so callers can print the right
// message.
type Outcome int

const (
	// OutcomeSkipped means there was nothing to do: the device isn't
	// enrolled, has no pinned Control URL, or has no access token on disk.
	OutcomeSkipped Outcome = iota
	// OutcomeDone means the Control Plane call was made and succeeded (a 401
	// — already deregistered — also counts as success).
	OutcomeDone
)

// Deregister makes a best-effort server-side deauth (ModeLogout) or revoke
// (ModeRevoke) using the credentials on disk in stateDir. It never touches
// local files — callers own local wipe.
//
// It returns OutcomeSkipped (nil error) when there is nothing to call for
// (not enrolled / no token). On an attempted call it returns OutcomeDone
// with a nil error on success, or OutcomeDone with the transport/HTTP error
// so the caller can warn. The error is advisory only: callers must proceed
// with teardown regardless.
func Deregister(ctx context.Context, stateDir string, mode Mode) (Outcome, error) {
	id, err := identity.Load(stateDir)
	if err != nil || id == nil || id.ControlURL == "" {
		return OutcomeSkipped, nil // not enrolled / no CP pinned — nothing to do
	}
	access, _ := identity.LoadAccessToken(stateDir)
	if access == "" {
		return OutcomeSkipped, nil // no token to authenticate the call
	}
	cli := controlclient.NewWithBearer(id.ControlURL, func() string { return access })
	switch mode {
	case ModeRevoke:
		return OutcomeDone, cli.Revoke(ctx)
	default:
		return OutcomeDone, cli.Logout(ctx)
	}
}
