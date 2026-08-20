package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// Executor residency budgets (waired#835 §9). Vars so tests can shrink
// them; the non-setup path keeps benchPollDeadline / benchNoEngineGrace
// unchanged, which is the acceptance-item-12 regression bar.
var (
	// setupResidencyBudget bounds how long `waired init` stays in the
	// foreground watching the model download once a browser setup is
	// actually driving this host. It is NOT matched to the control
	// plane's setup ticket TTL any more: since waired#944 that window
	// starts at 60 min and then slides while both the browser and this
	// device are live, so there is no single number left to match — and
	// 60 min is now the *shortest* it can be, which would guarantee the
	// terminal gives up while the wizard is still working. This tracks
	// the CP's ceiling instead (api.SetupTicketMaxTTL, 8 h).
	//
	// The accepted cost is terminal occupancy: on a stalled pull with
	// piped stdin the takeover watch is inert, so there is no Enter
	// escape and `waired init` can hold a terminal for eight hours.
	// Hitting the budget is a soft skip, never a failure — the daemon
	// keeps pulling either way and the CLI falls through to its tail.
	//
	// This does not bound the engine install: runSetupEngineInstall runs
	// before the wait under its own setupVLLMInstallTimeout /
	// ollamaInstallTimeoutDefault.
	setupResidencyBudget = 8 * time.Hour

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
	// attachNote records why the attach did not go cleanly, in one line
	// meant for the operator. Empty when it did, and empty for the one
	// benign case: a daemon older than this feature answers 404, and
	// staying inert is the documented outcome above for it.
	//
	// Written in attachSetupExecutor before the heartbeat goroutine
	// starts and read after, so it needs no lock — the same reason
	// supported does not have one (waired-agent#746).
	attachNote string

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
	// step names which §7 row the phase and the figures below belong to,
	// and progress is the last transfer report for it. The heartbeat
	// resends both for the same reason it resends the phase: the daemon's
	// view of a step must not decay to nothing between two real updates
	// (waired-agent#197).
	step     string
	progress executorProgress
	// progressAt paces Progress. The installer's own callback fires far
	// faster than either this local IPC or the CP's 1-push-per-2 s intake
	// can use, so ordinary byte updates are throttled; a step change or a
	// terminal phase always posts immediately.
	progressAt time.Time
	// driver is this surface's claim on the setup (waired-agent#198),
	// repeated on every post so a daemon that restarted mid-run learns it
	// again rather than reporting a setup nobody is driving.
	driver string
	// writtenIntegrations names the coding tools this process configured,
	// for the daemon to record when it has no instruction of its own to
	// read them from (waired-agent#646). Repeated on every post for the
	// same reason the driver is — a daemon that restarted mid-run has to
	// learn it again — and read by the daemon only on the integration
	// row's `done` edge.
	writtenIntegrations []string
}

// executorProgress is one step's byte-level transfer figures.
type executorProgress struct {
	completed int64
	total     int64
	rateBps   int64
}

// executorProgressInterval throttles ordinary byte updates from the
// installer's progress callback. 500 ms is four updates per CP push
// window — enough that the browser's bar moves smoothly, few enough that
// a download does not spend its time posting about itself. A var for the
// same reason as the budgets above: tests shrink it.
var executorProgressInterval = 500 * time.Millisecond

// attachSetupExecutor probes the daemon for the executor routes and, when
// they exist, attaches a lease and starts heartbeating it. The returned
// session is never nil; check Supported, and AttachNote for why an attach
// that did not go cleanly did not.
//
// The probe and the attach do not share a transport, which is what makes
// the note worth keeping (waired-agent#746). The probe is a read, and
// /setup/state is on the loopback-TCP allow-list (waired#836), so it can
// succeed over TCP on a host whose IPC socket is unavailable. The attach
// is a write, and writes are socket-only since waired#838. A daemon that
// answers the probe is therefore not evidence that the lease will take,
// and every gate downstream reads Supported.
func attachSetupExecutor(mgmtURL string, elevated bool) *executorSession {
	s := &executorSession{mgmtURL: mgmtURL, elevated: elevated, stop: make(chan struct{})}
	if _, err := s.fetchState(); err != nil {
		// A daemon older than this feature answers 404, and being inert
		// is the documented, correct outcome for it — say nothing. Every
		// other cause (unreachable, a 5xx, a body that will not parse)
		// lands in the same inert state for a reason the operator can
		// act on, so record it.
		if !isMgmtStatus(err, http.StatusNotFound) {
			s.attachNote = fmt.Sprintf("could not ask the background service about setup (%v); "+
				"its setup steps will be skipped. Run \"waired doctor\" to see why.", err)
		}
		return s
	}
	s.supported = true
	if _, err := s.postStepErr(true, management.SetupExecutorPhaseIdle, "", "", "", "", executorProgress{}); err != nil {
		// Deliberately still supported: heartbeat resends attached=true
		// every setupExecutorHeartbeatInterval, so the lease recovers on
		// its own. Going inert here would let one failed write cancel
		// the engine install for the whole run.
		s.attachNote = fmt.Sprintf("could not tell the background service that setup is running (%v); "+
			"retrying in the background. If the browser shows no progress, run \"waired doctor\".", err)
	}
	s.wg.Add(1)
	go s.heartbeat()
	s.watchSignals()
	return s
}

// Supported reports whether the daemon speaks the executor lease.
func (s *executorSession) Supported() bool { return s != nil && s.supported }

// AttachNote returns the one-line reason the attach did not go cleanly,
// or "" when it did or when the daemon simply predates the routes.
func (s *executorSession) AttachNote() string {
	if s == nil {
		return ""
	}
	return s.attachNote
}

// reportAttachNote prints AttachNote, if there is one. Split out so the
// caller decides where the line goes and the session stays a producer of
// facts — and so this is testable without standing up a login flow.
func reportAttachNote(out io.Writer, s *executorSession) {
	if note := s.AttachNote(); note != "" {
		writePromptf(out, "warn: %s\n", note)
	}
}

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
//
// It carries no error code: everything that reaches here either is not a
// failure or is one whose text is the only evidence there is. The paths
// that DO know their code call postStep (waired-agent#135).
func (s *executorSession) post(attached bool, phase, engine, errText string) management.SetupStateResponse {
	step, prog := s.currentProgress()
	return s.postStep(attached, phase, engine, errText, "", step, prog)
}

// postStep is post with an explicit step, error code, and transfer
// figures. It swallows the error for the reason post documents; the one
// caller that must see it — the initial attach — uses postStepErr.
func (s *executorSession) postStep(attached bool, phase, engine, errText, errCode, step string, prog executorProgress) management.SetupStateResponse {
	st, _ := s.postStepErr(attached, phase, engine, errText, errCode, step, prog)
	return st
}

// postStepErr is postStep with the transport error returned rather than
// dropped. Only attachSetupExecutor calls it: there is nothing before the
// first post to have succeeded, and no second chance until the next
// heartbeat, so it is the one post whose failure the operator cannot
// infer from anything else (waired-agent#746).
func (s *executorSession) postStepErr(attached bool, phase, engine, errText, errCode, step string, prog executorProgress) (management.SetupStateResponse, error) {
	if !s.Supported() {
		return management.SetupStateResponse{}, nil
	}
	body, _ := json.Marshal(management.SetupExecutorRequest{
		Attached:           attached,
		Elevated:           s.elevated,
		Phase:              phase,
		Engine:             engine,
		Error:              errText,
		ErrorCode:          errCode,
		Step:               step,
		CompletedBytes:     prog.completed,
		TotalBytes:         prog.total,
		RateBps:            prog.rateBps,
		Driver:             s.currentDriver(),
		IntegrationTargets: s.currentWrittenIntegrations(),
	})
	out, err := httpPost(s.mgmtURL+"/waired/v1/setup/executor", body)
	if err != nil {
		return management.SetupStateResponse{}, err
	}
	var st management.SetupStateResponse
	_ = json.Unmarshal(out, &st)
	return st, nil
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

// TakeOver claims the setup for this terminal (waired-agent#198).
//
// The lease is deliberately NOT released here. Before this, taking over
// dropped it, and the wizard — which cannot tell a deliberate handoff
// from a crash — reported `executor_gone` and sent the operator back to
// a machine that was in fact busy setting itself up. Holding the lease
// with a driver claim says which of the two happened, and keeps the
// answer honest: if this process dies, the lease expires and the claim
// goes with it.
func (s *executorSession) TakeOver() {
	if !s.Supported() {
		return
	}
	s.mu.Lock()
	s.driver = signer.SetupDriverTerminal
	s.mu.Unlock()
	s.post(true, s.currentPhase(), s.currentEngine(), "")
}

func (s *executorSession) currentDriver() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.driver
}

func (s *executorSession) currentWrittenIntegrations() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writtenIntegrations
}

// IntegrationDone reports the coding-tool row finished and names what this
// process configured (waired-agent#646).
//
// The wizard's own apply does not need the names — the daemon is serving it
// the instruction and takes them from there — but the terminal's does. A
// `waired init` run from a terminal has no instruction to apply: the
// control plane's desired columns are written by the management API, so
// nothing on this device knows what was written except the process that
// wrote it. Without this the row went unreported, and a machine whose
// coding tools were demonstrably configured showed no coding-tools row at
// all for the rest of its life.
func (s *executorSession) IntegrationDone(targets []string) {
	if !s.Supported() {
		return
	}
	s.mu.Lock()
	s.writtenIntegrations = targets
	s.mu.Unlock()
	s.DoneStep(management.SetupStepIntegration)
}

// currentProgress returns the step the lease is reporting against and its
// last transfer figures, so a heartbeat repeats them rather than letting
// the daemon's view of the row decay between real updates.
func (s *executorSession) currentProgress() (string, executorProgress) {
	if s == nil {
		return "", executorProgress{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.step, s.progress
}

// Installing claims the engine install for this lease.
func (s *executorSession) Installing(engine string) management.SetupStateResponse {
	s.setPhase(management.SetupExecutorPhaseInstalling, engine)
	return s.post(true, management.SetupExecutorPhaseInstalling, engine, "")
}

// Progress reports byte-level movement on one step (waired-agent#197).
//
// This is what the browser wizard has been missing: the engine install
// runs in this elevated process and draws its bar on this terminal, so
// without a report the wizard has "Working on it…" and nothing else for
// the length of a 1.4 GB download. Ordinary updates are throttled to
// executorProgressInterval; a change of step is not, because that is the
// event that closes one row and opens the next.
//
// A step change also posts the OUTGOING step as done first, so the row
// the operator was watching finishes rather than being abandoned
// mid-bar.
func (s *executorSession) Progress(step, engine string, completed, total, rateBps int64) {
	if !s.Supported() || step == "" {
		return
	}
	prog := executorProgress{completed: completed, total: total, rateBps: rateBps}

	s.mu.Lock()
	prev := s.step
	changed := prev != step
	throttled := !changed && time.Since(s.progressAt) < executorProgressInterval
	if !throttled {
		s.progressAt = time.Now()
	}
	s.step = step
	s.progress = prog
	if engine != "" {
		s.engine = engine
	}
	// Reporting progress on a step IS its in-flight phase, and it
	// replaces whatever the previous step ended on. Carrying a finished
	// step's `done` forward would have the heartbeat announce the new
	// step complete before it started.
	s.phase = management.SetupExecutorPhaseInstalling
	phase := s.phase
	s.mu.Unlock()

	if throttled {
		return
	}
	if changed && prev != "" {
		// The previous row is finished by definition: the executor only
		// moves on once it is. No figures — the daemon keeps the ones it
		// already has for that row, and a `done` row does not draw a bar.
		s.postStep(true, management.SetupExecutorPhaseDone, engine, "", "", prev, executorProgress{})
	}
	s.postStep(true, phase, engine, "", "", step, prog)
}

// Done reports a completed install and drops the claim.
func (s *executorSession) Done(engine string) {
	s.setPhase(management.SetupExecutorPhaseDone, engine)
	s.post(true, management.SetupExecutorPhaseDone, engine, "")
}

// DoneStep / FailedStep report a terminal phase for a NON-engine step —
// today only the coding-agent integration (waired#935). They are
// deliberately not Done/Failed: those set the session phase, which the
// heartbeat then repeats and the daemon reads as the engine install's
// state. A row outside the engine must not be able to say anything about
// the engine.
func (s *executorSession) DoneStep(step string) {
	s.setStepPhase(step, management.SetupExecutorPhaseDone)
	s.postStep(true, management.SetupExecutorPhaseDone, "", "", "", step, executorProgress{})
}

// FailedStep reports a failed non-engine step. It declares no code: the
// integration failures are small file writes whose text is the only
// evidence, and classifyIntegrationFailure already reads the one case
// (permission denied) that is worth distinguishing.
func (s *executorSession) FailedStep(step, errText string) {
	s.FailedStepCode(step, "", errText)
}

// FailedStepCode is FailedStep for the failures this process can classify
// better than the daemon can. The daemon only ever receives the text, so a
// cause that is carried in the error's TYPE rather than its words — a
// context deadline is the one that matters here — is invisible by the time
// it gets there. Everything whose evidence IS the text stays FailedStep's,
// so there is one implementation of each rule rather than two.
func (s *executorSession) FailedStepCode(step, code, errText string) {
	s.setStepPhase(step, management.SetupExecutorPhaseFailed)
	s.postStep(true, management.SetupExecutorPhaseFailed, "", errText, code, step, executorProgress{})
}

// setStepPhase moves the lease's reporting focus to one step and records
// its phase. The heartbeat repeats whatever is set here, so leaving a
// terminal phase attached to the step it belongs to is what keeps a
// finished integration from being re-announced as an engine install —
// and vice versa.
func (s *executorSession) setStepPhase(step, phase string) {
	s.mu.Lock()
	s.step = step
	s.phase = phase
	s.mu.Unlock()
}

// Failed reports a failed install with its detail and drops the claim, so
// the wizard shows the real reason rather than a generic executor_gone.
//
// code is this executor's own §7 classification, or "" to let the daemon
// classify from the text (waired-agent#135). Empty is right for a
// failure that arrived as an installer's error string — the daemon's
// disk-full detection is the best reading anyone has of those. It is
// wrong for a failure this process DECIDED on: not elevated, opted out,
// a host that cannot run this engine. Those know their code, and passing
// it is what stops "run as administrator" from being reported as a
// network problem.
func (s *executorSession) Failed(engine, code, errText string) {
	s.setPhase(management.SetupExecutorPhaseFailed, engine)
	step, prog := s.currentProgress()
	s.postStep(true, management.SetupExecutorPhaseFailed, engine, errText, code, step, prog)
}

// setPhase records an ENGINE phase, and moves the lease's reporting
// focus back to the engine rows: an empty step is the engine install
// everywhere in this protocol, and a phase left attached to whatever
// step was last reported (the integration, say) would be read by the
// daemon as that step's outcome.
func (s *executorSession) setPhase(phase, engine string) {
	s.mu.Lock()
	s.phase = phase
	s.engine = engine
	s.step = ""
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
// host, the takeover watch, and the setup watch that carries the same
// question past this window (#308) — the two watches are never nil, so
// callers can poll unconditionally. Non-interactive / --no-browser / an
// older daemon all return the legacy budget with both watches inert,
// which is what keeps those paths byte-identical.
//
// in is the process's stdin owner, or nil when init is not running on a
// terminal. Two things follow from a nil owner: the offer line is not
// printed (there is no keyboard to press it on), and the watch is inert
// — a piped stdin belongs to the script driving init, and letting it
// take the terminal over is how a scripted answer went missing (#185).
func awaitBrowserSetup(s *executorSession, in *stdinReader, out io.Writer, nonInteractive, noBrowser bool) (time.Duration, bool, *enterWatch, *setupWatch) {
	if !s.Supported() || nonInteractive || noBrowser {
		// A run that never offers the browser is terminal-driven by
		// definition, so say so (waired-agent#646). Without a claim the
		// daemon has to guess from the desired state, and the guess is
		// wrong twice over on these paths: a host with none reports no
		// setup at all, and one carrying a leftover instruction reports a
		// browser that nobody opened. An inert session is a no-op, which
		// is what keeps a daemon without the routes byte-identical.
		s.TakeOver()
		return benchPollDeadline, false, newTakeoverWatch(nil), newSetupWatch(nil, false)
	}
	writePrompt(out, "Setup is continuing in your browser…")
	// waired#939: the persistence warning comes BEFORE the offer to switch.
	// This process is the elevated executor and the browser has no route to
	// replace it, so "don't close this" outranks "you may leave" — and the
	// order is what stops the offer reading as permission to walk away.
	// Printed whether or not there is a keyboard: it is true either way.
	writePrompt(out, setupKeepTerminalOpenLine)
	if in != nil {
		writePrompt(out, dim("(press Enter to continue in the terminal instead)"))
	}
	// #184: drop anything typed before the offer existed — most of all
	// the Enter pressed at the sign-in step above, which used to arrive
	// here and switch modes without ever being acknowledged.
	in.Discard()
	enter := newTakeoverWatch(in)
	budget, active := awaitSetupBudget(s, setupAwaitGrace, out, enter)
	if !active && !enter.Fired() && in != nil {
		// #309: the grace expired with nothing driving, so this terminal
		// is the driver and there is nothing left to take over. Keeping
		// the confirm watch armed asked about a browser that was not
		// there — and answered a second Enter with "Continuing in your
		// browser — keep this terminal window open until setup finishes",
		// which was simply untrue. From here Enter means what it means
		// everywhere else a terminal owns a long download (waired#774):
		// put the wait in the background.
		//
		// Not swapped when the takeover fired: awaitSetupBudget already
		// claimed the driver for it, and runInitViaDaemon reads Fired()
		// afterwards to keep the lease honest (waired-agent#198).
		in.Discard() // typed at the offer that just ended (#184)
		writePrompt(out, dim("(press Enter anytime to continue in the background)"))
		enter = newBackgroundWatch(in)
	}
	// #308: the grace above is one 3-minute window. When it expires with
	// nothing started, the long waits downstream keep asking — the
	// operator reading the model picker for four minutes is the ordinary
	// case this window was too short for. If one does start, the wait
	// closes this watch, and a closed watch can no longer background
	// either (init_takeover.go) — leaving would release the lease under a
	// setup the wizard believes is running.
	return budget, active, enter, newSetupWatch(s, active)
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
// started. Confirming the takeover ends the wait and returns the legacy
// budget, which is how the operator takes the terminal back.
func awaitSetupBudget(s *executorSession, grace time.Duration, out io.Writer, enter *enterWatch) (time.Duration, bool) {
	if !s.Supported() {
		return benchPollDeadline, false
	}
	if st := s.State(); setupDriving(st) {
		enter.Close(out)
		return setupResidencyBudget, true
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		took, note := enter.Poll()
		if note != "" && out != nil {
			writePrompt(out, note)
		}
		if took {
			s.TakeOver()
			return benchPollDeadline, false
		}
		time.Sleep(setupStatePollInterval)
		if st := s.State(); setupDriving(st) {
			// The point of no return (waired-agent#198): the operator has
			// confirmed their choices in the browser and the desired state
			// is written. Terminal takeover is no longer accepted, so the
			// offer is withdrawn here rather than left standing as
			// something that would silently fail.
			enter.Close(out)
			return setupResidencyBudget, true
		}
	}
	if out != nil {
		writePrompt(out, "No setup started in the browser; continuing here.")
	}
	// The grace expired with nothing driving, so this terminal is the
	// driver from here on — the same conclusion #309 already draws one
	// frame up when it swaps the takeover offer for the background one.
	// Claiming it is what says so to the daemon (waired-agent#646).
	//
	// It matters most on a device carrying a leftover instruction: with no
	// claim the daemon derives "the browser wrote desired state, so the
	// browser is driving", and the wizard then reports a browser session
	// nobody opened — over a setup this terminal is running
	// (waired-agent#645).
	s.TakeOver()
	return benchPollDeadline, false
}
