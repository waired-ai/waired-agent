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

// Tests for what the executor's byte-level reports become on the wire
// (waired-agent#197) and for the keepalive that keeps a long step from
// looking dead (#130).

// hasStepID reports whether the snapshot carries a row with this id.
// stepByID fails the test when it does not, which is the wrong shape for
// asserting a row's ABSENCE.
func hasStepID(p *signer.SetupProgress, id string) bool {
	if p == nil {
		return false
	}
	for _, s := range p.Steps {
		if s.ID == id {
			return true
		}
	}
	return false
}

// assertCompletableDocument checks the one rule this repo cannot import:
// §7's completion rule lives in the control plane (at least one reported
// step, and every reported step done or skipped), so a document that
// fails it leaves the device showing "setup unfinished" forever and keeps
// the model card shut, however healthy the machine is.
//
// Written down once, here, because the alternative is each test asserting
// half of it and nothing asserting the pair (waired-agent#753).
func assertCompletableDocument(t *testing.T, p *signer.SetupProgress) {
	t.Helper()
	if p == nil {
		t.Fatal("no document pushed; the completion rule has nothing to read")
	}
	if len(p.Steps) == 0 {
		t.Fatal("document has no steps; the completion rule can never accept it")
	}
	for _, s := range p.Steps {
		if s.Status != signer.SetupStatusDone && s.Status != signer.SetupStatusSkipped {
			t.Errorf("step %q = %q; the completion rule reads every row", s.ID, s.Status)
		}
	}
}

// attachDownload puts a live elevated lease on the reconciler and has it
// report one engine-download tick.
func attachDownload(t *testing.T, r *setupReconciler, completed, total, rate int64) {
	t.Helper()
	r.NoteExecutor(context.Background(), management.SetupExecutorRequest{
		Attached: true, Elevated: true,
		Phase: management.SetupExecutorPhaseInstalling, Engine: "ollama",
		Step:           management.SetupStepEngineDownload,
		CompletedBytes: completed, TotalBytes: total, RateBps: rate,
	})
}

// A host that already has the engine, or one whose executor predates the
// split, must not grow a download row: the wizard would wait on a
// transfer that is never going to happen.
func TestSetupNoDownloadRowWithoutAReport(t *testing.T) {
	f := &fakeSetupProvider{}
	r, _ := leasedReconciler(t, f, "ollama", "")
	r.NoteExecutor(context.Background(), management.SetupExecutorRequest{
		Attached: true, Elevated: true,
		Phase: management.SetupExecutorPhaseInstalling, Engine: "ollama",
	})

	p := r.snapshot(context.Background())
	if hasStepID(p, setupStepEngineDownload) {
		t.Fatalf("steps = %+v, want no engine_download row", p.Steps)
	}
	if step := stepByID(t, p, setupStepEngineInstall); step.Status != signer.SetupStatusRunning {
		t.Fatalf("engine_install = %+v, want running (the legacy single-row wire)", step)
	}
}

// The whole point of #197: the figures the terminal is drawing reach the
// wire, so the wizard can draw the same download.
func TestSetupDownloadRowCarriesBytesAndRate(t *testing.T) {
	f := &fakeSetupProvider{}
	r, _ := leasedReconciler(t, f, "ollama", "")
	attachDownload(t, r, 700<<20, 1400<<20, 76_281_364)

	step := stepByID(t, r.snapshot(context.Background()), setupStepEngineDownload)
	if step.Status != signer.SetupStatusRunning {
		t.Fatalf("engine_download = %+v, want running", step)
	}
	if step.CompletedBytes != 700<<20 || step.TotalBytes != 1400<<20 {
		t.Errorf("bytes = %d/%d, want 700/1400 MiB", step.CompletedBytes, step.TotalBytes)
	}
	if step.RateBps != 76_281_364 {
		t.Errorf("rate_bps = %d, want the reported rate", step.RateBps)
	}
}

// Exactly one row may be live. Before this the install row went `running`
// off the mere presence of a lease, so the wizard showed two spinners and
// the byte bar belonged to neither of them (#187's rule, one row down).
func TestSetupInstallRowWaitsForTheDownload(t *testing.T) {
	f := &fakeSetupProvider{}
	r, _ := leasedReconciler(t, f, "ollama", "")
	attachDownload(t, r, 1, 1400<<20, 1000)

	p := r.snapshot(context.Background())
	if step := stepByID(t, p, setupStepEngineDownload); step.Status != signer.SetupStatusRunning {
		t.Fatalf("engine_download = %+v, want running", step)
	}
	if step := stepByID(t, p, setupStepEngineInstall); step.Status != signer.SetupStatusPending {
		t.Fatalf("engine_install = %+v, want pending while the bytes are still arriving", step)
	}

	// The executor moves on: the download row completes and the install
	// row takes over as the live one.
	r.NoteExecutor(context.Background(), management.SetupExecutorRequest{
		Attached: true, Elevated: true,
		Phase: management.SetupExecutorPhaseDone, Engine: "ollama",
		Step: management.SetupStepEngineDownload,
	})
	r.NoteExecutor(context.Background(), management.SetupExecutorRequest{
		Attached: true, Elevated: true,
		Phase: management.SetupExecutorPhaseInstalling, Engine: "ollama",
		Step: management.SetupStepEngineInstall,
	})

	p = r.snapshot(context.Background())
	dl := stepByID(t, p, setupStepEngineDownload)
	if dl.Status != signer.SetupStatusDone {
		t.Fatalf("engine_download = %+v, want done", dl)
	}
	if dl.CompletedBytes != 0 || dl.TotalBytes != 0 || dl.RateBps != 0 {
		t.Errorf("finished download still carries figures %+v; a done row draws no bar", dl)
	}
	if step := stepByID(t, p, setupStepEngineInstall); step.Status != signer.SetupStatusRunning {
		t.Fatalf("engine_install = %+v, want running", step)
	}
}

// A download that fails is reported once, on the row it failed on. The
// install row must not add a second red step for the same event — the
// operator would read two failures and two things to fix.
func TestSetupDownloadFailureIsReportedOnce(t *testing.T) {
	f := &fakeSetupProvider{}
	r, _ := leasedReconciler(t, f, "ollama", "")
	attachDownload(t, r, 1<<20, 1400<<20, 1000)
	r.NoteExecutor(context.Background(), management.SetupExecutorRequest{
		Attached: true, Elevated: true,
		Phase: management.SetupExecutorPhaseFailed, Engine: "ollama",
		Step:  management.SetupStepEngineDownload,
		Error: "write /var/lib/waired: no space left on device",
	})

	p := r.snapshot(context.Background())
	dl := stepByID(t, p, setupStepEngineDownload)
	if dl.Status != signer.SetupStatusFailed {
		t.Fatalf("engine_download = %+v, want failed", dl)
	}
	if dl.ErrorCode != signer.SetupErrorDiskFull {
		t.Errorf("error_code = %q, want disk_full classified from the detail", dl.ErrorCode)
	}
	if step := stepByID(t, p, setupStepEngineInstall); step.Status != signer.SetupStatusPending {
		t.Fatalf("engine_install = %+v, want pending — nothing was downloaded to install", step)
	}
}

// An engine that is present outranks whatever the last lease said about
// the download: it plainly arrived.
func TestSetupDownloadRowCompletesOnAPresentEngine(t *testing.T) {
	f := &fakeSetupProvider{}
	r, _ := leasedReconciler(t, f, "ollama", "")
	attachDownload(t, r, 1<<20, 1400<<20, 1000)
	f.setEngine(true, false)

	step := stepByID(t, r.snapshot(context.Background()), setupStepEngineDownload)
	if step.Status != signer.SetupStatusDone {
		t.Fatalf("engine_download = %+v, want done once the engine is on the host", step)
	}
}

// The install claim is what stops a second elevated executor from
// starting its own install. `engine_download: done` is a step boundary
// INSIDE one install, not the end of it, so it must not release the
// claim — while a real terminal phase on the install itself must.
func TestSetupDownloadDoneKeepsTheInstallClaim(t *testing.T) {
	f := &fakeSetupProvider{}
	r, _ := leasedReconciler(t, f, "ollama", "")
	attachDownload(t, r, 1, 2, 3)
	if got := r.SetupState(context.Background()).InstallClaimed; got != "ollama" {
		t.Fatalf("InstallClaimed = %q after claiming, want ollama", got)
	}

	r.NoteExecutor(context.Background(), management.SetupExecutorRequest{
		Attached: true, Elevated: true,
		Phase: management.SetupExecutorPhaseDone, Engine: "ollama",
		Step: management.SetupStepEngineDownload,
	})
	if got := r.SetupState(context.Background()).InstallClaimed; got != "ollama" {
		t.Fatalf("InstallClaimed = %q after the download finished, want the claim held", got)
	}

	r.NoteExecutor(context.Background(), management.SetupExecutorRequest{
		Attached: true, Elevated: true,
		Phase: management.SetupExecutorPhaseDone, Engine: "ollama",
		Step: management.SetupStepEngineInstall,
	})
	if got := r.SetupState(context.Background()).InstallClaimed; got != "" {
		t.Fatalf("InstallClaimed = %q after the install finished, want it released", got)
	}
}

// TestSetupPushKeepalive is the #130 regression bar. Content dedup froze
// last_check for the length of any step with no moving field, and the
// wizard's 120 s staleness window then called a healthy 15-minute install
// offline. The dedup stays — it is what keeps a fleet at rest silent —
// but it may not outlast setupKeepaliveInterval.
func TestSetupPushKeepalive(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		mu.Lock()
		bodies++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()
	count := func() int { mu.Lock(); defer mu.Unlock(); return bodies }

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cli := controlclient.NewWithBearer(srv.URL, func() string { return "tok" })

	f := &fakeSetupProvider{modelState: catalog.ModelStateNotPresent}
	c := newFakeClock()
	r := newSetupReconciler(f, cli, "dev-1", priv, quietLogger())
	r.now = c.now
	r.interval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); r.runPush(ctx) }()

	r.Apply(ctx, desiredFrame("", "m1", 0))
	waitFor(t, func() bool { return count() >= 1 }, "first push")

	// Ticks keep firing with identical content and a clock that has not
	// moved: still one push.
	time.Sleep(50 * time.Millisecond)
	if got := count(); got != 1 {
		t.Fatalf("unchanged content pushed %d times before the keepalive was due, want 1", got)
	}

	c.advance(setupKeepaliveInterval + time.Second)
	waitFor(t, func() bool { return count() >= 2 }, "keepalive push for unchanged content")

	cancel()
	<-done
}

// PRODUCT CONTRACT (waired-agent#413). The two readers of engine
// presence must not disagree about when the engine appeared.
//
// snapshot() probes the engine itself, on runPush's 2 s ticker. Apply
// makes an independent probe and runs ONLY when a control-plane frame
// arrives — nothing local schedules one when the binary lands on disk.
// So the moment it appeared, snapshot() saw `installed` and moved the
// engine rows to done, while Apply had not yet run its engine-appeared
// edge: modelApplied / modelRejected still held the failure from the
// engine-less attempt, and the model row reported that stale failure
// until the next frame happened to arrive. The window is bounded by
// control-plane frame cadence, which is not observable from this repo.
//
// Asserted through snapshot() with NO second Apply, because "a frame
// arrived" is precisely the thing that must stop being required.
func TestSetupEngineAppearedIsNoticedWithoutAControlPlaneFrame(t *testing.T) {
	ctx := context.Background()
	f := &fakeSetupProvider{stateDir: t.TempDir()}
	// Engine present, and applying the model is refused: this is how a
	// rejection gets recorded at all. #307 made the engine-LESS case
	// impossible (PullModel refuses outright), so the record that
	// resurfaces is one taken while the engine was there — a reinstall,
	// or a profiler cache that briefly reported it missing, which is the
	// same come-and-go the edge exists for.
	f.setEngine(true, false)
	f.applyErr = errors.New("ollama pull: model refused")

	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	// retryFrame, not desiredFrame: applying is gated on the instruction
	// being one this daemon watched change (#308) or an explicit retry
	// (#136). A first frame on a fresh reconciler is neither, so a plain
	// desiredFrame would never reach setupApplyModel and nothing would be
	// refused.
	r.Apply(ctx, retryFrame("ollama", "qwen3.5-2b", 1))

	if got := r.SetupState(ctx).ModelErrorDetail; got == "" {
		t.Fatal("no refusal recorded — the precondition of this test did not happen")
	}

	// The engine goes away and comes back. NO control-plane frame
	// arrives for either transition: only snapshot()'s own 2 s probe
	// sees them, which is exactly the case Apply could not cover.
	f.setEngine(false, false)
	r.snapshot(ctx)

	f.setEngine(true, false)
	f.applyErr = nil
	startsBefore := f.engineStartCount()

	p := r.snapshot(ctx)

	if got := stepByID(t, p, setupStepModelPull); got.Status == signer.SetupStatusFailed {
		t.Errorf("model_pull is still %+v — the stale refusal outlived the engine reappearing", got)
	}
	if got := r.SetupState(ctx).ModelErrorDetail; got != "" {
		t.Errorf("recorded refusal = %q, want it cleared by the engine-appeared edge", got)
	}
	// ...and the row is not merely repainted: the edge re-admits the
	// model, which is what actually gets the download moving again.
	if got := f.engineStartCount(); got <= startsBefore {
		t.Errorf("engine starts = %d, was %d — the engine-appeared edge did not dispatch", got, startsBefore)
	}
}

// The edge is keyed on the TRANSITION, not on every probe: a genuinely
// failing download must not be re-queued in a loop by the 2 s ticker.
func TestSetupEngineAppearedFiresOnceAcrossRepeatedSnapshots(t *testing.T) {
	ctx := context.Background()
	f := &fakeSetupProvider{stateDir: t.TempDir()}
	f.setEngine(true, false)

	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	r.Apply(ctx, desiredFrame("ollama", "qwen3.5-2b", 0))

	f.setEngine(false, false)
	r.snapshot(ctx)

	f.setEngine(true, false)
	r.snapshot(ctx)
	afterEdge := f.engineStartCount()

	for i := 0; i < 5; i++ {
		r.snapshot(ctx)
	}
	if got := f.engineStartCount(); got != afterEdge {
		t.Errorf("engine starts = %d after five more ticks, want %d — the edge re-fired", got, afterEdge)
	}
}

// The end of the chain: a machine with an engine, an active model and NO
// preference must push a document the completion rule can accept.
//
// The unit tests prove observedSetup describes such a host; this proves
// the description survives runPush and lands on the wire, which is the
// only part the control plane ever sees. Modelled on TestSetupPushDedupes
// because the BODY is the assertion here, not the push count
// (waired-agent#753, #756).
func TestObservedSetupPushesAFinishedDocument(t *testing.T) {
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

	f := autoSelectedHost()
	r := newSetupReconciler(f, cli, "dev-1", priv, quietLogger())
	r.interval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); r.runPush(ctx) }()

	// The empty frame is not a detail: it is what a terminal-installed
	// device receives forever, because only the management API writes the
	// desired columns and `waired init` has no route to them.
	r.Apply(ctx, desiredFrame("", "", 0))
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(bodies) >= 1 },
		"a push from a host nobody instructed")

	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	var req struct {
		Progress signer.SetupProgress `json:"progress"`
	}
	if err := json.Unmarshal(bodies[0], &req); err != nil {
		t.Fatalf("decoding the pushed body: %v", err)
	}
	assertCompletableDocument(t, &req.Progress)
	// Nothing is driving: no lease was taken, and there is no desired
	// state for the browser derivation to read as a claim.
	if req.Progress.Driver != "" {
		t.Errorf("driver = %q, want none", req.Progress.Driver)
	}
}
