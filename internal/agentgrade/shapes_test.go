package agentgrade

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/gateway"
)

// shapeRejection500 is ollama 0.32.13's answer to a non-leading
// instruction turn on qwen3.8, verbatim
// (docs/knowledges/20260827/1330-qwen38-on-a-24gb-card.md §1).
const shapeRejection500 = `{"error":{"message":"system message must be at the beginning","type":"api_error"}}`

const chatCompletion200 = `{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"length"}]}`

// fakeEngine serves /v1/chat/completions. rejectRoles names the message
// role sequences it refuses, standing in for a strict chat template;
// knownModels gates the negative control.
type fakeEngine struct {
	rejectRoles map[string]bool
	knownModel  string
	always200   bool
	seen        []string
}

func newFakeEngine(t *testing.T, e *fakeEngine) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		roles := make([]string, 0, len(body.Messages))
		for _, m := range body.Messages {
			roles = append(roles, m.Role)
		}
		key := strings.Join(roles, ",")
		e.seen = append(e.seen, key)

		w.Header().Set("Content-Type", "application/json")
		if e.always200 {
			_, _ = w.Write([]byte(chatCompletion200))
			return
		}
		if e.knownModel != "" && body.Model != e.knownModel {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"model not found"}}`))
			return
		}
		if e.rejectRoles[key] {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(shapeRejection500))
			return
		}
		_, _ = w.Write([]byte(chatCompletion200))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func shapeProbeFor(url string) ShapeProbe {
	return ShapeProbe{
		EngineURL:     url,
		EngineName:    "ollama",
		EngineVersion: "0.32.13",
		Client:        http.DefaultClient,
	}
}

func TestShapeProbe_RecordsARejectionWithItsMarker(t *testing.T) {
	eng := &fakeEngine{
		knownModel: "qwen3.8:27b",
		// The three shapes ollama 0.32.13 refused on qwen3.8.
		rejectRoles: map[string]bool{
			"user,system":                            true,
			"system,system,user":                     true,
			"system,user,assistant,tool,system,user": true,
		},
	}
	srv := newFakeEngine(t, eng)

	rep, err := shapeProbeFor(srv.URL).Run(context.Background(), "qwen3.8:27b")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := rep.Valid(); err != nil {
		t.Fatalf("report should be valid: %v", err)
	}
	if rep.Expected != len(gateway.EngineShapes()) || rep.Measured != rep.Expected {
		t.Fatalf("expected %d measured %d, want both %d", rep.Expected, rep.Measured, len(gateway.EngineShapes()))
	}

	rejected := rep.Rejected()
	if len(rejected) != 3 {
		t.Fatalf("rejected %d shapes, want 3: %+v", len(rejected), rep.Results)
	}
	for _, r := range rejected {
		if r.Marker != ShapeMarkerRequestShape {
			t.Errorf("%s: marker = %q, want %q", r.Shape, r.Marker, ShapeMarkerRequestShape)
		}
		if r.Status != http.StatusInternalServerError {
			t.Errorf("%s: status = %d, want 500", r.Shape, r.Status)
		}
	}

	// The row that broke us must be one of them, by name.
	var got bool
	for _, r := range rejected {
		if r.Shape == "trailing-system" {
			got = true
		}
	}
	if !got {
		t.Error("the trailing-system row (#1035) was not among the rejections")
	}
}

// TestShapeProbe_AcceptingEveryShapeIsAValidResult is the answer most
// models give, and it has to stay distinguishable from a broken run.
func TestShapeProbe_AcceptingEveryShapeIsAValidResult(t *testing.T) {
	srv := newFakeEngine(t, &fakeEngine{knownModel: "qwen3.6:35b"})

	rep, err := shapeProbeFor(srv.URL).Run(context.Background(), "qwen3.6:35b")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := rep.Valid(); err != nil {
		t.Fatalf("report should be valid: %v", err)
	}
	if len(rep.Rejected()) != 0 {
		t.Errorf("rejected %d shapes, want 0", len(rep.Rejected()))
	}
	if !rep.ControlOK {
		t.Error("the control should have held: the engine 404s an absent model")
	}
}

// TestShapeProbe_AnEngineThatAcceptsAnythingFailsTheControl is the
// case the control exists for: every row reads "accepted" and the run
// still must not count.
func TestShapeProbe_AnEngineThatAcceptsAnythingFailsTheControl(t *testing.T) {
	srv := newFakeEngine(t, &fakeEngine{always200: true})

	rep, err := shapeProbeFor(srv.URL).Run(context.Background(), "anything")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(rep.Rejected()) != 0 {
		t.Fatalf("precondition: this engine rejects nothing")
	}
	if rep.ControlOK {
		t.Fatal("the control held against an engine that answers 200 to an absent model")
	}
	if err := rep.Valid(); err == nil {
		t.Fatal("a report whose control did not hold must not be valid")
	} else if !strings.Contains(err.Error(), "negative control") {
		t.Errorf("unhelpful reason: %v", err)
	}
}

func TestShapeProbe_TransportFailureIsNotARecord(t *testing.T) {
	// A port nothing is listening on.
	rep, err := shapeProbeFor("http://127.0.0.1:1").Run(context.Background(), "m")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.Measured != rep.Expected {
		t.Fatalf("every row should have been attempted: %d of %d", rep.Measured, rep.Expected)
	}
	if err := rep.Valid(); err == nil {
		t.Fatal("a report with an errored row must not be valid")
	}
	for _, r := range rep.Results {
		if r.Outcome != ShapeError {
			t.Errorf("%s: outcome = %q, want %q", r.Shape, r.Outcome, ShapeError)
		}
		if r.Detail == "" {
			t.Errorf("%s: an errored row must say why", r.Shape)
		}
	}
}

// TestShapeProbe_A200ThatIsNotACompletionIsNotAnAcceptance keeps a
// proxy, a stub, or a captive portal from being recorded as a model
// accepting every shape.
func TestShapeProbe_A200ThatIsNotACompletionIsNotAnAcceptance(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	rep, err := shapeProbeFor(srv.URL).Run(context.Background(), "m")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, r := range rep.Results {
		if r.Outcome != ShapeError {
			t.Errorf("%s: outcome = %q, want %q", r.Shape, r.Outcome, ShapeError)
		}
	}
	if err := rep.Valid(); err == nil {
		t.Fatal("a report of non-completions must not be valid")
	}
}

func TestShapeReport_ValidRefusesAnUnstampedEngineVersion(t *testing.T) {
	srv := newFakeEngine(t, &fakeEngine{knownModel: "m"})
	p := shapeProbeFor(srv.URL)
	p.EngineVersion = ""

	rep, err := p.Run(context.Background(), "m")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := rep.Valid(); err == nil {
		t.Fatal("a report with no engine version must not be valid")
	} else if !strings.Contains(err.Error(), "engine version") {
		t.Errorf("unhelpful reason: %v", err)
	}
}

func TestShapeReport_ValidRefusesAPartialRun(t *testing.T) {
	rep := ShapeReport{EngineVersion: "0.32.15", Expected: 6, Measured: 4, ControlOK: true}
	if err := rep.Valid(); err == nil {
		t.Fatal("a partial run must not be valid")
	} else if !strings.Contains(err.Error(), "partial") {
		t.Errorf("unhelpful reason: %v", err)
	}
}

// TestShapeProbe_PostsEveryShapeAndTheControl pins that the probe
// actually drives the table rather than a subset of it.
func TestShapeProbe_PostsEveryShapeAndTheControl(t *testing.T) {
	eng := &fakeEngine{knownModel: "m"}
	srv := newFakeEngine(t, eng)

	if _, err := shapeProbeFor(srv.URL).Run(context.Background(), "m"); err != nil {
		t.Fatalf("run: %v", err)
	}
	// One request per shape, plus the control.
	if want := len(gateway.EngineShapes()) + 1; len(eng.seen) != want {
		t.Fatalf("engine saw %d requests, want %d: %v", len(eng.seen), want, eng.seen)
	}
	for _, s := range gateway.EngineShapes() {
		key := strings.Join(s.EngineRoles(), ",")
		var found bool
		for _, seen := range eng.seen {
			if seen == key {
				found = true
			}
		}
		if !found {
			t.Errorf("shape %q (%s) never reached the engine", s.Name, key)
		}
	}
}
