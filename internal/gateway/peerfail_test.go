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

// A peer leg that fails before it commits (waired-agent#1171).
//
// The measured shape: a turn is dispatched, the peer's daemon restarts
// ~25 s in, the dial fails in milliseconds — inside the grace, so the
// liveness watch has not asked anything and never will — and the turn was
// answered as a bare 502 whose reason named an engine that took no part in
// it. Two things follow, and they are separate:
//
//   - the NAME is wrong on every peer leg, and naming it where it happens
//     is the rule waired-agent#1168 settled
//     (docs/decisions/20260904/0215-a-hangup-is-not-the-engines-failure.md);
//   - the STATUS is wrong only where the turn has nowhere else to go. A
//     5xx is retried by Claude Code, and each retry re-runs selection, so
//     for a leg with substitutes the retry IS the recovery. A pin has no
//     substitute (waired-agent#325), so every retry re-asks the machine
//     that just failed.
//
// PIN: product contract for the pinned row — waired-agent#1180, the ruling
// already applied to the pre-dispatch twin at anthropic.go's
// ErrPinnedPeerUnreachable arm: the turn ends now, naming the computer,
// rather than after ten anonymous retries.

// deadPeerURL is an address nothing is listening on: the dial fails at
// once, which is what a restarting daemon does.
func deadPeerURL(t *testing.T) string {
	t.Helper()
	dead := httptest.NewServer(http.NewServeMux())
	url := dead.URL
	dead.Close()
	return url
}

func TestFailedPeerLegReason(t *testing.T) {
	peer := router.Selection{Runtime: remoteRuntimePrefix + "peerX"}
	local := router.Selection{Runtime: "ollama"}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	for _, tc := range []struct {
		name       string
		ctx        context.Context
		sel        router.Selection
		rr         *requestRec
		wantReason string
		wantEnd    bool
	}{
		{"a peer leg says the peer could not be reached",
			context.Background(), peer, &requestRec{}, LocalErrorPeerUnreachable, false},
		{"a pinned peer leg ends the turn",
			context.Background(), peer, &requestRec{pinnedPeer: true}, LocalErrorPeerUnreachable, true},
		// This device's own engine. engine_request_failed already names
		// what happened, and this helper does not answer for it.
		{"a local leg is left alone",
			context.Background(), local, &requestRec{}, "", false},
		{"a pinned flag on a local leg changes nothing",
			context.Background(), local, &requestRec{pinnedPeer: true}, "", false},
		// The client left. Every call under its context fails with it, and
		// the peer is not what went wrong (waired-agent#1168).
		{"a cancelled request is the client leaving, not the peer",
			cancelled, peer, &requestRec{pinnedPeer: true}, LocalErrorClientDisconnected, false},
		// A direct call with no record is not a pin, which leaves the leg
		// on the status it has today.
		{"a rec-less call is not a pin",
			context.Background(), peer, nil, LocalErrorPeerUnreachable, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason, end := failedPeerLegReason(tc.ctx, tc.sel, tc.rr)
			if reason != tc.wantReason || end != tc.wantEnd {
				t.Errorf("failedPeerLegReason = (%q, %v), want (%q, %v)",
					reason, end, tc.wantReason, tc.wantEnd)
			}
		})
	}
}

// TestPeerLegDispatchFailure drives both transports, because the two must
// not describe one failure differently — the reason the non-streaming twin
// was kept in step from the day it was written.
func TestPeerLegDispatchFailure(t *testing.T) {
	peerSel := router.Selection{Runtime: remoteRuntimePrefix + "peerX", PeerDisplayID: "peerX"}

	for _, tc := range []struct {
		name       string
		pinned     bool
		wantStatus int
		wantReason string
		wantBody   string
	}{
		{
			name: "a peer with substitutes keeps a status Claude Code retries",
			// The retry is a fresh request, so it re-runs selection: for
			// this leg it is a recovery path, not ten wasted attempts.
			// Same reading that left ErrPeersDidNotAnswer on a 503 when
			// the fail-closed change went in.
			pinned: false, wantStatus: http.StatusBadGateway,
			wantReason: LocalErrorPeerUnreachable, wantBody: "peerX",
		},
		{
			name:   "a pinned peer ends the turn naming the computer",
			pinned: true, wantStatus: http.StatusBadRequest,
			wantReason: LocalErrorPinnedPeerUnreachable,
			wantBody:   `The computer this turn is pinned to, peerX, is not answering.`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, transport := range []string{"stream", "non-stream"} {
				t.Run(transport, func(t *testing.T) {
					dead := deadPeerURL(t)
					h := NewHandlerSet(Deps{HTTPClient: http.DefaultClient})
					w := httptest.NewRecorder()
					rr := &requestRec{pinnedPeer: tc.pinned}
					rr.succeed() // as the handler does, before it knows anything

					if transport == "stream" {
						h.proxyAnthropicStream(context.Background(), http.DefaultClient, dead,
							[]byte(ttfbStreamBody), "waired/default", nil, w,
							waitPolicy{Budget: time.Minute, Reason: LocalErrorPeerTTFBTimeout},
							peerSel, rr, nil)
					} else {
						h.proxyAnthropicNonStream(context.Background(), http.DefaultClient, dead,
							[]byte(ttfbStreamBody), "waired/default", nil, w, peerSel, rr, nil)
					}

					if w.Code != tc.wantStatus {
						t.Fatalf("status = %d, want %d; body=%s", w.Code, tc.wantStatus, w.Body.String())
					}
					if got := w.Header().Get(HeaderLocalError); got != tc.wantReason {
						t.Errorf("HeaderLocalError = %q, want %q", got, tc.wantReason)
					}
					// Recorded, not just written: rr.succeed() ran before
					// dispatch, so an exit that does not record leaves a
					// finished 200 turn in the event ring.
					if rr.ev.ErrorReason != tc.wantReason {
						t.Errorf("recorded error_reason = %q, want %q", rr.ev.ErrorReason, tc.wantReason)
					}
					if rr.ev.Status != tc.wantStatus {
						t.Errorf("recorded status = %d, want %d", rr.ev.Status, tc.wantStatus)
					}
					if !strings.Contains(w.Body.String(), tc.wantBody) {
						t.Errorf("body does not carry %q: %s", tc.wantBody, w.Body.String())
					}
					if strings.Contains(w.Body.String(), "message_start") {
						t.Errorf("stream committed before the failure: %s", w.Body.String())
					}
				})
			}
		})
	}
}

// The sentence names the computer a person would recognise, when this
// device knows one. Ending the turn here is only worth doing if the
// person can act on what it says, and a 32-character device id is the
// one string they cannot (waired-agent#1180).
func TestPinnedPeerDispatchFailureNamesTheComputer(t *testing.T) {
	peerSel := router.Selection{Runtime: remoteRuntimePrefix + "dev_abc", PeerDisplayID: "dev_abc"}

	for _, tc := range []struct {
		name  string
		facts func(string) PeerFacts
		want  string
	}{
		{"a named peer is named", func(string) PeerFacts {
			return PeerFacts{Name: "sv-mag", Known: true, EngineLive: true}
		}, "The computer this turn is pinned to, sv-mag, is not answering."},
		// A Public Share peer carries no name here by construction, so it
		// keeps the pseudonym its caller already holds (spec §8.5).
		{"an unnamed peer keeps its display identifier", func(string) PeerFacts {
			return PeerFacts{Known: true, EngineLive: true}
		}, "The computer this turn is pinned to, dev_abc, is not answering."},
		{"no lookup wired at all", nil,
			"The computer this turn is pinned to, dev_abc, is not answering."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dead := deadPeerURL(t)
			h := NewHandlerSet(Deps{HTTPClient: http.DefaultClient, PeerFacts: tc.facts})
			w := httptest.NewRecorder()
			rr := &requestRec{pinnedPeer: true}
			rr.succeed()

			h.proxyAnthropicStream(context.Background(), http.DefaultClient, dead,
				[]byte(ttfbStreamBody), "waired/default", nil, w,
				waitPolicy{Budget: time.Minute, Reason: LocalErrorPeerTTFBTimeout}, peerSel, rr, nil)

			if !strings.Contains(w.Body.String(), tc.want) {
				t.Errorf("body does not say %q: %s", tc.want, w.Body.String())
			}
			// The header stays the functional identifier whatever the
			// sentence says: surfaces key on it.
			if got := w.Header().Get(HeaderInferencePeer); got != "dev_abc" {
				t.Errorf("%s = %q, want dev_abc", HeaderInferencePeer, got)
			}
		})
	}
}

// A local leg is untouched by any of it: this device's engine failing to
// answer is engine_request_failed, and it stays a 502.
func TestLocalLegDispatchFailureIsUnchanged(t *testing.T) {
	dead := deadPeerURL(t)
	h := NewHandlerSet(Deps{HTTPClient: http.DefaultClient})
	w := httptest.NewRecorder()
	rr := &requestRec{}
	rr.succeed()

	h.proxyAnthropicStream(context.Background(), http.DefaultClient, dead,
		[]byte(ttfbStreamBody), "waired/default", nil, w, waitPolicy{}, localSel, rr, nil)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", w.Code, w.Body.String())
	}
	if rr.ev.ErrorReason != "engine_request_failed" {
		t.Errorf("recorded error_reason = %q, want %q", rr.ev.ErrorReason, "engine_request_failed")
	}
}

// A peer leg on a cancelled request never becomes a pinned-peer failure:
// the client hung up, and blaming the peer for our own departure is what
// waired-agent#1168 was opened about.
func TestPinnedPeerLegDoesNotBlameThePeerForAHangup(t *testing.T) {
	dead := deadPeerURL(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	h := NewHandlerSet(Deps{HTTPClient: http.DefaultClient})
	w := httptest.NewRecorder()
	rr := &requestRec{pinnedPeer: true}
	rr.succeed()

	h.proxyAnthropicStream(ctx, http.DefaultClient, dead,
		[]byte(ttfbStreamBody), "waired/default", nil, w,
		waitPolicy{Budget: time.Minute, Reason: LocalErrorPeerTTFBTimeout},
		router.Selection{Runtime: remoteRuntimePrefix + "peerX", PeerDisplayID: "peerX"}, rr, nil)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (the client left; nothing is fail-closed here)", w.Code)
	}
	if rr.ev.ErrorReason != LocalErrorClientDisconnected {
		t.Errorf("recorded error_reason = %q, want %q", rr.ev.ErrorReason, LocalErrorClientDisconnected)
	}
}
