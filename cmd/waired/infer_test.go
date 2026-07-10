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
	if got != defaultInferGatewayURL {
		t.Errorf("gateway default = %q, want %q", got, defaultInferGatewayURL)
	}
	if defaultInferGatewayURL != "http://127.0.0.1:9479" {
		t.Errorf("defaultInferGatewayURL = %q, want the no-token :9479 gateway", defaultInferGatewayURL)
	}
}

// inferSSEBody is a minimal OpenAI SSE stream for the happy-path tests.
const inferSSEBody = "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"

func TestRunInferChat_NoTokenNoHeader(t *testing.T) {
	t.Setenv("WAIRED_STATE_DIR", t.TempDir()) // no secrets/gateway-token here

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
		t.Errorf("Authorization sent without a token file: %q", gotAuth)
	}
	if !strings.Contains(out, "ok") {
		t.Errorf("output = %q, want streamed content", out)
	}
}

func TestRunInferChat_AttachesBearerWhenTokenReadable(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("WAIRED_STATE_DIR", stateDir)
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

	_ = captureStdout(t, func() {
		if err := runInferChat(srv.URL, "waired/default", "hi", false); err != nil {
			t.Errorf("runInferChat: %v", err)
		}
	})
	if gotAuth != "Bearer tok123" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer tok123")
	}
}

func TestRunInferChat_401Hint(t *testing.T) {
	t.Setenv("WAIRED_STATE_DIR", t.TempDir())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"missing or malformed Authorization header (Bearer expected)"}}`)
	}))
	defer srv.Close()

	err := runInferChat(srv.URL, "waired/default", "hi", false)
	if err == nil {
		t.Fatal("expected error on 401")
	}
	if !strings.Contains(err.Error(), "gateway returned 401") {
		t.Errorf("error = %v, want the status surfaced", err)
	}
	if !strings.Contains(err.Error(), "9479") {
		t.Errorf("error = %v, want a hint pointing at the token-less :9479 default", err)
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
