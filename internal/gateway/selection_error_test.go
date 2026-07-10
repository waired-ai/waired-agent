package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/router"
)

// Before this mapping existed, ErrPinnedPeerUnreachable fell into the
// default: branches — 500 api_error / "selection_failed" — which reads
// as a gateway bug rather than "the operator-pinned peer is down".

func TestAnthropicMessages_PinnedPeerUnreachableMapsTo503(t *testing.T) {
	sel := &fakeSelector{err: router.ErrPinnedPeerUnreachable}
	gw := newGatewayUnderTest(t, sel, "")

	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages",
		strings.NewReader(`{"model":"qwen3-8b-instruct","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error.Type != "waired_pinned_peer_unreachable" {
		t.Fatalf("error type = %q, want waired_pinned_peer_unreachable", got.Error.Type)
	}
	if h := w.Header().Get(HeaderLocalError); h != "pinned_peer_unreachable" {
		t.Fatalf("%s = %q, want pinned_peer_unreachable (intercept fallback reason)", HeaderLocalError, h)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("Retry-After must hint the client to back off")
	}
}

func TestChatCompletions_PinnedPeerUnreachableMapsTo503(t *testing.T) {
	sel := &fakeSelector{err: router.ErrPinnedPeerUnreachable}
	gw := newGatewayUnderTest(t, sel, "")

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"qwen3-8b-instruct","messages":[{"role":"user","content":"hi"}]}`))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error.Code != "waired_pinned_peer_unreachable" {
		t.Fatalf("error code = %q, want waired_pinned_peer_unreachable", got.Error.Code)
	}
}

func TestSelectionStatusAndReason_PinnedPeerUnreachable(t *testing.T) {
	if s := selectionStatus(router.ErrPinnedPeerUnreachable); s != http.StatusServiceUnavailable {
		t.Fatalf("selectionStatus = %d, want 503", s)
	}
	if r := selectionErrorReason(router.ErrPinnedPeerUnreachable); r != "pinned_peer_unreachable" {
		t.Fatalf("selectionErrorReason = %q, want pinned_peer_unreachable", r)
	}
}
