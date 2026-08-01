package agentgrade

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The measured body: qwen3.5:4b-q4_K_M emitted tool-call syntax ollama
// could not parse, and ollama answered 500. The gateway wraps the
// engine's message in its own upstream_error envelope, which is the
// exact shape the probe sees.
const engineParseFailureBody = `{"type":"error","error":{"type":"upstream_error",` +
	`"message":"{\"error\":{\"message\":\"XML syntax error on line 14: element ` +
	`\\u003cfunction\\u003e closed by \\u003c/parameter\\u003e\",\"type\":\"api_error\"}}"}}`

func probeAgainst(t *testing.T, h http.HandlerFunc) Probe {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return Probe{BaseURL: srv.URL, Timeout: 5 * time.Second}
}

// A model whose output the engine cannot parse must be GRADED, not
// excused. Recording it as unmeasured would let unparseable output be
// the one way to dodge the gate: an unmeasured model is never retired.
func TestRun_engineParseFailureGradesTheModel(t *testing.T) {
	p := probeAgainst(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(engineParseFailureBody))
	})

	rep, err := p.Run(context.Background(), "subject")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Grade != GradeFail {
		t.Fatalf("grade = %q, want %q (error=%q)", rep.Grade, GradeFail, rep.Error)
	}
	if rep.Results[0].Verdict != VerdictMalformedToolCall {
		t.Errorf("verdict = %q, want %q", rep.Results[0].Verdict, VerdictMalformedToolCall)
	}
	if !strings.Contains(rep.Results[0].Evidence, "XML syntax error") {
		t.Errorf("evidence should carry the engine's message, got %q", rep.Results[0].Evidence)
	}
}

// The other half of the same split: an engine that is simply not
// answering is NOT a verdict about the model. Getting this wrong is
// waired-ai/waired-agent#203 — an upstream failure recorded as a
// quality result, de-rating something that was never tested.
func TestRun_engineDownIsNotAVerdict(t *testing.T) {
	for _, tt := range []struct {
		name string
		h    http.HandlerFunc
	}{
		{"503 with no parse marker", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"model runner has unexpectedly stopped"}}`))
		}},
		{"404 model not ready", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"router: model is not in ready state on disk"}}`))
		}},
		{"500 out of memory", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"failed to allocate CUDA buffer"}}`))
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := probeAgainst(t, tt.h)
			rep, err := p.Run(context.Background(), "subject")
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if rep.Grade != GradeUnknown {
				t.Errorf("grade = %q, want %q — an unreachable engine is not a model verdict",
					rep.Grade, GradeUnknown)
			}
			if rep.Results[0].Verdict != VerdictError {
				t.Errorf("verdict = %q, want %q", rep.Results[0].Verdict, VerdictError)
			}
		})
	}
}

// A fail-open — the request escaping local routing to the real upstream
// — must be visible in the error, not silently graded. Without the
// marker, "the model answered badly" and "this answer came from
// somewhere else entirely" are indistinguishable (waired-agent#29).
func TestRun_failOpenMarkerSurfaces(t *testing.T) {
	p := probeAgainst(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Waired-Fallback", "local_status_404")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"upstream"}`))
	})
	rep, err := p.Run(context.Background(), "subject")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Grade != GradeUnknown {
		t.Fatalf("grade = %q, want %q", rep.Grade, GradeUnknown)
	}
	if !strings.Contains(rep.Results[0].Detail, "did not stay local") {
		t.Errorf("detail must name the fail-open, got %q", rep.Results[0].Detail)
	}
}

func TestRun_passingModel(t *testing.T) {
	p := probeAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		// The last user turn is the case prompt; answer the two
		// tool-requiring cases with a structured call and the greeting
		// with prose.
		last := ""
		if n := len(req.Messages); n > 0 {
			last = string(req.Messages[n-1].Content)
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(last, "hello") {
			_, _ = w.Write([]byte(`{"type":"message","role":"assistant","content":[{"type":"text","text":"Hello! How can I help?"}],"stop_reason":"end_turn"}`))
			return
		}
		_, _ = w.Write([]byte(`{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/etc/hostname"}}],"stop_reason":"tool_use"}`))
	})

	rep, err := p.Run(context.Background(), "subject")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Grade != GradePass {
		t.Fatalf("grade = %q, want %q; results=%+v", rep.Grade, GradePass, rep.Results)
	}
	if len(rep.Results) != len(Cases) {
		t.Errorf("got %d results for %d cases", len(rep.Results), len(Cases))
	}
}

func TestIsEngineParseFailure(t *testing.T) {
	if !IsEngineParseFailure(engineParseFailureBody) {
		t.Error("the measured qwen3.5:4b body must be recognised")
	}
	// Every marker names a parse of GENERATED content. An engine that is
	// down, loading, or out of memory must not match, or the split
	// collapses in the direction that excuses bad models.
	for _, body := range []string{
		`{"error":"model runner has unexpectedly stopped"}`,
		`{"error":"failed to allocate CUDA buffer"}`,
		`{"error":"connection refused"}`,
		`{"error":"context deadline exceeded"}`,
		`{"error":"router: model is not in ready state on disk"}`,
		``,
	} {
		if IsEngineParseFailure(body) {
			t.Errorf("body %q must NOT read as a model parse failure", body)
		}
	}
}
