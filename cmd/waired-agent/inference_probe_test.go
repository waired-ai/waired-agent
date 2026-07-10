package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/controlclient"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
	"github.com/waired-ai/waired-agent/proto/signer"
)

func TestProbeLocalOllama_Reachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"models":[{"name":"llama3.1:8b"},{"name":"qwen2.5:7b"}]}`)
	}))
	defer srv.Close()

	got := probeLocalOllama(context.Background(), srv.URL, time.Second)
	if !got.Reachable {
		t.Error("probe should succeed against a 200 server")
	}
	if got.Type != signer.InferenceTypeOllama {
		t.Errorf("Type = %q, want %q", got.Type, signer.InferenceTypeOllama)
	}
	if len(got.Models) != 2 || got.Models[0] != "llama3.1:8b" {
		t.Errorf("Models did not parse from /api/tags: %v", got.Models)
	}
	if got.Endpoint != srv.URL {
		t.Errorf("Endpoint = %q, want %q", got.Endpoint, srv.URL)
	}
	if got.LastCheck == "" {
		t.Error("LastCheck must be stamped")
	}
	if got.LastError != "" {
		t.Errorf("LastError should be empty on success, got %q", got.LastError)
	}
}

func TestProbeLocalOllama_5xxIsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	got := probeLocalOllama(context.Background(), srv.URL, time.Second)
	if got.Reachable {
		t.Error("5xx should count as unreachable")
	}
	if got.LastError == "" {
		t.Error("LastError should describe the failure")
	}
}

func TestProbeLocalOllama_NoServer(t *testing.T) {
	got := probeLocalOllama(context.Background(), "http://127.0.0.1:1", 200*time.Millisecond)
	if got.Reachable {
		t.Error("dial of port 1 should fail")
	}
	if got.LastError == "" {
		t.Error("LastError should describe the dial failure")
	}
}

func TestRunLocalInferenceProbe_DisabledPinsLocalFalse(t *testing.T) {
	dir := t.TempDir()
	w := state.NewWriter(dir, state.State{Phase: state.PhaseActive, InferenceReachableLocal: true})
	if err := w.Set(w.Snapshot()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runLocalInferenceProbe(ctx, inferenceProbeDeps{
		StateWriter: w,
		EngineKind:  signer.InferenceTypeOllama,
		EnginePort:  11434,
		Disabled:    true,
		Logger:      slog.Default(),
	})

	got, err := state.Read(dir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.InferenceReachableLocal {
		t.Error("disabled=true must pin InferenceReachableLocal to false")
	}
}

func TestRunLocalInferenceProbe_PicksUpReachableEngine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"models":[]}`)
	}))
	defer srv.Close()

	port, err := portFromURL(srv.URL)
	if err != nil {
		t.Fatalf("port: %v", err)
	}

	dir := t.TempDir()
	w := state.NewWriter(dir, state.State{Phase: state.PhaseActive})
	if err := w.Set(w.Snapshot()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// runLocalInferenceProbe ticks once synchronously then blocks until
	// ctx expires, so this budget doubles as both the probe window and
	// the test's run time. 50ms was too tight under load — the initial
	// tick's httptest round-trip + state write would occasionally exceed
	// it, cancelling the probe mid-flight (Reachable=false). 500ms gives
	// 10x headroom; it stays well under HeartbeatInterval (5s) so no
	// second tick fires, and the ~+0.45s/test is negligible.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	runLocalInferenceProbe(ctx, inferenceProbeDeps{
		StateWriter: w,
		EngineKind:  signer.InferenceTypeOllama,
		EnginePort:  port,
		Logger:      slog.Default(),
	})

	got, err := state.Read(dir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !got.InferenceReachableLocal {
		t.Errorf("InferenceReachableLocal = false, want true (probe URL was %s)", srv.URL)
	}
}

// TestRunLocalInferenceProbe_FeedsAggregatorAndPushClient verifies the
// Phase 3 wiring: a single tick should drive the local-state file,
// the in-memory aggregator (via UpdateLocal), and the CP push client.
func TestRunLocalInferenceProbe_FeedsAggregatorAndPushClient(t *testing.T) {
	// Reachable Ollama mock.
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"models":[{"name":"llama3.1:8b"}]}`)
	}))
	defer ollama.Close()
	port, err := portFromURL(ollama.URL)
	if err != nil {
		t.Fatalf("port: %v", err)
	}

	// CP mock that captures the pushed payload.
	machinePub, machinePriv, _ := ed25519.GenerateKey(rand.Reader)
	var pushCount int32
	var capturedState signer.InferenceState
	cpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&pushCount, 1)
		body, _ := io.ReadAll(r.Body)
		sig, _ := base64.StdEncoding.DecodeString(r.Header.Get("X-Waired-Body-Signature"))
		if !ed25519.Verify(ed25519.PublicKey(machinePub), body, sig) {
			t.Errorf("CP mock: body signature did not verify")
		}
		var req struct {
			State signer.InferenceState `json:"state"`
		}
		_ = json.Unmarshal(body, &req)
		capturedState = req.State
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","content_changed":true}`))
	}))
	defer cpSrv.Close()

	dir := t.TempDir()
	stWriter := state.NewWriter(dir, state.State{Phase: state.PhaseActive})
	if err := stWriter.Set(stWriter.Snapshot()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	agg := inferencemesh.New("dev-self", 15*time.Second, time.Now)
	cli := controlclient.New(cpSrv.URL, "tok")

	// 50ms < HeartbeatInterval (5s), so the loop runs the immediate tick
	// once and then ctx-cancels before the ticker fires. Matches the
	// _PicksUpReachableEngine test's pattern.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	runLocalInferenceProbe(ctx, inferenceProbeDeps{
		StateWriter: stWriter,
		Aggregator:  agg,
		PushClient:  cli,
		DeviceID:    "dev-self",
		MachineKey:  machinePriv,
		EngineKind:  signer.InferenceTypeOllama,
		EnginePort:  port,
		Logger:      slog.Default(),
	})

	if got := atomic.LoadInt32(&pushCount); got < 1 {
		t.Errorf("push count = %d, want ≥ 1", got)
	}
	if !capturedState.Reachable {
		t.Errorf("captured state Reachable=false, want true: %+v", capturedState)
	}
	if len(capturedState.Models) == 0 {
		t.Errorf("captured state has no Models (parsed from /api/tags)")
	}

	snap := agg.Snapshot()
	if snap.Self.InferenceState == nil || !snap.Self.InferenceState.Reachable {
		t.Errorf("aggregator Self.InferenceState not populated: %+v", snap.Self)
	}
}

// Phase 6: when IsShared returns false the probe must update the
// state writer + aggregator locally (so the wrapper and tray still
// see the engine) but skip the CP push (so mesh peers stop seeing
// this engine within the 15 s staleness window).
func TestRunLocalInferenceProbe_SkipsPushWhenShareDenied(t *testing.T) {
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"models":[{"name":"llama3.1:8b"}]}`)
	}))
	defer ollama.Close()
	port, err := portFromURL(ollama.URL)
	if err != nil {
		t.Fatalf("port: %v", err)
	}

	_, machinePriv, _ := ed25519.GenerateKey(rand.Reader)
	var pushCount int32
	cpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&pushCount, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer cpSrv.Close()

	dir := t.TempDir()
	stWriter := state.NewWriter(dir, state.State{Phase: state.PhaseActive})
	if err := stWriter.Set(stWriter.Snapshot()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	agg := inferencemesh.New("dev-self", 15*time.Second, time.Now)
	cli := controlclient.New(cpSrv.URL, "tok")

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	runLocalInferenceProbe(ctx, inferenceProbeDeps{
		StateWriter: stWriter,
		Aggregator:  agg,
		PushClient:  cli,
		DeviceID:    "dev-self",
		MachineKey:  machinePriv,
		EngineKind:  signer.InferenceTypeOllama,
		EnginePort:  port,
		IsShared:    func() bool { return false },
		Logger:      slog.Default(),
	})

	if got := atomic.LoadInt32(&pushCount); got != 0 {
		t.Errorf("CP push count = %d, want 0 when share denied", got)
	}
	// Local-side wiring must still update.
	snap := agg.Snapshot()
	if snap.Self.InferenceState == nil || !snap.Self.InferenceState.Reachable {
		t.Errorf("aggregator Self.InferenceState should still be populated when share denied: %+v", snap.Self)
	}
}

// When IsShared returns true (or is nil), the push proceeds — verify
// the gate doesn't accidentally block by default.
func TestRunLocalInferenceProbe_IsSharedTrueAllowsPush(t *testing.T) {
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"models":[{"name":"llama3.1:8b"}]}`)
	}))
	defer ollama.Close()
	port, err := portFromURL(ollama.URL)
	if err != nil {
		t.Fatalf("port: %v", err)
	}

	_, machinePriv, _ := ed25519.GenerateKey(rand.Reader)
	var pushCount int32
	cpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&pushCount, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer cpSrv.Close()

	dir := t.TempDir()
	stWriter := state.NewWriter(dir, state.State{Phase: state.PhaseActive})
	if err := stWriter.Set(stWriter.Snapshot()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	agg := inferencemesh.New("dev-self", 15*time.Second, time.Now)
	cli := controlclient.New(cpSrv.URL, "tok")

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	runLocalInferenceProbe(ctx, inferenceProbeDeps{
		StateWriter: stWriter,
		Aggregator:  agg,
		PushClient:  cli,
		DeviceID:    "dev-self",
		MachineKey:  machinePriv,
		EngineKind:  signer.InferenceTypeOllama,
		EnginePort:  port,
		IsShared:    func() bool { return true },
		Logger:      slog.Default(),
	})

	if got := atomic.LoadInt32(&pushCount); got < 1 {
		t.Errorf("CP push count = %d, want ≥ 1 when share enabled", got)
	}
}

func portFromURL(s string) (int, error) {
	u, err := url.Parse(s)
	if err != nil {
		return 0, err
	}
	if u.Port() == "" {
		return 0, fmt.Errorf("no port in %q", s)
	}
	return strconv.Atoi(u.Port())
}

func TestProbeLocalVLLM_Reachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
		case "/v1/models":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"Qwen/Qwen3-8B-Instruct","object":"model","owned_by":"vllm"}]}`)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	got := probeLocalVLLM(context.Background(), srv.URL, time.Second)
	if !got.Reachable {
		t.Error("probe should succeed when /health is 200")
	}
	if got.Type != signer.InferenceTypeVLLM {
		t.Errorf("Type = %q, want %q", got.Type, signer.InferenceTypeVLLM)
	}
	if len(got.Models) != 1 || got.Models[0] != "Qwen/Qwen3-8B-Instruct" {
		t.Errorf("Models did not parse from /v1/models: %v", got.Models)
	}
	if got.Endpoint != srv.URL {
		t.Errorf("Endpoint = %q, want %q", got.Endpoint, srv.URL)
	}
	if got.LastCheck == "" {
		t.Error("LastCheck must be stamped")
	}
	if got.LastError != "" {
		t.Errorf("LastError should be empty on success, got %q", got.LastError)
	}
}

func TestProbeLocalVLLM_HealthFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		t.Errorf("/v1/models should not be reached when /health fails; got %q", r.URL.Path)
	}))
	defer srv.Close()

	got := probeLocalVLLM(context.Background(), srv.URL, time.Second)
	if got.Reachable {
		t.Error("/health 503 must map to Reachable=false")
	}
	if got.LastError == "" {
		t.Error("LastError must describe the /health failure")
	}
	if len(got.Models) != 0 {
		t.Errorf("Models should be nil when /health fails, got %v", got.Models)
	}
}

func TestProbeLocalVLLM_ModelsBestEffort(t *testing.T) {
	// /health 200 but /v1/models 500: engine is up, but model list
	// is unavailable. probe stays Reachable=true (the engine answers)
	// while Models stays nil.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
		case "/v1/models":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	got := probeLocalVLLM(context.Background(), srv.URL, time.Second)
	if !got.Reachable {
		t.Error("Reachable must follow /health, not /v1/models")
	}
	if len(got.Models) != 0 {
		t.Errorf("Models should be nil when /v1/models 500, got %v", got.Models)
	}
}

func TestProbeLocalVLLM_NoServer(t *testing.T) {
	got := probeLocalVLLM(context.Background(), "http://127.0.0.1:1", 200*time.Millisecond)
	if got.Reachable {
		t.Error("dial of port 1 should fail")
	}
	if got.LastError == "" {
		t.Error("LastError should describe the dial failure")
	}
}

// TestRunLocalInferenceProbe_DispatchesByEngineKind drives the loop
// against a fake vLLM server and verifies the CP push carries
// Type=vllm + the served-model-name from /v1/models — the bug Phase
// 5 is closing (probe was hard-coded to Ollama before).
func TestRunLocalInferenceProbe_DispatchesByEngineKind(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
		case "/v1/models":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"data":[{"id":"Qwen/Qwen3-8B-Instruct"}]}`)
		case "/api/tags":
			t.Errorf("EngineKind=vllm must not probe ollama's /api/tags")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	port, err := portFromURL(srv.URL)
	if err != nil {
		t.Fatalf("port: %v", err)
	}

	machinePub, machinePriv, _ := ed25519.GenerateKey(rand.Reader)
	var capturedState signer.InferenceState
	cpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sig, _ := base64.StdEncoding.DecodeString(r.Header.Get("X-Waired-Body-Signature"))
		if !ed25519.Verify(ed25519.PublicKey(machinePub), body, sig) {
			t.Errorf("CP body signature did not verify")
		}
		var req struct {
			State signer.InferenceState `json:"state"`
		}
		_ = json.Unmarshal(body, &req)
		capturedState = req.State
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","content_changed":true}`))
	}))
	defer cpSrv.Close()

	dir := t.TempDir()
	stWriter := state.NewWriter(dir, state.State{Phase: state.PhaseActive})
	if err := stWriter.Set(stWriter.Snapshot()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cli := controlclient.New(cpSrv.URL, "tok")

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	runLocalInferenceProbe(ctx, inferenceProbeDeps{
		StateWriter: stWriter,
		PushClient:  cli,
		DeviceID:    "dev-self",
		MachineKey:  machinePriv,
		EngineKind:  signer.InferenceTypeVLLM,
		EnginePort:  port,
		Logger:      slog.Default(),
	})

	if capturedState.Type != signer.InferenceTypeVLLM {
		t.Errorf("pushed Type = %q, want %q (full state: %+v)",
			capturedState.Type, signer.InferenceTypeVLLM, capturedState)
	}
	if !capturedState.Reachable {
		t.Errorf("pushed Reachable=false, want true: %+v", capturedState)
	}
	if len(capturedState.Models) != 1 || capturedState.Models[0] != "Qwen/Qwen3-8B-Instruct" {
		t.Errorf("pushed Models = %v, want [Qwen/Qwen3-8B-Instruct]", capturedState.Models)
	}
}

// TestRunLocalInferenceProbe_NoneKindPinsFalse: when EngineKind is
// empty / "none", the loop must short-circuit identically to
// Disabled=true. Without this, an engine-less host would push spurious
// "reachable=false, type=ollama" entries to its peers.
func TestRunLocalInferenceProbe_NoneKindPinsFalse(t *testing.T) {
	cpSrv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("CP push must not fire when EngineKind is none")
	}))
	defer cpSrv.Close()

	dir := t.TempDir()
	w := state.NewWriter(dir, state.State{Phase: state.PhaseActive, InferenceReachableLocal: true})
	if err := w.Set(w.Snapshot()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, machinePriv, _ := ed25519.GenerateKey(rand.Reader)
	cli := controlclient.New(cpSrv.URL, "tok")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runLocalInferenceProbe(ctx, inferenceProbeDeps{
		StateWriter: w,
		PushClient:  cli,
		DeviceID:    "dev-self",
		MachineKey:  machinePriv,
		EngineKind:  signer.InferenceTypeNone,
		EnginePort:  11434,
		Logger:      slog.Default(),
	})

	got, err := state.Read(dir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.InferenceReachableLocal {
		t.Error("EngineKind=none must pin InferenceReachableLocal to false")
	}
}

// captureLogHandler is a slog.Handler that records every record's
// level + message for assertion. We don't bother with attributes —
// tests only need to count records and inspect message text.
type captureLogHandler struct {
	records []capturedLogRecord
}

type capturedLogRecord struct {
	level slog.Level
	msg   string
}

func (h *captureLogHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *captureLogHandler) Handle(_ context.Context, r slog.Record) error {
	h.records = append(h.records, capturedLogRecord{level: r.Level, msg: r.Message})
	return nil
}
func (h *captureLogHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *captureLogHandler) WithGroup(_ string) slog.Handler      { return h }

func newCaptureLogger() (*slog.Logger, *captureLogHandler) {
	h := &captureLogHandler{}
	return slog.New(h), h
}

func TestNarrowPublishedModels_NoActiveNoChange(t *testing.T) {
	s := &signer.InferenceState{Models: []string{"qwen3:8b"}}
	var sig string
	logger, cap := newCaptureLogger()
	narrowPublishedModels(s, "", &sig, logger)
	if len(s.Models) != 1 || s.Models[0] != "qwen3:8b" {
		t.Errorf("Models mutated unexpectedly: %v", s.Models)
	}
	if len(cap.records) != 0 {
		t.Errorf("no log expected; got %+v", cap.records)
	}
}

func TestNarrowPublishedModels_MatchSilent(t *testing.T) {
	s := &signer.InferenceState{Models: []string{"qwen3:8b"}}
	var sig string
	logger, cap := newCaptureLogger()
	narrowPublishedModels(s, "qwen3:8b", &sig, logger)
	if len(s.Models) != 1 || s.Models[0] != "qwen3:8b" {
		t.Errorf("Models should remain [qwen3:8b]; got %v", s.Models)
	}
	if len(cap.records) != 0 {
		t.Errorf("matched probe must not emit log; got %+v", cap.records)
	}
}

func TestNarrowPublishedModels_SurplusWarnAndNarrow(t *testing.T) {
	s := &signer.InferenceState{Models: []string{"qwen3:8b", "llama:13b", "phi3:14b"}}
	var sig string
	logger, cap := newCaptureLogger()
	narrowPublishedModels(s, "qwen3:8b", &sig, logger)
	if len(s.Models) != 1 || s.Models[0] != "qwen3:8b" {
		t.Errorf("Models must be narrowed to [qwen3:8b]; got %v", s.Models)
	}
	if len(cap.records) != 1 || cap.records[0].level != slog.LevelWarn {
		t.Fatalf("want one warn record; got %+v", cap.records)
	}
	if !strings.Contains(cap.records[0].msg, "surplus") {
		t.Errorf("warn msg should mention surplus; got %q", cap.records[0].msg)
	}
}

func TestNarrowPublishedModels_ActiveNotServedWarn(t *testing.T) {
	s := &signer.InferenceState{Models: []string{"llama:13b"}}
	var sig string
	logger, cap := newCaptureLogger()
	narrowPublishedModels(s, "qwen3:8b", &sig, logger)
	if len(s.Models) != 1 || s.Models[0] != "qwen3:8b" {
		t.Errorf("Models must be [qwen3:8b] even when engine reports a different tag; got %v", s.Models)
	}
	if len(cap.records) != 1 || cap.records[0].level != slog.LevelWarn {
		t.Fatalf("want one warn record; got %+v", cap.records)
	}
	if !strings.Contains(cap.records[0].msg, "not served") {
		t.Errorf("warn msg should mention 'not served'; got %q", cap.records[0].msg)
	}
}

func TestNarrowPublishedModels_EmptyEngineInfoLog(t *testing.T) {
	s := &signer.InferenceState{Models: nil}
	var sig string
	logger, cap := newCaptureLogger()
	narrowPublishedModels(s, "qwen3:8b", &sig, logger)
	if len(s.Models) != 1 || s.Models[0] != "qwen3:8b" {
		t.Errorf("Models must be optimistically set to [qwen3:8b]; got %v", s.Models)
	}
	if len(cap.records) != 1 || cap.records[0].level != slog.LevelInfo {
		t.Fatalf("want one info record; got %+v", cap.records)
	}
	if !strings.Contains(cap.records[0].msg, "not yet reported") {
		t.Errorf("info msg should mention 'not yet reported'; got %q", cap.records[0].msg)
	}
}

func TestNarrowPublishedModels_NoActiveResetsSig(t *testing.T) {
	s := &signer.InferenceState{Models: []string{"qwen3:8b"}}
	var sig string
	logger, _ := newCaptureLogger()
	narrowPublishedModels(s, "", &sig, logger)
	if sig != "" {
		t.Errorf("empty activeTag must reset sig; got %q", sig)
	}
}

func TestNarrowPublishedModels_DedupAcrossTicks(t *testing.T) {
	var sig string
	logger, cap := newCaptureLogger()

	// Tick 1: surplus → warn.
	s1 := &signer.InferenceState{Models: []string{"qwen3:8b", "llama:13b"}}
	narrowPublishedModels(s1, "qwen3:8b", &sig, logger)

	// Tick 2: same surplus → no new warn.
	s2 := &signer.InferenceState{Models: []string{"qwen3:8b", "llama:13b"}}
	narrowPublishedModels(s2, "qwen3:8b", &sig, logger)

	// Tick 3: different surplus → new warn.
	s3 := &signer.InferenceState{Models: []string{"qwen3:8b", "phi3:14b"}}
	narrowPublishedModels(s3, "qwen3:8b", &sig, logger)

	// Expect exactly two warns: tick 1 and tick 3.
	if len(cap.records) != 2 {
		t.Fatalf("expected 2 records (deduped middle tick); got %d: %+v", len(cap.records), cap.records)
	}
	for i, r := range cap.records {
		if r.level != slog.LevelWarn {
			t.Errorf("record %d: want warn, got %v", i, r.level)
		}
	}
}

func TestActiveEngineTag(t *testing.T) {
	tests := []struct {
		name    string
		state   catalog.State
		wantTag string
		wantOK  bool
	}{
		{
			name:    "no active",
			state:   catalog.State{},
			wantTag: "",
			wantOK:  false,
		},
		{
			name: "active model not in Models map",
			state: catalog.State{
				Active: &catalog.ActiveSelection{Runtime: catalog.RuntimeOllama, ModelID: "qwen3-8b", VariantID: "q4"},
				Models: map[string]catalog.ModelState{},
			},
			wantTag: "",
			wantOK:  false,
		},
		{
			name: "ollama tag resolves",
			state: catalog.State{
				Active: &catalog.ActiveSelection{Runtime: catalog.RuntimeOllama, ModelID: "qwen3-8b", VariantID: "q4"},
				Models: map[string]catalog.ModelState{
					"qwen3-8b": {VariantID: "q4", OllamaTag: "qwen3:8b-q4_K_M"},
				},
			},
			wantTag: "qwen3:8b-q4_K_M",
			wantOK:  true,
		},
		{
			name: "vllm repo resolves",
			state: catalog.State{
				Active: &catalog.ActiveSelection{Runtime: catalog.RuntimeVLLM, ModelID: "qwen3-8b", VariantID: "fp16"},
				Models: map[string]catalog.ModelState{
					"qwen3-8b": {VariantID: "fp16", HFRepo: "Qwen/Qwen3-8B"},
				},
			},
			wantTag: "Qwen/Qwen3-8B",
			wantOK:  true,
		},
		{
			name: "variant id mismatch yields no tag",
			state: catalog.State{
				Active: &catalog.ActiveSelection{Runtime: catalog.RuntimeOllama, ModelID: "qwen3-8b", VariantID: "q4"},
				Models: map[string]catalog.ModelState{
					"qwen3-8b": {VariantID: "q8", OllamaTag: "qwen3:8b-q8"},
				},
			},
			wantTag: "",
			wantOK:  false,
		},
		{
			name: "unknown runtime yields no tag",
			state: catalog.State{
				Active: &catalog.ActiveSelection{Runtime: "mlx", ModelID: "qwen3-8b", VariantID: "q4"},
				Models: map[string]catalog.ModelState{
					"qwen3-8b": {VariantID: "q4", OllamaTag: "qwen3:8b-q4"},
				},
			},
			wantTag: "",
			wantOK:  false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := activeEngineTag(tc.state)
			if got != tc.wantTag || ok != tc.wantOK {
				t.Errorf("activeEngineTag = (%q, %v); want (%q, %v)", got, ok, tc.wantTag, tc.wantOK)
			}
		})
	}
}
