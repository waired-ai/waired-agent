package gateway

import (
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// TestFailClosedExitsFor is waired-agent#1303's sub-agent half: the way out
// a fail-closed message offers has to be a control that reaches the leg
// that failed.
//
// Product contract. The main-class exits are ratified by
// docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md
// decision 4; that sub-agent placement is one documented switch, and not
// /model, by the same record's decision 6 and by
// docs/decisions/20260906/0343-subagents-are-placed-by-the-documented-knob.md.
func TestFailClosedExitsFor(t *testing.T) {
	main := failClosedExitsFor("")
	if main != failClosedExits {
		t.Errorf("the main-class exits moved: %q", main)
	}
	// Byte-identical to what shipped: docs-site quotes it, and the
	// intercept's own route test pins it.
	if !strings.Contains(main, "/model") {
		t.Errorf("the main-class exits stopped naming /model: %q", main)
	}

	sub := failClosedExitsFor(state.ClaudeClassSub)
	if strings.Contains(sub, "/model") {
		t.Errorf("a sub-agent leg was sent to /model, which does not place sub-agents: %q", sub)
	}
	if !strings.Contains(sub, "waired claude subagents") {
		t.Errorf("the sub-agent exits do not name the switch that places them: %q", sub)
	}
	if !strings.Contains(sub, "waired doctor") {
		t.Errorf("the sub-agent exits dropped the diagnosis exit: %q", sub)
	}
}

// TestFailClosedMessage_CarriesTheClassExits checks the seam is actually
// wired: a class reaches the sentence, not just the helper.
func TestFailClosedMessage_CarriesTheClassExits(t *testing.T) {
	got := failClosedMessage(state.ClaudeClassSub, "router: nothing here can serve this")
	if strings.Contains(got, "/model") {
		t.Errorf("failClosedMessage ignored the class: %q", got)
	}
	if !strings.HasPrefix(got, "Nothing here can serve this.") {
		t.Errorf("the detail rendering changed: %q", got)
	}
}

// TestPreCommitAbortMessage_StillBusy: the wait that ends at its ceiling
// while the peer is working says the computer is busy. "Produced no
// response" is true of the bytes and false about the machine, and it is
// what a subagent leg cut at 20 s told the reader on real hardware
// (waired-agent#1303, S7).
func TestPreCommitAbortMessage_StillBusy(t *testing.T) {
	got := preCommitAbortMessage("peer sv-xps15", LocalErrorPeerStillBusy, 100*time.Second)
	if !strings.Contains(got, "busy") {
		t.Errorf("message = %q, want it to say the computer is busy", got)
	}
	if strings.Contains(got, "produced no response") {
		t.Errorf("message = %q, still gives the old account of a working peer", got)
	}
	if !strings.Contains(got, "sv-xps15") {
		t.Errorf("message = %q, want it to name the computer", got)
	}
	// The other three arms are unchanged.
	if strings.Contains(preCommitAbortMessage("peer x", LocalErrorPeerTTFBTimeout, time.Second), "busy") {
		t.Error("a silent ceiling now claims the peer was busy")
	}
}
