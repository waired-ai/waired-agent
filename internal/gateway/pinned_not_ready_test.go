package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/router"
)

// A pinned computer that answered its probe and is not ready ends the turn
// with a retryable 503 that names the reason, not with "is not answering".
//
// PRODUCT CONTRACT, ratifying source: owner decision 2026-09-16 on
// waired-agent#1369 (retryable status and a message naming the reason, in
// the form #1393 introduced; wording approved the same day).
func TestSelectAndProbe_PinnedPeerNotReadyNamesTheReason(t *testing.T) {
	const head = `{"model_id":"qwen3:8b-q4_K_M","capacity_total":1,"capacity_used":0,`
	for _, tc := range []struct {
		name, body, phrase string
	}{
		{"running its benchmark", head + `"engine_ready":true,"paused":false,"share_enabled":true,"measuring":true}`,
			"running its benchmark (takes a few minutes)"},
		{"loading the model", head + `"engine_ready":true,"paused":false,"share_enabled":true,"model_loading":true}`,
			"loading the model into memory"},
		{"engine not ready", head + `"engine_ready":false,"paused":false,"share_enabled":true}`,
			"engine not ready"},
		{"paused", head + `"engine_ready":true,"paused":true,"share_enabled":true}`,
			"paused"},
		{"sharing off", head + `"engine_ready":true,"paused":false,"share_enabled":false}`,
			"sharing off"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rtPin := &stubRT{status: 200, body: tc.body}
			rtFree := &stubRT{status: 200, body: readyBody(0, 4)}
			upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				t.Error("a turn pinned to a computer that is not ready was served")
				_, _ = w.Write([]byte(`{}`))
			}))
			t.Cleanup(upstreamSrv.Close)

			sel := &phase8MultiSelector{cands: []router.Candidate{
				phase8PinnedCandidate("peer-pin"),
				phase8RemoteCandidate("peer-free"),
			}}
			rec := &captureRecorder{}
			h := buildPhase8Gateway(t, sel,
				map[string]http.RoundTripper{"peer-pin": rtPin, "peer-free": rtFree}, upstreamSrv.URL)
			h.deps.Recorder = rec
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
			if env.Error.Code != "waired_pinned_peer_not_ready" {
				t.Errorf("error.code = %q, want waired_pinned_peer_not_ready (body=%s)", env.Error.Code, raw)
			}
			want := `router: the computer this turn is pinned to is not ready (tried "peer-pin": ` + tc.phrase + `)`
			if env.Error.Message != want {
				t.Errorf("message = %q\nwant      %q", env.Error.Message, want)
			}
			if got := resp.Header.Get("Retry-After"); got != "30" {
				t.Errorf("Retry-After = %q, want 30", got)
			}
			if got := resp.Header.Get(HeaderLocalError); got != LocalErrorPinnedPeerNotReady {
				t.Errorf("%s = %q, want %q", HeaderLocalError, got, LocalErrorPinnedPeerNotReady)
			}
			if got := resp.Header.Get(HeaderInferencePeer); got != "peer-pin" {
				t.Errorf("%s = %q, want peer-pin", HeaderInferencePeer, got)
			}
			// Not an unreachable pin: the tray's "pinned computer unreachable"
			// event would send the reader to a network that is working.
			if got := rec.pinFailuresSnapshot(); len(got) != 0 {
				t.Errorf("RecordPinnedPeerUnreachable emits = %+v, want none", got)
			}
		})
	}
}

// The Claude listener gives the same error the same retryable status, with
// the overloaded_error type Claude Code retries on.
func TestRespondAnthropicSelectionError_PinnedPeerNotReady(t *testing.T) {
	err := &pinnedPeerNotReadyError{display: "dev-1", name: "sv-mag", phrase: "running its benchmark (takes a few minutes)"}
	w := httptest.NewRecorder()
	respondAnthropicSelectionError(w, "", err, 0)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	var env struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if env.Error.Type != "overloaded_error" {
		t.Errorf("error.type = %q, want overloaded_error", env.Error.Type)
	}
	want := `router: the computer this turn is pinned to is not ready (tried "sv-mag": running its benchmark (takes a few minutes))`
	if env.Error.Message != want {
		t.Errorf("message = %q\nwant      %q", env.Error.Message, want)
	}
	if got := w.Header().Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After = %q, want 30", got)
	}
	if got := w.Header().Get(HeaderLocalError); got != LocalErrorPinnedPeerNotReady {
		t.Errorf("%s = %q", HeaderLocalError, got)
	}
	if got := w.Header().Get(HeaderInferencePeer); got != "dev-1" {
		t.Errorf("%s = %q, want the display id, not the name", HeaderInferencePeer, got)
	}
	if selectionErrorReason(err) != LocalErrorPinnedPeerNotReady {
		t.Errorf("selectionErrorReason = %q", selectionErrorReason(err))
	}
	if !errors.Is(err, router.ErrAllPeersOverloaded) {
		t.Error("the not-ready pin no longer reads as a wait to errors.Is")
	}
}
