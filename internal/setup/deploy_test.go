package setup

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/download"
)

// fakePuller records its invocation and replays a canned outcome. It
// substitutes for *download.Puller via the Puller interface seam.
type fakePuller struct {
	calls    int
	tag      string
	emitErr  error
	progress []download.Progress
}

func (f *fakePuller) Pull(_ context.Context, tag string, onProgress func(download.Progress)) error {
	f.calls++
	f.tag = tag
	for _, p := range f.progress {
		if onProgress != nil {
			onProgress(p)
		}
	}
	return f.emitErr
}

func defaultInference() agentconfig.InferenceConfig {
	d := agentconfig.Defaults().Inference
	d.BundledModelID = "qwen2.5-coder-7b-instruct"
	d.PullOnStartup = true
	d.Enabled = true
	d.ShareWithMesh = true
	d.LocalGatewayPort = 9473
	return d
}

// withOllamaInPATH ensures exec.LookPath("ollama") succeeds inside the
// test. We create an executable stub named "ollama" in a temp dir and
// point $PATH at it for the test's lifetime. Skipped on Windows (no
// exec-bit, and ollama discovery needs a real .exe).
func withOllamaInPATH(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("ollama stub setup not implemented for windows")
	}
	dir := t.TempDir()
	stub := dir + "/ollama"
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PATH", dir)
}

func TestDeploy_AllowMutationsFalse_Baseline(t *testing.T) {
	withOllamaInPATH(t)
	factoryCalled := false
	res, err := Deploy(context.Background(), DeployOptions{
		AllowMutations: false,
		Inference:      defaultInference(),
		PullerFactory: func(string) Puller {
			factoryCalled = true
			return &fakePuller{}
		},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if factoryCalled {
		t.Errorf("PullerFactory must not be invoked when AllowMutations=false")
	}
	if !res.OllamaInstalled {
		t.Errorf("OllamaInstalled = false, want true (stub on PATH)")
	}
	if res.BundledModel != "qwen2.5-coder-7b-instruct" {
		t.Errorf("BundledModel = %q", res.BundledModel)
	}
}

func TestDeploy_AllowMutationsTrue_NoOllama(t *testing.T) {
	t.Setenv("PATH", "")                 // strip $PATH lookup
	t.Setenv("WAIRED_OLLAMA_BINARY", "") // and the env override
	// DetectOllama now resolves via download.ResolveBinary, which also
	// stats OS well-known install paths (e.g. /usr/local/bin/ollama,
	// /Applications/Ollama.app/...). Skip if the host actually has one
	// of those so we only exercise the genuine not-found path.
	if _, err := download.ResolveBinary(""); err == nil {
		t.Skip("environment still has a resolvable ollama; cannot test not-found path")
	}
	factoryCalled := false
	res, err := Deploy(context.Background(), DeployOptions{
		AllowMutations: true,
		Inference:      defaultInference(),
		PullerFactory: func(string) Puller {
			factoryCalled = true
			return &fakePuller{}
		},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if factoryCalled {
		t.Errorf("PullerFactory must not be invoked when ollama missing")
	}
	if res.OllamaInstalled {
		t.Errorf("OllamaInstalled = true, want false")
	}
	if !hasNoteMatching(res.Notes, "ollama missing on PATH") {
		t.Errorf("expected 'ollama missing' note, got %#v", res.Notes)
	}
	if !hasNoteMatching(res.Notes, "waired models pull") {
		t.Errorf("expected retry hint in notes, got %#v", res.Notes)
	}
}

func TestDeploy_AllowMutationsTrue_InferenceDisabled(t *testing.T) {
	withOllamaInPATH(t)
	cfg := defaultInference()
	cfg.Enabled = false

	factoryCalled := false
	res, err := Deploy(context.Background(), DeployOptions{
		AllowMutations: true,
		Inference:      cfg,
		PullerFactory: func(string) Puller {
			factoryCalled = true
			return &fakePuller{}
		},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if factoryCalled {
		t.Errorf("PullerFactory must not be invoked when Enabled=false")
	}
	if !hasNoteMatching(res.Notes, "inference disabled by operator choice") {
		t.Errorf("expected disabled note, got %#v", res.Notes)
	}
}

func TestDeploy_AllowMutationsTrue_PullSucceeds(t *testing.T) {
	withOllamaInPATH(t)
	fp := &fakePuller{
		progress: []download.Progress{
			{State: download.StatePulling, Percent: 50, Message: "pulling"},
			{State: download.StateSuccess, Percent: -1, Message: "success"},
		},
	}
	var events []download.Progress
	res, err := Deploy(context.Background(), DeployOptions{
		AllowMutations: true,
		Inference:      defaultInference(),
		EngineProbe:    func(context.Context, string) bool { return true },
		PullerFactory:  func(string) Puller { return fp },
		ProgressSink: func(ev PullEvent) {
			events = append(events, ev.Progress)
		},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if fp.calls != 1 {
		t.Errorf("Pull calls = %d, want 1", fp.calls)
	}
	if fp.tag == "" {
		t.Errorf("expected non-empty ollama tag for bundled qwen2.5 coder")
	}
	if len(events) != 2 {
		t.Errorf("ProgressSink calls = %d, want 2", len(events))
	}
	for _, note := range res.Notes {
		if strings.Contains(note, "pull failed") {
			t.Errorf("unexpected failure note: %q", note)
		}
	}
}

func TestDeploy_AllowMutationsTrue_PullFails(t *testing.T) {
	withOllamaInPATH(t)
	fp := &fakePuller{emitErr: errors.New("boom")}
	res, err := Deploy(context.Background(), DeployOptions{
		AllowMutations: true,
		Inference:      defaultInference(),
		EngineProbe:    func(context.Context, string) bool { return true },
		PullerFactory:  func(string) Puller { return fp },
	})
	if err != nil {
		t.Fatalf("Deploy must not return error on pull failure: %v", err)
	}
	if !hasNoteMatching(res.Notes, "ollama pull failed") {
		t.Errorf("expected failure note, got %#v", res.Notes)
	}
	if !hasNoteMatching(res.Notes, "retry with `waired models pull") {
		t.Errorf("expected retry hint in failure note, got %#v", res.Notes)
	}
}

func TestDeploy_AllowMutationsTrue_PullCtxIsolated(t *testing.T) {
	// Parent ctx with a very short budget; pull ctx default is 30 min.
	// If the pull's ctx were not isolated, the fake puller would see a
	// cancelled context immediately on the second Pull. We don't drive
	// that here — we just assert the production wiring path compiles
	// and Deploy returns within seconds with the override timeout.
	withOllamaInPATH(t)
	fp := &fakePuller{}
	parent, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	time.Sleep(60 * time.Millisecond) // parent ctx now expired.
	_, err := Deploy(parent, DeployOptions{
		AllowMutations: true,
		Inference:      defaultInference(),
		EngineProbe:    func(context.Context, string) bool { return true },
		PullerFactory:  func(string) Puller { return fp },
		PullCtxTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	// Pull was attempted because the pull ctx is its own 5s budget.
	if fp.calls != 1 {
		t.Errorf("Pull calls = %d, want 1 (pull ctx must be isolated from expired parent)", fp.calls)
	}
}

// TestDeploy_SkipsPrePullWhenEngineDown: `ollama pull` needs a serving
// engine to land in. Bundled mode's engine (waired-agent's child on
// the waired-owned port) typically isn't running during init — the
// pre-pull must skip with a note instead of feeding whatever answers
// the upstream default port.
func TestDeploy_SkipsPrePullWhenEngineDown(t *testing.T) {
	withOllamaInPATH(t)
	fp := &fakePuller{}
	res, err := Deploy(context.Background(), DeployOptions{
		AllowMutations: true,
		Inference:      defaultInference(),
		EngineProbe:    func(context.Context, string) bool { return false },
		PullerFactory:  func(string) Puller { return fp },
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if fp.calls != 0 {
		t.Errorf("Pull calls = %d, want 0 when no engine answers", fp.calls)
	}
	if !hasNoteMatching(res.Notes, "skipping pre-pull") {
		t.Errorf("expected skip note, got %#v", res.Notes)
	}
}

// TestDetectOllama_ResolvesViaEnvOverride proves DetectOllama no longer
// depends on $PATH: with $PATH stripped but $WAIRED_OLLAMA_BINARY set to
// an off-PATH executable, detection still succeeds and parses the
// version. This is the seam that lets ResolveBinary find macOS
// Ollama.app / Homebrew installs and Windows-service installs that a
// plain LookPath would miss. (#268)
func TestDetectOllama_ResolvesViaEnvOverride(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub setup not implemented for windows")
	}
	dir := t.TempDir()
	stub := dir + "/ollama"
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho \"ollama version is 9.9.9\"\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PATH", "") // ensure detection cannot come from $PATH.
	t.Setenv("WAIRED_OLLAMA_BINARY", stub)

	det := DetectOllama(context.Background())
	if !det.Installed {
		t.Fatalf("Installed = false, want true (resolved via WAIRED_OLLAMA_BINARY)")
	}
	if det.Path != stub {
		t.Errorf("Path = %q, want %q", det.Path, stub)
	}
	if det.Version != "9.9.9" {
		t.Errorf("Version = %q, want 9.9.9", det.Version)
	}
	if !det.Supported {
		t.Errorf("Supported = false, want true for version 9.9.9")
	}
}

// TestDetectOllama_NotInstalled checks the zero-value path when no
// ollama is resolvable through any of ResolveBinary's sources.
func TestDetectOllama_NotInstalled(t *testing.T) {
	t.Setenv("PATH", "")
	t.Setenv("WAIRED_OLLAMA_BINARY", "")
	if _, err := download.ResolveBinary(""); err == nil {
		t.Skip("environment still has a resolvable ollama; cannot test not-found path")
	}
	det := DetectOllama(context.Background())
	if det.Installed {
		t.Errorf("Installed = true, want false; det = %#v", det)
	}
}

func hasNoteMatching(notes []string, needle string) bool {
	for _, n := range notes {
		if strings.Contains(n, needle) {
			return true
		}
	}
	return false
}
