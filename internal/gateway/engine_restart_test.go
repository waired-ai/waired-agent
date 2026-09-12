package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/router"
)

// stoppedDuringTheLeg is the Deps hook for a device whose engine was stopped
// AFTER the leg began — the case the classification is for.
//
// It answers by the rule the real implementation uses (the stop instant is
// later than the instant handed in) rather than by a fixed timestamp,
// because the instant proxyAnthropicStream hands in is its own dispatch
// start, which a test cannot name before calling it.
func stoppedDuringTheLeg() func(time.Time) bool {
	return func(since time.Time) bool { return !since.IsZero() }
}

// stoppedBeforeTheLeg is the same hook for a bounce that had already
// finished when the leg began.
func stoppedBeforeTheLeg() func(time.Time) bool {
	return func(time.Time) bool { return false }
}

// deadEngineURL is an address nothing is listening on, so postToEngine fails
// the way it does when `ollama serve` has just been killed.
func deadEngineURL(t *testing.T) string {
	t.Helper()
	dead := httptest.NewServer(http.NewServeMux())
	url := dead.URL
	dead.Close()
	return url
}

// TestEngineRestart_TransportErrorIsNotTheEnginesFailure is waired-agent#1304
// on the pre-headers path: the engine did not fail, this device stopped it.
//
// Product contract (owner ruling 2026-09-12, waired-ai/waired#1361 lane
// L106): the turn is recorded as engine_restarted, and the person is told
// what happened and that sending it again will work — not handed the socket
// error the kill produced ("wsarecv: An existing connection was forcibly
// closed by the remote host"), which describes the same event and offers
// nothing to act on.
func TestEngineRestart_TransportErrorIsNotTheEnginesFailure(t *testing.T) {
	h := NewHandlerSet(Deps{
		HTTPClient: http.DefaultClient,
		// Stopped after this request began: the bounce happened under it.
		LocalEngineRestarted: stoppedDuringTheLeg(),
	})
	w := newFlushRecorder()
	rr := &requestRec{start: time.Now().Add(-time.Second)}
	rr.succeed()

	h.proxyAnthropicStream(context.Background(), http.DefaultClient, deadEngineURL(t),
		[]byte(ttfbStreamBody), "waired/default", nil, w,
		waitPolicy{Keepalive: time.Hour}, localSel, rr, nil)

	if got := rr.ev.ErrorReason; got != LocalErrorEngineRestarted {
		t.Errorf("ErrorReason = %q, want %q", got, LocalErrorEngineRestarted)
	}
	if got := w.Header().Get(HeaderLocalError); got != LocalErrorEngineRestarted {
		t.Errorf("%s = %q, want %q", HeaderLocalError, got, LocalErrorEngineRestarted)
	}
	if !strings.Contains(w.body(), "Send the turn again") {
		t.Errorf("the reader was not told what to do: %q", w.body())
	}
	if strings.Contains(w.body(), "connection refused") {
		t.Errorf("the socket error was handed to the reader: %q", w.body())
	}
}

// TestEngineRestart_ABounceBeforeTheRequestIsNotClaimed is the other half of
// the same contract, and the reason the hook takes an instant rather than
// answering "is a bounce happening now". A switch that finished before this
// turn started cannot be why this turn failed, so the engine keeps it.
func TestEngineRestart_ABounceBeforeTheRequestIsNotClaimed(t *testing.T) {
	h := NewHandlerSet(Deps{
		HTTPClient:           http.DefaultClient,
		LocalEngineRestarted: stoppedBeforeTheLeg(),
	})
	w := newFlushRecorder()
	rr := &requestRec{start: time.Now()}
	rr.succeed()

	h.proxyAnthropicStream(context.Background(), http.DefaultClient, deadEngineURL(t),
		[]byte(ttfbStreamBody), "waired/default", nil, w,
		waitPolicy{Keepalive: time.Hour}, localSel, rr, nil)

	if got := rr.ev.ErrorReason; got != "engine_request_failed" {
		t.Errorf("ErrorReason = %q, want engine_request_failed", got)
	}
}

// TestEngineRestart_APeerLegNeverClaimsOurOwnBounce: what this device did to
// its own engine says nothing about a turn that went to a peer, so the peer
// classification has to survive a bounce happening here at the same moment.
func TestEngineRestart_APeerLegNeverClaimsOurOwnBounce(t *testing.T) {
	h := NewHandlerSet(Deps{
		HTTPClient:           http.DefaultClient,
		LocalEngineRestarted: stoppedDuringTheLeg(),
	})
	peer := router.Selection{Runtime: remoteRuntimePrefix + "peerX"}
	if h.localEngineRestartedUnder(peer, time.Now().Add(-time.Minute)) {
		t.Error("a peer leg claimed this device's own engine restart")
	}
	if !h.localEngineRestartedUnder(localSel, time.Now().Add(-time.Minute)) {
		t.Error("a local leg did not see this device's own engine restart")
	}
}

// TestEngineRestart_UnwiredDepClaimsNothing pins the overlay listener's
// position. It serves a PEER's traffic, so it is left without the hook on
// purpose: what this device did to its own engine is a sentence for its own
// operator, not a verdict handed to a caller about a machine it does not
// administer.
func TestEngineRestart_UnwiredDepClaimsNothing(t *testing.T) {
	h := NewHandlerSet(Deps{HTTPClient: http.DefaultClient})
	if h.localEngineRestartedUnder(localSel, time.Now().Add(-time.Minute)) {
		t.Error("a handler set with no LocalEngineRestarted claimed a restart")
	}
	if got := h.engineFailureReason(context.Background(), localSel, time.Now(), "engine_request_failed"); got != "engine_request_failed" {
		t.Errorf("engineFailureReason = %q, want engine_request_failed", got)
	}
}

// TestEngineFailureReason_ClientDepartureOutranksOurRestart: both can be true
// at once — a person interrupts the turn and a switch bounces the engine — and
// the client leaving has to win. It cancels the request context, so every call
// under it fails and the engine records OUR disconnect; nothing observed after
// that is evidence about anything else
// (docs/decisions/20260904/0215-a-hangup-is-not-the-engines-failure.md).
func TestEngineFailureReason_ClientDepartureOutranksOurRestart(t *testing.T) {
	h := NewHandlerSet(Deps{
		HTTPClient:           http.DefaultClient,
		LocalEngineRestarted: stoppedDuringTheLeg(),
	})
	dead, cancel := context.WithCancel(context.Background())
	cancel()

	if got := h.engineFailureReason(dead, localSel, time.Now().Add(-time.Minute), "engine_request_failed"); got != LocalErrorClientDisconnected {
		t.Errorf("engineFailureReason = %q, want %q", got, LocalErrorClientDisconnected)
	}
	if got := h.engineFailureReason(context.Background(), localSel, time.Now().Add(-time.Minute), "engine_request_failed"); got != LocalErrorEngineRestarted {
		t.Errorf("engineFailureReason = %q, want %q", got, LocalErrorEngineRestarted)
	}
}
