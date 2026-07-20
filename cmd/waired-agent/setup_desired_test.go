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
	stateDir        string
}

func (f *fakeSetupProvider) setupEngineState(context.Context, string) (bool, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.engineInstalled, f.engineReady
}

func (f *fakeSetupProvider) setupStateDir() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stateDir
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

// setEngine scripts the (installed, ready) pair the reconciler observes,
// which is how the executor's install becomes visible to it.
func (f *fakeSetupProvider) setEngine(installed, ready bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.engineInstalled = installed
	f.engineReady = ready
}

func (f *fakeSetupProvider) setModelState(state, errText string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.modelState = state
	f.modelErr = errText
}

func (f *fakeSetupProvider) pullCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pulls)
}

// fakeClock drives the reconciler's `now` seam so lease expiry is tested
// without sleeping.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// stepByID returns the named step from a snapshot, failing the test when
// it is absent — a missing step is always a bug in these cases.
func stepByID(t *testing.T, p *signer.SetupProgress, id string) signer.SetupStep {
	t.Helper()
	if p == nil {
		t.Fatalf("snapshot is nil, want a step %q", id)
	}
	for _, s := range p.Steps {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("step %q missing from %+v", id, p.Steps)
	return signer.SetupStep{}
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

// --- executor lease (waired#835 §9/§11) ---

// leasedReconciler wires a reconciler to a controllable clock with one
// desired frame already applied.
func leasedReconciler(t *testing.T, f *fakeSetupProvider, engine, model string) (*setupReconciler, *fakeClock) {
	t.Helper()
	c := newFakeClock()
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	r.now = c.now
	r.Apply(context.Background(), desiredFrame(engine, model, 0))
	return r, c
}

// TestSetupSnapshotNoExecutorStillPermissionDenied pins the UNCHANGED
// pre-lease wire for the case that has not moved: nobody ever attached,
// so this is a privileges problem, not a liveness one. Sending the
// operator to re-run a command would be wrong here — there is no reason
// to think a second run would attach either.
func TestSetupSnapshotNoExecutorStillPermissionDenied(t *testing.T) {
	f := &fakeSetupProvider{}
	r, _ := leasedReconciler(t, f, "ollama", "")
	step := stepByID(t, r.snapshot(context.Background()), setupStepEngineInstall)
	if step.Status != signer.SetupStatusFailed || step.ErrorCode != signer.SetupErrorPermissionDenied {
		t.Fatalf("engine step = %+v, want failed/permission_denied", step)
	}
}

// TestSetupSnapshotUnelevatedExecutorIsPermissionDenied: an executor is
// present but cannot install. Reporting executor_gone here would send the
// operator to re-run a command that fails the same way.
func TestSetupSnapshotUnelevatedExecutorIsPermissionDenied(t *testing.T) {
	f := &fakeSetupProvider{}
	r, _ := leasedReconciler(t, f, "ollama", "")
	r.NoteExecutor(context.Background(), management.SetupExecutorRequest{Attached: true, Elevated: false})
	step := stepByID(t, r.snapshot(context.Background()), setupStepEngineInstall)
	if step.Status != signer.SetupStatusFailed || step.ErrorCode != signer.SetupErrorPermissionDenied {
		t.Fatalf("engine step = %+v, want failed/permission_denied", step)
	}
}

// TestSetupSnapshotElevatedExecutorIsRunning: with an elevated executor
// attached the install step must read as in-progress. Before the lease
// existed this reported a hard failure from the very first push, which
// is the regression this wave exists to fix.
func TestSetupSnapshotElevatedExecutorIsRunning(t *testing.T) {
	f := &fakeSetupProvider{}
	r, _ := leasedReconciler(t, f, "ollama", "")
	for _, phase := range []string{management.SetupExecutorPhaseIdle, management.SetupExecutorPhaseInstalling} {
		r.NoteExecutor(context.Background(), management.SetupExecutorRequest{
			Attached: true, Elevated: true, Phase: phase, Engine: "ollama",
		})
		step := stepByID(t, r.snapshot(context.Background()), setupStepEngineInstall)
		if step.Status != signer.SetupStatusRunning {
			t.Fatalf("phase %q: engine step = %+v, want running", phase, step)
		}
	}
}

// TestSetupSnapshotExecutorGoneAfterTTL covers §9-4: the executor was
// here and is gone, which is the RECOVERABLE case — NAVI offers the
// command to re-run.
func TestSetupSnapshotExecutorGoneAfterTTL(t *testing.T) {
	f := &fakeSetupProvider{}
	r, clock := leasedReconciler(t, f, "ollama", "")
	r.NoteExecutor(context.Background(), management.SetupExecutorRequest{
		Attached: true, Elevated: true, Phase: management.SetupExecutorPhaseInstalling, Engine: "ollama",
	})
	clock.advance(setupExecutorTTL + time.Second)
	step := stepByID(t, r.snapshot(context.Background()), setupStepEngineInstall)
	if step.Status != signer.SetupStatusFailed || step.ErrorCode != signer.SetupErrorExecutorGone {
		t.Fatalf("engine step = %+v, want failed/executor_gone", step)
	}
}

// TestSetupExecutorReleaseIsImmediate: Ctrl-C releases explicitly, so the
// wizard must not wait out the whole TTL to stop spinning.
func TestSetupExecutorReleaseIsImmediate(t *testing.T) {
	f := &fakeSetupProvider{}
	r, _ := leasedReconciler(t, f, "ollama", "")
	ctx := context.Background()
	r.NoteExecutor(ctx, management.SetupExecutorRequest{Attached: true, Elevated: true})
	r.NoteExecutor(ctx, management.SetupExecutorRequest{Attached: false})
	step := stepByID(t, r.snapshot(ctx), setupStepEngineInstall)
	if step.Status != signer.SetupStatusFailed || step.ErrorCode != signer.SetupErrorExecutorGone {
		t.Fatalf("engine step = %+v, want failed/executor_gone", step)
	}
}

// TestSetupExecutorGoneDoesNotStallOtherSteps pins §9-5: steps that do
// not need the executor keep going after it dies.
func TestSetupExecutorGoneDoesNotStallOtherSteps(t *testing.T) {
	f := &fakeSetupProvider{
		modelState:     catalog.ModelStateDownloading,
		modelCompleted: 512,
		modelTotal:     4096,
		bench:          management.BenchmarkStatusResponse{State: management.BenchmarkStateRunning},
	}
	c := newFakeClock()
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	r.now = c.now
	ctx := context.Background()
	r.Apply(ctx, desiredFrame("ollama", "m-1", 3))
	r.NoteExecutor(ctx, management.SetupExecutorRequest{Attached: true, Elevated: true})
	r.NoteExecutor(ctx, management.SetupExecutorRequest{Attached: false})

	snap := r.snapshot(ctx)
	if got := stepByID(t, snap, setupStepEngineInstall); got.ErrorCode != signer.SetupErrorExecutorGone {
		t.Fatalf("engine step = %+v, want executor_gone", got)
	}
	if got := stepByID(t, snap, setupStepModelPull); got.Status != signer.SetupStatusRunning || got.CompletedBytes != 512 {
		t.Fatalf("model step = %+v, want running with bytes", got)
	}
	if got := stepByID(t, snap, setupStepBenchmark); got.Status != signer.SetupStatusRunning {
		t.Fatalf("benchmark step = %+v, want running", got)
	}
}

// TestSetupInstallClaimIsLeaseBound is the blocker regression test.
// A claim bound to desired_engine instead of to the lease would make the
// executor_gone recovery copy ("re-run sudo waired init") a no-op: the
// re-run would see the stale claim and skip the install, leaving the step
// red forever. It would also let one local POST block installation
// permanently.
func TestSetupInstallClaimIsLeaseBound(t *testing.T) {
	f := &fakeSetupProvider{}
	r, clock := leasedReconciler(t, f, "ollama", "")
	ctx := context.Background()
	r.NoteExecutor(ctx, management.SetupExecutorRequest{
		Attached: true, Elevated: true, Phase: management.SetupExecutorPhaseInstalling, Engine: "ollama",
	})
	if got := r.SetupState(ctx).InstallClaimed; got != "ollama" {
		t.Fatalf("install_claimed = %q while the lease is live, want ollama", got)
	}
	clock.advance(setupExecutorTTL + time.Second)
	if got := r.SetupState(ctx).InstallClaimed; got != "" {
		t.Fatalf("install_claimed = %q after the claiming lease expired, want empty", got)
	}
	// A fresh executor (the operator re-ran the command) can claim it.
	r.NoteExecutor(ctx, management.SetupExecutorRequest{
		Attached: true, Elevated: true, Phase: management.SetupExecutorPhaseInstalling, Engine: "ollama",
	})
	if got := r.SetupState(ctx).InstallClaimed; got != "ollama" {
		t.Fatalf("install_claimed = %q after re-attach, want ollama", got)
	}
}

// TestSetupExecutorFailedPhaseCarriesItsOwnError: when the executor tried
// and failed, its text beats anything the daemon could infer.
func TestSetupExecutorFailedPhaseCarriesItsOwnError(t *testing.T) {
	f := &fakeSetupProvider{}
	r, _ := leasedReconciler(t, f, "ollama", "")
	ctx := context.Background()
	r.NoteExecutor(ctx, management.SetupExecutorRequest{
		Attached: true, Elevated: true, Phase: management.SetupExecutorPhaseFailed,
		Engine: "ollama", Error: "download failed: no space left on device",
	})
	step := stepByID(t, r.snapshot(ctx), setupStepEngineInstall)
	if step.Status != signer.SetupStatusFailed || step.ErrorCode != signer.SetupErrorDiskFull {
		t.Fatalf("engine step = %+v, want failed/disk_full", step)
	}
	if step.ErrorDetail == "" {
		t.Fatal("engine step lost the executor's error detail")
	}
	if got := r.SetupState(ctx).InstallClaimed; got != "" {
		t.Fatalf("install_claimed = %q after a failed attempt, want empty so a retry can claim it", got)
	}
}

// --- pull re-admission (the second blocker) ---

// TestSetupPullReadmittedWhenEngineBecomesInstalled is the other blocker
// regression test. On an engine-less host the inference subsystem starts
// inert, so the first PullModel fails and the one-shot admission would
// keep the download red for the rest of the process's life — even after
// the executor installs the engine seconds later.
func TestSetupPullReadmittedWhenEngineBecomesInstalled(t *testing.T) {
	f := &fakeSetupProvider{modelState: catalog.ModelStateNotPresent}
	ctx := context.Background()
	r, _ := leasedReconciler(t, f, "ollama", "m-1")
	// First frame: no engine, the pull is admitted once and fails.
	f.setModelState(catalog.ModelStateFailed, "pull: no engine available")
	r.Apply(ctx, desiredFrame("ollama", "m-1", 0))
	if got := f.pullCount(); got != 1 {
		t.Fatalf("pulls before the engine appeared = %d, want 1", got)
	}
	if got := stepByID(t, r.snapshot(ctx), setupStepModelPull); got.Status != signer.SetupStatusFailed {
		t.Fatalf("model step = %+v, want failed", got)
	}

	// The executor installs the engine; the next frame must re-admit
	// exactly one pull.
	f.setEngine(true, false)
	f.setModelState(catalog.ModelStateNotPresent, "")
	r.Apply(ctx, desiredFrame("ollama", "m-1", 0))
	if got := f.pullCount(); got != 2 {
		t.Fatalf("pulls after the engine appeared = %d, want 2", got)
	}
	// ...and no more on subsequent frames.
	for i := 0; i < 3; i++ {
		r.Apply(ctx, desiredFrame("ollama", "m-1", 0))
	}
	if got := f.pullCount(); got != 2 {
		t.Fatalf("pulls after repeated frames = %d, want 2", got)
	}
}

// TestSetupPullNotReadmittedWithoutEngineTransition: a download that
// fails for a real reason on a host that already has the engine must not
// be re-queued on every frame.
func TestSetupPullNotReadmittedWithoutEngineTransition(t *testing.T) {
	f := &fakeSetupProvider{engineInstalled: true, modelState: catalog.ModelStateNotPresent}
	ctx := context.Background()
	r, _ := leasedReconciler(t, f, "ollama", "m-1")
	f.setModelState(catalog.ModelStateFailed, "connection reset")
	for i := 0; i < 5; i++ {
		r.Apply(ctx, desiredFrame("ollama", "m-1", 0))
	}
	if got := f.pullCount(); got != 1 {
		t.Fatalf("pulls with a stable installed engine = %d, want 1", got)
	}
}

// TestSetupPullFailureClassification: an out-of-disk failure is the most
// likely way a multi-GB download dies, and telling the operator to check
// their internet connection sends them nowhere.
func TestSetupPullFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		errText string
		want    string
	}{
		{"write /var/lib/waired/blob: no space left on device", signer.SetupErrorDiskFull},
		{"ERROR: There is not enough space on the disk.", signer.SetupErrorDiskFull},
		{"insufficient disk space for model", signer.SetupErrorDiskFull},
		{"dial tcp: connection reset by peer", signer.SetupErrorNetworkError},
		{"", signer.SetupErrorNetworkError},
	} {
		if got := classifySetupFailure(tc.errText); got != tc.want {
			t.Errorf("classifySetupFailure(%q) = %q, want %q", tc.errText, got, tc.want)
		}
	}
}

// TestSetupDetailIsClamped keeps a long installer log from costing a
// whole push (the CP clamps at the same size).
func TestSetupDetailIsClamped(t *testing.T) {
	long := make([]byte, setupDetailMax*3)
	for i := range long {
		long[i] = 'x'
	}
	if got := len(clampSetupDetail(string(long))); got != setupDetailMax {
		t.Fatalf("clamped length = %d, want %d", got, setupDetailMax)
	}
}

// TestSetupStateProjection covers what the executor actually reads.
func TestSetupStateProjection(t *testing.T) {
	f := &fakeSetupProvider{engineInstalled: true, engineReady: true}
	ctx := context.Background()
	r, _ := leasedReconciler(t, f, "ollama", "m-1")
	st := r.SetupState(ctx)
	if !st.Active || st.DesiredEngine != "ollama" || st.DesiredModelID != "m-1" {
		t.Fatalf("state = %+v, want the applied desired triple", st)
	}
	if !st.EngineInstalled || !st.EngineReady {
		t.Fatalf("state = %+v, want the engine reported installed and ready", st)
	}
	if st.ExecutorAttached {
		t.Fatalf("state = %+v, want no executor attached", st)
	}
}

// TestSetupStatePublishesStateDir: the executor has no state dir of its
// own on the daemon path (runInitViaDaemon never receives one), and a
// CLI-side guess diverges silently from a daemon started with
// --state-dir. So the daemon declares it (waired#835 §11.1).
func TestSetupStatePublishesStateDir(t *testing.T) {
	f := &fakeSetupProvider{stateDir: "/var/lib/waired"}
	r, _ := leasedReconciler(t, f, "ollama", "")
	if st := r.SetupState(context.Background()); st.StateDir != "/var/lib/waired" {
		t.Fatalf("state = %+v, want the provider's state dir published", st)
	}
}

// TestSetupStatePublishesStateDirWithoutDesiredEngine: #115 served this
// only alongside a desired engine, on the reasoning that there is
// nothing to install otherwise. That was wrong — `waired init` on the
// daemon path installs the engine whenever the host wants inference,
// with or without a browser wizard, and no desired engine is set in that
// case. Withholding the path is what would leave a terminal-only install
// with no engine at all.
func TestSetupStatePublishesStateDirWithoutDesiredEngine(t *testing.T) {
	f := &fakeSetupProvider{stateDir: "/var/lib/waired"}
	r, _ := leasedReconciler(t, f, "", "m-1")
	if st := r.SetupState(context.Background()); st.StateDir != "/var/lib/waired" {
		t.Fatalf("state = %+v, want the state dir served without a desired engine", st)
	}
}

// TestSetupStateBeforeAnyDesiredFrame: an executor that polls before the
// operator has clicked anything must see active=false rather than an
// error, so it can keep waiting out its grace.
func TestSetupStateBeforeAnyDesiredFrame(t *testing.T) {
	r := newSetupReconciler(&fakeSetupProvider{}, nil, "dev-1", nil, quietLogger())
	if st := r.SetupState(context.Background()); st.Active {
		t.Fatalf("state = %+v, want active=false before any desired frame", st)
	}
}

// TestSetupNilReconcilerIsInert guards the switchboard delegate, which
// hands us a nil receiver before enrollment.
func TestSetupNilReconcilerIsInert(t *testing.T) {
	var r *setupReconciler
	ctx := context.Background()
	if st := r.SetupState(ctx); st.Active {
		t.Fatalf("nil reconciler state = %+v, want zero", st)
	}
	if st := r.NoteExecutor(ctx, management.SetupExecutorRequest{Attached: true}); st.Active {
		t.Fatalf("nil reconciler NoteExecutor = %+v, want zero", st)
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
