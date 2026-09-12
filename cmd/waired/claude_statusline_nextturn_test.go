package main

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
)

// TestRenderStatusline_TheThirdState is the item waired-agent#1129 stayed
// open on after PR #1154.
//
// Product contract, ratifying source waired-agent#1129 (the two comments of
// 2026-08-29 and 2026-08-30 listing it as the remaining work): the footer
// and the turn must not contradict each other. It read this device's engine
// health and "the mesh is reachable", and neither consults the operator's
// minimum model class — so on an engine-less host under a floor that
// excluded every peer it printed a GREEN "on Waired (peer …)" while every
// turn was refused as too small. Reproduced on the rc5 and rc6 fleets.
//
// Wording alone could not fix it: noWairedTargetReason is reached only when
// BOTH booleans are false, and the green branch was taken because one of
// them was true.
func TestRenderStatusline_TheThirdState(t *testing.T) {
	// The measured shape: no local engine, a reachable mesh, and a floor
	// that excluded every peer in it.
	engineless := "disabled"
	reachable := meshView{known: true, reachable: true, names: map[string]string{}}

	t.Run("without the daemon's answer it renders as it always did", func(t *testing.T) {
		// An older agent does not compute it. The line must claim nothing
		// new rather than inventing a verdict.
		got := renderStatusline(management.ClaudeRoutingState{}, engineless, nil, reachable, "")
		if !strings.Contains(got, "on Waired") {
			t.Errorf("the fallback rendering moved: %q", got)
		}
	})

	t.Run("the daemon says nothing can take the turn", func(t *testing.T) {
		route := management.ClaudeRoutingState{
			NextTurn: &management.ClaudeNextTurn{
				CanServe: false,
				Reason:   "no computer runs a large model",
			},
		}
		got := renderStatusline(route, engineless, nil, reachable, "")
		if strings.Contains(got, "on Waired") {
			t.Errorf("still green while every turn would be refused: %q", got)
		}
		if !strings.Contains(got, "cannot answer") {
			t.Errorf("the line does not say the turn would fail: %q", got)
		}
		// The floor is the operator's own setting, so the footer names the
		// setting rather than reporting a fault — and "no peer", which the
		// two-axis wording would have said, is not what happened.
		if !strings.Contains(got, "no computer runs a large model") {
			t.Errorf("the daemon's reason did not reach the line: %q", got)
		}
		if strings.Contains(got, "no peer") {
			t.Errorf("the old two-axis wording survived: %q", got)
		}
	})

	t.Run("the daemon says this device can", func(t *testing.T) {
		route := management.ClaudeRoutingState{
			NextTurn: &management.ClaudeNextTurn{CanServe: true, Where: "local"},
		}
		// Health says disabled, but the daemon's ordering says this turn
		// has somewhere to go. The daemon is the one that decides.
		got := renderStatusline(route, engineless, nil, reachable, "")
		if !strings.Contains(got, "on Waired") {
			t.Errorf("the line refused a turn the daemon says it can serve: %q", got)
		}
	})

	t.Run("a busy computer stays green and says so", func(t *testing.T) {
		// waired-agent#1303: busy is not broken. The turn is retried onto
		// that computer, so the colour must not change — but "why is this
		// slow" deserves an answer.
		route := management.ClaudeRoutingState{
			LastServedBy: "dev_peer",
			NextTurn: &management.ClaudeNextTurn{
				CanServe: true, Where: "remote", Peer: "dev_peer",
				Reason: "every computer is busy",
			},
		}
		mesh := meshView{known: true, reachable: true, names: map[string]string{"dev_peer": "sv-macmini"}}
		got := renderStatusline(route, "ready", nil, mesh, "")
		if !strings.Contains(got, "on Waired") {
			t.Errorf("a busy computer was rendered as a fault: %q", got)
		}
		if !strings.Contains(got, "every computer is busy") {
			t.Errorf("the line does not say why the turn will be slow: %q", got)
		}
		if !strings.Contains(got, "sv-macmini") {
			t.Errorf("the line stopped naming the computer: %q", got)
		}
	})

	t.Run("a session on an Anthropic model is unaffected", func(t *testing.T) {
		// sessionSide decides first and always has: the footer hangs under
		// ONE session, and a session that picked Opus is not on Waired no
		// matter what this device could serve (waired-agent#1037).
		route := management.ClaudeRoutingState{
			NextTurn: &management.ClaudeNextTurn{CanServe: false, Reason: "every computer is busy"},
		}
		got := renderStatusline(route, engineless, nil, reachable, "claude-opus-5")
		if !strings.Contains(got, "Anthropic") {
			t.Errorf("the session's own model stopped deciding: %q", got)
		}
	})
}

// TestCannotAnswerReason keeps the fallback honest: the daemon's account
// wins when there is one, and the shipped two-axis wording is what a
// reader gets when there is not.
func TestCannotAnswerReason(t *testing.T) {
	mesh := meshView{known: true, reachable: false, names: map[string]string{}}
	if got := cannotAnswerReason(management.ClaudeRoutingState{}, "disabled", mesh); got != "local disabled, no peer" {
		t.Errorf("fallback = %q, want the shipped two-axis wording", got)
	}
	route := management.ClaudeRoutingState{
		NextTurn: &management.ClaudeNextTurn{Reason: "the pinned computer is not answering"},
	}
	if got := cannotAnswerReason(route, "disabled", mesh); got != "the pinned computer is not answering" {
		t.Errorf("with an answer = %q, want the daemon's", got)
	}
	// An answer with no reason is not an account of anything; fall back.
	route.NextTurn = &management.ClaudeNextTurn{CanServe: false}
	if got := cannotAnswerReason(route, "disabled", mesh); got != "local disabled, no peer" {
		t.Errorf("empty reason = %q, want the fallback", got)
	}
}
