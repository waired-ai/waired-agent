package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/router"
)

// stopCounter is the Deps hook's shape: a count of the times this device has
// stopped its own engine on purpose, which the provider increments and the
// gateway only reads.
type stopCounter struct{ n atomic.Uint64 }

func (c *stopCounter) count() uint64 { return c.n.Load() }
func (c *stopCounter) stop()         { c.n.Add(1) }

// stopUnderTheLeg returns a hook that counts one stop the first time the
// gateway reads it after dispatching — the leg reads it once before, so this
// models "the engine was taken away while this request was in the air"
// without depending on a clock.
func stopUnderTheLeg() func() uint64 {
	var reads atomic.Uint64
	return func() uint64 {
		if reads.Add(1) == 1 {
			return 0 // the reading the leg takes before it dispatches
		}
		return 1 // by the time it failed, a stop had happened
	}
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
		HTTPClient:       http.DefaultClient,
		LocalEngineStops: stopUnderTheLeg(),
	})
	w := newFlushRecorder()
	rr := &requestRec{start: time.Now()}
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
// the same contract, and the reason the hook is read twice rather than asked
// "is a bounce happening". A switch that finished before this turn started
// cannot be why this turn failed, so the engine keeps it.
func TestEngineRestart_ABounceBeforeTheRequestIsNotClaimed(t *testing.T) {
	stops := &stopCounter{}
	stops.stop() // a bounce that has already finished
	h := NewHandlerSet(Deps{
		HTTPClient:       http.DefaultClient,
		LocalEngineStops: stops.count,
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
	stops := &stopCounter{}
	h := NewHandlerSet(Deps{
		HTTPClient:       http.DefaultClient,
		LocalEngineStops: stops.count,
	})
	before := h.localEngineStops()
	stops.stop()

	peer := router.Selection{Runtime: remoteRuntimePrefix + "peerX"}
	if h.localEngineRestartedUnder(peer, before) {
		t.Error("a peer leg claimed this device's own engine restart")
	}
	if !h.localEngineRestartedUnder(localSel, before) {
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
	if got := h.localEngineStops(); got != 0 {
		t.Errorf("localEngineStops = %d with no dep wired, want 0", got)
	}
	if h.localEngineRestartedUnder(localSel, 0) {
		t.Error("a handler set with no LocalEngineStops claimed a restart")
	}
	if got := h.engineFailureReason(context.Background(), localSel, 0, "engine_request_failed"); got != "engine_request_failed" {
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
	stops := &stopCounter{}
	h := NewHandlerSet(Deps{
		HTTPClient:       http.DefaultClient,
		LocalEngineStops: stops.count,
	})
	before := h.localEngineStops()
	stops.stop()

	dead, cancel := context.WithCancel(context.Background())
	cancel()

	if got := h.engineFailureReason(dead, localSel, before, "engine_request_failed"); got != LocalErrorClientDisconnected {
		t.Errorf("engineFailureReason = %q, want %q", got, LocalErrorClientDisconnected)
	}
	if got := h.engineFailureReason(context.Background(), localSel, before, "engine_request_failed"); got != LocalErrorEngineRestarted {
		t.Errorf("engineFailureReason = %q, want %q", got, LocalErrorEngineRestarted)
	}
}

// TestEngineRestart_TheNonStreamingLegSaysItToo: Claude Code retries a cut
// stream as a NON-streaming request (measured 2026-09-12, see
// docs/knowledges/20260912/1100-claude-code-gives-up-on-a-silent-leg.md), so
// that leg is where the person actually reads the message. ADR
// 20260807/1648's rule that one physical event must not be written up
// differently by the transport that met it applies to this heading too.
func TestEngineRestart_TheNonStreamingLegSaysItToo(t *testing.T) {
	h := NewHandlerSet(Deps{
		HTTPClient:       http.DefaultClient,
		LocalEngineStops: stopUnderTheLeg(),
	})
	w := newFlushRecorder()
	rr := &requestRec{start: time.Now()}
	rr.succeed()

	h.proxyAnthropicNonStream(context.Background(), http.DefaultClient, deadEngineURL(t),
		[]byte(ttfbStreamBody), "waired/default", nil, w, localSel, rr, nil)

	if got := rr.ev.ErrorReason; got != LocalErrorEngineRestarted {
		t.Errorf("ErrorReason = %q, want %q", got, LocalErrorEngineRestarted)
	}
	if !strings.Contains(w.body(), "Send the turn again") {
		t.Errorf("the reader was not told what to do: %q", w.body())
	}
	if strings.Contains(w.body(), "connection refused") {
		t.Errorf("the socket error was handed to the reader: %q", w.body())
	}
}
