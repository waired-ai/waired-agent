package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/controlclient"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// fakeSetupProvider implements setupProvider with scriptable state. Its
// benchmark start flips the status to running, mirroring the real
// single-flight job so idempotency tests reflect the production contract.
type fakeSetupProvider struct {
	mu              sync.Mutex
	engineInstalled bool
	engineReady     bool
	modelState      string
	modelCompleted  int64
	modelTotal      int64
	modelErr        string
	bench           management.BenchmarkStatusResponse
	benchStarts     []int
	pulls           []string
	pullErr         error
}

func (f *fakeSetupProvider) setupEngineState(context.Context, string) (bool, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.engineInstalled, f.engineReady
}

func (f *fakeSetupProvider) setupModelState(string) (string, int64, int64, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.modelState, f.modelCompleted, f.modelTotal, f.modelErr
}

func (f *fakeSetupProvider) BenchmarkStatus() management.BenchmarkStatusResponse {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bench
}

func (f *fakeSetupProvider) startSetupBenchmark(gen int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.benchStarts = append(f.benchStarts, gen)
	f.bench.State = management.BenchmarkStateRunning
}

func (f *fakeSetupProvider) PullModel(_ context.Context, model string) (management.PullJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulls = append(f.pulls, model)
	if f.pullErr != nil {
		return management.PullJob{}, f.pullErr
	}
	f.modelState = catalog.ModelStateQueued
	return management.PullJob{JobID: "job-1", ModelID: model, Status: "queued"}, nil
}

func desiredFrame(engine, model string, gen int) *signer.InferenceState {
	return &signer.InferenceState{
		DesiredEngine:       engine,
		DesiredModelID:      model,
		DesiredBenchmarkGen: gen,
	}
}

// TestSetupApplyIdempotent pins the §6 contract: streaming has no frame
// dedup, so replaying the identical desired state must trigger each
// action at most once — one benchmark job, one pull admission.
func TestSetupApplyIdempotent(t *testing.T) {
	f := &fakeSetupProvider{modelState: catalog.ModelStateNotPresent}
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	ctx := context.Background()

	frame := desiredFrame("", "qwen3-8b-instruct", 2)
	for i := 0; i < 3; i++ {
		r.Apply(ctx, frame)
	}
	if len(f.benchStarts) != 1 || f.benchStarts[0] != 2 {
		t.Fatalf("benchStarts = %v, want exactly one start at gen 2", f.benchStarts)
	}
	if len(f.pulls) != 1 || f.pulls[0] != "qwen3-8b-instruct" {
		t.Fatalf("pulls = %v, want exactly one pull", f.pulls)
	}
}

// TestSetupApplyZeroDesiredIsFree pins the fleet-at-rest guarantee: a
// host that never ran a NAVI setup does no work and reports nothing.
func TestSetupApplyZeroDesiredIsFree(t *testing.T) {
	f := &fakeSetupProvider{}
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	ctx := context.Background()

	r.Apply(ctx, nil)
	r.Apply(ctx, &signer.InferenceState{})
	if len(f.benchStarts) != 0 || len(f.pulls) != 0 {
		t.Fatalf("zero desired must not act: starts=%v pulls=%v", f.benchStarts, f.pulls)
	}
	if snap := r.snapshot(ctx); snap != nil {
		t.Fatalf("zero desired must snapshot nil, got %+v", snap)
	}
}

// TestSetupApplySkipsPresentModelAndAnsweredBenchmark: a model that is
// already local (any in-flight/ready state) is never re-pulled, and a
// benchmark that already ran at the requested generation — even a
// FAILED one — is never rerun (failure is an answer; NAVI re-bumps).
func TestSetupApplySkipsPresentModelAndAnsweredBenchmark(t *testing.T) {
	f := &fakeSetupProvider{
		modelState: catalog.ModelStateReady,
		bench:      management.BenchmarkStatusResponse{State: management.BenchmarkStateFailed, Gen: 3, Error: "boom"},
	}
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	r.Apply(context.Background(), desiredFrame("", "qwen3-8b-instruct", 3))
	if len(f.pulls) != 0 {
		t.Fatalf("ready model re-pulled: %v", f.pulls)
	}
	if len(f.benchStarts) != 0 {
		t.Fatalf("answered (failed) gen rerun: %v", f.benchStarts)
	}
}

// TestSetupSnapshotStatuses walks the §7 step derivations.
func TestSetupSnapshotStatuses(t *testing.T) {
	ctx := context.Background()

	// Engine missing → failed + permission_denied (§11: install is the
	// executor's job).
	f := &fakeSetupProvider{modelState: catalog.ModelStateDownloading, modelCompleted: 512, modelTotal: 4096}
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	r.Apply(ctx, desiredFrame("ollama", "m1", 1))

	snap := r.snapshot(ctx)
	if snap == nil || len(snap.Steps) != 3 {
		t.Fatalf("snapshot = %+v, want 3 steps", snap)
	}
	eng, mod, bench := snap.Steps[0], snap.Steps[1], snap.Steps[2]
	if eng.ID != setupStepEngineInstall || eng.Status != signer.SetupStatusFailed || eng.ErrorCode != signer.SetupErrorPermissionDenied {
		t.Fatalf("engine step = %+v, want failed/permission_denied", eng)
	}
	if mod.ID != setupStepModelPull || mod.Status != signer.SetupStatusRunning || mod.CompletedBytes != 512 || mod.TotalBytes != 4096 {
		t.Fatalf("model step = %+v, want running with bytes", mod)
	}
	// startSetupBenchmark flipped the fake to running.
	if bench.ID != setupStepBenchmark || bench.Status != signer.SetupStatusRunning {
		t.Fatalf("benchmark step = %+v, want running", bench)
	}

	// Engine installed-but-not-ready → running; ready → done. Benchmark
	// done at gen carries the measurement.
	f.mu.Lock()
	f.engineInstalled = true
	f.modelState = catalog.ModelStateReady
	f.bench = management.BenchmarkStatusResponse{State: management.BenchmarkStateDone, Gen: 1, MeasuredTokps: 42.5}
	f.mu.Unlock()
	snap = r.snapshot(ctx)
	if snap.Steps[0].Status != signer.SetupStatusRunning {
		t.Fatalf("installed engine step = %+v, want running", snap.Steps[0])
	}
	if snap.Steps[1].Status != signer.SetupStatusDone {
		t.Fatalf("ready model step = %+v, want done", snap.Steps[1])
	}
	if snap.Steps[2].Status != signer.SetupStatusDone || snap.Benchmark == nil ||
		snap.Benchmark.Gen != 1 || snap.Benchmark.MeasuredTokps != 42.5 {
		t.Fatalf("benchmark step = %+v benchmark=%+v, want done + measurement", snap.Steps[2], snap.Benchmark)
	}

	f.mu.Lock()
	f.engineReady = true
	f.mu.Unlock()
	if snap = r.snapshot(ctx); snap.Steps[0].Status != signer.SetupStatusDone {
		t.Fatalf("ready engine step = %+v, want done", snap.Steps[0])
	}

	// Failed pull carries the stored error as network_error.
	f.mu.Lock()
	f.modelState = catalog.ModelStateFailed
	f.modelErr = "connection reset"
	f.mu.Unlock()
	snap = r.snapshot(ctx)
	if snap.Steps[1].Status != signer.SetupStatusFailed || snap.Steps[1].ErrorCode != signer.SetupErrorNetworkError ||
		snap.Steps[1].ErrorDetail != "connection reset" {
		t.Fatalf("failed model step = %+v, want failed/network_error", snap.Steps[1])
	}
}

// TestSetupPullRejectedReportsModelNotFound: PullModel refusing the ID
// (not in the catalog) surfaces as failed/model_not_found and is not
// retried on later frames.
func TestSetupPullRejectedReportsModelNotFound(t *testing.T) {
	f := &fakeSetupProvider{modelState: catalog.ModelStateNotPresent, pullErr: errors.New("unknown model")}
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	ctx := context.Background()

	frame := desiredFrame("", "no-such-model", 0)
	r.Apply(ctx, frame)
	r.Apply(ctx, frame)
	if len(f.pulls) != 1 {
		t.Fatalf("rejected pull retried: %v", f.pulls)
	}
	snap := r.snapshot(ctx)
	if len(snap.Steps) != 1 || snap.Steps[0].Status != signer.SetupStatusFailed ||
		snap.Steps[0].ErrorCode != signer.SetupErrorModelNotFound || snap.Steps[0].ErrorDetail != "unknown model" {
		t.Fatalf("rejected pull step = %+v, want failed/model_not_found", snap.Steps[0])
	}
}

// TestSetupPushDedupes drives runPush against a fake CP: identical
// snapshots push once, a content change pushes again, and the payload
// is a validly signed UpsertSetupProgressRequest shape.
func TestSetupPushDedupes(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies [][]byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v1/devices/self/setup-progress" {
			t.Errorf("unexpected path %s", req.URL.Path)
		}
		b, _ := io.ReadAll(req.Body)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cli := controlclient.NewWithBearer(srv.URL, func() string { return "tok" })

	f := &fakeSetupProvider{modelState: catalog.ModelStateNotPresent}
	r := newSetupReconciler(f, cli, "dev-1", priv, quietLogger())
	r.interval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); r.runPush(ctx) }()

	r.Apply(ctx, desiredFrame("", "m1", 0)) // → pending pull → queued
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(bodies) >= 1 }, "first push")
	time.Sleep(50 * time.Millisecond) // several ticks with unchanged content
	mu.Lock()
	afterFirst := len(bodies)
	mu.Unlock()
	if afterFirst != 1 {
		t.Fatalf("unchanged content pushed %d times, want 1", afterFirst)
	}

	f.mu.Lock()
	f.modelState = catalog.ModelStateReady
	f.mu.Unlock()
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(bodies) >= 2 }, "second push after change")

	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	var req struct {
		DeviceID string               `json:"device_id"`
		IssuedAt string               `json:"issued_at"`
		Nonce    string               `json:"nonce"`
		Progress signer.SetupProgress `json:"progress"`
	}
	if err := json.Unmarshal(bodies[1], &req); err != nil {
		t.Fatalf("unmarshal push body: %v", err)
	}
	if req.DeviceID != "dev-1" || req.Nonce == "" || req.IssuedAt == "" {
		t.Fatalf("push envelope = %+v", req)
	}
	if len(req.Progress.Steps) != 1 || req.Progress.Steps[0].Status != signer.SetupStatusDone {
		t.Fatalf("second push progress = %+v, want model done", req.Progress)
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
