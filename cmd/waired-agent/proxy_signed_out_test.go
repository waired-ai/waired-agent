package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// proxyHandle is the one thing that survives a sign-out still pointing at the
// session: the Claude listener on :9472 is built at boot, and the handler is
// published once, at activation. Before waired-agent#1310 there was no way to
// unpublish it — SetLocalInference(nil) returned early — so a signed-out
// daemon went on answering Waired-addressed turns out of a torn-down session.
//
// PIN: product contract — sign-out takes local inference off :9472
// (waired-ai/waired-agent#1310, owner ruling 2026-09-12: close the port's local
// side and error on a Waired model id).
func TestProxyHandleSignOutUnpublishesLocalInference(t *testing.T) {
	ph := &proxyHandle{}

	if ok, _ := ph.localServing(); ok {
		t.Error("a handle with nothing published reported that it can serve")
	}

	ph.SetLocalInference(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "LOCAL")
	}))
	if ok, _ := ph.localServing(); !ok {
		t.Fatal("publishing local inference did not make the handle serve")
	}

	ph.ClearLocalInference(signedOutReason)
	ok, reason := ph.localServing()
	if ok {
		t.Error("sign-out left local inference published")
	}
	if reason != signedOutReason {
		t.Errorf("reason = %q, want the sign-out sentence", reason)
	}

	// Signing back in republishes, and drops the sign-out reason with it.
	ph.SetLocalInference(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	if ok, reason := ph.localServing(); !ok || reason != "" {
		t.Errorf("after signing back in: serving=%v reason=%q, want true and no reason", ok, reason)
	}
}

// The guard branch answers in the Anthropic error shape rather than with a
// bare 502 — a 5xx here is one Claude Code retries ten times before showing
// the person anything (waired-agent#1180).
func TestProxyHandleAdapterAnswersInTheAnthropicShape(t *testing.T) {
	ph := &proxyHandle{}
	ph.ClearLocalInference(signedOutReason)

	srv := httptest.NewServer(ph.localAdapter())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/anthropic/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

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
		t.Fatalf("answer is not JSON: %s", body)
	}
	if got.Error.Type != "waired_cannot_serve" {
		t.Errorf("error type = %q, want waired_cannot_serve", got.Error.Type)
	}
	if !strings.Contains(got.Error.Message, "waired init") {
		t.Errorf("the sign-out answer does not name the command that undoes it: %q", got.Error.Message)
	}
}
