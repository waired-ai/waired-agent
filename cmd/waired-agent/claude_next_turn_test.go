package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/router"
)

// TestNextTurnReason pins the phrases the status-line segment prints inside
// parentheses.
//
// Record of today's behaviour for the wording: no ruling pins these words,
// and they are quoted in docs-site, so they move with it. What IS a product
// contract is that each case is distinguishable — the whole point of
// waired-agent#1129's third state was that one wording stood for several
// different situations, including one it was blind to.
func TestNextTurnReason(t *testing.T) {
	floorErr := &router.SizeFloorError{Err: router.ErrModelNotReady, Floor: "large", LocalArmOnlyFloor: true}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"the operator's floor", floorErr, "no computer runs a large model"},
		{"local inference off", router.ErrLocalInferenceOff, "local inference is off, and no other computer can answer"},
		{"a pin that is not answering", router.ErrPinnedPeerUnreachable, "the pinned computer is not answering"},
		{"every computer busy", router.ErrAllPeersOverloaded, "every computer is busy"},
		{"nobody answered the probe", router.ErrPeersDidNotAnswer, "no computer answered"},
		{"no such model anywhere", router.ErrModelNotFound, "no computer serves this model"},
	}
	seen := map[string]string{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := nextTurnReason(c.err)
			if got != c.want {
				t.Errorf("nextTurnReason = %q, want %q", got, c.want)
			}
			// Each situation must be tellable from the others: that is what
			// the old single wording could not do.
			if prev, dup := seen[got]; dup {
				t.Errorf("%q is also what %q produces", got, prev)
			}
			seen[got] = c.name
			// It shares one status-line row with whatever else the user
			// has there.
			if len(got) > 60 {
				t.Errorf("reason is %d chars, too long for a footer: %q", len(got), got)
			}
			// A footer is not a place for a package-prefixed diagnosis.
			if strings.Contains(got, "router:") || strings.Contains(got, "gateway:") {
				t.Errorf("reason leaks an internal prefix: %q", got)
			}
		})
	}

	t.Run("a model still on its way says so", func(t *testing.T) {
		// ErrModelNotReady means two different things and the footer has to
		// tell them apart: weights arriving (clears itself) versus nobody
		// serving it (never will).
		arriving := &router.ModelNotReadyError{
			ModelID: "qwen3.5-4b", State: "downloading", LocalArrivalAnswers: true,
		}
		if got := nextTurnReason(arriving); got != "the model is still downloading" {
			t.Errorf("nextTurnReason = %q, want the arriving wording", got)
		}
	})

	t.Run("an unforeseen error does not leak a diagnosis", func(t *testing.T) {
		long := errors.New("router: " + strings.Repeat("something very detailed ", 10))
		got := nextTurnReason(long)
		if len(got) > 60 || strings.Contains(got, "router:") {
			t.Errorf("nextTurnReason = %q", got)
		}
	})
}

// TestNextTurnForClaude_BusyIsNotCannot is the half the status line depends
// on, and it was unreachable when this was first written: the renderer has a
// green-but-busy form, and nothing produced it.
//
// Product contract, ratifying source waired-agent#1303 (owner ruling
// 2026-09-12: a busy pin keeps its retryable 503, because the retry is what
// carries the turn). A surface that painted "every computer is busy" red
// would send a person to fix something that is working.
func TestNextTurnForClaude_BusyIsNotCannot(t *testing.T) {
	// nextTurnAfterFailure is the seam: it is the whole of what
	// nextTurnForClaude does once selection has failed, so this reaches the
	// deciding line without standing a fleet up. An earlier version of this
	// test asserted `errors.Is(c.err, ErrAllPeersOverloaded)` in its own
	// body instead — which is a test of the standard library, and stayed
	// green when the product was mutated to CanServe: false.
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"every computer busy", router.ErrAllPeersOverloaded, true},
		{"a busy pin, which unwraps to it", &router.PinnedPeerBusyError{PeerName: "sv-mag"}, true},
		{"local inference off", router.ErrLocalInferenceOff, false},
		{"the operator's floor", &router.SizeFloorError{Err: router.ErrModelNotReady, Floor: "large"}, false},
		{"a pin that is not answering", router.ErrPinnedPeerUnreachable, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := nextTurnAfterFailure(c.err)
			if got == nil {
				t.Fatal("a failure with no segment leaves the footer nothing to print")
			}
			if got.CanServe != c.want {
				t.Errorf("CanServe = %v, want %v for %v", got.CanServe, c.want, c.err)
			}
			if got.Reason == "" {
				t.Error("a refusal with no reason leaves the footer with nothing to print")
			}
			// Busy keeps the green AND says why; the rest are red. Both
			// halves matter — a green with no reason is the contradiction
			// waired-agent#1129 was left open on.
			if c.want && got.Where != "" {
				t.Errorf("Where = %q, want empty: nothing was selected", got.Where)
			}
		})
	}
}
