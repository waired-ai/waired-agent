package intercept

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestModelsSubpathIdRewrite(t *testing.T) {
	var gotPath string
	s := newServer(t, Deps{
		LocalInference:       recordingHandler(&gotPath),
		PassthroughTransport: fakeUpstream(nil),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/models/claude-sonnet-4")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotPath != "/anthropic/v1/models/claude-sonnet-4" {
		t.Errorf("single-model dispatch path = %q, want /anthropic/v1/models/claude-sonnet-4", gotPath)
	}
}

func TestModelsServedLocallyWhenALocalHandlerIsWired(t *testing.T) {
	var gotPath string
	var last http.Request
	s := newServer(t, Deps{
		LocalInference:       recordingHandler(&gotPath),
		PassthroughTransport: fakeUpstream(&last),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotPath != "/anthropic/v1/models" {
		t.Errorf("local dispatch path = %q, want /anthropic/v1/models", gotPath)
	}
	if resp.Header.Get("X-Fake-Upstream") == "1" {
		t.Error("a wired local handler must answer /v1/models, not the real Anthropic API")
	}
}

func TestModelsPassThroughWithNoLocalHandler(t *testing.T) {
	var localHit string
	var last http.Request
	s := newServer(t, Deps{
		PassthroughTransport: fakeUpstream(&last),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("X-Fake-Upstream") != "1" {
		t.Error("with no local handler wired, /v1/models must pass through")
	}
	if localHit != "" {
		t.Errorf("nothing may be dispatched locally with no handler wired, got path %q", localHit)
	}
}
