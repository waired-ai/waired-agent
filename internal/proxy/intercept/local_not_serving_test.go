package intercept

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A listener that outlives the session behind it needs a way to be told the
// session is gone. :9472 is built at boot — it has to answer before enrollment
// — and the local-inference handler is published once, at activation. After
// `waired logout` it was still published, so a Waired-addressed turn was still
// answered locally: the gateway's per-request lazy start brought the inference
// engine back up on a computer that had just been signed out, while
// `waired doctor` and the :9473 gateway both reported local inference as off
// (waired-agent#1310, measured on macOS against 0.0.3-rc6 — 200 in 26 s with
// X-Waired-Local-Model set).
//
// PIN: product contract — a Waired-addressed turn on a computer that cannot
// serve it fails with a reason the client shows at once
// (waired-ai/waired-agent#1310; the 400 and the error type are
// docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md
// decision 4, which this reuses rather than re-deciding).

const notServingReason = "This computer is signed out of Waired, so this turn has nowhere to run."

func notServing() (bool, string) { return false, notServingReason }

func TestWairedTurnFailsWhenLocalInferenceStoppedServing(t *testing.T) {
	var last http.Request
	var localHits int
	local := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		localHits++
		_, _ = io.WriteString(w, "LOCAL")
	})
	s := newServer(t, Deps{
		LocalInference:       local,
		LocalServing:         notServing,
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
	body, _ := io.ReadAll(resp.Body)

	if localHits != 0 {
		t.Error("the turn reached local inference on a computer that reported it was not serving")
	}
	if resp.Header.Get("X-Fake-Upstream") == "1" {
		t.Error("a Waired id was relayed to the real Anthropic API")
	}
	// 400, not 5xx: Claude Code retries 5xx ten times before showing anything,
	// so a 503 here is a minute of anonymous "API error" (waired-agent#1180).
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (body=%s)", resp.StatusCode, body)
	}
	var got struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("answer is not the Anthropic error shape: %s", body)
	}
	if got.Error.Type != "waired_cannot_serve" {
		t.Errorf("error type = %q, want waired_cannot_serve", got.Error.Type)
	}
	if got.Error.Message != notServingReason {
		t.Errorf("message = %q, want the caller's reason — a computer that was signed out\n"+
			"must not be told it was never set up", got.Error.Message)
	}
}

// The other half of the same state: stop offering the ids too. The splice
// exists so a machine that CAN serve keeps its rows in the picker while a turn
// runs in the cloud; a signed-out one advertising them is how a picker fills up
// with entries whose every turn comes back an error.
func TestModelsStopsAdvertisingWairedIDsWhenNotServing(t *testing.T) {
	s := newServer(t, Deps{
		LocalInference:       recordingHandler(new(string)),
		LocalServing:         notServing,
		PassthroughTransport: fakeUpstream(nil),
	})
	s.cfg.ModelRouteDirectives = true
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.Header.Get("X-Local-Inference") == "1" {
		t.Error("/v1/models was served from the handler of a session that is gone")
	}
	if resp.Header.Get("X-Fake-Upstream") != "1" {
		t.Fatalf("/v1/models did not pass through (body=%s)", body)
	}
	for _, id := range []string{"waired", "waired/"} {
		if strings.Contains(string(body), id) {
			t.Errorf("a Waired id is still advertised while this computer cannot serve one: %s", body)
		}
	}
}

// And the states stay apart: a nil LocalServing is the shape every caller but
// sign-out has, and it must keep meaning "serving".
func TestNilLocalServingStillServes(t *testing.T) {
	var gotPath string
	s := newServer(t, Deps{
		LocalInference:       recordingHandler(&gotPath),
		PassthroughTransport: fakeUpstream(nil),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"`+legacyAutoModel+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("X-Local-Inference") != "1" {
		t.Error("a nil LocalServing stopped local inference from answering")
	}
}
