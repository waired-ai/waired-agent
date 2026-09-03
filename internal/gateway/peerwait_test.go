package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/internal/runtime"
)

// The peer leg waits while the peer works (waired-agent#1040).
//
// PIN: product contract — owner ruling 2026-08-28 on waired-agent#1040. A
// request that is being worked on is not cut off for being slow; what ends
// the wait is the peer saying it has stopped, the peer going silent, or the
// ceiling.

func okHealth(engineReady bool, used int) router.ProbeResult {
	return router.ProbeResult{
		Outcome: router.ProbeOK,
		Status: router.HealthStatus{
			EngineReady:   engineReady,
			CapacityTotal: 2,
			CapacityUsed:  used,
		},
	}
}

// The sentence a person reads carries a duration, not a clock reading.
//
// Two of the three ways the wait ends carry a MEASURED figure, and an
// unrounded Duration prints every digit it had.
func TestPreCommitAbortMessage(t *testing.T) {
	ragged := 550166123 * time.Microsecond // 9m10.166123s
	for _, tc := range []struct {
		name   string
		reason string
		waited time.Duration
		want   string
	}{
		{"a peer that said it had stopped", LocalErrorPeerStoppedServing, ragged,
			"the peer stopped working on this request after 9m10s"},
		{"a peer that went silent", LocalErrorPeerUnreachable, ragged,
			"the peer stopped answering after 9m10s"},
		// The configured cases are whole seconds already and must read
		// exactly as they did before rounding was added.
		{"the flat budget, unchanged", LocalErrorPeerTTFBTimeout, time.Minute,
			"peer produced no response within 1m0s"},
		{"the ceiling, unchanged", LocalErrorPeerTTFBTimeout, 30 * time.Minute,
			"peer produced no response within 30m0s"},
		// And a measured figure reaching the default branch (a peer with no
		// /healthz: the grace elapsed, which is not a round number of
		// nanoseconds by the time it is read).
		{"a measured wait on the default branch", LocalErrorPeerTTFBTimeout, time.Minute + 234*time.Millisecond,
			"peer produced no response within 1m0s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := preCommitAbortMessage("peer", tc.reason, tc.waited); got != tc.want {
				t.Errorf("preCommitAbortMessage = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClassifyPeerWork(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  router.ProbeResult
		want peerWorkVerdict
	}{
		{"engine up and a slot in use is the peer working", okHealth(true, 1), peerWorking},
		{"more than one slot is still working", okHealth(true, 2), peerWorking},
		// This device's request holds one of the peer's slots for as long as
		// it is being served, engine start included — so zero in use means
		// that request is not there any more.
		{"engine up and nothing in flight means our request is gone", okHealth(true, 0), peerIdle},
		{"engine down cannot be prefilling anything", okHealth(false, 1), peerIdle},
		{"a transport error is not a verdict about the peer",
			router.ProbeResult{Outcome: router.ProbeTransportError}, peerSilent},
		// An auth rejection is usually a live path and a refused envelope —
		// this device failing to ask, not the peer failing to work. Reading
		// it as "the peer vanished" would name the wrong machine in the
		// reroute notice, so it falls open to the flat budget instead.
		{"an auth error is this device failing to ask",
			router.ProbeResult{Outcome: router.ProbeAuthError}, peerUnknowable},
		// A peer with no /healthz cannot be asked, so nothing is learned and
		// the flat budget has to stand.
		{"a peer that predates the health endpoint is unknowable",
			router.ProbeResult{Outcome: router.ProbeLegacyPeer}, peerUnknowable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyPeerWork(tc.res); got != tc.want {
				t.Errorf("classifyPeerWork = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPeerLivenessFor(t *testing.T) {
	peer := router.Selection{Runtime: remoteRuntimePrefix + "peerX"}
	local := router.Selection{Runtime: "ollama"}
	health := func(context.Context, string) router.ProbeResult { return okHealth(true, 1) }
	ceiling := func(class string) time.Duration {
		if class == "sub" {
			return 0
		}
		return 10 * time.Minute
	}
	full := Deps{PeerWaitCeiling: ceiling, PeerHealth: health}

	for _, tc := range []struct {
		name    string
		deps    Deps
		sel     router.Selection
		class   string
		grace   time.Duration
		wantNil bool
		wantID  string
	}{
		{
			name: "a main peer leg with a ceiling is watched",
			deps: full, sel: peer, class: "main", grace: time.Minute,
			wantID: "peerX",
		},
		{
			// waired-agent#1041: the subagent class keeps the flat deadline.
			name: "the subagent class has no ceiling and stays flat",
			deps: full, sel: peer, class: "sub", grace: 20 * time.Second,
			wantNil: true,
		},
		{
			// The operator turned the deadline off; a watch would put a
			// bound back where they removed one.
			name: "a disabled budget is not turned into a watch",
			deps: full, sel: peer, class: "main", grace: 0,
			wantNil: true,
		},
		{
			name: "a ceiling no longer than the grace extends nothing",
			deps: Deps{PeerHealth: health, PeerWaitCeiling: func(string) time.Duration { return time.Minute }},
			sel:  peer, class: "main", grace: time.Minute,
			wantNil: true,
		},
		{
			name: "nothing to ask with",
			deps: Deps{PeerWaitCeiling: ceiling}, sel: peer, class: "main", grace: time.Minute,
			wantNil: true,
		},
		{
			// The production wiring: no test seam, a real adapter factory.
			name: "a peer adapter factory is enough to ask",
			deps: Deps{PeerWaitCeiling: ceiling, PeerAdapterFactory: func(string) (runtime.Adapter, error) {
				return nil, nil
			}},
			sel: peer, class: "main", grace: time.Minute,
			wantID: "peerX",
		},
		{
			name: "a local leg is not a peer leg",
			deps: full, sel: local, class: "main", grace: time.Minute,
			wantNil: true,
		},
		{
			name: "no ceiling wired at all (every surface but the intercept)",
			deps: Deps{PeerHealth: health}, sel: peer, class: "main", grace: time.Minute,
			wantNil: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := peerLivenessFor(tc.deps, tc.sel, tc.class, tc.grace)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("peerLivenessFor = %+v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("peerLivenessFor = nil, want a watch")
			}
			if got.PeerID != tc.wantID {
				t.Errorf("PeerID = %q, want %q", got.PeerID, tc.wantID)
			}
			if got.Grace != tc.grace {
				t.Errorf("Grace = %v, want the class budget %v", got.Grace, tc.grace)
			}
			if got.Ceiling <= got.Grace {
				t.Errorf("Ceiling %v must be longer than Grace %v", got.Ceiling, got.Grace)
			}
		})
	}
}

// scriptedPeer answers each health check from a list, repeating the last
// entry once the script runs out — so "keeps working" is one row rather than
// a count nobody can read.
type scriptedPeer struct {
	mu      sync.Mutex
	script  []router.ProbeResult
	asked   []string
	callNum int
}

func (s *scriptedPeer) probe(_ context.Context, deviceID string) router.ProbeResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, deviceID)
	i := s.callNum
	s.callNum++
	if i >= len(s.script) {
		i = len(s.script) - 1
	}
	return s.script[i]
}

func (s *scriptedPeer) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.callNum
}

func TestWatchPeerWhileItWorks(t *testing.T) {
	// Real durations, kept tiny: the watch has no clock of its own, and a
	// fake one here would test a seam rather than the loop.
	const (
		grace    = 10 * time.Millisecond
		interval = 5 * time.Millisecond
		ceiling  = 400 * time.Millisecond
	)
	working := okHealth(true, 1)
	idle := okHealth(true, 0)
	silent := router.ProbeResult{Outcome: router.ProbeTransportError}
	legacy := router.ProbeResult{Outcome: router.ProbeLegacyPeer}

	for _, tc := range []struct {
		name       string
		script     []router.ProbeResult
		wantReason string
		minCalls   int
	}{
		{
			// The whole point: a peer that keeps saying it is working is
			// never ended by this watch. It runs to the ceiling instead.
			name:       "a peer that keeps working is waited on to the ceiling",
			script:     []router.ProbeResult{working},
			wantReason: LocalErrorPeerTTFBTimeout,
			minCalls:   5,
		},
		{
			name:       "a peer that says it is working on nothing ends the wait",
			script:     []router.ProbeResult{working, working, idle},
			wantReason: LocalErrorPeerStoppedServing,
			minCalls:   3,
		},
		{
			name:       "an engine that went down ends the wait",
			script:     []router.ProbeResult{working, okHealth(false, 1)},
			wantReason: LocalErrorPeerStoppedServing,
			minCalls:   2,
		},
		{
			// One unanswered check is not evidence — a lost packet, a relay
			// hiccup — so it takes peerLivenessMisses in a row.
			name:       "one silent check is survivable, two in a row are not",
			script:     []router.ProbeResult{silent, working, silent, silent},
			wantReason: LocalErrorPeerUnreachable,
			minCalls:   4,
		},
		{
			// Fail open to the behaviour that shipped: the grace period IS
			// the flat budget, and it has just elapsed.
			name:       "a peer with no health endpoint falls back to the flat deadline",
			script:     []router.ProbeResult{legacy},
			wantReason: LocalErrorPeerTTFBTimeout,
			minCalls:   1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer := &scriptedPeer{script: tc.script}
			h := NewHandlerSet(Deps{PeerHealth: peer.probe})
			start := time.Now()
			reason, waited := h.watchPeerWhileItWorks(t.Context(),
				peerLiveness{PeerID: "peerX", Grace: grace, Ceiling: ceiling, Interval: interval}, start)

			if reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q (after %d checks)", reason, tc.wantReason, peer.calls())
			}
			if waited < grace {
				t.Errorf("waited = %v, want at least the grace period %v", waited, grace)
			}
			if got := peer.calls(); got < tc.minCalls {
				t.Errorf("asked the peer %d times, want at least %d", got, tc.minCalls)
			}
			for _, id := range peer.asked {
				if id != "peerX" {
					t.Fatalf("asked about %q, want the selected peer", id)
				}
			}
		})
	}
}

// A peer whose /healthz hangs must not outlast the ceiling.
//
// The loop evaluates every deadline between checks, so a check that never
// comes back would hold it there — and the ceiling exists precisely for the
// case where the liveness signal is wrong or absent. Two independent
// mechanisms cover it: each check is bounded by the polling interval, and the
// ceiling is a timer that answers to nothing the peer does.
func TestWatchPeerWhileItWorks_AHungPeerStillHitsTheCeiling(t *testing.T) {
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })
	h := NewHandlerSet(Deps{PeerHealth: func(ctx context.Context, _ string) router.ProbeResult {
		// Never answers on its own; returns only when its context ends.
		select {
		case <-ctx.Done():
		case <-blocked:
		}
		return router.ProbeResult{Outcome: router.ProbeTransportError}
	}})

	start := time.Now()
	reason, waited := h.watchPeerWhileItWorks(t.Context(), peerLiveness{
		PeerID: "peerX", Grace: 5 * time.Millisecond,
		Ceiling: 200 * time.Millisecond, Interval: 10 * time.Millisecond,
	}, start)

	// Two bounded checks that answer nothing read as a peer that went
	// silent, which is a truthful account and arrives well inside the
	// ceiling. What must not happen is the wait outliving it.
	if reason == "" {
		t.Fatal("the wait never ended")
	}
	if waited > time.Second {
		t.Errorf("waited %v, far past the 200ms ceiling — a hung check held the loop", waited)
	}
}

// The ceiling is armed as a timer beside the loop, so it fires even when the
// loop cannot reach its own check of it.
func TestArmPreCommitWatch_CeilingFiresWithoutTheLoop(t *testing.T) {
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })
	h := NewHandlerSet(Deps{PeerHealth: func(ctx context.Context, _ string) router.ProbeResult {
		select {
		case <-ctx.Done():
		case <-blocked:
		}
		return okHealth(true, 1)
	}})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cancelled := make(chan struct{})
	w := h.armPreCommitWatch(ctx, waitPolicy{
		Budget: 5 * time.Millisecond,
		Reason: LocalErrorPeerTTFBTimeout,
		Liveness: &peerLiveness{
			PeerID: "peerX", Grace: 5 * time.Millisecond,
			// An interval longer than the ceiling: the loop's own ceiling
			// check is unreachable, so only the timer can end this.
			Ceiling: 40 * time.Millisecond, Interval: time.Hour,
		},
	}, func() { close(cancelled) })

	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("the request was never cancelled; the ceiling did not fire")
	}
	reason, waited := w.disarm()
	if reason != LocalErrorPeerTTFBTimeout {
		t.Errorf("reason = %q, want %q", reason, LocalErrorPeerTTFBTimeout)
	}
	if waited != 40*time.Millisecond {
		t.Errorf("waited = %v, want the ceiling", waited)
	}
}

// Nothing is asked of the peer inside the grace period: an ordinary turn
// that answers within the old deadline behaves exactly as it did, and costs
// no overlay round trips at all.
func TestWatchPeerWhileItWorks_SilentInsideTheGrace(t *testing.T) {
	peer := &scriptedPeer{script: []router.ProbeResult{okHealth(true, 1)}}
	h := NewHandlerSet(Deps{PeerHealth: peer.probe})

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	reason, _ := h.watchPeerWhileItWorks(ctx,
		peerLiveness{PeerID: "peerX", Grace: time.Hour, Ceiling: 2 * time.Hour}, time.Now())

	if reason != "" {
		t.Errorf("reason = %q, want none — the watch was stopped, not tripped", reason)
	}
	if got := peer.calls(); got != 0 {
		t.Errorf("asked the peer %d times inside the grace period, want 0", got)
	}
}

// End to end through the streaming proxy: a peer slower than the grace
// period, that keeps reporting it is working, is NOT rerouted — this is the
// 0.0.3-rc4 case where a healthy peer three minutes into a 30k-token prefill
// lost the turn to the Anthropic API.
func TestProxyAnthropicStream_WorkingPeerIsNotAborted(t *testing.T) {
	engine := slowFirstByteEngine(120 * time.Millisecond)
	defer engine.Close()
	peer := &scriptedPeer{script: []router.ProbeResult{okHealth(true, 1)}}
	h := NewHandlerSet(Deps{HTTPClient: http.DefaultClient, PeerHealth: peer.probe})
	w := httptest.NewRecorder()
	w.Header().Set(HeaderInferencePeer, "peerX")

	h.proxyAnthropicStream(context.Background(), http.DefaultClient, engine.URL,
		[]byte(ttfbStreamBody), "waired/default", nil, w,
		waitPolicy{
			Budget: 20 * time.Millisecond,
			Reason: LocalErrorPeerTTFBTimeout,
			Liveness: &peerLiveness{
				PeerID: "peerX", Grace: 20 * time.Millisecond,
				Ceiling: 5 * time.Second, Interval: 5 * time.Millisecond,
			},
		}, router.Selection{Runtime: remoteRuntimePrefix + "peerX"}, nil, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the peer said it was working; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Hi") {
		t.Errorf("expected the peer's content in the stream: %s", w.Body.String())
	}
	if peer.calls() == 0 {
		t.Error("the peer was never asked; the wait was still a flat deadline")
	}
}

// The other half: a peer that stops working ends the leg pre-commit, with a
// reason of its own so the reroute notice can say what happened rather than
// calling it a timeout.
func TestProxyAnthropicStream_PeerThatStopsWorkingIsAborted(t *testing.T) {
	engine := slowFirstByteEngine(300 * time.Millisecond)
	defer engine.Close()
	peer := &scriptedPeer{script: []router.ProbeResult{okHealth(true, 0)}}
	h := NewHandlerSet(Deps{HTTPClient: http.DefaultClient, PeerHealth: peer.probe})
	w := httptest.NewRecorder()
	w.Header().Set(HeaderInferencePeer, "peerX")

	h.proxyAnthropicStream(context.Background(), http.DefaultClient, engine.URL,
		[]byte(ttfbStreamBody), "waired/default", nil, w,
		waitPolicy{
			Budget: 10 * time.Millisecond,
			Reason: LocalErrorPeerTTFBTimeout,
			Liveness: &peerLiveness{
				PeerID: "peerX", Grace: 10 * time.Millisecond,
				Ceiling: 2 * time.Second, Interval: 5 * time.Millisecond,
			},
		}, router.Selection{Runtime: remoteRuntimePrefix + "peerX"}, nil, nil)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get(HeaderLocalError); got != LocalErrorPeerStoppedServing {
		t.Errorf("HeaderLocalError = %q, want %q", got, LocalErrorPeerStoppedServing)
	}
	if strings.Contains(w.Body.String(), "message_start") {
		t.Errorf("stream committed before abort: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "stopped working") {
		t.Errorf("the error does not say what the peer reported: %s", w.Body.String())
	}
}
