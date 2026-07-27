package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
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
