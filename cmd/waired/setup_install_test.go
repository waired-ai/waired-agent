package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/internal/setup"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// fakeEngineInstaller records install attempts without downloading a
// multi-GB engine.
type fakeEngineInstaller struct {
	mu     sync.Mutex
	calls  []string // stateDir per call
	handed []string // stateDir passed to the ownership handoff
	// detectedIn / repairedIn record the stateDir each detection and repair
	// was asked about. The repair path is only correct if it is handed the
	// daemon's state dir, so a fake that dropped it would make the wrong-dir
	// bug unwritable (CLAUDE.md §Test discipline).
	detectedIn []string
	repairedIn []repairCall
	err        error
	detected   setup.OllamaDetection
	// repairChanged / repairErr script setupRepairDarwinBundle's answer.
	repairChanged bool
	repairErr     error
	// sigBroken scripts the codesign/spctl probe, and sigProbes records what
	// it was asked about — a fake that dropped the detection could not express
	// "we probed the wrong install".
	sigBroken bool
	sigProbes []setup.OllamaDetection
}

type repairCall struct {
	GOOS     string
	StateDir string
	Det      setup.OllamaDetection
}

// install swaps in the seams for the duration of one test and returns
// the recorder.
func (f *fakeEngineInstaller) install(t *testing.T) *fakeEngineInstaller {
	t.Helper()
	prevInstall, prevDetect, prevHand := setupInstallEngine, setupDetectEngine, setupHandState
	prevNoExec, prevRepair := setupDetectEngineNoExec, setupRepairDarwinBundle
	prevSig := setupEngineSignatureBroken
	setupEngineSignatureBroken = func(_ context.Context, det setup.OllamaDetection) bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.sigProbes = append(f.sigProbes, det)
		return f.sigBroken
	}
	setupInstallEngine = func(_ bool, stateDir string, _ func(infruntime.OllamaInstallProgress)) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, stateDir)
		return f.err
	}
	setupDetectEngine = func(_ context.Context, stateDir string) setup.OllamaDetection {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.detectedIn = append(f.detectedIn, stateDir)
		return f.detected
	}
	setupDetectEngineNoExec = func(stateDir string) setup.OllamaDetection {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.detectedIn = append(f.detectedIn, stateDir)
		return f.detected
	}
	setupRepairDarwinBundle = func(goos, stateDir string, det setup.OllamaDetection) (bool, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.repairedIn = append(f.repairedIn, repairCall{GOOS: goos, StateDir: stateDir, Det: det})
		return f.repairChanged, f.repairErr
	}
	setupHandState = func(stateDir string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.handed = append(f.handed, stateDir)
	}
	t.Cleanup(func() {
		setupInstallEngine, setupDetectEngine, setupHandState = prevInstall, prevDetect, prevHand
		setupDetectEngineNoExec, setupRepairDarwinBundle = prevNoExec, prevRepair
		setupEngineSignatureBroken = prevSig
	})
	return f
}

func (f *fakeEngineInstaller) repairs() []repairCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]repairCall(nil), f.repairedIn...)
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

// TestSetupEngineInstall_RepairsTheDarwinBundleBeforeEveryGate is the
// reachability contract for #329's migration, and it is the whole reason the
// repair sits where it does.
//
// A host broken by the old in-bundle marker satisfies EVERY early return in
// setupEngineInstall: the engine is installed, all its files are present, the
// daemon reports EngineInstalled=true. It just cannot be executed. So a repair
// placed after those gates would never run on the hosts that need it — which
// is exactly how the wizard kept reporting OK forever. It has to run first.
func TestSetupEngineInstall_RepairsTheDarwinBundleBeforeEveryGate(t *testing.T) {
	shrinkSetupTimers(t)
	f := &fakeEngineInstaller{
		detected: setup.OllamaDetection{
			Installed:              true,
			Path:                   "/Applications/Ollama.app/Contents/Resources/ollama",
			LegacyBundleMarkerPath: "/Applications/Ollama.app/.waired-managed.json",
		},
		repairChanged: true,
	}
	f.install(t)

	d := &fakeSetupDaemon{}
	// EngineInstalled: the state a broken host actually serves — this is the
	// gate that used to swallow the whole function.
	st := activeInstallState()
	st.EngineInstalled = true
	d.setState(st)
	srv := d.server(t)

	s := attachSetupExecutor(srv.URL, true)
	defer s.Release()

	var out bytes.Buffer
	if err := setupEngineInstall(context.Background(), s, &out, "darwin", true); err != nil {
		t.Fatalf("setupEngineInstall = %v, want nil", err)
	}

	repairs := f.repairs()
	if len(repairs) != 1 {
		t.Fatalf("repair calls = %d, want exactly 1 despite EngineInstalled=true", len(repairs))
	}
	if repairs[0].GOOS != "darwin" || repairs[0].StateDir != "/var/lib/waired" {
		t.Errorf("repair called with goos=%q stateDir=%q, want darwin + the daemon's state dir",
			repairs[0].GOOS, repairs[0].StateDir)
	}
	if !strings.Contains(out.String(), "Repaired the AI engine") {
		t.Errorf("output = %q, want the repair to be announced", out.String())
	}
	// Repairing is not installing: the engine is present, so nothing downloads.
	if got := f.installed(); len(got) != 0 {
		t.Errorf("installer calls = %v, want none", got)
	}
}

// The repair must never fire on the two OSes that have no bundle to break,
// and must not fire on a healthy Mac.
func TestSetupEngineInstall_NoRepairWhenNotNeeded(t *testing.T) {
	shrinkSetupTimers(t)
	broken := setup.OllamaDetection{
		Installed:              true,
		LegacyBundleMarkerPath: "/Applications/Ollama.app/.waired-managed.json",
	}
	for _, tc := range []struct {
		name     string
		goos     string
		detected setup.OllamaDetection
	}{
		{"linux", "linux", broken},
		{"windows", "windows", broken},
		{"healthy darwin", "darwin", setup.OllamaDetection{Installed: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeEngineInstaller{detected: tc.detected}
			f.install(t)
			d := &fakeSetupDaemon{}
			st := activeInstallState()
			st.EngineInstalled = true
			d.setState(st)
			srv := d.server(t)

			s := attachSetupExecutor(srv.URL, true)
			defer s.Release()
			if err := setupEngineInstall(context.Background(), s, io.Discard, tc.goos, true); err != nil {
				t.Fatal(err)
			}
			if got := f.repairs(); len(got) != 0 {
				t.Errorf("repair calls = %+v, want none", got)
			}
		})
	}
}

// TestSetupEngineInstall_EngineNeedsRepairReopensThePresenceGate is the
// reachability contract for #330's repair arm.
//
// EngineInstalled is pure file presence, and a host whose engine can never
// start satisfies it — which is why the executor returned early, reported
// nothing, and the wizard sat green over a dead engine on every rerun. The
// daemon's EngineNeedsRepair is what reopens the gate; without it the repair
// arm added to engineInstallDecision would be unreachable from both the
// browser wizard and `waired init`.
func TestSetupEngineInstall_EngineNeedsRepairReopensThePresenceGate(t *testing.T) {
	shrinkSetupTimers(t)
	f := &fakeEngineInstaller{
		detected: setup.OllamaDetection{
			Installed: true,
			Path:      "/Applications/Ollama.app/Contents/Resources/ollama",
		},
		sigBroken: true,
	}
	f.install(t)

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

	// It got past the gate, probed the install it was told about, and ran the
	// installer (engineActionRepair shares the install arm).
	if got := f.sigProbes; len(got) != 1 || got[0].Path == "" {
		t.Fatalf("signature probes = %+v, want one probe carrying the detected path", got)
	}
	if got := f.installed(); len(got) != 1 || got[0] != "/var/lib/waired" {
		t.Fatalf("installer calls = %v, want one repair call with the daemon's state dir", got)
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
	f := &fakeEngineInstaller{detected: setup.OllamaDetection{Installed: true}}
	f.install(t)

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
	if got := f.sigProbes; len(got) != 0 {
		t.Errorf("signature probes = %+v, want none — the gate should close before any probe", got)
	}
}

// TestSetupEngineInstall_SignatureFailureDeclaresItsCode: a codesign rejection
// must reach the daemon as engine_not_ready, not as the network_error the
// text-classification catch-all would assign.
func TestSetupEngineInstall_SignatureFailureDeclaresItsCode(t *testing.T) {
	shrinkSetupTimers(t)
	sigErr := bundleSignatureVerdict(bundleSignatureReport{
		Path: "/Applications/Ollama.app", Probed: true,
		CodesignOut: realCodesignRejection, CodesignErr: errors.New("exit status 1"),
	})
	f := &fakeEngineInstaller{err: sigErr}
	f.install(t)

	d := &fakeSetupDaemon{}
	d.setState(activeInstallState())
	srv := d.server(t)

	s := attachSetupExecutor(srv.URL, true)
	defer s.Release()
	if err := setupEngineInstall(context.Background(), s, io.Discard, "darwin", true); err == nil {
		t.Fatal("want the install failure to propagate")
	}

	last := lastPhase(t, d)
	if last.Phase != management.SetupExecutorPhaseFailed {
		t.Fatalf("phase = %q, want failed", last.Phase)
	}
	if last.ErrorCode != signer.SetupErrorEngineNotReady {
		t.Errorf("error_code = %q, want %q", last.ErrorCode, signer.SetupErrorEngineNotReady)
	}
	if !strings.Contains(last.Error, "unsealed contents present in the bundle root") {
		t.Errorf("error = %q, want codesign's own diagnosis carried through", last.Error)
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
		detected    setup.OllamaDetection
		wantInstall bool
		wantPhase   string
		wantDetail  string
		wantCode    string
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
			wantCode: signer.SetupErrorPermissionDenied,
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
			wantPhase: management.SetupExecutorPhaseFailed, wantDetail: "WAIRED_NO_VLLM",
			wantCode: signer.SetupErrorPermissionDenied,
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
			if last.ErrorCode != tc.wantCode {
				t.Fatalf("declared error_code = %q, want %q", last.ErrorCode, tc.wantCode)
			}
		})
	}
}
