package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/internal/runtime"
)

// End-to-end #409: the same measured transcripts, driven through the
// real handler rather than the parser, so the assertions cover the parts
// a unit test cannot — block ordering, stop_reason, the SSE event
// sequence, and what the client actually receives.

// fakeEngineReplying serves one canned assistant turn on both the
// streaming and non-streaming surfaces. text is delivered as several
// deltas when streaming, chopped at an awkward offset so the sentinel
// straddles a delta boundary the way a real engine's tokens do.
func fakeEngineReplying(t *testing.T, text string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &probe)
		if !probe.Stream {
			w.Header().Set("Content-Type", "application/json")
			msg, _ := json.Marshal(map[string]any{
				"id": "chatcmpl-409",
				"choices": []any{map[string]any{
					"index":         0,
					"message":       map[string]any{"role": "assistant", "content": text},
					"finish_reason": "stop",
				}},
				"usage": map[string]int{"prompt_tokens": 7, "completion_tokens": 11},
			})
			_, _ = w.Write(msg)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f, _ := w.(http.Flusher)
		for _, chunk := range chopForStreaming(text) {
			payload, _ := json.Marshal(map[string]any{
				"choices": []any{map[string]any{
					"index": 0,
					"delta": map[string]any{"content": chunk},
				}},
			})
			_, _ = w.Write([]byte("data: " + string(payload) + "\n\n"))
			if f != nil {
				f.Flush()
			}
		}
		_, _ = w.Write([]byte(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if f != nil {
			f.Flush()
		}
	})
	return httptest.NewServer(mux)
}

// chopForStreaming splits s into 7-byte deltas: small enough that every
// sentinel in the fixtures lands across a boundary at least once, which
// is the case a whole-delta scanner would miss.
func chopForStreaming(s string) []string {
	const n = 7
	var out []string
	for i := 0; i < len(s); i += n {
		end := min(i+n, len(s))
		out = append(out, s[i:end])
	}
	return out
}

// recoveryRequestBody is a /v1/messages request offering the tools the
// measured transcripts call into.
func recoveryRequestBody(t *testing.T, stream bool) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model":      "waired/default",
		"max_tokens": 256,
		"stream":     stream,
		"messages":   []any{map[string]any{"role": "user", "content": "read /etc/hostname"}},
		"tools":      readTools(),
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return string(body)
}

func recoveryGateway(t *testing.T, upstreamURL string, rec Recorder) *Server {
	t.Helper()
	reg := runtime.NewRegistry()
	reg.Register(fakeAdapter{baseURL: upstreamURL})
	return NewServer(ServerConfig{}, Deps{
		Selector: &fakeSelector{sel: router.Selection{
			Runtime: "ollama", EngineModel: "qwen2.5-coder:7b", ModelID: "qwen2.5-coder-7b-instruct",
		}},
		Runtimes:       reg,
		ListManifests:  asManifestList(nil),
		HTTPClient:     http.DefaultClient,
		AllowOpenAI:    true,
		AllowAnthropic: true,
		Recorder:       rec,
	})
}

func serveRecovery(t *testing.T, gw *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	return w
}

// Product contract (#409): on the non-streaming path each measured
// dialect arrives at the client as a structured tool_use block with
// stop_reason tool_use, and the leaked fragment is not also delivered as
// text.
func TestAnthropicMessages_NonStreamRecoversLeakedToolCall(t *testing.T) {
	for _, tc := range []struct {
		name, transcript, wantTool string
	}{
		{"qwen3-coder XML", xmlFunctionTranscript, "Bash"},
		{"qwen2.5-coder fenced JSON", fencedJSONTranscript, "Read"},
		{"granite4 delimiter", delimitedTranscript, "Read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := fakeEngineReplying(t, tc.transcript)
			defer upstream.Close()
			rec := &captureRecorder{}
			gw := recoveryGateway(t, upstream.URL, rec)

			w := serveRecovery(t, gw, recoveryRequestBody(t, false))

			var resp AnthropicResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v (%s)", err, w.Body.String())
			}
			if resp.StopReason != "tool_use" {
				t.Errorf("stop_reason = %q, want tool_use", resp.StopReason)
			}
			var toolUse *AnthropicContentBlock
			for i := range resp.Content {
				if resp.Content[i].Type == "tool_use" {
					toolUse = &resp.Content[i]
				}
				if resp.Content[i].Type == "text" && strings.Contains(resp.Content[i].Text, "function=") {
					t.Errorf("the leaked fragment is still visible as text: %q", resp.Content[i].Text)
				}
			}
			if toolUse == nil {
				t.Fatalf("no tool_use block; content=%+v", resp.Content)
			}
			if toolUse.Name != tc.wantTool {
				t.Errorf("tool = %q, want %q", toolUse.Name, tc.wantTool)
			}
			if !json.Valid(toolUse.Input) {
				t.Errorf("tool_use input is not valid JSON: %s", toolUse.Input)
			}

			// The recovery must be recorded, or a silent behaviour
			// change replaces a visible failure.
			events := rec.requestsSnapshot()
			if len(events) != 1 {
				t.Fatalf("recorded %d request events, want 1", len(events))
			}
			if events[0].ToolRecovery == "" {
				t.Error("RequestEvent.ToolRecovery is empty on a turn that recovered a call")
			}
		})
	}
}

// Product contract: a turn where the engine's parser worked is untouched
// — no recovery, no stop_reason rewrite, and ToolRecovery stays empty so
// the telemetry counts only real repairs.
func TestAnthropicMessages_NonStreamLeavesOrdinaryTurnsAlone(t *testing.T) {
	upstream := fakeEngineReplying(t, "The hostname file holds a single line.")
	defer upstream.Close()
	rec := &captureRecorder{}
	gw := recoveryGateway(t, upstream.URL, rec)

	w := serveRecovery(t, gw, recoveryRequestBody(t, false))

	var resp AnthropicResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", resp.StopReason)
	}
	if len(resp.Content) != 1 || resp.Content[0].Type != "text" {
		t.Fatalf("content = %+v, want a single text block", resp.Content)
	}
	if events := rec.requestsSnapshot(); len(events) != 1 || events[0].ToolRecovery != "" {
		t.Errorf("ToolRecovery = %q on an ordinary turn, want empty", events[0].ToolRecovery)
	}
}

// Product contract, and the reason #409 is not a non-streaming-only fix:
// Claude Code always streams. The fragment must never reach the client
// as a text_delta — SSE has no retraction, so a fragment that ships is
// permanent — and the tool_use must arrive with stop_reason tool_use.
func TestAnthropicMessages_StreamRecoversLeakedToolCall(t *testing.T) {
	for _, tc := range []struct {
		name, transcript, wantTool string
	}{
		{"qwen3-coder XML", xmlFunctionTranscript, "Bash"},
		{"qwen2.5-coder fenced JSON", fencedJSONTranscript, "Read"},
		{"granite4 delimiter", delimitedTranscript, "Read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := fakeEngineReplying(t, tc.transcript)
			defer upstream.Close()
			rec := &captureRecorder{}
			gw := recoveryGateway(t, upstream.URL, rec)

			sse := serveRecovery(t, gw, recoveryRequestBody(t, true)).Body.String()

			streamedText := collectTextDeltas(t, sse)
			for _, leak := range []string{"<function=", "<parameter=", "[TOOL_CALLS]", `"arguments"`, "</tool_call>"} {
				if strings.Contains(streamedText, leak) {
					t.Errorf("streamed text carries %q, which the client can never un-see:\n%q", leak, streamedText)
				}
			}
			if !strings.Contains(sse, `"type":"tool_use"`) {
				t.Errorf("no tool_use content block in the stream:\n%s", sse)
			}
			if !strings.Contains(sse, `"name":"`+tc.wantTool+`"`) {
				t.Errorf("tool_use is not %q:\n%s", tc.wantTool, sse)
			}
			if !strings.Contains(sse, `"stop_reason":"tool_use"`) {
				t.Errorf("stop_reason is not tool_use:\n%s", sse)
			}
			if events := rec.requestsSnapshot(); len(events) != 1 || events[0].ToolRecovery == "" {
				t.Error("the streaming recovery was not recorded")
			}
		})
	}
}

// Product contract: the sieve must not eat ordinary streamed prose. The
// client receives exactly the text the engine produced, and the turn
// still ends as end_turn.
func TestAnthropicMessages_StreamOrdinaryTextIsUnchanged(t *testing.T) {
	const reply = "The hostname file holds a single line, and nothing else."
	upstream := fakeEngineReplying(t, reply)
	defer upstream.Close()
	gw := recoveryGateway(t, upstream.URL, nil)

	sse := serveRecovery(t, gw, recoveryRequestBody(t, true)).Body.String()

	if got := collectTextDeltas(t, sse); got != reply {
		t.Errorf("streamed text = %q, want %q", got, reply)
	}
	if !strings.Contains(sse, `"stop_reason":"end_turn"`) {
		t.Errorf("stop_reason is not end_turn:\n%s", sse)
	}
	if strings.Contains(sse, `"type":"tool_use"`) {
		t.Errorf("invented a tool_use block on an ordinary turn:\n%s", sse)
	}
}

// fakeEngineStreamingToolCall serves a turn where the engine's OWN
// parser worked: it streams text alongside proper tool_calls deltas.
func fakeEngineStreamingToolCall(t *testing.T, text string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f, _ := w.(http.Flusher)
		write := func(s string) {
			_, _ = w.Write([]byte("data: " + s + "\n\n"))
			if f != nil {
				f.Flush()
			}
		}
		for _, chunk := range chopForStreaming(text) {
			payload, _ := json.Marshal(map[string]any{
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": chunk}}},
			})
			write(string(payload))
		}
		// Arguments arrive split, the way OpenAI-compatible engines
		// stream them.
		write(`{"choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"file_path\":"}}]}}]}`)
		write(`{"choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"\"/etc/hostname\"}"}}]}}]}`)
		write(`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)
		write("[DONE]")
	})
	return httptest.NewServer(mux)
}

// Product contract: a streamed tool_call the engine parsed itself is
// reassembled and delivered. Untested before #409 despite the
// reassembler being live, and this PR moves code around it.
func TestAnthropicMessages_StreamStructuredToolCall(t *testing.T) {
	upstream := fakeEngineStreamingToolCall(t, "Reading it now.")
	defer upstream.Close()
	rec := &captureRecorder{}
	gw := recoveryGateway(t, upstream.URL, rec)

	sse := serveRecovery(t, gw, recoveryRequestBody(t, true)).Body.String()

	if got := collectTextDeltas(t, sse); got != "Reading it now." {
		t.Errorf("streamed text = %q, want the prose intact", got)
	}
	if !strings.Contains(sse, `"name":"Read"`) {
		t.Errorf("the reassembled tool_use is missing:\n%s", sse)
	}
	if !strings.Contains(sse, `{\"file_path\":\"/etc/hostname\"}`) {
		t.Errorf("tool_use arguments were not reassembled from the split deltas:\n%s", sse)
	}
	if !strings.Contains(sse, `"stop_reason":"tool_use"`) {
		t.Errorf("stop_reason is not tool_use:\n%s", sse)
	}
	if events := rec.requestsSnapshot(); len(events) != 1 || events[0].ToolRecovery != "" {
		t.Error("ToolRecovery must stay empty when the engine's own parser worked")
	}
}

// Product contract: when the engine DID emit a structured tool_call, the
// gateway must not also mine the text for a second one. The engine's
// parser demonstrably worked, so anything call-shaped left in the prose
// is the model talking about a call, not making one — and the held text
// is released verbatim rather than swallowed.
func TestAnthropicMessages_StreamStructuredCallSuppressesRecovery(t *testing.T) {
	const prose = `Next I would run {"name":"Bash","arguments":{"command":"ls"}} if asked.`
	upstream := fakeEngineStreamingToolCall(t, prose)
	defer upstream.Close()
	rec := &captureRecorder{}
	gw := recoveryGateway(t, upstream.URL, rec)

	sse := serveRecovery(t, gw, recoveryRequestBody(t, true)).Body.String()

	if got := collectTextDeltas(t, sse); got != prose {
		t.Errorf("streamed text = %q, want %q byte-for-byte", got, prose)
	}
	if strings.Contains(sse, `"name":"Bash"`) {
		t.Errorf("recovered a second call out of prose:\n%s", sse)
	}
	if events := rec.requestsSnapshot(); len(events) != 1 || events[0].ToolRecovery != "" {
		t.Error("ToolRecovery must stay empty when the engine's own parser worked")
	}
}

// collectTextDeltas concatenates every text_delta in an SSE stream —
// i.e. exactly the prose the client renders.
func collectTextDeltas(t *testing.T, sse string) string {
	t.Helper()
	var out strings.Builder
	for _, line := range strings.Split(sse, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) != nil {
			continue
		}
		if ev.Type == "content_block_delta" && ev.Delta.Type == "text_delta" {
			out.WriteString(ev.Delta.Text)
		}
	}
	return out.String()
}
