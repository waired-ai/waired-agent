package intercept

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// bodyCapturingUpstream is fakeUpstream plus request-body capture, for
// asserting what actually reaches the real Anthropic API.
func bodyCapturingUpstream(bodies *[]string) http.RoundTripper {
	return rtFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		*bodies = append(*bodies, string(b))
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"type":"message"}`)),
			Request:    r,
		}, nil
	})
}

func postJSON(t *testing.T, url, body string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

// TestAnUnknownIdIsServedHere: an id neither side owns stays on this machine.
//
// It used to be spelled with waired's subagent label, which managed settings
// pinned as every subagent's model. waired-agent#1186 retired the label, and
// the guarantee it stood in for is about ANY unrecognised id: relaying one
// would only buy a 404 upstream, and the sentinel's claim is that nothing but
// a real Anthropic id leaves the machine.
func TestAnUnknownIdIsServedHere(t *testing.T) {
	var localHit bool
	var bodies []string
	s := newServer(t, Deps{
		LocalInference:       recordingHandler2(&localHit),
		PassthroughTransport: bodyCapturingUpstream(&bodies),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	postJSON(t, srv.URL+"/v1/messages", `{"model":"some-other-vendor/model","max_tokens":16}`)
	if !localHit {
		t.Error("an unrecognised id was not served here")
	}
	if len(bodies) != 0 {
		t.Errorf("an unrecognised id reached the real Anthropic API: %v", bodies)
	}
}

func TestAnthropicModePassesNonWairedBodyByteIdentical(t *testing.T) {
	var bodies []string
	s := newServer(t, Deps{
		PassthroughTransport: bodyCapturingUpstream(&bodies),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	// Unusual formatting must survive untouched: since waired-agent#1186 the
	// relay does not decode the body at all.
	body := "{\n  \"model\": \"claude-fable-5\",\n  \"big\": 9007199254740993\n}"
	postJSON(t, srv.URL+"/v1/messages", body)
	if len(bodies) != 1 || bodies[0] != body {
		t.Fatalf("upstream body = %q, want byte-identical original", bodies)
	}

}

// A body whose model cannot be read names no side, so it is served here — the
// same answer an unknown id gets, and the one that keeps the guarantee simple:
// only a turn carrying a real Anthropic model id leaves this machine
// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md).
func TestUnreadableBodyIsServedHere(t *testing.T) {
	var localHit bool
	var bodies []string
	s := newServer(t, Deps{
		LocalInference:       recordingHandler2(&localHit),
		PassthroughTransport: bodyCapturingUpstream(&bodies),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	postJSON(t, srv.URL+"/v1/messages", `{"model":`)
	if !localHit {
		t.Error("a body nobody could parse was not served here")
	}
	if len(bodies) != 0 {
		t.Errorf("an unparseable body was relayed to the real Anthropic API: %v", bodies)
	}
}

// TestTheRetiredCloudRowFailsClosed: a session still holding the cloud id is
// answered here rather than relayed.
//
// The row named the real Anthropic API. It stopped being offered when a real
// Anthropic id in /model started meaning that on its own (waired-agent#1037),
// and relaying it meant rewriting the body's model to some other id — the one
// place waired put a model on the wire the user never typed, and what
// waired-agent#1036 cost. waired-agent#1186 stops relaying it.
func TestTheRetiredCloudRowFailsClosed(t *testing.T) {
	var localHit bool
	var bodies []string
	s := newServer(t, Deps{
		LocalInference:       recordingHandler2(&localHit),
		PassthroughTransport: bodyCapturingUpstream(&bodies),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	for _, id := range []string{legacyCloudModel, legacyCloudBareModel} {
		localHit = false
		postJSON(t, srv.URL+"/v1/messages", `{"model":"`+id+`","max_tokens":16}`)
		if !localHit {
			t.Errorf("%q was not answered here", id)
		}
	}
	if len(bodies) != 0 {
		t.Errorf("the retired cloud row still reaches the real Anthropic API: %v", bodies)
	}
}

// TestARealAnthropicIdTravelsByteExact is the other half of the same rule.
// Nothing decodes a passthrough body now, but "inspect without modifying" is
// what the gateway contract asks for, so it is asserted rather than inferred
// from the absence of a decoder.
func TestARealAnthropicIdTravelsByteExact(t *testing.T) {
	var bodies []string
	s := newServer(t, Deps{PassthroughTransport: bodyCapturingUpstream(&bodies)})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	// Big integers, unicode escapes and unusual spacing are exactly what a
	// decode-and-re-encode round trip would quietly normalise.
	body := "{\n  \"model\": \"claude-opus-4-8\",\n  \"n\": 9007199254740993,\n" +
		"  \"s\": \"\\u00e9\\ud83d\\ude00\"\n}"
	postJSON(t, srv.URL+"/v1/messages", body)
	postJSON(t, srv.URL+"/v1/messages/count_tokens", body)
	if len(bodies) != 2 {
		t.Fatalf("upstream saw %d bodies, want 2", len(bodies))
	}
	for i, got := range bodies {
		if got != body {
			t.Errorf("body %d = %q, want byte-identical original", i, got)
		}
	}
}

// upstreamModel reads the "model" field out of a body the fake upstream saw.
func upstreamModel(t *testing.T, body string) string {
	t.Helper()
	var obj struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("upstream body is not JSON: %v", err)
	}
	return obj.Model
}
