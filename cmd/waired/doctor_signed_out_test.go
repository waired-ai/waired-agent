package main

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
)

// The engine row's "not ready" arm used to end in "Turns addressed to Waired
// go to another of your computers" whatever state the computer was in. On one
// that had just been signed out that is false twice over: it is off the mesh,
// so there are no other computers, and the Claude gateway now answers a Waired
// model id with that reason rather than running it anywhere. Measured on macOS
// against 0.0.3-rc6, printed in the same doctor run as
// "Local Gateway — unreachable" (waired-agent#1310).
//
// PIN: product contract — a signed-out computer is not told its turns will be
// answered elsewhere (waired-ai/waired-agent#1310).
func TestEngineFindingSignedOutDoesNotPromiseAnotherComputer(t *testing.T) {
	state := management.AgentState{EngineReady: false}

	signedIn := engineFinding(state, true)
	if !strings.Contains(signedIn.Detail, "another of your computers") {
		t.Errorf("signed in: detail = %q, want the mesh sentence", signedIn.Detail)
	}

	signedOut := engineFinding(state, false)
	if strings.Contains(signedOut.Detail, "another of your computers") {
		t.Errorf("signed out: detail = %q, still promises a mesh this computer left", signedOut.Detail)
	}
	if !strings.Contains(signedOut.Detail, "signed out") {
		t.Errorf("signed out: detail = %q, does not name the state it is in", signedOut.Detail)
	}
	if !strings.Contains(signedOut.Detail, "waired init") {
		t.Errorf("signed out: detail = %q, does not name the command that undoes it", signedOut.Detail)
	}
}
