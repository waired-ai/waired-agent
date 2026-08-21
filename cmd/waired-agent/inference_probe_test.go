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

	// This test has no CP, so the completion signal is the state write the
	// assertion below reads. The 500 ms budget it replaces had already been
	// raised once from 50 ms for the same reason (#567): the initial tick's
	// httptest round-trip plus the state write occasionally outran it under
	// load, cancelling the probe mid-flight and reading Reachable=false.
	probeRunUntil(t, inferenceProbeDeps{
		StateWriter: w,
		EngineKind:  signer.InferenceTypeOllama,
		EnginePort:  port,
		Logger:      slog.Default(),
	}, "record the engine as reachable", func() bool {
		got, err := state.Read(dir)
		return err == nil && got.InferenceReachableLocal
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
	//
	// It answers on a loopback port the whole test binary can reach, so it
	// takes only the traffic this fixture's own client stamped. Without
	// that it counted, captured and signature-verified anything that
	// arrived: a stranger's request zeroed capturedState and was reported
	// as a product signature defect, on a PR whose diff could not reach the
	// probe (waired-agent#933; the same symptom is recorded at #567).
	machinePub, machinePriv, _ := ed25519.GenerateKey(rand.Reader)
	var pushCount int32
	var capturedState signer.InferenceState
	var cpForeign foreignTraffic
	noteForeignTraffic(t, &cpForeign)
	cpStamp := newFixtureStamp(t)
	cpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !cpForeign.mine(r, cpStamp) ||
			r.Method != http.MethodPost ||
			r.URL.Path != "/v1/devices/self/inference-status" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		sig, _ := base64.StdEncoding.DecodeString(r.Header.Get("X-Waired-Body-Signature"))
		if !ed25519.Verify(ed25519.PublicKey(machinePub), body, sig) {
			// Kept, and now trustworthy: this is the subject's own request,
			// so a verification failure here really is the product's.
			t.Errorf("CP mock: body signature did not verify")
		}
		var req struct {
			State signer.InferenceState `json:"state"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			// Never overwrite the capture with the result of a body that
			// did not parse — that is how an unreadable request used to
			// masquerade as a push of an all-zero state.
			t.Errorf("CP mock: stamped push did not parse: %v", err)
			return
		}
		capturedState = req.State
		// AFTER capturedState, never before. The counter is what
		// probeRunUntil waits on, so incrementing it first would let the
		// run end — and the assertions below read capturedState — while
		// this handler was still filling it in. Ordered this way the
		// atomic pair is also the happens-before edge that makes the
		// unsynchronised read safe.
		atomic.AddInt32(&pushCount, 1)
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
	stampClient(cli.HTTP, cpStamp)

	// probeRunUntil cancels as soon as the predicate below is satisfied, so
	// the loop runs its immediate tick and the ticker never fires.
	probeRunUntil(t, inferenceProbeDeps{
		StateWriter: stWriter,
		Aggregator:  agg,
		PushClient:  cli,
		DeviceID:    "dev-self",
		MachineKey:  machinePriv,
		EngineKind:  signer.InferenceTypeOllama,
		EnginePort:  port,
		Logger:      slog.Default(),
		// waired-agent#970. Wired here rather than in a test of their
		// own because the thing worth pinning is that the tick CARRIES
		// them: the getters are covered separately, and a projection
		// nothing publishes is the shape of defect the producer guard
		// exists for.
		ModelMeasurements: func() []signer.ModelMeasurement {
			return []signer.ModelMeasurement{{
				ModelID: "qwen3.5-9b", VariantID: "q4-gguf", DecodeTokps: 11,
			}}
		},
		ServingEngineVersion: func() string { return "0.32.13" },
	}, "push a state to the control plane", func() bool {
		return atomic.LoadInt32(&pushCount) >= 1
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
	// waired-agent#970: what this host measured, and the engine it serves
	// with, reach the control plane. Without them it ranks the device's
	// catalog page on hardware alone and goes on recommending a model the
	// machine has already timed and rejected.
	if len(capturedState.ModelMeasurements) != 1 ||
		capturedState.ModelMeasurements[0].ModelID != "qwen3.5-9b" ||
		capturedState.ModelMeasurements[0].DecodeTokps != 11 {
		t.Errorf("measurements did not reach the push: %+v", capturedState.ModelMeasurements)
	}
	if capturedState.ServingEngineVersion != "0.32.13" {
		t.Errorf("serving engine version did not reach the push: %q",
			capturedState.ServingEngineVersion)
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

	probeRunUntil(t, inferenceProbeDeps{
		StateWriter: stWriter,
		Aggregator:  agg,
		PushClient:  cli,
		DeviceID:    "dev-self",
		MachineKey:  machinePriv,
		EngineKind:  signer.InferenceTypeOllama,
		EnginePort:  port,
		IsShared:    func() bool { return false },
		Logger:      slog.Default(),
	}, "push a state to the control plane", func() bool { return pushes() >= 1 })

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

	// Each configuration runs until IT has pushed — counted against the
	// bodies seen before it started, so the second run cannot be satisfied by
	// the first one's push.
	for _, shared := range []func() bool{nil, func() bool { return true }} {
		mu.Lock()
		before := len(bodies)
		mu.Unlock()
		probeRunUntil(t, inferenceProbeDeps{
			StateWriter: stWriter,
			PushClient:  controlclient.New(cpSrv.URL, "tok"),
			DeviceID:    "dev-self",
			MachineKey:  machinePriv,
			EngineKind:  signer.InferenceTypeOllama,
			EnginePort:  port,
			IsShared:    shared,
			Logger:      slog.Default(),
		}, "push a state to the control plane", func() bool {
			mu.Lock()
			defer mu.Unlock()
			return len(bodies) > before
		})
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

	probeRunUntil(t, inferenceProbeDeps{
		StateWriter: stWriter,
		Aggregator:  agg,
		PushClient:  cli,
		DeviceID:    "dev-self",
		MachineKey:  machinePriv,
		EngineKind:  signer.InferenceTypeOllama,
		EnginePort:  port,
		IsShared:    func() bool { return true },
		Logger:      slog.Default(),
	}, "push a state to the control plane", func() bool {
		return atomic.LoadInt32(&pushCount) >= 1
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
	// Counted separately from capturedState: probeRunUntil polls the signal
	// from another goroutine, and capturedState is written here without a lock.
	var pushCount int32
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
		atomic.AddInt32(&pushCount, 1)
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

	probeRunUntil(t, inferenceProbeDeps{
		StateWriter: stWriter,
		PushClient:  cli,
		DeviceID:    "dev-self",
		MachineKey:  machinePriv,
		EngineKind:  signer.InferenceTypeVLLM,
		EnginePort:  port,
		Logger:      slog.Default(),
	}, "push a state to the control plane", func() bool {
		return atomic.LoadInt32(&pushCount) >= 1
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
// probeRunUntil is bounded by the package-wide waitBackstop (see
// wait_backstop_test.go). This was the first wait in the package to be given a
// backstop rather than a budget — waired-agent#567, told in full below — and
// waired-agent#720 later moved the rest of the package onto the same figure.

// probeSettlePoll paces probeRunUntil's check of the completion signal. Small
// enough to be invisible next to a test's real work, and it costs nothing on a
// signal that has already fired: done() is checked before the first sleep.
const probeSettlePoll = 2 * time.Millisecond

// probeRunUntil runs runLocalInferenceProbe and stops it the moment `done`
// reports that the thing the test is about to assert has actually happened.
//
// runLocalInferenceProbe ticks once synchronously and then blocks on ctx, so
// a ctx deadline used to double as the whole test budget: 500 ms had to cover
// an httptest round-trip, an aggregator update, a signed CP push and a state
// write, on whatever else the runner was doing at that moment. Thirteen tests
// shared that figure and any of them could lose the race — waired-agent#567
// caught TestRunLocalInferenceProbe_ReportsShareDenied failing in exactly
// 0.50 s with every field of the pushed state at its zero value, on a PR whose
// diff could not reach the probe.
//
// The assertion each of those tests wants is "the probe reported X". A fixed
// wall-clock figure is not part of that claim, and a test that encodes one
// fails for a reason that is not about the subject. So the deadline becomes a
// backstop and the completion signal ends the run.
//
// This does not weaken the "exactly one push" assertions. The probe's second
// tick is a state.HeartbeatInterval (5 s) away, so cancelling on the first
// push lands three orders of magnitude inside the window that would have
// produced a second one — a wider margin than the 500 ms it replaces.
func probeRunUntil(t *testing.T, deps inferenceProbeDeps, what string, done func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitBackstop)
	defer cancel()

	watching := make(chan struct{})
	go func() {
		defer close(watching)
		for {
			if done() {
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(probeSettlePoll):
			}
		}
	}()

	runLocalInferenceProbe(ctx, deps)
	<-watching
	if !done() {
		t.Fatalf("the probe did not %s within %s — the backstop, not a budget: something is wrong with the subject, not with the runner", what, waitBackstop)
	}
}

// probeRunOnce runs runLocalInferenceProbe for a path that RETURNS on its own
// rather than blocking on ctx, so there is nothing to wait for.
//
// Both callers are absence assertions ("nothing was pushed"), and both reach
// an early return before any loop: Disabled short-circuits runHardwareOnlyReport
// at its first line, and the decision is made synchronously on the way there.
// Waiting a fixed 500 ms for that proved nothing and slowed the suite; the
// backstop here exists only so a future change that DOES make the path block
// fails loudly instead of hanging.
func probeRunOnce(t *testing.T, deps inferenceProbeDeps) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitBackstop)
	defer cancel()
	runLocalInferenceProbe(ctx, deps)
	if ctx.Err() != nil {
		t.Fatalf("the probe blocked instead of returning: this path is expected to short-circuit, so an absence assertion after it no longer proves anything")
	}
}

func capturingCP(t *testing.T) (*httptest.Server, func() int32, func() signer.InferenceState) {
	t.Helper()
	var count int32
	var mu sync.Mutex
	var last signer.InferenceState
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			State signer.InferenceState `json:"state"`
		}
		_ = json.Unmarshal(body, &req)
		mu.Lock()
		last = req.State
		mu.Unlock()
		// LAST, and that is the whole contract of this counter: probeRunUntil
		// ends the run the instant pushes() goes non-zero and then reads
		// lastState(). Incremented first — as it was — the counter means "a
		// request arrived", so a reader could win the race against this
		// handler's own unmarshal and see a zero-value InferenceState.
		// Incremented here it means "a push is fully recorded", which is what
		// every caller actually waits for.
		atomic.AddInt32(&count, 1)
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
	probeRunUntil(t, engineLessDeps(t, cp.URL),
		"push a hardware profile", func() bool { return pushes() >= 1 })

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
	probeRunOnce(t, deps)

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
	probeRunUntil(t, deps, "push a hardware profile", func() bool { return pushes() >= 1 })

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
	// Wait on the getter, not on a clock: an absence assertion is only worth
	// anything once the code that would have pushed has actually run. A fixed
	// 500 ms could end before the first push() and pass vacuously.
	var hwCalls int32
	deps.Hardware = func() *signer.HardwareSummary {
		atomic.AddInt32(&hwCalls, 1)
		return nil
	}
	probeRunUntil(t, deps, "consult the hardware getter",
		func() bool { return atomic.LoadInt32(&hwCalls) >= 1 })

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

	probeRunUntil(t, deps, "re-read the hardware profile and push",
		func() bool { return atomic.LoadInt32(&calls) >= 1 && pushes() >= 1 })

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
		probeRunUntil(t, inferenceProbeDeps{
			StateWriter:           stWriter,
			PushClient:            controlclient.New(cpSrv.URL, "tok"),
			DeviceID:              "dev-self",
			MachineKey:            machinePriv,
			EngineKind:            signer.InferenceTypeOllama,
			EnginePort:            port,
			EngineTags:            func() (string, string) { return advertise, advertise },
			DeclaredContextWindow: func() int { return window },
			Logger:                slog.Default(),
		}, "push a state to the control plane", func() bool {
			mu.Lock()
			defer mu.Unlock()
			return len(bodies) > 0
		})
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

// TestRunLocalInferenceProbe_AdvertiseTagIsReadLiveNotCapturedAtBoot pins
// that the advertised tag is re-read on every probe tick rather than
// captured once when the loop is wired.
//
// PRODUCT CONTRACT (waired-ai/waired-agent#656). narrowPublishedModels
// skips the "1 agent = 1 model" narrowing entirely while the tag is empty
// (its own doc comment says so), and the tag is empty on a daemon that
// booted before its Active selection was written — the ordinary case on a
// fresh install, because Active is committed asynchronously and, since
// waired-ai/waired-agent#812, choosing a model no longer restarts the
// agent. Captured at boot, that skip lasted the life of the process, so
// the host advertised every tag on disk — the host-speed probe model
// included, which is what put a 0.8b model on every node in the admin UI.
//
// This is the same defect waired-ai/waired-agent#387 fixed for Hardware
// and Capacity; those two are getters for exactly this reason and this
// one was left behind.
func TestRunLocalInferenceProbe_AdvertiseTagIsReadLiveNotCapturedAtBoot(t *testing.T) {
	const active = "qwen3.5:4b-q4_K_M"
	// What ollama has on disk: the model this host serves, the host-speed
	// probe model, and one an operator pulled by hand.
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"models":[{"name":"`+active+`"},{"name":"qwen3.5:0.8b-q8_0"},{"name":"llama3.1:8b"}]}`)
	}))
	defer ollama.Close()
	port, err := portFromURL(ollama.URL)
	if err != nil {
		t.Fatalf("port: %v", err)
	}

	_, machinePriv, _ := ed25519.GenerateKey(rand.Reader)

	var mu sync.Mutex
	var bodies []string
	// tag is what activeEngineTagsForActive would answer: empty until the
	// Active selection lands, then the engine tag for it.
	var tag string
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

	probeRunUntil(t, inferenceProbeDeps{
		StateWriter: stWriter,
		PushClient:  controlclient.New(cpSrv.URL, "tok"),
		DeviceID:    "dev-self",
		MachineKey:  machinePriv,
		EngineKind:  signer.InferenceTypeOllama,
		EnginePort:  port,
		EngineTags: func() (string, string) {
			mu.Lock()
			defer mu.Unlock()
			return tag, tag
		},
		Logger: slog.Default(),
	}, "narrow the advertisement once the Active selection landed", func() bool {
		mu.Lock()
		defer mu.Unlock()
		if len(bodies) == 0 {
			return false
		}
		if tag == "" {
			// The first push has been observed with no Active selection.
			// Commit one, exactly as activatePreferredIfNeeded does after
			// the model finishes downloading.
			tag = active
			return false
		}
		for _, b := range bodies[1:] {
			if strings.Contains(b, `"models":["`+active+`"]`) {
				return true
			}
		}
		return false
	})

	mu.Lock()
	defer mu.Unlock()
	// The first push predates the Active selection. Everything on disk goes
	// out — a record of today's behaviour, not a contract: it is what
	// narrowPublishedModels documents for an empty tag, and the reason the
	// tag must not stay empty.
	if !strings.Contains(bodies[0], "qwen3.5:0.8b-q8_0") {
		t.Errorf("expected the pre-Active push to carry the whole disk; got %s", bodies[0])
	}
	// The last push must carry exactly the one tag, with neither the
	// host-speed probe model nor the hand-pulled surplus.
	last := bodies[len(bodies)-1]
	if !strings.Contains(last, `"models":["`+active+`"]`) {
		t.Errorf("advertisement was never narrowed — the tag is still a boot snapshot: %s", last)
	}
	if strings.Contains(last, "qwen3.5:0.8b-q8_0") {
		t.Errorf("host-speed probe model reached peers: %s", last)
	}
	if strings.Contains(last, "llama3.1:8b") {
		t.Errorf("surplus model reached peers: %s", last)
	}
}

// waired#1064: the two fields a peer's picker reads. The shape mirrors the
// window test above, and the point of it is the ONE place they differ —
// these must survive narrowPublishedModels withdrawing the advertisement,
// because the withdrawn node is exactly the one a peer needs explained.
func TestRunLocalInferenceProbe_ActiveModelAndStateExplainAWithdrawnNode(t *testing.T) {
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
	push := func(advertise, model, subState string) string {
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
		probeRunUntil(t, inferenceProbeDeps{
			StateWriter:    stWriter,
			PushClient:     controlclient.New(cpSrv.URL, "tok"),
			DeviceID:       "dev-self",
			MachineKey:     machinePriv,
			EngineKind:     signer.InferenceTypeOllama,
			EnginePort:     port,
			EngineTags:     func() (string, string) { return advertise, advertise },
			ActiveModel:    func() string { return model },
			SubsystemState: func() string { return subState },
			Logger:         slog.Default(),
		}, "push a state to the control plane", func() bool {
			mu.Lock()
			defer mu.Unlock()
			return len(bodies) > 0
		})
		mu.Lock()
		defer mu.Unlock()
		if len(bodies) == 0 {
			t.Fatal("no push")
		}
		return bodies[0]
	}

	// Serving: both ride, alongside the engine tag they are NOT.
	b := push("llama3.1:8b", "llama3-1-8b-instruct", signer.SubsystemStateReady)
	if !strings.Contains(b, `"active_model":"llama3-1-8b-instruct"`) {
		t.Errorf("a serving node did not publish its model: %s", b)
	}
	if !strings.Contains(b, `"subsystem_state":"ready"`) {
		t.Errorf("a serving node did not publish its state: %s", b)
	}
	if !strings.Contains(b, `"models":["llama3.1:8b"]`) {
		t.Errorf("the engine tag stopped riding alongside it: %s", b)
	}

	// PRODUCT CONTRACT (waired#1064): the engine reports llama3.1:8b while
	// the Active selection says something else, so narrowPublishedModels
	// withdraws the advertisement — mid-pull, mid-switch, or a diverged
	// engine. ContextWindow goes with it; these two must NOT, because a
	// peer with no model tag and no explanation is exactly the node this
	// issue exists to stop rendering as a bare "unavailable".
	b = push("qwen3.5-9b:q4_K_M", "qwen3-5-9b-instruct", signer.SubsystemStateLoading)
	if strings.Contains(b, `"models"`) {
		t.Fatalf("precondition: the advertisement should have been withdrawn: %s", b)
	}
	if !strings.Contains(b, `"active_model":"qwen3-5-9b-instruct"`) {
		t.Errorf("a withdrawn node stopped naming its model: %s", b)
	}
	if !strings.Contains(b, `"subsystem_state":"loading"`) {
		t.Errorf("a withdrawn node stopped explaining itself: %s", b)
	}

	// A node with no committed selection declares neither, and both stay
	// off the wire entirely so its peer entry is byte-identical for a
	// reader that predates them.
	b = push("llama3.1:8b", "", "")
	if strings.Contains(b, "active_model") || strings.Contains(b, "subsystem_state") {
		t.Errorf("a node declaring nothing put the fields on the wire: %s", b)
	}
}

// waired-agent#647: the push carries WHEN a person here chose, and stays
// silent otherwise. The silence is the load-bearing half — the control
// plane treats a claim as licence to move its own desired-model
// instruction, so a host that never chose must not appear to have.
func TestRunLocalInferenceProbe_LocalModelChoiceRidesOnlyWhenSomeoneChose(t *testing.T) {
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
	push := func(chosenAt func() string) string {
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
		probeRunUntil(t, inferenceProbeDeps{
			StateWriter:        stWriter,
			PushClient:         controlclient.New(cpSrv.URL, "tok"),
			DeviceID:           "dev-self",
			MachineKey:         machinePriv,
			EngineKind:         signer.InferenceTypeOllama,
			EnginePort:         port,
			EngineTags:         func() (string, string) { return "llama3.1:8b", "llama3.1:8b" },
			LocalModelChoiceAt: chosenAt,
			Logger:             slog.Default(),
		}, "push a state to the control plane", func() bool {
			mu.Lock()
			defer mu.Unlock()
			return len(bodies) > 0
		})
		mu.Lock()
		defer mu.Unlock()
		if len(bodies) == 0 {
			t.Fatal("no push")
		}
		return bodies[0]
	}

	b := push(func() string { return "2026-08-10T02:31:04.512Z" })
	if !strings.Contains(b, `"local_model_choice_at":"2026-08-10T02:31:04.512Z"`) {
		t.Errorf("a host where someone chose did not publish when: %s", b)
	}

	// The getter answers "" for every no-claim case the provider knows —
	// no file, an abandoned question, an instruction the reconciler
	// applied, a record from before provenance existed.
	if b := push(func() string { return "" }); strings.Contains(b, "local_model_choice_at") {
		t.Errorf("a host making no claim put the field on the wire: %s", b)
	}
	// An agent built before the getter was wired, and every provider that
	// does not have one: byte-identical to what it pushed before.
	if b := push(nil); strings.Contains(b, "local_model_choice_at") {
		t.Errorf("an unwired probe put the field on the wire: %s", b)
	}
}

// waired#1232: the push carries the residency this host actually has and
// when a person here last set one, and stays silent when it has nothing
// to claim on either axis independently.
//
// The silence is load-bearing on both. A reported residency is what the
// control plane realigns its instruction ONTO, and the timestamp is what
// licenses the move — a host that publishes a value it does not have, or
// a choice nobody made, would have its instruction rewritten to match a
// fiction.
func TestRunLocalInferenceProbe_ResidencyRidesWithItsProvenance(t *testing.T) {
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
	push := func(residency, chosenAt func() string) string {
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
		probeRunUntil(t, inferenceProbeDeps{
			StateWriter:            stWriter,
			PushClient:             controlclient.New(cpSrv.URL, "tok"),
			DeviceID:               "dev-self",
			MachineKey:             machinePriv,
			EngineKind:             signer.InferenceTypeOllama,
			EnginePort:             port,
			EngineTags:             func() (string, string) { return "llama3.1:8b", "llama3.1:8b" },
			Residency:              residency,
			LocalResidencyChoiceAt: chosenAt,
			Logger:                 slog.Default(),
		}, "push a state to the control plane", func() bool {
			mu.Lock()
			defer mu.Unlock()
			return len(bodies) > 0
		})
		mu.Lock()
		defer mu.Unlock()
		if len(bodies) == 0 {
			t.Fatal("no push")
		}
		return bodies[0]
	}

	b := push(
		func() string { return "45m0s" },
		func() string { return "2026-08-21T09:15:04.5Z" },
	)
	if !strings.Contains(b, `"residency_idle_timeout":"45m0s"`) {
		t.Errorf("the residency this host has did not reach the wire: %s", b)
	}
	if !strings.Contains(b, `"local_residency_choice_at":"2026-08-21T09:15:04.5Z"`) {
		t.Errorf("the provenance did not reach the wire: %s", b)
	}

	// "0s" is a REPORTED VALUE — held indefinitely — and must survive
	// omitempty. It is what a vLLM host reports, and what any ollama host
	// on the product default reports (waired-agent#943).
	if b := push(func() string { return "0s" }, nil); !strings.Contains(b, `"residency_idle_timeout":"0s"`) {
		t.Errorf("an indefinite hold was swallowed as if it were no claim: %s", b)
	}

	// Each axis is silent on its own terms: a host that reports a value
	// but no local choice is the ordinary case, and it must not appear to
	// have chosen.
	b = push(func() string { return "45m0s" }, func() string { return "" })
	if !strings.Contains(b, `"residency_idle_timeout":"45m0s"`) {
		t.Errorf("the value was dropped along with the absent provenance: %s", b)
	}
	if strings.Contains(b, "local_residency_choice_at") {
		t.Errorf("a host where nobody chose appeared to have: %s", b)
	}

	// An agent built before the getters were wired: byte-identical to
	// what it pushed before either field existed.
	if b := push(nil, nil); strings.Contains(b, "residency_idle_timeout") ||
		strings.Contains(b, "local_residency_choice_at") {
		t.Errorf("an unwired probe put the fields on the wire: %s", b)
	}
}
