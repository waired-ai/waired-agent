package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/router"
)

// The capacity queue (waired-agent#786).
//
// A mesh peer advertises cap=1. While its slot is busy, the router's
// admission filter rejects every other request before any probe runs,
// and that rejection used to leave selectAndProbe immediately — so each
// of Claude Code's concurrent sub-requests got a 503 with latency_ms 0
// in the journal while the peer was mid-stream. The brief queue that
// exists for exactly this state (selectAndProbe step 4) never saw them.
//
// These tests pin the two halves: the rejection is retryable, and how
// long the pipeline is willing to hold the caller depends on whether the
// leg has a fallback to take.

// capacitySelector is the Selector-side fake. It returns the router's
// real capacity error — wrapped the way endpoint_router.meshSelectionError
// wraps it, so the errors.Is unwrap is genuinely exercised rather than
// matched against a bare sentinel — for the first fullFor calls, then
// commits.
//
// It records every request it was handed (not just a count): a fake that
// drops its arguments cannot fail the case where the retry re-selects
// with a different request than the first round.
type capacitySelector struct {
	fullFor int
	sel     router.Selection
	calls   []router.Request
}

func (c *capacitySelector) Select(_ context.Context, req router.Request) (router.Selection, error) {
	c.calls = append(c.calls, req)
	return c.sel, nil
}

func (c *capacitySelector) SelectK(_ context.Context, req router.Request, _ int) ([]router.Candidate, error) {
	c.calls = append(c.calls, req)
	if len(c.calls) <= c.fullFor {
		return nil, fmt.Errorf("%w: %q", router.ErrAllPeersOverloaded, req.Model)
	}
	return []router.Candidate{router.NewLocalCandidate(c.sel)}, nil
}

// refusingSelector always answers with the given error, so a test can
// tell "retried and then gave up" from "returned on the first round".
type refusingSelector struct {
	err   error
	calls int
}

func (r *refusingSelector) Select(_ context.Context, _ router.Request) (router.Selection, error) {
	r.calls++
	return router.Selection{}, r.err
}

func (r *refusingSelector) SelectK(_ context.Context, _ router.Request, _ int) ([]router.Candidate, error) {
	r.calls++
	return nil, r.err
}

func capacityTestSelection() router.Selection {
	return router.Selection{
		EndpointID:    "local-ollama-qwen3.5-2b",
		ModelID:       "qwen3.5-2b",
		EngineModel:   "qwen3.5:2b-q4_K_M",
		Runtime:       "ollama",
		ExecutionMode: "local",
		Release:       func() {},
	}
}

// TestSelectAndProbe_CapacityRejectionIsRetried is the #786 regression.
// The selector is full for one round and free on the next; before the
// fix the first rejection returned straight to the client.
func TestSelectAndProbe_CapacityRejectionIsRetried(t *testing.T) {
	sel := &capacitySelector{fullFor: 1, sel: capacityTestSelection()}
	h := NewHandlerSet(Deps{Selector: sel})

	got, err := h.selectAndProbe(context.Background(), router.Request{Model: "qwen3.5-2b"}, 0)
	if err != nil {
		t.Fatalf("selectAndProbe: %v — a peer that freed its slot on the second round must be used", err)
	}
	if got.Sel.ModelID != "qwen3.5-2b" {
		t.Errorf("selection model = %q, want qwen3.5-2b", got.Sel.ModelID)
	}
	if len(sel.calls) != 2 {
		t.Fatalf("SelectK calls = %d, want 2 — the capacity rejection did not reach the brief queue", len(sel.calls))
	}
	// The fake records real arguments, so the retry can be checked for
	// asking the same question rather than merely happening.
	if sel.calls[1].Model != sel.calls[0].Model {
		t.Errorf("retry re-selected for %q, first round asked for %q",
			sel.calls[1].Model, sel.calls[0].Model)
	}
}

// TestSelectAndProbe_CapacityRejectionKeepsItsError pins that giving up
// reports the SELECTOR's error, message and all. Re-deriving it from
// probe results would replace `router: every matching mesh peer is at
// capacity: "qwen3.5-2b"` — which names the model the operator asked
// for — with a bare sentinel, or worse with ErrPeersDidNotAnswer, which
// is a claim about probes that never ran (waired-agent#624).
func TestSelectAndProbe_CapacityRejectionKeepsItsError(t *testing.T) {
	sel := &capacitySelector{fullFor: 99, sel: capacityTestSelection()}
	h := NewHandlerSet(Deps{Selector: sel})

	_, err := h.selectAndProbe(context.Background(), router.Request{Model: "qwen3.5-2b"}, 0)
	if !errors.Is(err, router.ErrAllPeersOverloaded) {
		t.Fatalf("err = %v, want ErrAllPeersOverloaded", err)
	}
	if errors.Is(err, router.ErrPeersDidNotAnswer) {
		t.Error("a capacity rejection was reported as an unanswered mesh")
	}
	if want := `"qwen3.5-2b"`; !strings.Contains(err.Error(), want) {
		t.Errorf("err = %q, want it to still name the model (%s)", err, want)
	}
	if sel.calls != nil && len(sel.calls) != probeAttempts {
		t.Errorf("SelectK calls = %d, want %d (probeAttempts) with no capacity budget",
			len(sel.calls), probeAttempts)
	}
}

// TestSelectAndProbe_NonTransientSelectionErrorsAreNotQueued keeps the
// brief queue from delaying answers it cannot change. These are verdicts
// about the request or the configuration, not about how busy anything is.
func TestSelectAndProbe_NonTransientSelectionErrorsAreNotQueued(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"model not found", router.ErrModelNotFound},
		{"capability not met", router.ErrCapabilityNotMet},
		{"model not ready", router.ErrModelNotReady},
		{"hardware insufficient", router.ErrHardwareInsufficient},
		{"runtime not installed", router.ErrRuntimeNotInstalled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sel := &refusingSelector{err: fmt.Errorf("%w: %q", tc.err, "qwen3.5-2b")}
			h := NewHandlerSet(Deps{Selector: sel})

			_, err := h.selectAndProbe(context.Background(), router.Request{Model: "qwen3.5-2b"}, time.Minute)
			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want %v", err, tc.err)
			}
			if sel.calls != 1 {
				t.Errorf("SelectK calls = %d, want 1 — this verdict does not change by waiting", sel.calls)
			}
		})
	}
}

// TestQueueAgain is the decision itself, as a table over the facts that
// decide it. The pipeline's sleeps are real time; this is not.
func TestQueueAgain(t *testing.T) {
	for _, tc := range []struct {
		name         string
		attempt      int
		elapsed      time.Duration
		capacityWait time.Duration
		capacityFull bool
		want         bool
	}{
		// The waired-agent#624 retry rounds are unconditional: a probe
		// that did not come back is not evidence about the peer.
		{"first round, no budget", 1, 0, 0, false, true},
		{"first round, unreachable mesh", 1, 0, 0, false, true},
		{"last of the base rounds, no budget", probeAttempts, time.Second, 0, true, false},
		// Past them, only capacity is worth waiting out, and only with a
		// budget to spend.
		{"capacity but no budget", probeAttempts, time.Second, 0, true, false},
		{"budget but not capacity", probeAttempts, time.Second, 20 * time.Second, false, false},
		{"capacity inside the budget", probeAttempts, time.Second, 20 * time.Second, true, true},
		{"capacity at the budget", probeAttempts, 20 * time.Second, 20 * time.Second, true, false},
		{"capacity past the budget", probeAttempts, 25 * time.Second, 20 * time.Second, true, false},
		// One more sleep must fit, or the caller is held past the promise.
		{"no room for another sleep", probeAttempts, 19900 * time.Millisecond, 20 * time.Second, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := queueAgain(tc.attempt, tc.elapsed, tc.capacityWait, tc.capacityFull)
			if got != tc.want {
				t.Errorf("queueAgain(%d, %v, %v, %v) = %v, want %v",
					tc.attempt, tc.elapsed, tc.capacityWait, tc.capacityFull, got, tc.want)
			}
		})
	}
}

// TestCapacityQueueBudget pins which legs may wait and for how long.
//
// A record of today's behaviour, not a product contract: the ceiling is
// borrowed from the configured pre-first-byte window because that number
// already exists and already says how long a first byte may take. The
// arming rule is the load-bearing half — a leg the intercept can reroute
// (auto) must keep failing fast, or a turn with somewhere else to go
// waits for nothing.
func TestCapacityQueueBudget(t *testing.T) {
	deps := Deps{TTFBBudget: func(class string) time.Duration {
		if class == "sub" {
			return 20 * time.Second
		}
		return 60 * time.Second
	}}
	for _, tc := range []struct {
		name            string
		deps            Deps
		fallbackAllowed bool
		class           string
		want            time.Duration
	}{
		{"waired route, main", deps, false, "main", 60 * time.Second},
		{"waired route, subagent", deps, false, "sub", 20 * time.Second},
		{"auto route never queues", deps, true, "main", 0},
		{"auto route, subagent", deps, true, "sub", 0},
		{"no budget configured", Deps{}, false, "main", 0},
		{"budget disabled for the class", Deps{TTFBBudget: func(string) time.Duration { return 0 }}, false, "main", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", nil)
			if tc.fallbackAllowed {
				r.Header.Set(HeaderFallbackAllowed, "1")
			}
			if got := capacityQueueBudget(tc.deps, r, tc.class); got != tc.want {
				t.Errorf("capacityQueueBudget = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRetryAfterForCapacity: the hint must never undercut the wait that
// just failed. Five seconds stays the floor it has always been.
func TestRetryAfterForCapacity(t *testing.T) {
	for _, tc := range []struct {
		queued time.Duration
		want   string
	}{
		{0, "5"},
		{250 * time.Millisecond, "5"},
		{5 * time.Second, "5"},
		{5500 * time.Millisecond, "6"},
		{20 * time.Second, "20"},
		{60 * time.Second, "60"},
	} {
		if got := retryAfterForCapacity(tc.queued); got != tc.want {
			t.Errorf("retryAfterForCapacity(%v) = %q, want %q", tc.queued, got, tc.want)
		}
	}
}

// TestRoundWasCapacityFull separates "peers answered and were busy" from
// "nothing answered", because only the first is worth waiting out.
func TestRoundWasCapacityFull(t *testing.T) {
	for _, tc := range []struct {
		name    string
		results []router.ProbeResult
		want    bool
	}{
		{"nothing was asked", nil, false},
		{"every probe was a transport error", []router.ProbeResult{
			{Outcome: router.ProbeTransportError},
			{Outcome: router.ProbeTransportError},
		}, false},
		{"a peer answered and was full", []router.ProbeResult{
			{Outcome: router.ProbeTransportError},
			{Outcome: router.ProbeOK},
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := roundWasCapacityFull(tc.results); got != tc.want {
				t.Errorf("roundWasCapacityFull = %v, want %v", got, tc.want)
			}
		})
	}
}
