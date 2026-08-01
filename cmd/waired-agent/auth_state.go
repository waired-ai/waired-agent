package main

import (
	"context"
	"sync/atomic"

	"github.com/waired-ai/waired-agent/internal/management"
)

// authGate is the session-scoped answer to "can this device still renew
// its Control-Plane credentials, and what should stop if it cannot".
//
// It exists because the two halves have to move together
// (waired-agent#318). When auto-refresh classifies a terminal error the
// device holds a bearer that will never be renewed again — every
// CP-facing loop keeps pushing with it and every push 401s, forever (the
// live incident logged ~107k such lines). Cancelling those loops is the
// fix; recording *why* is what lets the tray and `waired doctor` say
// "sign in again on this device" instead of showing an agent that has
// mysteriously gone quiet.
//
// Scope is one activation: `waired init` republishes a session with a
// fresh gate, so recovery needs no daemon restart.
type authGate struct {
	// quiesce cancels the context handed to the CP-facing loops only.
	// Local inference, the gateway and the management server run on the
	// session context and keep serving — an expired CP credential says
	// nothing about the host's ability to answer local requests, which
	// matches what the incident showed.
	quiesce context.CancelFunc

	state atomic.Pointer[authStatus]
}

type authStatus struct {
	state  string
	detail string
}

// attach derives the CP-facing context from the session context and
// arms the gate with its cancel.
//
// Split from construction because the refresh loop's terminal callback
// has to close over the gate before the session context exists, and a
// gate that could be armed only at construction would force the caller
// to thread a cancel by hand.
func (g *authGate) attach(sessionCtx context.Context) context.Context {
	cpCtx, cancel := context.WithCancel(sessionCtx)
	g.quiesce = cancel
	// A terminal error that landed during the pre-flight refresh — before
	// there was anything to cancel — still has to take effect.
	if s := g.state.Load(); s != nil && s.state != management.AuthStateOK {
		cancel()
	}
	return cpCtx
}

// markReauthRequired records the terminal cause and stops the CP-facing
// loops. Idempotent: the refresh loop fires it once, but a caller that
// double-fires must not double-cancel into a panic (context.CancelFunc
// is itself idempotent, so this only guards the recorded state).
func (g *authGate) markReauthRequired(cause error) {
	if g == nil {
		return
	}
	detail := ""
	if cause != nil {
		detail = cause.Error()
	}
	g.state.Store(&authStatus{state: management.AuthStateReauthRequired, detail: detail})
	if g.quiesce != nil {
		g.quiesce()
	}
}

// apply stamps the current auth state onto an IdentityView.
func (g *authGate) apply(v *management.IdentityView) {
	if g == nil {
		return
	}
	if s := g.state.Load(); s != nil {
		v.AuthState, v.AuthDetail = s.state, s.detail
		return
	}
	v.AuthState = management.AuthStateOK
}
