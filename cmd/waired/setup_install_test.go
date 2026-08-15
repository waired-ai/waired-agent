package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// fakeEngineInstaller records install attempts without downloading a
// multi-GB engine.
type fakeEngineInstaller struct {
	mu     sync.Mutex
	calls  []string // stateDir per call
	handed []string // stateDir passed to the ownership handoff
	err    error
}

// install swaps in the seams for the duration of one test and returns
// the recorder.
func (f *fakeEngineInstaller) install(t *testing.T) *fakeEngineInstaller {
	t.Helper()
	prevInstall, prevHand := setupInstallEngine, setupHandState
	setupInstallEngine = func(_ bool, stateDir string, _ func(infruntime.OllamaInstallProgress)) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, stateDir)
		return f.err
	}
	setupHandState = func(stateDir string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.handed = append(f.handed, stateDir)
	}
	t.Cleanup(func() {
		setupInstallEngine, setupHandState = prevInstall, prevHand
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

// lastPhase is what the DAEMON would conclude from the lease updates, not
// literally the last POST.
//
// The two differ by design. The heartbeat repeats the current phase every
// setupExecutorHeartbeatInterval carrying no error text at all
// (`s.post(true, s.currentPhase(), s.currentEngine(), "")`), so a terminal
// failure is followed — within 5 ms under shrinkSetupTimers — by an identical
// phase whose Error is "". The daemon survives that: keeping a stored detail
// through a text-less repeat is exactly what waired-agent#131 fixed, and
// TestSetupFailureDetailSurvivesHeartbeatAndRelease pins it. A test reading
// `reqs[len(reqs)-1]` did not, so it asserted against a snapshot the product
// never acts on, and lost a race nobody had declared.
//
// It went red first on the slowest CI leg (darwin, seeded host) while passing
// everywhere else, which is what a latent race looks like — the window is real
// on every runner and only scheduling decides who hits it.
//
// Folding on equal phase AND step is what keeps this from hiding a defect: a
// failure genuinely reported without detail still folds to no detail, and a
// detail posted under a different phase is never carried onto this one.
func lastPhase(t *testing.T, d *fakeSetupDaemon) management.SetupExecutorRequest {
	t.Helper()
	reqs := d.noted()
	if len(reqs) == 0 {
		t.Fatal("executor sent no lease updates")
	}
	folded := reqs[0]
	for _, next := range reqs[1:] {
		if next.Phase == folded.Phase && next.Step == folded.Step && next.Error == "" {
			next.Error, next.ErrorCode = folded.Error, folded.ErrorCode
		}
		folded = next
	}
	return folded
}

// TestSetupEngineInstall_EngineNeedsRepairReopensThePresenceGate is the
// reachability contract for #330's repair arm.
//
// EngineInstalled is pure file presence, and a host whose engine can never
// start satisfies it — which is why the executor returned early, reported
// nothing, and the wizard sat green over a dead engine on every rerun. The
// daemon's EngineNeedsRepair is what reopens the gate; without it a host that
// needs reinstalling is unreachable from both the browser wizard and
// `waired init`.
func TestSetupEngineInstall_EngineNeedsRepairReopensThePresenceGate(t *testing.T) {
	shrinkSetupTimers(t)
	f := (&fakeEngineInstaller{}).install(t)

	d := &fakeSetupDaemon{}
	st := activeInstallState()
	st.EngineInstalled = true   // the files are all there …
	st.EngineNeedsRepair = true // … and the daemon has given up starting it
	d.setState(st)
	srv := d.server(t)

	s := attachSetupExecutor(srv.URL, true)
	defer s.Release()
	if err := setupEngineInstall(context.Background(), s, io.Discard, "darwin", true); err != nil {
		t.Fatalf("setupEngineInstall = %v, want nil", err)
	}

	// It got past the gate and ran the installer against the daemon's state
	// dir.
	if got := f.installed(); len(got) != 1 || got[0] != "/var/lib/waired" {
		t.Fatalf("installer calls = %v, want one install call with the daemon's state dir", got)
	}
	if last := lastPhase(t, d); last.Phase != management.SetupExecutorPhaseDone {
		t.Fatalf("final phase = %q, want done", last.Phase)
	}
}

// The mirror: a healthy installed engine must still short-circuit. Reopening
// the gate for every host would put an install attempt on the critical path of
// every ordinary setup.
func TestSetupEngineInstall_HealthyInstallStillSkips(t *testing.T) {
	shrinkSetupTimers(t)
	f := (&fakeEngineInstaller{}).install(t)

	d := &fakeSetupDaemon{}
	st := activeInstallState()
	st.EngineInstalled = true
	d.setState(st)
	srv := d.server(t)

	s := attachSetupExecutor(srv.URL, true)
	defer s.Release()
	if err := setupEngineInstall(context.Background(), s, io.Discard, "darwin", true); err != nil {
		t.Fatal(err)
	}
	if got := f.installed(); len(got) != 0 {
		t.Errorf("installer calls = %v, want none for a healthy install", got)
	}
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
	if err := setupEngineInstall(context.Background(), s, io.Discard, "linux", true); err != nil {
		t.Fatalf("a successful install returned %v, want nil (#188)", err)
	}

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
	setupInstallEngine = func(_ bool, _ string, _ func(infruntime.OllamaInstallProgress)) error {
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
	// #188: the caller needs the same answer the wizard gets, or it walks
	// into a model wait for an engine that will never appear.
	if err := setupEngineInstall(context.Background(), s, io.Discard, "linux", true); err == nil {
		t.Fatal("a failed install returned nil")
	}

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
//
// wantCode is the §7 code this executor DECLARES (waired-agent#135).
// Both refusals below are decisions this process made, and both used to
// reach the wizard as network_error because the daemon could only guess
// from the prose; the empty rows are the failures whose text really is
// the only evidence, where the daemon's own classification is still the
// right answer.
func TestSetupEngineInstallPerOS(t *testing.T) {
	tests := []struct {
		name        string
		goos        string
		elevated    bool
		optOut      bool
		wantInstall bool
		wantPhase   string
		wantDetail  string
		wantCode    string
		// wantOptOut is whether the returned error must carry
		// errEngineOptOut — the marker init's exit status turns on (#551).
		wantOptOut bool
	}{
		{
			name: "linux elevated installs", goos: "linux", elevated: true,
			wantInstall: true, wantPhase: management.SetupExecutorPhaseDone,
		},
		{
			name: "linux unelevated reports permission", goos: "linux",
			wantPhase: management.SetupExecutorPhaseFailed, wantDetail: "administrator privileges",
			wantCode: signer.SetupErrorPermissionDenied,
		},
		{
			name: "windows elevated installs", goos: "windows", elevated: true,
			wantInstall: true, wantPhase: management.SetupExecutorPhaseDone,
		},
		{
			name: "windows unelevated reports permission", goos: "windows",
			wantPhase: management.SetupExecutorPhaseFailed, wantDetail: "administrator privileges",
			wantCode: signer.SetupErrorPermissionDenied,
		},
		{
			name: "darwin elevated installs", goos: "darwin", elevated: true,
			wantInstall: true, wantPhase: management.SetupExecutorPhaseDone,
		},
		{
			// Before #492 macOS had no elevation gate at all, because it
			// installed into the admin-group-writable /Applications. The engine
			// now lands under the root-owned state dir, so it needs root like
			// the other two — and saying so beats failing mid-download.
			name: "darwin unelevated reports permission", goos: "darwin",
			wantPhase: management.SetupExecutorPhaseFailed, wantDetail: "administrator privileges",
			wantCode: signer.SetupErrorPermissionDenied,
		},

		{
			// wantOptOut is the half #551 turned on: the wizard row is
			// unchanged (phase/detail/code below are what they always
			// were), and the only new fact is that the CALLER can now
			// recognise this as the operator's instruction.
			name: "opt-out refuses and says why", goos: "linux", elevated: true, optOut: true,
			wantPhase:  management.SetupExecutorPhaseFailed,
			wantDetail: "engine installs are turned off on this device (WAIRED_NO_OLLAMA)",
			wantCode:   signer.SetupErrorPermissionDenied, wantOptOut: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			shrinkSetupTimers(t)
			if tc.optOut {
				t.Setenv("WAIRED_NO_OLLAMA", "1")
			}
			f := (&fakeEngineInstaller{}).install(t)
			d := &fakeSetupDaemon{}
			d.setState(activeInstallState())
			srv := d.server(t)

			s := attachSetupExecutor(srv.URL, true)
			defer s.Release()
			err := setupEngineInstall(context.Background(), s, io.Discard, tc.goos, tc.elevated)

			// The return value used to be discarded here, so nothing
			// asserted the value `waired init`'s exit code is derived
			// from. The negative rows carry as much weight as the
			// positive one: errEngineOptOut turning up on an arm that is
			// a genuine fault would silently exit 0 on it (#551).
			if got := errors.Is(err, errEngineOptOut); got != tc.wantOptOut {
				t.Errorf("errors.Is(err, errEngineOptOut) = %v, want %v (err = %v)", got, tc.wantOptOut, err)
			}
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
			if last.ErrorCode != tc.wantCode {
				t.Fatalf("declared error_code = %q, want %q", last.ErrorCode, tc.wantCode)
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

// TestEngineArrivalPending is the #188 half of the no_engine grace: the
// model wait may only ignore its own give-up window while an engine can
// still plausibly show up. Everything else — above all a setup whose
// install has just failed — gets the ordinary grace back, instead of
// sitting on "Waiting for the AI engine to start…" for the hour the
// setup budget allows.
func TestEngineArrivalPending(t *testing.T) {
	tests := []struct {
		name string
		st   management.SetupStateResponse
		want bool
	}{
		{"no setup at all", management.SetupStateResponse{}, false},
		{"engine not picked yet", management.SetupStateResponse{Active: true}, true},
		{"install claimed by a live lease", management.SetupStateResponse{
			Active: true, DesiredEngine: "ollama", EngineInstalled: true, InstallClaimed: "ollama"}, true},
		{"desired engine not in place", management.SetupStateResponse{
			Active: true, DesiredEngine: "ollama"}, true},
		{"engine installed, nobody installing", management.SetupStateResponse{
			Active: true, DesiredEngine: "ollama", EngineInstalled: true}, false},
		{"inactive setup with an engine on the way", management.SetupStateResponse{
			DesiredEngine: "ollama", InstallClaimed: "ollama"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := engineArrivalPending(tc.st); got != tc.want {
				t.Fatalf("engineArrivalPending = %v, want %v", got, tc.want)
			}
		})
	}
}

// A finished install closes the arrival window, even when the daemon still
// cannot see an engine.
//
// PRODUCT CONTRACT — waired-agent#778. The vLLM venv built and verified, but
// its interpreter symlinked into the installing root user's home, so the
// unprivileged daemon could not read it and answered engine_installed=false
// permanently. With only engineArrivalPending's view that reads as "an
// engine is still on its way", the no_engine grace never arms, and init
// waits out the whole setup residency budget printing nothing — measured at
// 6+ minutes against a 3-minute grace on the reproduction host.
//
// A live InstallClaimed still keeps the window open: that is a DIFFERENT
// lease mid-install, and giving up on it is #188 in reverse.
func TestEngineArrivalPendingAfterInstall(t *testing.T) {
	wedged := management.SetupStateResponse{
		Active: true, DesiredEngine: "vllm", EngineInstalled: false,
	}
	tests := []struct {
		name       string
		st         management.SetupStateResponse
		installRan bool
		want       bool
	}{
		{"the #778 state, install finished", wedged, true, false},
		{"same state, no local install ran", wedged, false, true},
		{"install finished but another lease is installing", management.SetupStateResponse{
			Active: true, DesiredEngine: "vllm", InstallClaimed: "vllm"}, true, true},
		{"install finished and the engine is there", management.SetupStateResponse{
			Active: true, DesiredEngine: "ollama", EngineInstalled: true}, true, false},
		{"no setup driving", management.SetupStateResponse{}, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := engineArrivalPendingAfterInstall(tc.st, tc.installRan); got != tc.want {
				t.Fatalf("engineArrivalPendingAfterInstall(%+v, installRan=%v) = %v, want %v",
					tc.st, tc.installRan, got, tc.want)
			}
		})
	}
}

// fakeVLLMInstaller records vLLM install attempts and answers the GPU /
// already-present probes, without building a real ~9 GB venv.
type fakeVLLMInstaller struct {
	mu    sync.Mutex
	calls []string // stateDir per install
	// sinks records the progress callback each call was handed. It is
	// recorded rather than dropped because "the executor passed a live
	// sink" is the whole of waired-agent#255 on this path, and a fake
	// that swallowed the argument would make that case unwritable.
	sinks  []func(infruntime.InstallProgress)
	handed []string // stateDir passed to the ownership handoff
	err    error
	nvidia bool // what setupDetectNVIDIA reports
	active bool // what setupVLLMActive reports
}

// install swaps in the vLLM seams for one test and returns the recorder.
func (f *fakeVLLMInstaller) install(t *testing.T) *fakeVLLMInstaller {
	t.Helper()
	prevInstall, prevNvidia, prevActive, prevHand := setupInstallVLLM, setupDetectNVIDIA, setupVLLMActive, setupHandState
	setupInstallVLLM = func(stateDir string, sink func(infruntime.InstallProgress)) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, stateDir)
		f.sinks = append(f.sinks, sink)
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

func (f *fakeVLLMInstaller) progressSinks() []func(infruntime.InstallProgress) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]func(infruntime.InstallProgress){}, f.sinks...)
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
	// A live lease must hand the installer a live sink, or the wizard is
	// back to 45 minutes of "Working on it…" (waired-agent#255). This is
	// the one place that wiring is observable.
	if sinks := f.progressSinks(); len(sinks) != 1 || sinks[0] == nil {
		t.Fatalf("progress sinks = %d, want exactly one non-nil sink", len(sinks))
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
	setupInstallVLLM = func(_ string, _ func(infruntime.InstallProgress)) error {
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
		wantCode         string
		// Same marker as the ollama table: vLLM's opt-out is the same
		// class of answer and must reach init's exit status the same way.
		wantOptOut bool
	}{
		{
			name: "linux nvidia elevated installs", goos: "linux", elevated: true, nvidia: true,
			wantInstall: true, wantPhase: management.SetupExecutorPhaseDone,
		},
		{
			// internal, not permission_denied: none of the eight §7 codes
			// means "this computer cannot run this engine", and the detail
			// is what names the missing hardware.
			name: "no gpu fails fast", goos: "linux", elevated: true, nvidia: false,
			wantPhase: management.SetupExecutorPhaseFailed, wantDetail: "NVIDIA",
			wantCode: signer.SetupErrorInternal,
		},
		{
			name: "non-linux is unsupported", goos: "darwin", elevated: true, nvidia: true,
			wantPhase: management.SetupExecutorPhaseFailed, wantDetail: "Linux",
			wantCode: signer.SetupErrorInternal,
		},
		{
			name: "unelevated reports permission", goos: "linux", nvidia: true,
			wantPhase: management.SetupExecutorPhaseFailed, wantDetail: "administrator privileges",
			wantCode: signer.SetupErrorPermissionDenied,
		},
		{
			name: "opt-out refuses and says why", goos: "linux", elevated: true, nvidia: true, optOut: true,
			wantPhase:  management.SetupExecutorPhaseFailed,
			wantDetail: "engine installs are turned off on this device (WAIRED_NO_VLLM)",
			wantCode:   signer.SetupErrorPermissionDenied, wantOptOut: true,
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
			err := setupEngineInstall(context.Background(), s, io.Discard, tc.goos, tc.elevated)

			if got := errors.Is(err, errEngineOptOut); got != tc.wantOptOut {
				t.Errorf("errors.Is(err, errEngineOptOut) = %v, want %v (err = %v)", got, tc.wantOptOut, err)
			}
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
			if last.ErrorCode != tc.wantCode {
				t.Fatalf("declared error_code = %q, want %q", last.ErrorCode, tc.wantCode)
			}
		})
	}
}
