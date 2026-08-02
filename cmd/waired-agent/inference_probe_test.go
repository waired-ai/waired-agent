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
	"reflect"
	"strconv"
	"strings"
	"sync"
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

	agg := inferencemesh.New("dev-self", inferencemesh.Policy{}, time.Now)
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

// TestRunLocalInferenceProbe_ReportsShareDenied is the waired#1030
// product contract, and it INVERTS the Phase 6 pin it replaces
// (SkipsPushWhenShareDenied, which required zero pushes).
//
// Suppressing the push did stop peers from seeing the engine, but it also
// stopped the control plane from hearing anything at all — and the stored
// state does not clear, it freezes, so the admin view of a device whose
// operator ran `waired inference share off` stayed frozen at its last
// report forever, showing as STALE two minutes later. A device that never
// shared read as having no engine, and the setup wizard scored its model
// catalog blind.
//
// The agent now keeps reporting and states the fact instead; withholding
// the engine from peers moves to the control plane, which can do it
// without lying to the operator. Refusing the request is still the
// agent's job and still lives in the overlay listener's shareGate.
func TestRunLocalInferenceProbe_ReportsShareDenied(t *testing.T) {
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
	cpSrv, pushes, lastState := capturingCP(t)

	dir := t.TempDir()
	stWriter := state.NewWriter(dir, state.State{Phase: state.PhaseActive})
	if err := stWriter.Set(stWriter.Snapshot()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	agg := inferencemesh.New("dev-self", inferencemesh.Policy{}, time.Now)
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

	if got := pushes(); got == 0 {
		t.Fatal("no CP push while sharing is off; the admin view goes stale again")
	}
	st := lastState()
	if !st.NotShared {
		t.Errorf("pushed not_shared=false while sharing is off: %+v", st)
	}
	// The report is otherwise the truth about a healthy engine — that is
	// the point. Reporting it as unreachable would trade one lie for another.
	if !st.Reachable || st.Type != signer.InferenceTypeOllama {
		t.Errorf("pushed state misdescribes a healthy engine: %+v", st)
	}
	// Local-side wiring must still update.
	snap := agg.Snapshot()
	if snap.Self.InferenceState == nil || !snap.Self.InferenceState.Reachable {
		t.Errorf("aggregator Self.InferenceState should still be populated when share denied: %+v", snap.Self)
	}
}

// TestRunLocalInferenceProbe_SharingOmitsTheField is the byte-form half:
// the default (sharing on) must not put not_shared on the wire at all.
// Product contract — the field is negative-sense + omitempty precisely so
// every unmodified host keeps encoding exactly as it did before it existed.
func TestRunLocalInferenceProbe_SharingOmitsTheField(t *testing.T) {
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
	var mu sync.Mutex
	var bodies []string
	cpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer cpSrv.Close()

	dir := t.TempDir()
	stWriter := state.NewWriter(dir, state.State{Phase: state.PhaseActive})
	if err := stWriter.Set(stWriter.Snapshot()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// A fresh deadline per configuration — one shared context would expire
	// during the first run and make the second a no-op.
	for _, shared := range []func() bool{nil, func() bool { return true }} {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		runLocalInferenceProbe(ctx, inferenceProbeDeps{
			StateWriter: stWriter,
			PushClient:  controlclient.New(cpSrv.URL, "tok"),
			DeviceID:    "dev-self",
			MachineKey:  machinePriv,
			EngineKind:  signer.InferenceTypeOllama,
			EnginePort:  port,
			IsShared:    shared,
			Logger:      slog.Default(),
		})
		cancel()
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) < 2 {
		t.Fatalf("expected a push from each configuration, got %d", len(bodies))
	}
	for i, b := range bodies {
		if strings.Contains(b, "not_shared") {
			t.Errorf("push %d put not_shared on the wire while sharing: %s", i, b)
		}
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
	agg := inferencemesh.New("dev-self", inferencemesh.Policy{}, time.Now)
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
//
// Product contract. It carries no Hardware, which is what makes it the
// regression anchor for #387: the hardware-only report added there must
// stay silent for a host that has nothing to say about itself, so this
// test asserts the same "no push at all" it always did, unchanged.
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

// --- #387: the host profile on an engine-less host ---------------------
//
// Until #387 the summary rode ONLY the probe result, and the probe loop
// returns before it pushes anything when there is no engine to probe. So
// a host that has not decided on an engine — the exact state the browser
// setup wizard operates in — had no path at all to tell the control
// plane what it is, and the wizard scored its catalog blind.

// capturingCP is the CP mock the push-observing tests share: it counts
// pushes and keeps the last state body. Returns the server plus accessors
// so each test states its own expectation.
func capturingCP(t *testing.T) (*httptest.Server, func() int32, func() signer.InferenceState) {
	t.Helper()
	var count int32
	var mu sync.Mutex
	var last signer.InferenceState
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&count, 1)
		body, _ := io.ReadAll(r.Body)
		var req struct {
			State signer.InferenceState `json:"state"`
		}
		_ = json.Unmarshal(body, &req)
		mu.Lock()
		last = req.State
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","content_changed":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() int32 { return atomic.LoadInt32(&count) },
		func() signer.InferenceState {
			mu.Lock()
			defer mu.Unlock()
			return last
		}
}

// testHardwareSummary is the profile the engine-less tests publish. A
// discrete NVIDIA host, so every field the control plane's host-fit reads
// is non-zero and a dropped one shows up as a diff rather than a zero
// that could have come from anywhere.
func testHardwareSummary() *signer.HardwareSummary {
	return &signer.HardwareSummary{
		RAMTotalGB: 64,
		GPUs: []signer.HardwareGPUSummary{{
			Model: "NVIDIA GeForce RTX 4090", VRAMTotalMB: 24564,
			ComputeCap: "8.9", Vendor: "nvidia",
		}},
	}
}

// engineLessDeps is the fresh-host shape: inference enabled, but no
// engine decided yet, so there is no port and no kind to probe.
func engineLessDeps(t *testing.T, cpURL string) inferenceProbeDeps {
	t.Helper()
	dir := t.TempDir()
	w := state.NewWriter(dir, state.State{Phase: state.PhaseActive})
	if err := w.Set(w.Snapshot()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, machinePriv, _ := ed25519.GenerateKey(rand.Reader)
	return inferenceProbeDeps{
		StateWriter: w,
		PushClient:  controlclient.New(cpURL, "tok"),
		DeviceID:    "dev-self",
		MachineKey:  machinePriv,
		EngineKind:  signer.InferenceTypeNone,
		EnginePort:  0,
		Hardware:    func() *signer.HardwareSummary { return testHardwareSummary() },
		Logger:      slog.Default(),
	}
}

// TestRunLocalInferenceProbe_EngineLessPublishesHardware is the #387
// product contract: a host with no engine still tells the control plane
// what it is, on the existing inference-status channel.
//
// The pushed state must be an honest description of that host —
// type=none, reachable=false, no endpoint, no models — carrying only the
// hardware. Anything else would have peers reading it as a broken engine.
func TestRunLocalInferenceProbe_EngineLessPublishesHardware(t *testing.T) {
	cp, pushes, lastState := capturingCP(t)

	// 500 ms is one immediate report and no ticker fire
	// (hardwareOnlyPushInterval is 60 s), so the count below is exact.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	runLocalInferenceProbe(ctx, engineLessDeps(t, cp.URL))

	if got := pushes(); got != 1 {
		t.Fatalf("push count = %d, want exactly 1", got)
	}
	st := lastState()
	if st.Type != signer.InferenceTypeNone {
		t.Errorf("pushed type = %q, want %q", st.Type, signer.InferenceTypeNone)
	}
	if st.Reachable {
		t.Error("pushed reachable=true for a host with no engine")
	}
	if st.Endpoint != "" || len(st.Models) != 0 {
		t.Errorf("pushed endpoint=%q models=%v, want both empty", st.Endpoint, st.Models)
	}
	if st.LastCheck == "" {
		t.Error("pushed last_check is empty (the CP validator rejects that)")
	} else if _, err := time.Parse(time.RFC3339Nano, st.LastCheck); err != nil {
		t.Errorf("pushed last_check %q does not parse as RFC3339Nano: %v", st.LastCheck, err)
	}
	if !reflect.DeepEqual(st.Hardware, testHardwareSummary()) {
		t.Errorf("pushed hardware\n got %+v\nwant %+v", st.Hardware, testHardwareSummary())
	}
}

// TestRunLocalInferenceProbe_EngineLessRespectsDisabled pins the
// --disable-inference contract (product contract): an operator who turned
// inference off is not participating, and #387 must not make that host
// start talking about its GPU.
func TestRunLocalInferenceProbe_EngineLessRespectsDisabled(t *testing.T) {
	cp, pushes, _ := capturingCP(t)

	deps := engineLessDeps(t, cp.URL)
	deps.Disabled = true
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	runLocalInferenceProbe(ctx, deps)

	if got := pushes(); got != 0 {
		t.Errorf("push count = %d, want 0 when inference is disabled", got)
	}
}

// TestRunLocalInferenceProbe_EngineLessReportsShareDenied: the
// hardware-only report follows the SAME rule as the normal push
// (product contract), and it INVERTS the #387 pin it replaces
// (EngineLessRespectsShareDenied, which required zero pushes and named
// the resulting blind spot as a separate decision to make).
//
// waired#1030 made it: the summary no longer rides the served map for a
// device that is not sharing — the control plane withholds the whole
// state from peers — so publishing it widens nothing, and withholding it
// left exactly the hosts the setup wizard needs to score invisible to it.
func TestRunLocalInferenceProbe_EngineLessReportsShareDenied(t *testing.T) {
	cp, pushes, lastState := capturingCP(t)

	deps := engineLessDeps(t, cp.URL)
	deps.IsShared = func() bool { return false }
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	runLocalInferenceProbe(ctx, deps)

	if got := pushes(); got != 1 {
		t.Fatalf("push count = %d, want exactly 1 (sharing off is not silence)", got)
	}
	st := lastState()
	if !st.NotShared {
		t.Errorf("pushed not_shared=false while sharing is off: %+v", st)
	}
	if !reflect.DeepEqual(st.Hardware, testHardwareSummary()) {
		t.Errorf("pushed hardware\n got %+v\nwant %+v", st.Hardware, testHardwareSummary())
	}
}

// TestRunLocalInferenceProbe_EngineLessSkipsUnknownProfile: a host that
// cannot profile itself keeps the field off the wire entirely rather than
// pushing a state with nothing in it (product contract — the same rule
// hardwareSummaryFor already applies by returning nil).
func TestRunLocalInferenceProbe_EngineLessSkipsUnknownProfile(t *testing.T) {
	cp, pushes, _ := capturingCP(t)

	deps := engineLessDeps(t, cp.URL)
	deps.Hardware = func() *signer.HardwareSummary { return nil }
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	runLocalInferenceProbe(ctx, deps)

	if got := pushes(); got != 0 {
		t.Errorf("push count = %d, want 0 when the host profile is unknown", got)
	}
}

// TestRunLocalInferenceProbe_ReadsHardwareEachTick covers the second half
// of #387 on the ENGINE-FUL path: the summary used to be a pointer
// captured at boot, so a host that gained a GPU or a driver kept
// reporting the old answer until the daemon restarted. It is now read
// through a getter, per tick.
//
// Product contract. The assertion is that what lands on the wire is what
// the getter returned at push time, not a value snapshotted when the deps
// were built — which is what a re-read makes possible and a captured
// pointer cannot.
func TestRunLocalInferenceProbe_ReadsHardwareEachTick(t *testing.T) {
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"models":[{"name":"llama3.1:8b"}]}`)
	}))
	defer ollama.Close()
	port, err := portFromURL(ollama.URL)
	if err != nil {
		t.Fatalf("port: %v", err)
	}
	cp, pushes, lastState := capturingCP(t)

	// The driver landed after the deps were constructed: the getter's
	// first answer is the post-change one.
	var calls int32
	upgraded := &signer.HardwareSummary{
		RAMTotalGB: 64,
		GPUs: []signer.HardwareGPUSummary{{
			Model: "NVIDIA GeForce RTX 4090", VRAMTotalMB: 24564,
			ComputeCap: "8.9", Vendor: "nvidia",
		}},
	}

	deps := engineLessDeps(t, cp.URL)
	deps.EngineKind = signer.InferenceTypeOllama
	deps.EnginePort = port
	deps.Hardware = func() *signer.HardwareSummary {
		atomic.AddInt32(&calls, 1)
		return upgraded
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	runLocalInferenceProbe(ctx, deps)

	if got := atomic.LoadInt32(&calls); got < 1 {
		t.Fatalf("hardware getter called %d times, want ≥ 1 — the probe is not re-reading it", got)
	}
	if got := pushes(); got < 1 {
		t.Fatalf("push count = %d, want ≥ 1", got)
	}
	st := lastState()
	if st.Type != signer.InferenceTypeOllama || !st.Reachable {
		t.Errorf("engine-ful push changed shape: type=%q reachable=%v", st.Type, st.Reachable)
	}
	if !reflect.DeepEqual(st.Hardware, upgraded) {
		t.Errorf("pushed hardware\n got %+v\nwant %+v", st.Hardware, upgraded)
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
	narrowPublishedModels(s, "", "", &sig, logger)
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
	narrowPublishedModels(s, "qwen3:8b", "qwen3:8b", &sig, logger)
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
	narrowPublishedModels(s, "qwen3:8b", "qwen3:8b", &sig, logger)
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

// TestNarrowPublishedModels_ActiveNotServedAdvertisesNothing pins a
// PRODUCT CONTRACT (waired-agent#324): when the engine's report does
// not contain the active model, this node is not a candidate for it
// and must say so. It previously advertised the tag anyway, which
// turned a diverged engine into a mesh peer that 404s every request.
func TestNarrowPublishedModels_ActiveNotServedAdvertisesNothing(t *testing.T) {
	s := &signer.InferenceState{Models: []string{"llama:13b"}}
	var sig string
	logger, cap := newCaptureLogger()
	narrowPublishedModels(s, "qwen3:8b", "qwen3:8b", &sig, logger)
	if len(s.Models) != 0 {
		t.Errorf("Models must be empty when the engine does not serve the active model; got %v", s.Models)
	}
	if len(cap.records) != 1 || cap.records[0].level != slog.LevelWarn {
		t.Fatalf("want one warn record; got %+v", cap.records)
	}
	if !strings.Contains(cap.records[0].msg, "not served") {
		t.Errorf("warn msg should mention 'not served'; got %q", cap.records[0].msg)
	}
}

// TestNarrowPublishedModels_EmptyEngineAdvertisesNothing pins the
// other half of the same PRODUCT CONTRACT: an engine that has not
// reported any tag yet (model still pulling, wedged setup) must not
// be advertised as serving one. That optimistic window is what routed
// consumers to a mid-setup peer and produced the rc7 review's 4xx
// "model not found" class (waired-agent#324).
func TestNarrowPublishedModels_EmptyEngineAdvertisesNothing(t *testing.T) {
	s := &signer.InferenceState{Models: nil}
	var sig string
	logger, cap := newCaptureLogger()
	narrowPublishedModels(s, "qwen3:8b", "qwen3:8b", &sig, logger)
	if len(s.Models) != 0 {
		t.Errorf("Models must stay empty while the engine reports nothing; got %v", s.Models)
	}
	if len(cap.records) != 1 || cap.records[0].level != slog.LevelInfo {
		t.Fatalf("want one info record; got %+v", cap.records)
	}
	if !strings.Contains(cap.records[0].msg, "has not reported") {
		t.Errorf("info msg should say the engine has not reported yet; got %q", cap.records[0].msg)
	}
}

// TestNarrowPublishedModels_DerivedTagAdvertisesBase pins the
// waired-agent#324 defect-1 contract: a node serving a #642 derived
// `<base>-wb<batch>` model advertises the BASE tag. Consumer want sets
// are built from Variant.Source.Tag only, so a derived name matches
// nothing and makes the peer permanently unroutable. PRODUCT CONTRACT.
func TestNarrowPublishedModels_DerivedTagAdvertisesBase(t *testing.T) {
	const (
		base    = "qwen3.6:27b-mtp-q4_K_M"
		derived = base + "-wb2048"
	)
	for _, tc := range []struct {
		name     string
		reported []string
	}{
		{"engine lists both", []string{base, derived}},
		{"engine lists only the derived model", []string{derived}},
		{"engine lists only the base", []string{base}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &signer.InferenceState{Models: append([]string(nil), tc.reported...)}
			var sig string
			logger, cap := newCaptureLogger()
			narrowPublishedModels(s, base, derived, &sig, logger)
			if len(s.Models) != 1 || s.Models[0] != base {
				t.Fatalf("must advertise the base tag %q; got %v", base, s.Models)
			}
			// Neither name is surplus: one is what we advertise, the
			// other is what the engine loaded.
			if len(cap.records) != 0 {
				t.Errorf("derived/base pair must not trip the surplus warning; got %+v", cap.records)
			}
		})
	}
}

// TestNarrowPublishedModels_DerivedTagStillReportsSurplus confirms the
// derived-tag exemption is scoped: an unrelated extra tag is still
// surplus and still warns.
func TestNarrowPublishedModels_DerivedTagStillReportsSurplus(t *testing.T) {
	const (
		base    = "qwen3.6:27b-mtp-q4_K_M"
		derived = base + "-wb2048"
	)
	s := &signer.InferenceState{Models: []string{base, derived, "llama:13b"}}
	var sig string
	logger, cap := newCaptureLogger()
	narrowPublishedModels(s, base, derived, &sig, logger)
	if len(s.Models) != 1 || s.Models[0] != base {
		t.Fatalf("must advertise the base tag; got %v", s.Models)
	}
	if len(cap.records) != 1 || cap.records[0].level != slog.LevelWarn {
		t.Fatalf("want one surplus warn; got %+v", cap.records)
	}
	if !strings.Contains(cap.records[0].msg, "surplus") {
		t.Errorf("warn msg should mention surplus; got %q", cap.records[0].msg)
	}
}

func TestNarrowPublishedModels_NoActiveResetsSig(t *testing.T) {
	s := &signer.InferenceState{Models: []string{"qwen3:8b"}}
	var sig string
	logger, _ := newCaptureLogger()
	narrowPublishedModels(s, "", "", &sig, logger)
	if sig != "" {
		t.Errorf("empty activeTag must reset sig; got %q", sig)
	}
}

func TestNarrowPublishedModels_DedupAcrossTicks(t *testing.T) {
	var sig string
	logger, cap := newCaptureLogger()

	// Tick 1: surplus → warn.
	s1 := &signer.InferenceState{Models: []string{"qwen3:8b", "llama:13b"}}
	narrowPublishedModels(s1, "qwen3:8b", "qwen3:8b", &sig, logger)

	// Tick 2: same surplus → no new warn.
	s2 := &signer.InferenceState{Models: []string{"qwen3:8b", "llama:13b"}}
	narrowPublishedModels(s2, "qwen3:8b", "qwen3:8b", &sig, logger)

	// Tick 3: different surplus → new warn.
	s3 := &signer.InferenceState{Models: []string{"qwen3:8b", "phi3:14b"}}
	narrowPublishedModels(s3, "qwen3:8b", "qwen3:8b", &sig, logger)

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

// TestAdvertisedEngineTag is the wire-name half of TestActiveEngineTag:
// what this node tells PEERS it can serve, which diverges from what its
// own engine loaded exactly when a #642 derived batch model is in use.
// PRODUCT CONTRACT (waired-agent#324).
func TestAdvertisedEngineTag(t *testing.T) {
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
			name: "no derived model: advertise what the engine serves",
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
			name: "derived batch model: advertise the base tag it was built from",
			state: catalog.State{
				Active: &catalog.ActiveSelection{Runtime: catalog.RuntimeOllama, ModelID: "qwen3-27b", VariantID: "q4"},
				Models: map[string]catalog.ModelState{
					"qwen3-27b": {
						VariantID:     "q4",
						OllamaTag:     "qwen3.6:27b-mtp-q4_K_M-wb2048",
						BaseOllamaTag: "qwen3.6:27b-mtp-q4_K_M",
					},
				},
			},
			wantTag: "qwen3.6:27b-mtp-q4_K_M",
			wantOK:  true,
		},
		{
			name: "vllm is unaffected: no derived-model concept",
			state: catalog.State{
				Active: &catalog.ActiveSelection{Runtime: catalog.RuntimeVLLM, ModelID: "qwen3-8b", VariantID: "fp16"},
				Models: map[string]catalog.ModelState{
					"qwen3-8b": {VariantID: "fp16", HFRepo: "Qwen/Qwen3-8B", BaseOllamaTag: "ignored"},
				},
			},
			wantTag: "Qwen/Qwen3-8B",
			wantOK:  true,
		},
		{
			name: "variant id mismatch still yields no tag",
			state: catalog.State{
				Active: &catalog.ActiveSelection{Runtime: catalog.RuntimeOllama, ModelID: "qwen3-8b", VariantID: "q4"},
				Models: map[string]catalog.ModelState{
					"qwen3-8b": {VariantID: "q8", OllamaTag: "qwen3:8b-q8", BaseOllamaTag: "qwen3:8b"},
				},
			},
			wantTag: "",
			wantOK:  false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := advertisedEngineTag(tc.state)
			if got != tc.wantTag || ok != tc.wantOK {
				t.Errorf("advertisedEngineTag = (%q, %v); want (%q, %v)", got, ok, tc.wantTag, tc.wantOK)
			}
		})
	}
}

// TestRunLocalInferenceProbe_DeclaredWindowRidesTheAdvertisement pins
// that the declared window (waired#1031) and the advertised model move
// together. A window without a model would tell peers "I serve 200k" of
// nothing; the requesting router matches on the model first and would
// then route to a node that answers 404.
func TestRunLocalInferenceProbe_DeclaredWindowRidesTheAdvertisement(t *testing.T) {
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
	push := func(advertise string, window int) string {
		t.Helper()
		var mu sync.Mutex
		var bodies []string
		cpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			bodies = append(bodies, string(b))
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		}))
		defer cpSrv.Close()

		dir := t.TempDir()
		stWriter := state.NewWriter(dir, state.State{Phase: state.PhaseActive})
		if err := stWriter.Set(stWriter.Snapshot()); err != nil {
			t.Fatalf("seed: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		runLocalInferenceProbe(ctx, inferenceProbeDeps{
			StateWriter:           stWriter,
			PushClient:            controlclient.New(cpSrv.URL, "tok"),
			DeviceID:              "dev-self",
			MachineKey:            machinePriv,
			EngineKind:            signer.InferenceTypeOllama,
			EnginePort:            port,
			AdvertiseTag:          advertise,
			ServingTag:            advertise,
			DeclaredContextWindow: func() int { return window },
			Logger:                slog.Default(),
		})
		cancel()
		mu.Lock()
		defer mu.Unlock()
		if len(bodies) == 0 {
			t.Fatal("no push")
		}
		return bodies[0]
	}

	if b := push("llama3.1:8b", 200704); !strings.Contains(b, `"context_window":200704`) {
		t.Errorf("a serving node did not publish its window: %s", b)
	}
	// The engine reports llama3.1:8b; the agent's Active selection says
	// something else, so narrowPublishedModels withdraws the advertisement.
	// The window must go with it.
	if b := push("qwen3.5-9b:q4_K_M", 200704); strings.Contains(b, "context_window") {
		t.Errorf("published a window with no model to serve it: %s", b)
	}
	// A node that declares nothing keeps the field off the wire entirely,
	// so its peer entry stays byte-identical for readers that predate it.
	if b := push("llama3.1:8b", 0); strings.Contains(b, "context_window") {
		t.Errorf("a node declaring nothing put the field on the wire: %s", b)
	}
}
