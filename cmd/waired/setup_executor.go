package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
)

// Executor residency budgets (waired#835 §9). Vars so tests can shrink
// them; the non-setup path keeps benchPollDeadline / benchNoEngineGrace
// unchanged, which is the acceptance-item-12 regression bar.
var (
	// setupResidencyBudget bounds how long `waired init` stays resident
	// once a browser setup is actually running. Matched to the control
	// plane's setup ticket TTL so the terminal and the wizard stop caring
	// at the same moment. Widened to 60 min for the vLLM install envelope
	// (~6 GB download + CUDA JIT build, up to setupVLLMInstallTimeout); the
	// CP's store.SetupTicketTTL is widened to match (waired#835 Phase 2).
	// An ollama install finishes far inside this.
	setupResidencyBudget = 60 * time.Minute

	// setupAwaitGrace bounds the gap between "login finished" and "the
	// operator clicked Yes in the browser". The daemon cannot report this
	// window — the setup ticket lives control-plane-side and never
	// reaches the agent (§8.1) — so it is the executor's own patience.
	// Too short and someone reading the offer card loses their executor
	// before clicking; too long and someone who never opens the browser
	// sits at a silent terminal.
	setupAwaitGrace = 3 * time.Minute

	// setupStatePollInterval paces the executor's state polling.
	setupStatePollInterval = 2 * time.Second

	// setupExecutorHeartbeatInterval must stay comfortably under the
	// daemon's lease TTL (45 s); four missed beats declare us gone.
	setupExecutorHeartbeatInterval = 10 * time.Second
)

// executorSession is the elevated CLI's lease with the local daemon
// (waired#835 §9/§11). While it is held, the daemon can tell a working
// executor from a dead one and report engine_install honestly instead of
// leaving the wizard spinning.
//
// A session whose daemon does not expose the routes (404 — a daemon
// older than this feature) is INERT: every method is a no-op and
// Supported reports false, so the caller falls back to the pre-#835 flow
// unchanged.
type executorSession struct {
	mgmtURL   string
	elevated  bool
	supported bool

	mu       sync.Mutex
	released bool
	stop     chan struct{}
	wg       sync.WaitGroup
	sigCh    chan os.Signal
	// phase/engine are tracked so a heartbeat issued mid-install keeps
	// reporting "installing" rather than resetting the daemon's view to
	// idle — which would drop the install claim and let a second
	// elevated install start.
	phase  string
	engine string
}

// attachSetupExecutor probes the daemon for the executor routes and, when
// they exist, attaches a lease and starts heartbeating it. The returned
// session is never nil; check Supported.
func attachSetupExecutor(mgmtURL string, elevated bool) *executorSession {
	s := &executorSession{mgmtURL: mgmtURL, elevated: elevated, stop: make(chan struct{})}
	if _, err := s.fetchState(); err != nil {
		// 404 (older daemon) or unreachable — stay inert.
		return s
	}
	s.supported = true
	s.post(true, management.SetupExecutorPhaseIdle, "", "")
	s.wg.Add(1)
	go s.heartbeat()
	s.watchSignals()
	return s
}

// Supported reports whether the daemon speaks the executor lease.
func (s *executorSession) Supported() bool { return s != nil && s.supported }

// fetchState reads the daemon's current setup view.
func (s *executorSession) fetchState() (management.SetupStateResponse, error) {
	body, err := httpGet(s.mgmtURL + "/waired/v1/setup/state")
	if err != nil {
		return management.SetupStateResponse{}, err
	}
	var st management.SetupStateResponse
	if err := json.Unmarshal(body, &st); err != nil {
		return management.SetupStateResponse{}, err
	}
	return st, nil
}

// State returns the daemon's setup view, or the zero value when the
// session is inert or the daemon is momentarily unreachable.
func (s *executorSession) State() management.SetupStateResponse {
	if !s.Supported() {
		return management.SetupStateResponse{}
	}
	st, err := s.fetchState()
	if err != nil {
		return management.SetupStateResponse{}
	}
	return st
}

// post sends one lease update. Errors are deliberately swallowed: a
// failed heartbeat is indistinguishable to the operator from a slow one,
// and the daemon's TTL already covers a session that stops reporting.
func (s *executorSession) post(attached bool, phase, engine, errText string) management.SetupStateResponse {
	if !s.Supported() {
		return management.SetupStateResponse{}
	}
	body, _ := json.Marshal(management.SetupExecutorRequest{
		Attached: attached,
		Elevated: s.elevated,
		Phase:    phase,
		Engine:   engine,
		Error:    errText,
	})
	out, err := httpPost(s.mgmtURL+"/waired/v1/setup/executor", body)
	if err != nil {
		return management.SetupStateResponse{}
	}
	var st management.SetupStateResponse
	_ = json.Unmarshal(out, &st)
	return st
}

func (s *executorSession) heartbeat() {
	defer s.wg.Done()
	t := time.NewTicker(setupExecutorHeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.post(true, s.currentPhase(), s.currentEngine(), "")
		}
	}
}

func (s *executorSession) currentPhase() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase == "" {
		return management.SetupExecutorPhaseIdle
	}
	return s.phase
}

func (s *executorSession) currentEngine() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.engine
}

// Installing claims the engine install for this lease.
func (s *executorSession) Installing(engine string) management.SetupStateResponse {
	s.setPhase(management.SetupExecutorPhaseInstalling, engine)
	return s.post(true, management.SetupExecutorPhaseInstalling, engine, "")
}

// Done reports a completed install and drops the claim.
func (s *executorSession) Done(engine string) {
	s.setPhase(management.SetupExecutorPhaseDone, engine)
	s.post(true, management.SetupExecutorPhaseDone, engine, "")
}

// Failed reports a failed install with its detail and drops the claim, so
// the wizard shows the real reason rather than a generic executor_gone.
func (s *executorSession) Failed(engine, errText string) {
	s.setPhase(management.SetupExecutorPhaseFailed, engine)
	s.post(true, management.SetupExecutorPhaseFailed, engine, errText)
}

func (s *executorSession) setPhase(phase, engine string) {
	s.mu.Lock()
	s.phase = phase
	s.engine = engine
	s.mu.Unlock()
}

// Release drops the lease immediately so the wizard stops claiming the
// install is in progress the moment the terminal stops driving it
// (normal exit, Enter-to-background, Ctrl-C). Idempotent.
func (s *executorSession) Release() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.released {
		s.mu.Unlock()
		return
	}
	s.released = true
	close(s.stop)
	sigCh := s.sigCh
	s.mu.Unlock()
	if sigCh != nil {
		signal.Stop(sigCh)
	}
	s.wg.Wait()
	s.post(false, s.currentPhase(), s.currentEngine(), "")
}

// watchSignals releases the lease on Ctrl-C / SIGTERM before the process
// dies. Without it the wizard would keep showing "installing" until the
// lease TTL expired — §9-4 wants the operator to see the truth promptly.
// The process still exits: we only get one chance to say goodbye.
func (s *executorSession) watchSignals() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	s.mu.Lock()
	s.sigCh = ch
	s.mu.Unlock()
	go func() {
		if _, ok := <-ch; !ok {
			return
		}
		s.Release()
		os.Exit(130) // 128 + SIGINT, the conventional shell code
	}()
}

// awaitBrowserSetup is the post-login decision point (waired#835 §4.1/§9):
// tell the operator the browser has it, offer the way back to the
// terminal, and wait out the gap until they actually start setup there.
//
// Returns the model-wait budget, whether a browser setup is driving this
// host, and the Enter listener (never nil, so callers can Drain
// unconditionally). Non-interactive / --no-browser / an older daemon all
// return the legacy budget with no listener attached, which is what keeps
// those paths byte-identical.
func awaitBrowserSetup(s *executorSession, stdin *bufio.Scanner, out io.Writer, nonInteractive, noBrowser bool) (time.Duration, bool, *enterListener) {
	idle := &enterListener{resolved: true} // inert: Drain and Backgrounded are no-ops
	if !s.Supported() || nonInteractive || noBrowser {
		return benchPollDeadline, false, idle
	}
	writePrompt(out, "Setup is continuing in your browser…")
	writePrompt(out, dim("(press Enter to continue in the terminal instead)"))
	enter := listenForEnter(stdin)
	budget, active := awaitSetupBudget(s, setupAwaitGrace, out, enter)
	return budget, active, enter
}

// awaitSetupBudget decides how long the CLI stays resident after login.
//
// This is a LOOP, not a one-shot check, and that is the whole point: at
// LoginPhaseActive no desired-state frame has arrived yet (the agent only
// marks a setup active when one does), because the operator has not
// clicked anything in the browser yet. A single check here would always
// read active=false, keep the legacy 10-minute deadline — 3 minutes on an
// engine-less host — and the executor would be gone before the wizard's
// first write landed.
//
// Returns the residency budget to use and whether a setup actually
// started. Pressing Enter backgrounds the wait and returns the legacy
// budget, which is how the operator takes the terminal back.
func awaitSetupBudget(s *executorSession, grace time.Duration, out io.Writer, enter *enterListener) (time.Duration, bool) {
	if !s.Supported() {
		return benchPollDeadline, false
	}
	if st := s.State(); st.Active {
		return setupResidencyBudget, true
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if enter != nil && enter.Backgrounded() {
			return benchPollDeadline, false
		}
		time.Sleep(setupStatePollInterval)
		if st := s.State(); st.Active {
			return setupResidencyBudget, true
		}
	}
	if out != nil {
		writePrompt(out, "No setup started in the browser; continuing here.")
	}
	return benchPollDeadline, false
}
