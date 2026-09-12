package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/router"
)

// TestSelectAndProbe_PinnedBusyWithIdlePeersNamesThePin is the shape the
// 0.0.3-rc6 fleet actually produced (waired-agent#1303, S3): the computer
// the turn was pinned to was full, other computers were idle and ready, and
// the person was told "every matching mesh peer is at capacity".
//
// Both halves matter. The turn must NOT be answered by another computer —
// a pin is not substituted (owner ruling 2026-05-19, kept by
// docs/decisions/20260819/1900-routing-selects-a-node-not-a-model.md) — and
// the refusal must name the one computer that was considered.
//
// PRODUCT CONTRACT, ratifying source waired-agent#1303 (owner ruling
// 2026-09-12: keep the 503 and the retry, fix the sentence).
func TestSelectAndProbe_PinnedBusyWithIdlePeersNamesThePin(t *testing.T) {
	rtPin := &stubRT{status: 200, body: readyBody(1, 1)}  // full
	rtFree := &stubRT{status: 200, body: readyBody(0, 4)} // idle and ready
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("an idle peer answered a turn pinned to a busy one")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(upstreamSrv.Close)

	sel := &phase8MultiSelector{cands: []router.Candidate{
		phase8PinnedCandidate("peer-pin"),
		phase8RemoteCandidate("peer-free"),
	}}
	h := buildPhase8Gateway(t, sel,
		map[string]http.RoundTripper{"peer-pin": rtPin, "peer-free": rtFree}, upstreamSrv.URL)

	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)

	body := `{"model":"qwen3-8b-instruct","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body=%s)", resp.StatusCode, raw)
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &env)
	if env.Error.Code != "waired_pinned_peer_busy" {
		t.Errorf("error.code = %q, want waired_pinned_peer_busy (body=%s)", env.Error.Code, raw)
	}
	if strings.Contains(env.Error.Message, "every matching mesh peer") {
		t.Errorf("the refusal names the whole mesh while one computer was pinned: %q", env.Error.Message)
	}
	if !strings.Contains(env.Error.Message, "peer-pin") {
		t.Errorf("the refusal does not name the pinned computer: %q", env.Error.Message)
	}
	if !strings.Contains(env.Error.Message, "1 of 1") {
		t.Errorf("the refusal does not say how full the computer was: %q", env.Error.Message)
	}
	if peer := resp.Header.Get(HeaderInferencePeer); peer != "peer-pin" {
		t.Errorf("%s = %q, want peer-pin", HeaderInferencePeer, peer)
	}
	if le := resp.Header.Get(HeaderLocalError); le != LocalErrorPinnedPeerBusy {
		t.Errorf("%s = %q, want %q", HeaderLocalError, le, LocalErrorPinnedPeerBusy)
	}
}

// pinnedCapacitySelector is full for the first fullFor rounds and then
// hands back a ready pinned candidate, so the brief queue can be observed
// end to end. It records every round it was asked for.
type pinnedCapacitySelector struct {
	fullFor int
	calls   []router.Request
	sel     router.Selection
}

func (p *pinnedCapacitySelector) Select(_ context.Context, r router.Request) (router.Selection, error) {
	p.calls = append(p.calls, r)
	if len(p.calls) <= p.fullFor {
		return router.Selection{}, &router.PinnedPeerBusyError{
			PeerDisplayID: "peer-pin", PeerName: "sv-mag", CapacityUsed: 1, CapacityTotal: 1,
		}
	}
	return p.sel, nil
}

func (p *pinnedCapacitySelector) SelectK(_ context.Context, r router.Request, _ int) ([]router.Candidate, error) {
	p.calls = append(p.calls, r)
	if len(p.calls) <= p.fullFor {
		return nil, &router.PinnedPeerBusyError{
			PeerDisplayID: "peer-pin", PeerName: "sv-mag", CapacityUsed: 1, CapacityTotal: 1,
		}
	}
	c := router.NewLocalCandidate(p.sel)
	c.Pinned = true
	return []router.Candidate{c}, nil
}

// TestSelectAndProbe_PinnedBusyStillQueues is the guard on the owner's
// ruling of 2026-09-12: naming the refusal must not shorten the wait that
// makes the turn succeed. Measured on the fleet (S2): the pin's own turn
// ended after 32 s and the queued request was then served, 200.
//
// PRODUCT CONTRACT, ratifying source waired-agent#1303.
func TestSelectAndProbe_PinnedBusyStillQueues(t *testing.T) {
	sel := &pinnedCapacitySelector{fullFor: 1, sel: capacityTestSelection()}
	h := NewHandlerSet(Deps{Selector: sel})

	got, err := h.selectAndProbe(context.Background(), router.Request{Model: "qwen3.5-2b"}, 0)
	if err != nil {
		t.Fatalf("selectAndProbe: %v — a pin that freed its slot on the second round must be used", err)
	}
	if got.Sel.ModelID != "qwen3.5-2b" {
		t.Errorf("selection model = %q, want qwen3.5-2b", got.Sel.ModelID)
	}
	if len(sel.calls) != 2 {
		t.Fatalf("SelectK calls = %d, want 2 — the named refusal escaped the brief queue", len(sel.calls))
	}
}

// TestSelectAndProbe_PinnedBusyKeepsItsWordingAfterTheQueue: when the slot
// never frees, the sentence the person reads is still the pinned one, not
// the generic sentinel the retry loop could have re-derived.
func TestSelectAndProbe_PinnedBusyKeepsItsWordingAfterTheQueue(t *testing.T) {
	sel := &pinnedCapacitySelector{fullFor: 99, sel: capacityTestSelection()}
	h := NewHandlerSet(Deps{Selector: sel})

	_, err := h.selectAndProbe(context.Background(), router.Request{Model: "qwen3.5-2b"}, 0)
	if !errors.Is(err, router.ErrAllPeersOverloaded) {
		t.Fatalf("err = %v, want it to unwrap to ErrAllPeersOverloaded", err)
	}
	if _, ok := router.PinnedPeerBusy(err); !ok {
		t.Fatalf("err = %v, want the pinned wording to survive the queue", err)
	}
	if strings.Contains(err.Error(), "every matching mesh peer") {
		t.Errorf("err = %q, want it to name the pinned computer", err)
	}
}

// TestPinnedProbeFailure_UnansweredButTheMeshSaysItIsUp: a pin is not named
// unreachable on the strength of a probe that learned nothing.
//
// Measured 2026-09-12 (waired-agent#1303): seconds after pc-mbp14-m5's own
// daemon restarted, its probes to all three peers went unanswered while its
// own mesh snapshot still listed every one of them reachable (sv-macmini at
// rtt 53 ms). The pinned turn was refused at 2.0 s with "the computer this
// turn is pinned to, sv-macmini.local-1, is not answering" — a claim about
// the peer, caused by this computer. With the pin exempted the same round
// says what happened: no peer answered its readiness probe from this
// computer.
//
// PRODUCT CONTRACT. The rule is the one
// docs/decisions/20260906/0200-the-wait-reads-the-observer-the-mesh-already-has.md
// set for the post-dispatch wait, applied one layer earlier: when this
// device's own probe learns nothing, read the observation the mesh already
// carries — that peer's live call to its own engine.
func TestPinnedProbeFailure_UnansweredButTheMeshSaysItIsUp(t *testing.T) {
	pinned := phase8PinnedCandidate("peer-pin")
	unanswered := probedSelection{
		cands:        []router.Candidate{pinned},
		probeResults: []router.ProbeResult{{Outcome: router.ProbeTransportError}},
	}

	t.Run("the mesh says the engine is live: keep waiting", func(t *testing.T) {
		h := NewHandlerSet(Deps{PeerFacts: func(string) PeerFacts {
			return PeerFacts{Name: "sv-macmini", EngineLive: true, Known: true}
		}})
		if err := h.pinnedProbeFailure(unanswered); err != nil {
			t.Fatalf("err = %v, want nil — the mesh's own view contradicts \"not answering\"", err)
		}
	})

	t.Run("the mesh says the engine is not live: name it", func(t *testing.T) {
		h := NewHandlerSet(Deps{PeerFacts: func(string) PeerFacts {
			return PeerFacts{Name: "sv-macmini", EngineLive: false, Known: true}
		}})
		err := h.pinnedProbeFailure(unanswered)
		var pin *router.PinnedPeerUnreachableError
		if !errors.As(err, &pin) {
			t.Fatalf("err = %v, want PinnedPeerUnreachableError", err)
		}
	})

	t.Run("this device cannot tell: name it, as it always did", func(t *testing.T) {
		// No PeerFacts dep, or a peer the snapshot does not carry. The
		// second opinion does not exist, so nothing changes.
		h := NewHandlerSet(Deps{})
		err := h.pinnedProbeFailure(unanswered)
		var pin *router.PinnedPeerUnreachableError
		if !errors.As(err, &pin) {
			t.Fatalf("err = %v, want PinnedPeerUnreachableError", err)
		}
	})

	t.Run("a peer that ANSWERED not-ready is still named", func(t *testing.T) {
		// The exemption is only about an unanswered probe. A peer that
		// answered "my engine is not ready" told us something, and a live
		// mesh entry does not overturn it.
		h := NewHandlerSet(Deps{PeerFacts: func(string) PeerFacts {
			return PeerFacts{Name: "sv-macmini", EngineLive: true, Known: true}
		}})
		answered := probedSelection{
			cands: []router.Candidate{pinned},
			probeResults: []router.ProbeResult{{
				Outcome: router.ProbeOK,
				Status:  router.HealthStatus{EngineReady: false, ShareEnabled: true},
			}},
		}
		err := h.pinnedProbeFailure(answered)
		var pin *router.PinnedPeerUnreachableError
		if !errors.As(err, &pin) {
			t.Fatalf("err = %v, want PinnedPeerUnreachableError", err)
		}
	})
}
