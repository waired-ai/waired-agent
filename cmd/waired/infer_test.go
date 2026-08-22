package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStreamChatResponse_Tokens(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Hello"}}]}`,
		`data: {"choices":[{"delta":{"content":", "}}]}`,
		`data: {"choices":[{"delta":{"content":"world!"}}]}`,
		`data: [DONE]`,
		``,
	}, "\n")

	out := captureStdout(t, func() {
		if err := streamChatResponse(strings.NewReader(stream), false); err != nil {
			t.Fatalf("streamChatResponse: %v", err)
		}
	})

	if !strings.Contains(out, "Hello, world!") {
		t.Errorf("output missing reassembled text: %q", out)
	}
}

func TestStreamChatResponse_RawJSON(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hi"}}]}`,
		`data: [DONE]`,
		``,
	}, "\n")
	out := captureStdout(t, func() {
		if err := streamChatResponse(strings.NewReader(stream), true); err != nil {
			t.Fatalf("streamChatResponse: %v", err)
		}
	})
	if !strings.Contains(out, `"delta":{"content":"hi"}`) {
		t.Errorf("raw mode should emit chunk JSON, got %q", out)
	}
}

func TestInferGatewayFlagDefault(t *testing.T) {
	cmd := newInferCmd()
	got := cmd.Flags().Lookup("gateway").DefValue
	if got != defaultGatewayURL {
		t.Errorf("gateway default = %q, want %q", got, defaultGatewayURL)
	}
	// One local gateway. This used to point at :9479, a second listener that
	// existed only because :9473 wanted a Bearer a non-root CLI could not
	// read (#598); both are gone (waired-ai/waired#1277).
	if defaultGatewayURL != "http://127.0.0.1:9473" {
		t.Errorf("defaultGatewayURL = %q, want the single local gateway on :9473", defaultGatewayURL)
	}
}

// inferSSEBody is a minimal OpenAI SSE stream for the happy-path tests.
const inferSSEBody = "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"

// TestRunInferChat_NeverSendsAuthorization replaces three tests that pinned
// the opposite: one that a missing token file meant no header, one that a
// readable token file meant "Bearer <tok>", and one that a 401 came back
// with a hint about which gateway needed the token.
//
// None of those has a subject any more. `waired infer` used to read
// <state>/secrets/gateway-token on every run and attach it — to a default
// gateway that never checked it — which meant the common path shipped a
// secret to a listener with no opinion about it. The credential is gone
// (waired-ai/waired#1277), so the property worth holding is that the
// command sends nothing, whatever is lying around on disk.
func TestRunInferChat_NeverSendsAuthorization(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("WAIRED_STATE_DIR", stateDir)
	// A leftover from an older install: the file exists and is readable.
	// It must still not be picked up.
	if err := os.MkdirAll(filepath.Join(stateDir, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "secrets", "gateway-token"), []byte("tok123\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, inferSSEBody)
	}))
	defer srv.Close()

	out := captureStdout(t, func() {
		if err := runInferChat(srv.URL, "waired/default", "hi", false); err != nil {
			t.Errorf("runInferChat: %v", err)
		}
	})
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want none — a leftover token file must not be read", gotAuth)
	}
	if !strings.Contains(out, "ok") {
		t.Errorf("output = %q, want streamed content", out)
	}
}

func TestParseModelLifecycle(t *testing.T) {
	body := []byte(`{"models":{"ready":["qwen3-8b-instruct"],"downloading":[],"failed":[]}}`)
	line, done, err := parseModelLifecycle(body, "qwen3-8b-instruct")
	if !done || err != nil {
		t.Errorf("expected done=true err=nil, got done=%v err=%v line=%q", done, err, line)
	}
	if !strings.Contains(line, "ready") {
		t.Errorf("line = %q", line)
	}

	body = []byte(`{"models":{"ready":[],"downloading":["qwen3-8b-instruct"]}}`)
	line, done, _ = parseModelLifecycle(body, "qwen3-8b-instruct")
	if done {
		t.Errorf("downloading must not be done")
	}
	if !strings.Contains(line, "downloading") {
		t.Errorf("line = %q", line)
	}

	body = []byte(`{"models":{"failed":["qwen3-8b-instruct"]}}`)
	_, done, err = parseModelLifecycle(body, "qwen3-8b-instruct")
	if !done || err == nil {
		t.Errorf("failed must be done with error, got done=%v err=%v", done, err)
	}
}

// TestParseModelLifecycleCarriesTheFailureReason is waired-agent#328's
// regression bar. Product contract: when the daemon recorded WHY a pull
// stopped, both the printed line and the returned error say so.
//
// The rc7 review is the reason it exists — `waired models pull` printed
// "failed" and exited with "pull failed", and the operator had to read
// the daemon's journal to learn that the download had been killed rather
// than refused by a registry.
func TestParseModelLifecycleCarriesTheFailureReason(t *testing.T) {
	const reason = "download: start ollama: context canceled"
	body := []byte(`{"models":{"failed":["qwen3-8b-instruct"],` +
		`"failures":[{"model":"qwen3-8b-instruct","error":"` + reason + `"}]}}`)
	line, done, err := parseModelLifecycle(body, "qwen3-8b-instruct")
	if !done {
		t.Fatalf("failed must be done, got %v", done)
	}
	if !strings.Contains(line, reason) {
		t.Errorf("line = %q, want the reason in it", line)
	}
	if err == nil || !strings.Contains(err.Error(), reason) {
		t.Errorf("err = %v, want the reason in it", err)
	}

	// A failures entry for a DIFFERENT model must not be borrowed: two
	// concurrent pulls are ordinary, and quoting one model's cause under
	// another's name is worse than quoting none.
	body = []byte(`{"models":{"failed":["qwen3-8b-instruct"],` +
		`"failures":[{"model":"other-model","error":"no space left on device"}]}}`)
	line, _, err = parseModelLifecycle(body, "qwen3-8b-instruct")
	if strings.Contains(line, "no space") || (err != nil && strings.Contains(err.Error(), "no space")) {
		t.Errorf("borrowed another model's reason: line=%q err=%v", line, err)
	}

	// An older daemon sends no failures array at all. Degrade to the bare
	// line rather than to a dangling separator.
	body = []byte(`{"models":{"failed":["qwen3-8b-instruct"]}}`)
	line, _, err = parseModelLifecycle(body, "qwen3-8b-instruct")
	if line != "qwen3-8b-instruct: failed" {
		t.Errorf("line = %q, want the pre-#328 bare line", line)
	}
	if err == nil || err.Error() != "pull failed" {
		t.Errorf("err = %v, want the pre-#328 bare error", err)
	}
}

// captureStdout swaps os.Stdout for a pipe, runs fn, and returns the
// captured output. Tests use this rather than instrumenting the
// streaming path to avoid leaking io.Writer plumbing into the
// production code.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	w.Close()
	os.Stdout = old
	return <-done
}

// TestParseModelLifecycleReportsAModelNothingHasStartedOn is #403's
// consumer-side bar. `waired models pull --wait` printed NOTHING for a
// model in none of the three lists and spun to its timeout with no
// explanation, because that was the only observation available. It is
// not terminal — the pull was just accepted, so a state row is moments
// away — but the loop has to say what it is looking at.
func TestParseModelLifecycleReportsAModelNothingHasStartedOn(t *testing.T) {
	body := []byte(`{"models":{"ready":[],"downloading":[],"not_present":["qwen3-8b-instruct"]}}`)
	line, done, err := parseModelLifecycle(body, "qwen3-8b-instruct")
	if done || err != nil {
		t.Fatalf("not_present must keep polling, got done=%v err=%v", done, err)
	}
	if !strings.Contains(line, "not started yet") {
		t.Errorf("line = %q, want it to say nothing has started", line)
	}
}

// An older daemon sends no not_present list, and the loop keeps its
// pre-#403 silence rather than inventing a state for a model it cannot
// see.
func TestParseModelLifecycleStaysQuietWithoutTheList(t *testing.T) {
	body := []byte(`{"models":{"ready":["another-model"],"downloading":[]}}`)
	line, done, err := parseModelLifecycle(body, "qwen3-8b-instruct")
	if line != "" || done || err != nil {
		t.Fatalf("parseModelLifecycle = (%q, %v, %v), want the unchanged silent answer", line, done, err)
	}
}
