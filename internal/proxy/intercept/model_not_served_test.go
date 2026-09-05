package intercept

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A turn on a Waired id has no upstream to try, so the gateway's own answer is
// what the client gets — what waired-agent#788 exists to produce, and now the
// only shape there is (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md).
func TestWairedIDSurfacesModelNotServed(t *testing.T) {
	var last http.Request
	local := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"not_found_error"}}`)
	})
	s := newServer(t, Deps{
		LocalInference:       local,
		PassthroughTransport: fakeUpstream(&last),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"`+legacyAutoModel+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 — the client must see the failure, not wait on it", resp.StatusCode)
	}
	if resp.Header.Get("X-Fake-Upstream") == "1" {
		t.Error("a Waired id reached the real Anthropic API")
	}
	if resp.Header.Get("Retry-After") != "" {
		t.Error("a Retry-After on this answer is an invitation to the silent-retry loop this fixed")
	}
}
