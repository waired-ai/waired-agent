package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/setup"
)

// fakeEngineInstaller records install attempts without downloading a
// multi-GB engine.
type fakeEngineInstaller struct {
	mu       sync.Mutex
	calls    []string // stateDir per call
	handed   []string // stateDir passed to the ownership handoff
	err      error
	detected setup.OllamaDetection
}

// install swaps in the seams for the duration of one test and returns
// the recorder.
func (f *fakeEngineInstaller) install(t *testing.T) *fakeEngineInstaller {
	t.Helper()
	prevInstall, prevDetect, prevHand := setupInstallEngine, setupDetectEngine, setupHandState
	setupInstallEngine = func(_ bool, stateDir string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, stateDir)
		return f.err
	}
	setupDetectEngine = func(context.Context) setup.OllamaDetection { return f.detected }
	setupHandState = func(stateDir string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.handed = append(f.handed, stateDir)
	}
	t.Cleanup(func() {
		setupInstallEngine, setupDetectEngine, setupHandState = prevInstall, prevDetect, prevHand
	})
	return f
}

func (f *fakeEngineInstaller) installed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeEngineInstaller) handedOff() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.handed...)
}

// activeInstallState is the state a daemon serves when the wizard has
// asked for an engine and nothing has claimed the install yet.
func activeInstallState() management.SetupStateResponse {
	return management.SetupStateResponse{
		Active:        true,
		DesiredEngine: "ollama",
		StateDir:      "/var/lib/waired",
	}
}

// lastPhase returns the phase of the final lease update, which is what
// the daemon's snapshot reads.
func lastPhase(t *testing.T, d *fakeSetupDaemon) management.SetupExecutorRequest {
	t.Helper()
	reqs := d.noted()
	if len(reqs) == 0 {
		t.Fatal("executor sent no lease updates")
	}
	return reqs[len(reqs)-1]
}

// TestSetupEngineInstallHappyPath is the core of waired#835 §11: on the
// daemon path nothing else can install an engine, so this is what turns
// the wizard's first step green.
func TestSetupEngineInstallHappyPath(t *testing.T) {
	shrinkSetupTimers(t)
	f := (&fakeEngineInstaller{}).install(t)
	d := &fakeSetupDaemon{}
	d.setState(activeInstallState())
	srv := d.server(t)

	s := attachSetupExecutor(srv.URL, true)
	defer s.Release()
	setupEngineInstall(context.Background(), s, io.Discard, "linux", true)

	if got := f.installed(); len(got) != 1 || got[0] != "/var/lib/waired" {
		t.Fatalf("installer calls = %v, want one call with the daemon's state dir", got)
	}
	// #484: extracted as root, so the unprivileged daemon cannot read it
	// back without this.
	if got := f.handedOff(); len(got) != 1 || got[0] != "/var/lib/waired" {
		t.Fatalf("ownership handoff = %v, want one call with the state dir", got)
	}
	if last := lastPhase(t, d); last.Phase != management.SetupExecutorPhaseDone {
		t.Fatalf("final phase = %q, want done", last.Phase)
	}
}

// TestSetupEngineInstallClaimsBeforeInstalling pins the ordering the
// latch depends on: the daemon must see "installing" before the long
// download starts, or a second executor could start a parallel one.
func TestSetupEngineInstallClaimsBeforeInstalling(t *testing.T) {
	shrinkSetupTimers(t)
	d := &fakeSetupDaemon{}
	d.setState(activeInstallState())
	srv := d.server(t)

	s := attachSetupExecutor(srv.URL, true)
	defer s.Release()

	var phaseAtInstall string
	f := &fakeEngineInstaller{}
	f.install(t)
	setupInstallEngine = func(_ bool, _ string) error {
		phaseAtInstall = lastPhase(t, d).Phase
		return nil
	}
	setupEngineInstall(context.Background(), s, io.Discard, "linux", true)

	if phaseAtInstall != management.SetupExecutorPhaseInstalling {
		t.Fatalf("phase when the install began = %q, want installing", phaseAtInstall)
	}
}

// TestSetupEngineInstallReportsFailure: the executor's own words beat
// any guess the daemon could make, so the wizard shows the real reason
// instead of a generic executor_gone.
func TestSetupEngineInstallReportsFailure(t *testing.T) {
	shrinkSetupTimers(t)
	(&fakeEngineInstaller{err: errors.New("no space left on device")}).install(t)
	d := &fakeSetupDaemon{}
	d.setState(activeInstallState())
	srv := d.server(t)

	s := attachSetupExecutor(srv.URL, true)
	defer s.Release()
	setupEngineInstall(context.Background(), s, io.Discard, "linux", true)

	last := lastPhase(t, d)
	if last.Phase != management.SetupExecutorPhaseFailed {
		t.Fatalf("final phase = %q, want failed", last.Phase)
	}
	if !strings.Contains(last.Error, "no space left on device") {
		t.Fatalf("error detail = %q, want the installer's message", last.Error)
	}
}

// TestSetupEngineInstallSkipConditions covers every reason not to
// install. Each one must leave the installer untouched — an accidental
// install here is a multi-GB download nobody asked for.
func TestSetupEngineInstallSkipConditions(t *testing.T) {
	tests := []struct {
		name  string
		state management.SetupStateResponse
	}{
		{"no setup running", management.SetupStateResponse{DesiredEngine: "ollama", StateDir: "/s"}},
		{"no engine desired", management.SetupStateResponse{Active: true, StateDir: "/s"}},
		{
			"engine already installed",
			management.SetupStateResponse{Active: true, DesiredEngine: "ollama", EngineInstalled: true, StateDir: "/s"},
		},
		{
			"another executor holds the claim",
			management.SetupStateResponse{Active: true, DesiredEngine: "ollama", InstallClaimed: "ollama", StateDir: "/s"},
		},
		{
			"an unknown engine is left to the daemon",
			management.SetupStateResponse{Active: true, DesiredEngine: "mystery", StateDir: "/s"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			shrinkSetupTimers(t)
			f := (&fakeEngineInstaller{}).install(t)
			d := &fakeSetupDaemon{}
			d.setState(tc.state)
			srv := d.server(t)

			s := attachSetupExecutor(srv.URL, true)
			defer s.Release()
			setupEngineInstall(context.Background(), s, io.Discard, "linux", true)

			if got := f.installed(); len(got) != 0 {
				t.Fatalf("installed %v, want no install", got)
			}
		})
	}
}

// TestSetupEngineInstallWithoutStateDirRefuses: guessing a path would
// present as an install that "worked" and a step that never turns
// green, which is the worst outcome available here.
func TestSetupEngineInstallWithoutStateDirRefuses(t *testing.T) {
	shrinkSetupTimers(t)
	f := (&fakeEngineInstaller{}).install(t)
	d := &fakeSetupDaemon{}
	d.setState(management.SetupStateResponse{Active: true, DesiredEngine: "ollama"})
	srv := d.server(t)

	s := attachSetupExecutor(srv.URL, true)
	defer s.Release()
	setupEngineInstall(context.Background(), s, io.Discard, "linux", true)

	if got := f.installed(); len(got) != 0 {
		t.Fatalf("installed %v with no state dir, want refusal", got)
	}
	if last := lastPhase(t, d); last.Phase != management.SetupExecutorPhaseFailed {
		t.Fatalf("final phase = %q, want failed", last.Phase)
	}
}

// TestSetupEngineInstallPerOS is the cross-OS parity table. It also
// pins the two skip reasons that must report through the lease rather
// than dying silently: an unelevated executor and an opt-out host.
func TestSetupEngineInstallPerOS(t *testing.T) {
	tests := []struct {
		name        string
		goos        string
		elevated    bool
		optOut      bool
		detected    setup.OllamaDetection
		wantInstall bool
		wantPhase   string
		wantDetail  string
	}{
		{
			name: "linux elevated installs", goos: "linux", elevated: true,
			wantInstall: true, wantPhase: management.SetupExecutorPhaseDone,
		},
		{
			name: "linux unelevated reports permission", goos: "linux",
			wantPhase: management.SetupExecutorPhaseFailed, wantDetail: "administrator privileges",
		},
		{
			name: "windows elevated installs", goos: "windows", elevated: true,
			wantInstall: true, wantPhase: management.SetupExecutorPhaseDone,
		},
		{
			name: "windows unelevated reports permission", goos: "windows",
			wantPhase: management.SetupExecutorPhaseFailed, wantDetail: "administrator privileges",
		},
		{
			// /Applications is admin-group-writable, so macOS attempts
			// the install and lets it fail with a real message.
			name: "darwin installs unelevated", goos: "darwin",
			wantInstall: true, wantPhase: management.SetupExecutorPhaseDone,
		},
		{
			name: "darwin with its own engine is already done", goos: "darwin",
			detected:  setup.OllamaDetection{Installed: true, Path: "/Applications/Ollama.app"},
			wantPhase: management.SetupExecutorPhaseDone,
		},
		{
			name: "opt-out refuses and says why", goos: "linux", elevated: true, optOut: true,
			wantPhase: management.SetupExecutorPhaseFailed, wantDetail: "WAIRED_NO_OLLAMA",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			shrinkSetupTimers(t)
			if tc.optOut {
				t.Setenv("WAIRED_NO_OLLAMA", "1")
			}
			f := (&fakeEngineInstaller{detected: tc.detected}).install(t)
			d := &fakeSetupDaemon{}
			d.setState(activeInstallState())
			srv := d.server(t)

			s := attachSetupExecutor(srv.URL, true)
			defer s.Release()
			setupEngineInstall(context.Background(), s, io.Discard, tc.goos, tc.elevated)

			if got := len(f.installed()) > 0; got != tc.wantInstall {
				t.Fatalf("installed = %v, want %v", got, tc.wantInstall)
			}
			last := lastPhase(t, d)
			if last.Phase != tc.wantPhase {
				t.Fatalf("final phase = %q, want %q", last.Phase, tc.wantPhase)
			}
			if tc.wantDetail != "" && !strings.Contains(last.Error, tc.wantDetail) {
				t.Fatalf("error detail = %q, want it to mention %q", last.Error, tc.wantDetail)
			}
		})
	}
}

// TestSetupEngineInstallOlderDaemonIsInert is the acceptance-item-12
// bar: against a daemon without the executor routes, this must do
// nothing at all rather than install off its own guess.
func TestSetupEngineInstallOlderDaemonIsInert(t *testing.T) {
	shrinkSetupTimers(t)
	f := (&fakeEngineInstaller{}).install(t)
	d := &fakeSetupDaemon{notFound: true}
	srv := d.server(t)

	s := attachSetupExecutor(srv.URL, true)
	defer s.Release()
	setupEngineInstall(context.Background(), s, io.Discard, "linux", true)

	if got := f.installed(); len(got) != 0 {
		t.Fatalf("installed %v against an older daemon, want nothing", got)
	}
}

func TestSetupEngineInstallWanted(t *testing.T) {
	tests := []struct {
		name string
		st   management.SetupStateResponse
		want bool
	}{
		{"wanted", activeInstallState(), true},
		{"inactive", management.SetupStateResponse{DesiredEngine: "ollama"}, false},
		{"no desire", management.SetupStateResponse{Active: true}, false},
		{"present", management.SetupStateResponse{Active: true, DesiredEngine: "ollama", EngineInstalled: true}, false},
		{"claimed", management.SetupStateResponse{Active: true, DesiredEngine: "ollama", InstallClaimed: "ollama"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := setupEngineInstallWanted(tc.st); got != tc.want {
				t.Fatalf("setupEngineInstallWanted = %v, want %v", got, tc.want)
			}
		})
	}
}

// fakeVLLMInstaller records vLLM install attempts and answers the GPU /
// already-present probes, without building a real ~6 GB venv.
type fakeVLLMInstaller struct {
	mu     sync.Mutex
	calls  []string // stateDir per install
	handed []string // stateDir passed to the ownership handoff
	err    error
	nvidia bool // what setupDetectNVIDIA reports
	active bool // what setupVLLMActive reports
}

// install swaps in the vLLM seams for one test and returns the recorder.
func (f *fakeVLLMInstaller) install(t *testing.T) *fakeVLLMInstaller {
	t.Helper()
	prevInstall, prevNvidia, prevActive, prevHand := setupInstallVLLM, setupDetectNVIDIA, setupVLLMActive, setupHandState
	setupInstallVLLM = func(stateDir string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, stateDir)
		return f.err
	}
	setupDetectNVIDIA = func(context.Context) bool { return f.nvidia }
	setupVLLMActive = func(string) bool { return f.active }
	setupHandState = func(stateDir string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.handed = append(f.handed, stateDir)
	}
	t.Cleanup(func() {
		setupInstallVLLM, setupDetectNVIDIA, setupVLLMActive, setupHandState = prevInstall, prevNvidia, prevActive, prevHand
	})
	return f
}

func (f *fakeVLLMInstaller) installed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeVLLMInstaller) handedOff() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.handed...)
}

// activeVLLMInstallState is the state a daemon serves when the wizard asked
// for vLLM and nothing has claimed the install yet.
func activeVLLMInstallState() management.SetupStateResponse {
	return management.SetupStateResponse{
		Active:        true,
		DesiredEngine: "vllm",
		StateDir:      "/var/lib/waired",
	}
}

// TestVLLMInstallDecision pins the pure decision on every OS from an
// unprivileged runner: the two fast-fail conditions (non-Linux, no GPU) and
// the skips, with an already-present venv short-circuiting all of them.
func TestVLLMInstallDecision(t *testing.T) {
	tests := []struct {
		name                             string
		goos                             string
		elevated, nvidia, active, optOut bool
		want                             vllmInstallAction
	}{
		{"linux nvidia elevated installs", "linux", true, true, false, false, vllmActionInstall},
		{"already present short-circuits", "linux", true, true, true, false, vllmActionSkipPresent},
		{"present beats opt-out", "linux", true, true, true, true, vllmActionSkipPresent},
		{"opt-out refuses", "linux", true, true, false, true, vllmActionSkipOptOut},
		{"non-linux is unsupported", "darwin", true, true, false, false, vllmActionFailUnsupportedOS},
		{"windows is unsupported", "windows", true, true, false, false, vllmActionFailUnsupportedOS},
		{"no gpu fails fast", "linux", true, false, false, false, vllmActionFailNoGPU},
		{"unelevated skips", "linux", false, true, false, false, vllmActionSkipNotElevated},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := vllmInstallDecision(tc.goos, tc.elevated, tc.nvidia, tc.active, tc.optOut); got != tc.want {
				t.Fatalf("vllmInstallDecision = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestSetupVLLMInstallHappyPath: a Linux+NVIDIA host with the wizard asking
// for vLLM installs the venv, hands the state dir back, and reports done —
// the vLLM sibling of TestSetupEngineInstallHappyPath.
func TestSetupVLLMInstallHappyPath(t *testing.T) {
	shrinkSetupTimers(t)
	f := (&fakeVLLMInstaller{nvidia: true}).install(t)
	d := &fakeSetupDaemon{}
	d.setState(activeVLLMInstallState())
	srv := d.server(t)

	s := attachSetupExecutor(srv.URL, true)
	defer s.Release()
	setupEngineInstall(context.Background(), s, io.Discard, "linux", true)

	if got := f.installed(); len(got) != 1 || got[0] != "/var/lib/waired" {
		t.Fatalf("vllm installer calls = %v, want one call with the daemon's state dir", got)
	}
	if got := f.handedOff(); len(got) != 1 || got[0] != "/var/lib/waired" {
		t.Fatalf("ownership handoff = %v, want one call with the state dir", got)
	}
	if last := lastPhase(t, d); last.Phase != management.SetupExecutorPhaseDone {
		t.Fatalf("final phase = %q, want done", last.Phase)
	}
}

// TestSetupVLLMClaimsBeforeInstalling: like ollama, the daemon must see
// "installing" before the long venv build starts, or a second executor
// could kick off a parallel one.
func TestSetupVLLMClaimsBeforeInstalling(t *testing.T) {
	shrinkSetupTimers(t)
	d := &fakeSetupDaemon{}
	d.setState(activeVLLMInstallState())
	srv := d.server(t)

	s := attachSetupExecutor(srv.URL, true)
	defer s.Release()

	f := &fakeVLLMInstaller{nvidia: true}
	f.install(t)
	var phaseAtInstall string
	setupInstallVLLM = func(_ string) error {
		phaseAtInstall = lastPhase(t, d).Phase
		return nil
	}
	setupEngineInstall(context.Background(), s, io.Discard, "linux", true)

	if phaseAtInstall != management.SetupExecutorPhaseInstalling {
		t.Fatalf("phase when the vllm install began = %q, want installing", phaseAtInstall)
	}
}

// TestSetupVLLMInstallDecisionsThroughExecutor drives the whole decision
// tree through the executor and pins that each fast-fail / skip reports the
// right phase and a diagnosable detail, and never starts a doomed build.
func TestSetupVLLMInstallDecisionsThroughExecutor(t *testing.T) {
	tests := []struct {
		name             string
		goos             string
		elevated, nvidia bool
		active, optOut   bool
		wantInstall      bool
		wantPhase        string
		wantDetail       string
	}{
		{
			name: "linux nvidia elevated installs", goos: "linux", elevated: true, nvidia: true,
			wantInstall: true, wantPhase: management.SetupExecutorPhaseDone,
		},
		{
			name: "no gpu fails fast", goos: "linux", elevated: true, nvidia: false,
			wantPhase: management.SetupExecutorPhaseFailed, wantDetail: "NVIDIA",
		},
		{
			name: "non-linux is unsupported", goos: "darwin", elevated: true, nvidia: true,
			wantPhase: management.SetupExecutorPhaseFailed, wantDetail: "Linux",
		},
		{
			name: "unelevated reports permission", goos: "linux", nvidia: true,
			wantPhase: management.SetupExecutorPhaseFailed, wantDetail: "administrator privileges",
		},
		{
			name: "opt-out refuses and says why", goos: "linux", elevated: true, nvidia: true, optOut: true,
			wantPhase: management.SetupExecutorPhaseFailed, wantDetail: "WAIRED_NO_VLLM",
		},
		{
			name: "already present is done without a rebuild", goos: "linux", elevated: true, nvidia: true, active: true,
			wantPhase: management.SetupExecutorPhaseDone,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			shrinkSetupTimers(t)
			if tc.optOut {
				t.Setenv("WAIRED_NO_VLLM", "1")
			}
			f := (&fakeVLLMInstaller{nvidia: tc.nvidia, active: tc.active}).install(t)
			d := &fakeSetupDaemon{}
			d.setState(activeVLLMInstallState())
			srv := d.server(t)

			s := attachSetupExecutor(srv.URL, true)
			defer s.Release()
			setupEngineInstall(context.Background(), s, io.Discard, tc.goos, tc.elevated)

			if got := len(f.installed()) > 0; got != tc.wantInstall {
				t.Fatalf("installed = %v, want %v", got, tc.wantInstall)
			}
			last := lastPhase(t, d)
			if last.Phase != tc.wantPhase {
				t.Fatalf("final phase = %q, want %q", last.Phase, tc.wantPhase)
			}
			if tc.wantDetail != "" && !strings.Contains(last.Error, tc.wantDetail) {
				t.Fatalf("error detail = %q, want it to mention %q", last.Error, tc.wantDetail)
			}
		})
	}
}
