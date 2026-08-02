package agentgrade

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/gateway"
	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/internal/runtime"
)

// The claim this file exists to defend: a verdict does not depend on the
// transport the probe drove.
//
// That is not obvious and is not free. Since waired-ai/waired-agent#409
// the gateway recovers a leaked tool call through two unrelated
// implementations — a whole-body parse on one path, and on the other a
// sieve that must decide what to withhold before it has seen the end of
// the turn. If those two ever disagree, the probe would report it as a
// property of the MODEL. Driving the same canned engine reply through
// the real gateway on both paths, and comparing what the classifier
// makes of each, is what turns that into a build failure instead.
//
// It also pins readAnthropicStream against the real SSE encoder rather
// than against frames a test author wrote from the same reading of the
// code that produced the bug.

// engineReply is one canned assistant turn, named for what it is meant
// to exercise. Each body is a shape actually measured on this fixture
// (see verdict_test.go for the transcripts these are drawn from).
type engineReply struct {
	name string
	text string
	// wantTool is the tool the gateway must recover from this reply, on
	// BOTH paths. Empty means the reply must stay text.
	//
	// Asserted separately from the agreement check because two paths can
	// agree by both doing nothing: without this, a bug that disabled
	// recovery everywhere would leave the comparison green.
	wantTool string
}

var transportReplies = []engineReply{
	{
		name: "prose only",
		text: "Hello! I can help with that — what would you like me to look at?",
	},
	{
		name: "qwen3-coder XML dialect leaked as text",
		text: "I'll read that file for you.\n\n<function=Read>" +
			"<parameter=file_path>/etc/hostname</parameter></function>",
		wantTool: "Read",
	},
	{
		name: "fenced JSON object leaked as text",
		text: "Let me check.\n\n```json\n" +
			`{"name": "Read", "arguments": {"file_path": "/etc/hostname"}}` + "\n```",
		wantTool: "Read",
	},
	{
		name: "unoffered tool name (must stay text on both paths)",
		text: "```json\n" + `{"name": "LaunchMissiles", "arguments": {"target": "moon"}}` + "\n```",
	},
}

func TestProbeTransportsAgree(t *testing.T) {
	names, err := OfferedTools()
	if err != nil {
		t.Fatalf("OfferedTools: %v", err)
	}
	for _, reply := range transportReplies {
		t.Run(reply.name, func(t *testing.T) {
			engine := httptest.NewServer(cannedEngine(t, reply.text))
			defer engine.Close()
			gw := httptest.NewServer(transportGateway(engine.URL).Handler())
			defer gw.Close()
			base := gw.URL + "/anthropic"

			unary := runCases(t, Probe{BaseURL: base}, names)
			stream := runCases(t, Probe{BaseURL: base, Stream: true}, names)

			for i, c := range Cases {
				u, s := unary[i], stream[i]
				if u.Verdict != s.Verdict {
					t.Errorf("case %s: unary %s, stream %s — the transports disagree, "+
						"which is a gateway defect and not a fact about any model\n"+
						"  unary  detail: %s\n  stream detail: %s",
						c.Name, u.Verdict, s.Verdict, u.Detail, s.Detail)
				}
				if !slices.Equal(u.ToolsCalled, s.ToolsCalled) {
					t.Errorf("case %s: tools called unary %v, stream %v",
						c.Name, u.ToolsCalled, s.ToolsCalled)
				}
				if u.StopReason != s.StopReason {
					t.Errorf("case %s: stop_reason unary %q, stream %q",
						c.Name, u.StopReason, s.StopReason)
				}
				// Agreement alone is not the claim: both paths have to
				// have actually done the recovery.
				want := []string(nil)
				if reply.wantTool != "" {
					want = []string{reply.wantTool}
				}
				if !slices.Equal(u.ToolsCalled, want) {
					t.Errorf("case %s: tools called %v, want %v (unary)", c.Name, u.ToolsCalled, want)
				}
			}
		})
	}
}

// The probe must also report which transport it drove, since that is
// what a stored verdict records.
func TestProbeReportsTransport(t *testing.T) {
	engine := httptest.NewServer(cannedEngine(t, "hi"))
	defer engine.Close()
	gw := httptest.NewServer(transportGateway(engine.URL).Handler())
	defer gw.Close()

	for _, tc := range []struct {
		stream bool
		want   string
	}{{false, TransportUnary}, {true, TransportStream}} {
		p := Probe{BaseURL: gw.URL + "/anthropic", Stream: tc.stream, Trials: 1}
		rep, err := p.Run(context.Background(), "waired/test")
		if err != nil {
			t.Fatalf("stream=%t: %v", tc.stream, err)
		}
		// Run reports an unmeasurable turn as GradeUnknown rather than an
		// error, so without this the transport assertion below would pass
		// on a run where every single case failed to reach the gateway.
		if rep.Grade == GradeUnknown {
			t.Fatalf("stream=%t: nothing was measured: %s", tc.stream, rep.Error)
		}
		if rep.Transport != tc.want {
			t.Errorf("stream=%t: transport = %q, want %q", tc.stream, rep.Transport, tc.want)
		}
	}
}

// runCases drives every case once and returns the classified results in
// Cases order. One trial, because the engine is canned: the answer is
// deterministic and repeating it would only measure the fake.
func runCases(t *testing.T, p Probe, offered map[string]json.RawMessage) []Result {
	t.Helper()
	out := make([]Result, 0, len(Cases))
	for _, c := range Cases {
		res := p.one(context.Background(), "waired/test", c, offered)
		if res.Verdict == VerdictError {
			t.Fatalf("case %s (stream=%t) could not be measured: %s", c.Name, p.Stream, res.Detail)
		}
		out = append(out, res)
	}
	return out
}

// cannedEngine serves the same assistant text on both OpenAI surfaces.
// Streaming deltas are 7 bytes so every tool-call sentinel lands across a
// boundary at least once — the case a whole-delta scanner would miss.
func cannedEngine(t *testing.T, text string) http.Handler {
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
				"id": "chatcmpl-426",
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
		for i := 0; i < len(text); i += 7 {
			chunk := text[i:min(i+7, len(text))]
			payload, _ := json.Marshal(map[string]any{
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": chunk}}},
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
	return mux
}

func transportGateway(engineURL string) *gateway.Server {
	reg := runtime.NewRegistry()
	reg.Register(transportAdapter{baseURL: engineURL})
	return gateway.NewServer(gateway.ServerConfig{}, gateway.Deps{
		Selector: transportSelector{sel: router.Selection{
			Runtime: "ollama", EngineModel: "canned", ModelID: "canned",
		}},
		Runtimes:       reg,
		ListManifests:  func() []catalog.Manifest { return nil },
		HTTPClient:     http.DefaultClient,
		AllowAnthropic: true,
	})
}

type transportAdapter struct{ baseURL string }

func (a transportAdapter) Name() string                          { return "ollama" }
func (a transportAdapter) EnsureRunning(_ context.Context) error { return nil }
func (a transportAdapter) Health(_ context.Context) runtime.Health {
	return runtime.Health{State: runtime.StateReady}
}
func (a transportAdapter) Stop(_ context.Context) error { return nil }
func (a transportAdapter) BaseURL() string              { return a.baseURL }

type transportSelector struct{ sel router.Selection }

func (s transportSelector) Select(_ context.Context, _ router.Request) (router.Selection, error) {
	return s.sel, nil
}

// SelectK is the path the gateway actually takes. NewLocalCandidate is
// what supplies a working commit closure; a hand-built Candidate has a
// nil one and every request 503s as "every matching mesh peer is at
// capacity".
func (s transportSelector) SelectK(_ context.Context, _ router.Request, _ int) ([]router.Candidate, error) {
	return []router.Candidate{router.NewLocalCandidate(s.sel)}, nil
}
