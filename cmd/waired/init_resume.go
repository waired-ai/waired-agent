package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/waired-ai/waired-agent/internal/controlurl"
	"github.com/waired-ai/waired-agent/internal/identity"
	"github.com/waired-ai/waired-agent/internal/management"
)

// `waired init` on a device that is already signed in used to be an
// error — and on Windows it was the ONLY outcome, because the CLI
// resolved a state dir the daemon does not use, found no identity there,
// asked for a plain login, and reported the daemon's idempotent no-op
// ("active, no session id") as a protocol failure (#313). NAVI hands
// operators that exact command to resume a stuck setup, so setup was
// unresumable on Windows by any documented means.
//
// The model is `tailscale up`: the command is idempotent. An auth key is
// not spent while the existing credentials are valid (tailscale#19501 —
// "if valid state exists, reuse it"), re-authenticating is an explicit
// --force-reauth, and — unlike tailscale#7995, where the key is dropped
// in silence — an unused key is said out loud.

// daemonIdentityTimeout bounds the enrollment probe. It runs before the
// route is chosen, so an absent daemon must cost a connection refusal,
// not a wait.
const daemonIdentityTimeout = 3 * time.Second

// identityPath is the management route that reports what the daemon is
// enrolled as. Socket-only: it is not in tcpReadRoutes (#785, waired#836).
const identityPath = "/waired/v1/identity"

// daemonIdentity asks the daemon what it is enrolled as. A nil answer
// means "no answer" — no daemon, a daemon too old to serve the route, or
// a malformed reply — and every caller must read that as "unknown",
// never as "not enrolled": the whole point is that the CLI's own view of
// the disk is the thing under suspicion.
//
// A package var so tests can answer without a daemon.
// The read goes through mgmtReadRoute — the socket, with a loopback-TCP
// fallback. /waired/v1/identity is not in the daemon's tcpReadRoutes
// allow-list, so over plain TCP it answers 403 while the socket is bound,
// and this function returned nil on every call: the daemon was never
// actually asked (#785). A parse failure maps to nil like every other
// failure, so the contract above stays exact.
var daemonIdentity = func(mgmt string) *management.IdentityView {
	target, cl, err := mgmtReadRoute(mgmtURL(mgmt, identityPath), daemonIdentityTimeout)
	if err != nil {
		return nil
	}
	resp, err := cl.Get(target)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var v management.IdentityView
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil
	}
	return &v
}

// identityFromView renders a daemon-reported enrollment as the local
// record the renew summary prints. Returns nil unless the daemon says it
// is enrolled, so callers can use it exactly where identity.Load's own
// nil means "nothing here".
//
// It is deliberately NOT written to disk: the daemon owns that file, and
// this is a read of the daemon's answer, not a copy of its state.
func identityFromView(v *management.IdentityView) *identity.Identity {
	if v == nil || !v.Enrolled {
		return nil
	}
	return &identity.Identity{
		DeviceID:     v.DeviceID,
		DeviceName:   v.DeviceName,
		NetworkName:  v.NetworkName,
		AccountEmail: v.AccountEmail,
		ControlURL:   v.ControlURL,
	}
}

// reauthWanted decides whether this run should re-authenticate rather
// than resume.
//
// Two ways to say yes, and no third: the operator asked
// (--force-reauth), or the credentials are what is broken — auto-refresh
// has given up and only a fresh sign-in can fix it, which is what makes
// `waired init` the documented recovery for a reauth_required device.
// Everything else resumes, because re-signing a device that is signed in
// rotates its tokens for nothing.
func reauthWanted(force bool, v *management.IdentityView) bool {
	if force {
		return true
	}
	return v != nil && v.Enrolled && v.AuthState == management.AuthStateReauthRequired
}

// resumeLines is what the terminal says when it finds the device already
// signed in. Kept as a pure function so the copy is testable and lives
// in one place.
func resumeLines(accountEmail string, authKeyGiven bool) []string {
	head := "This device is already signed in — resuming setup."
	if accountEmail != "" {
		head = fmt.Sprintf("Already signed in as %s — resuming setup.", accountEmail)
	}
	lines := []string{head}
	if authKeyGiven {
		// tailscale#7995 is the counter-example: dropping the key in
		// silence leaves an operator believing they switched something.
		lines = append(lines, dim("The auth key was not used. Pass --force-reauth to sign in again with it."))
	}
	return lines
}

// accountEmailFromView is nil-safe sugar for the resume notice's one
// use of the daemon's answer.
func accountEmailFromView(v *management.IdentityView) string {
	if v == nil {
		return ""
	}
	return v.AccountEmail
}

// controlForRenew decides which control plane a RENEWING device should
// talk to, given what this run resolved, where that came from, and what
// the device is already enrolled to.
//
// Nobody on this computer said which control plane to use, so the one it
// is already enrolled to wins — that is not a switch, it is the absence of
// a request to switch (waired-agent#800). Losing the state dir loses
// agent.env with it (it lives inside the state dir on macOS and Windows),
// and controlurl.Resolve then falls through to the production default.
//
// Only the built-in default defers. An explicit --control or
// $WAIRED_CONTROL_URL is a request, and a request to move a device to
// another control plane is still refused by the caller.
func controlForRenew(resolved string, src controlurl.Source, enrolled string) string {
	if src == controlurl.SourceBuiltin && enrolled != "" {
		return enrolled
	}
	return resolved
}
