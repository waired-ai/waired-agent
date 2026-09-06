package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/internal/runtime"
)

// The reason the router worded has to survive the trip to the client
// (waired-agent#1201). Nothing pinned that before this file: the router
// tests stop at err.Error(), and the gateway tests that touch
// ErrModelNotReady are about the arriving/not-arriving split.
//
// PIN: product contract. #1201 is a message that never reached the person
// who could act on it, so "the router says the right thing" is not the
// property under test — "the client is shown it" is.

func newPublicRefusalGateway(t *testing.T, sel SelectorIface) *Server {
	t.Helper()
	return NewServer(ServerConfig{}, Deps{
		Selector:       sel,
		Runtimes:       runtime.NewRegistry(),
		ListManifests:  func() []catalog.Manifest { return []catalog.Manifest{qwenManifest()} },
		HTTPClient:     http.DefaultClient,
		AllowOpenAI:    true,
		AllowAnthropic: true,
	})
}

func TestPublicShareRefusal_ReachesTheClientVerbatim(t *testing.T) {
	const reason = "this computer is set not to use other people's public machines; " +
		"turn it on with `waired public use --auto` or `--explicit`"
	// The shape the peer-only branch builds for the public entry: no local
	// state, and LocalArrivalAnswers left false because that branch never
	// intended to run here (waired-agent#1252).
	sel := &fakeSelector{err: &router.ModelNotReadyError{
		Note: reason, Mesh: true, PublicShare: true,
	}}
	gw := newPublicRefusalGateway(t, sel)
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages",
		strings.NewReader(`{"model":"waired/public","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", w.Code, w.Body.String())
	}
	// A refusal the operator's own setting caused is not a wait.
	if ra := w.Header().Get("Retry-After"); ra != "" {
		t.Errorf("Retry-After = %q, want none — nothing about this resolves by waiting", ra)
	}
	var body struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not valid JSON: %v\n%s", err, w.Body.String())
	}
	want := "Waired public share declined this turn: " + reason + "." + failClosedExits
	if body.Error.Message != want {
		t.Errorf("message drifted\n got: %s\nwant: %s", body.Error.Message, want)
	}
	if body.Error.Type != "not_found_error" {
		t.Errorf("error.type = %q, want not_found_error", body.Error.Type)
	}
	if got := w.Header().Get(HeaderLocalError); got != LocalErrorModelNotServed {
		t.Errorf("%s = %q, want %q", HeaderLocalError, got, LocalErrorModelNotServed)
	}
}

// The client must not be sent away to wait because THIS computer happens
// to be downloading something the turn was never going to use.
//
// PIN: product contract (waired-agent#1252).
func TestPublicShareRefusal_IsNotAWaitWhileThisHostDownloads(t *testing.T) {
	sel := &fakeSelector{err: &router.ModelNotReadyError{
		Note:  "no public machine is reachable right now",
		State: catalog.ModelStateDownloading,
		Mesh:  true, PublicShare: true,
	}}
	gw := newPublicRefusalGateway(t, sel)
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages",
		strings.NewReader(`{"model":"waired/public","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — a local download is not what this turn is waiting for (body=%s)",
			w.Code, w.Body.String())
	}
	if ra := w.Header().Get("Retry-After"); ra != "" {
		t.Errorf("Retry-After = %q, want none", ra)
	}
	if !strings.Contains(w.Body.String(), "no public machine is reachable right now") {
		t.Errorf("the reason did not reach the body: %s", w.Body.String())
	}
}
