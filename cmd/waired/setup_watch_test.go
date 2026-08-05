package main

import (
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// scriptedState is the seam the setup watch is built on: the states the
// daemon reports, one per read, the last repeating. It records the reads
// so a test can pin the throttle rather than the wall clock.
type scriptedState struct {
	states []management.SetupStateResponse
	reads  int
}

func (s *scriptedState) next() management.SetupStateResponse {
	st := s.states[min(s.reads, len(s.states)-1)]
	s.reads++
	return st
}

func activeState() management.SetupStateResponse {
	return management.SetupStateResponse{Active: true, DesiredEngine: "ollama", EngineInstalled: true}
}

// newScriptedWatch arms a watch over the scripted states with the throttle
// off, so a test drives reads rather than seconds. It must be off rather
// than merely small: a sub-tick interval reads as "throttled" wherever the
// clock's resolution exceeds it, which on Windows swallowed consecutive
// polls and made the edge unobservable.
func newScriptedWatch(t *testing.T, s *scriptedState) *setupWatch {
	t.Helper()
	return &setupWatch{state: s.next, every: 0}
}

// TestSetupWatchReportsTheEdgeOnce is the #308 contract: a browser setup
// that starts AFTER awaitSetupBudget's grace expired must be reported —
// once, so the terminal narrates the handoff a single time.
func TestSetupWatchReportsTheEdgeOnce(t *testing.T) {
	s := &scriptedState{states: []management.SetupStateResponse{{}, {}, activeState()}}
	w := newScriptedWatch(t, s)

	for i := 0; i < 2; i++ {
		if started, _, _ := w.Poll(); started {
			t.Fatalf("watch reported a setup start on read %d, before one existed", i)
		}
		if w.Started() {
			t.Fatalf("Started latched on read %d, before one existed", i)
		}
	}
	started, budget, engineComing := w.Poll()
	if !started {
		t.Fatal("watch missed the browser setup starting")
	}
	if budget != setupResidencyBudget {
		t.Errorf("budget = %v, want the residency budget %v", budget, setupResidencyBudget)
	}
	if engineComing {
		t.Error("engineComing is true for a state whose engine is already installed")
	}
	if !w.Started() {
		t.Error("Started did not latch after the edge")
	}
	// Latched: the caller has already closed the takeover offer and moved
	// to the residency budget, and saying it twice would print the
	// handoff line again.
	if again, _, _ := w.Poll(); again {
		t.Error("watch reported the same setup start twice")
	}
	if reads := s.reads; reads != 3 {
		t.Errorf("watch kept polling the daemon after the edge (%d reads, want 3)", reads)
	}
}

// TestSetupWatchReportsEngineArrival pins that the edge carries the same
// engineComing the caller would have computed itself, from the same read.
func TestSetupWatchReportsEngineArrival(t *testing.T) {
	cases := []struct {
		name string
		st   management.SetupStateResponse
		want bool
	}{
		{"wizard has not picked an engine yet",
			management.SetupStateResponse{Active: true}, true},
		{"an install claim is live",
			management.SetupStateResponse{Active: true, DesiredEngine: "ollama", EngineInstalled: true, InstallClaimed: "other"}, true},
		{"desired engine is not in place",
			management.SetupStateResponse{Active: true, DesiredEngine: "ollama"}, true},
		{"engine already installed",
			management.SetupStateResponse{Active: true, DesiredEngine: "ollama", EngineInstalled: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &scriptedState{states: []management.SetupStateResponse{tc.st}}
			w := newScriptedWatch(t, s)
			started, _, engineComing := w.Poll()
			if !started {
				t.Fatal("watch missed the browser setup starting")
			}
			if engineComing != tc.want {
				t.Errorf("engineComing = %v, want %v (engineArrivalPending)", engineComing, tc.want)
			}
			if engineComing != engineArrivalPending(tc.st) {
				t.Errorf("engineComing disagrees with engineArrivalPending(%+v)", tc.st)
			}
		})
	}
}

// TestSetupWatchIgnoresLeftoverDesiredState: the watch exists to catch a
// browser setup starting mid-wait, and desired state that was already on
// the device before this run started is not one (#308). Without this the
// #308 fix would just move the false handoff from the grace into the
// model wait.
func TestSetupWatchIgnoresLeftoverDesiredState(t *testing.T) {
	stale := activeState()
	stale.DesiredStale = true
	s := &scriptedState{states: []management.SetupStateResponse{stale}}
	w := newScriptedWatch(t, s)

	for i := 0; i < 3; i++ {
		if started, _, _ := w.Poll(); started {
			t.Fatal("the watch reported leftover desired state as a browser setup starting")
		}
	}
	if w.Started() {
		t.Error("Started latched on leftover desired state")
	}
}

// TestSetupWatchThrottlesTheDaemon: the wait it lives in ticks once a
// second, and every read is a loopback HTTP round trip.
func TestSetupWatchThrottlesTheDaemon(t *testing.T) {
	s := &scriptedState{states: []management.SetupStateResponse{{}}}
	w := &setupWatch{state: s.next, every: time.Hour}
	for i := 0; i < 10; i++ {
		w.Poll()
	}
	if s.reads != 1 {
		t.Errorf("watch read the daemon %d times in one throttle window, want 1", s.reads)
	}
}

// TestSetupWatchInertShapes pins that every path which is byte-identical
// today stays byte-identical, and issues no /setup/state request at all:
// a nil watch, an older daemon (unsupported session), and a setup that was
// already driving before the wait began.
func TestSetupWatchInertShapes(t *testing.T) {
	var nilWatch *setupWatch
	if started, _, _ := nilWatch.Poll(); started {
		t.Error("a nil watch reported a setup start")
	}
	if nilWatch.Started() {
		t.Error("a nil watch latched Started")
	}

	s := &scriptedState{states: []management.SetupStateResponse{activeState()}}
	// alreadyActive: awaitSetupBudget saw the setup within its grace, so
	// the caller is already in browser-driven mode and there is no edge
	// left to observe.
	w := &setupWatch{state: s.next, alreadyActive: true}
	if started, _, _ := w.Poll(); started {
		t.Error("watch reported an edge for a setup that was already active")
	}
	if s.reads != 0 {
		t.Errorf("an inert watch polled the daemon %d times", s.reads)
	}

	// An unsupported session (a daemon older than the executor routes)
	// yields the same inert watch.
	if w := newSetupWatch(nil, false); w.state != nil {
		t.Error("newSetupWatch armed a watch over an unsupported session")
	}
	if started, _, _ := newSetupWatch(nil, false).Poll(); started {
		t.Error("an unsupported session reported a setup start")
	}
}

// newScriptedTarget arms a target over the scripted states with the
// throttle off, so a test drives reads rather than seconds — the same
// reasoning as newScriptedWatch, and the same trap avoided: setBenchTiming
// does not shrink setupStatePollInterval, so a target built by
// newModelTarget would read once in a millisecond-paced test and never
// again.
func newScriptedTarget(t *testing.T, s *scriptedState) *modelTarget {
	t.Helper()
	return &modelTarget{state: s.next, every: 0}
}

// wizardState is a live browser setup that has named a model.
func wizardState(model string) management.SetupStateResponse {
	st := activeState()
	st.DesiredModelID = model
	return st
}

// TestModelTargetIgnoresLeftoverDesiredState is the #308 half of the
// contract: the control plane never clears desired_model_id, so a device
// set up once carries an instruction for the rest of its life. Keying a
// wait on that would make a second `waired init` wait for a model chosen
// weeks ago.
func TestModelTargetIgnoresLeftoverDesiredState(t *testing.T) {
	s := &scriptedState{states: []management.SetupStateResponse{
		{Active: true, DesiredStale: true, DesiredModelID: "chosen-last-month"},
	}}
	target := newScriptedTarget(t, s)
	for i := 0; i < 3; i++ {
		if got := target.Poll(); got != "" {
			t.Fatalf("read %d: target = %q for a leftover instruction, want none", i, got)
		}
	}
}

// TestModelTargetKeepsTheTargetOnceLatched is the rule that keeps the fix
// working for the length of a real download. Every way setupDriving can go
// false mid-wait means something other than "the wizard left": the daemon's
// freshness window is 60 minutes against an 8-hour residency budget, a
// daemon that restarts reports stale permanently, and an unreachable one
// answers with the zero value. Clearing on any of those would silently
// revert the wait to reporting the agent's own model — the bug.
func TestModelTargetKeepsTheTargetOnceLatched(t *testing.T) {
	stale := wizardState("wizard-35b")
	stale.DesiredStale = true
	s := &scriptedState{states: []management.SetupStateResponse{
		wizardState("wizard-35b"),
		stale,                              // the freshness window elapsed, or the daemon restarted
		{},                                 // one unreachable read
		{Active: true, DesiredModelID: ""}, // driving again, but between instructions
	}}
	target := newScriptedTarget(t, s)
	for i := 0; i < 4; i++ {
		if got := target.Poll(); got != "wizard-35b" {
			t.Fatalf("read %d: target = %q, want it held at wizard-35b", i, got)
		}
	}
}

// TestModelTargetFollowsAChangedChoice pins the present tense: an operator
// who went back in the wizard and picked another model must not be shown a
// bar for the one they abandoned, because the daemon has already stopped
// fetching it.
func TestModelTargetFollowsAChangedChoice(t *testing.T) {
	s := &scriptedState{states: []management.SetupStateResponse{
		wizardState("first-choice"),
		wizardState("second-choice"),
	}}
	target := newScriptedTarget(t, s)
	if got := target.Poll(); got != "first-choice" {
		t.Fatalf("target = %q, want first-choice", got)
	}
	if got := target.Poll(); got != "second-choice" {
		t.Fatalf("target = %q after the operator changed their mind, want second-choice", got)
	}
}

// TestModelTargetThrottlesTheDaemon pins both halves of the throttle: it
// reads at most once per window, AND it keeps answering with the latched
// target in between. Returning "" between reads would flap the wait
// between keyed and unkeyed on most of its ticks.
func TestModelTargetThrottlesTheDaemon(t *testing.T) {
	s := &scriptedState{states: []management.SetupStateResponse{wizardState("wizard-35b")}}
	target := &modelTarget{state: s.next, every: time.Hour}
	for i := 0; i < 10; i++ {
		if got := target.Poll(); got != "wizard-35b" {
			t.Fatalf("poll %d: target = %q, want wizard-35b on every tick", i, got)
		}
	}
	if s.reads != 1 {
		t.Errorf("target read the daemon %d times in one throttle window, want 1", s.reads)
	}
}

// TestModelTargetBacksOffOnceLatched: before a target exists the read has
// to be prompt, because the wait cannot report the right model until it
// has one. Afterwards the only thing left to notice is an operator
// changing their mind, so the reads back off — at the unlatched cadence a
// wizard's 8-hour budget would be ~14k loopback GETs, each one synchronous
// inside a loop that ticks once a second.
func TestModelTargetBacksOffOnceLatched(t *testing.T) {
	s := &scriptedState{states: []management.SetupStateResponse{{}, wizardState("wizard-35b")}}
	target := &modelTarget{state: s.next, every: time.Minute}

	before := time.Now()
	target.Poll() // nothing named yet
	if gap := target.next.Sub(before); gap > 2*time.Minute {
		t.Errorf("unlatched read scheduled %v out; it must stay prompt until a target exists", gap)
	}

	target.next = time.Time{} // let the second read through
	before = time.Now()
	if got := target.Poll(); got != "wizard-35b" {
		t.Fatalf("target = %q, want wizard-35b", got)
	}
	if gap := target.next.Sub(before); gap < time.Duration(targetLatchedBackoff)*time.Minute {
		t.Errorf("latched read scheduled only %v out, want the backed-off interval", gap)
	}
}

// TestModelTargetInertShapes pins that every path which is byte-identical
// today stays byte-identical and issues no /setup/state request: a nil
// target, and an older daemon (unsupported session).
func TestModelTargetInertShapes(t *testing.T) {
	var nilTarget *modelTarget
	if got := nilTarget.Poll(); got != "" {
		t.Errorf("a nil target returned %q", got)
	}

	if target := newModelTarget(nil); target.state != nil {
		t.Error("newModelTarget armed a target over an unsupported session")
	}
	s := &scriptedState{states: []management.SetupStateResponse{wizardState("wizard-35b")}}
	inert := &modelTarget{}
	if got := inert.Poll(); got != "" {
		t.Errorf("an inert target returned %q", got)
	}
	if s.reads != 0 {
		t.Errorf("an inert target polled the daemon %d times", s.reads)
	}
}

// TestModelTargetResolvesAnAliasToTheCatalogID pins the id-space bridge.
// The two ends of the comparison the wait makes are resolved differently:
// PullModel keys the daemon's model state by manifest.ModelID after a
// LookupByAlias, while desired_model_id reaches the CLI without ever being
// resolved. An unresolved alias would never appear in models.ready, so the
// wait would run to its grace on a host that is downloading perfectly.
//
// Product contract, using a real shipped alias so a catalog change that
// dropped alias support would fail here rather than silently.
func TestModelTargetResolvesAnAliasToTheCatalogID(t *testing.T) {
	const alias, canonical = "qwen2.5-coder-14b", "qwen2.5-coder-14b-instruct"
	s := &scriptedState{states: []management.SetupStateResponse{wizardState(alias)}}
	if got := newScriptedTarget(t, s).Poll(); got != canonical {
		t.Errorf("target = %q for alias %q, want the catalog id %q", got, alias, canonical)
	}
}

// TestModelTargetPassesThroughAnUnknownID: an id the embedded catalog
// cannot resolve is kept rather than dropped. The CLI and the daemon ship
// the same catalog, so an id this build cannot resolve is one it cannot
// pull either — the honest answer is the wait's bounded grace saying so,
// not silently reverting to reporting the agent's own model.
func TestModelTargetPassesThroughAnUnknownID(t *testing.T) {
	s := &scriptedState{states: []management.SetupStateResponse{wizardState("model-from-a-newer-catalog")}}
	if got := newScriptedTarget(t, s).Poll(); got != "model-from-a-newer-catalog" {
		t.Errorf("target = %q, want the unknown id kept verbatim", got)
	}
}

// TestNewModelTargetReadsTheSessionsDesiredModel is the only test that
// carries desired_model_id over the real wire: through the daemon's
// /setup/state handler, an actual executorSession, and newModelTarget.
// Before this change `git grep DesiredModelID -- cmd/waired/` was empty, so
// nothing else covers the field being read at all.
func TestNewModelTargetReadsTheSessionsDesiredModel(t *testing.T) {
	shrinkSetupTimers(t)
	d := &fakeSetupDaemon{}
	d.setState(wizardState("wizard-35b"))
	srv := d.server(t)
	defer srv.Close()

	sess := attachSetupExecutor(srv.URL, false)
	defer sess.Release()
	if !sess.Supported() {
		t.Fatal("the fake daemon speaks the executor routes; the session should be supported")
	}
	if got := newModelTarget(sess).Poll(); got != "wizard-35b" {
		t.Errorf("target = %q over the real wire, want wizard-35b", got)
	}
}

// refusedState is a live wizard whose chosen model the daemon has
// refused to apply (waired-agent#404).
func refusedState(model, code, detail string) management.SetupStateResponse {
	st := wizardState(model)
	st.ModelState = "not_present"
	st.ModelErrorCode = code
	st.ModelErrorDetail = detail
	return st
}

// TestModelTargetReportsARefusal is #404's CLI-side bar. The refusal
// rides the read that already names the model, so learning about it costs
// no extra request.
func TestModelTargetReportsARefusal(t *testing.T) {
	s := &scriptedState{states: []management.SetupStateResponse{
		refusedState("wizard-35b", signer.SetupErrorEngineNotReady, "engine 0.24.0 is too old"),
	}}
	target := newScriptedTarget(t, s)
	if got := target.Poll(); got != "wizard-35b" {
		t.Fatalf("target = %q, want wizard-35b", got)
	}
	code, detail, ok := target.Refused()
	if !ok {
		t.Fatal("a recorded refusal was not reported")
	}
	if code != signer.SetupErrorEngineNotReady || detail != "engine 0.24.0 is too old" {
		t.Errorf("Refused() = (%q, %q), want the daemon's code and words", code, detail)
	}
}

// A daemon that reports no refusal — including one too old to carry the
// field at all — must leave the caller on its previous behaviour rather
// than on an assumption.
func TestModelTargetReportsNoRefusalWhenThereIsNone(t *testing.T) {
	s := &scriptedState{states: []management.SetupStateResponse{wizardState("wizard-35b")}}
	target := newScriptedTarget(t, s)
	target.Poll()
	if _, _, ok := target.Refused(); ok {
		t.Error("a healthy instruction reported a refusal")
	}
	var nilTarget *modelTarget
	if _, _, ok := nilTarget.Refused(); ok {
		t.Error("a nil target reported a refusal")
	}
}

// The refusal belongs to one desired model. An operator who picks again
// must not be shown the abandoned model's answer — the daemon drops its
// own record on the same event, and this latch has to follow.
func TestModelTargetDropsARefusalWhenTheChoiceChanges(t *testing.T) {
	s := &scriptedState{states: []management.SetupStateResponse{
		refusedState("first-choice", signer.SetupErrorEngineNotReady, "engine 0.24.0 is too old"),
		wizardState("second-choice"),
	}}
	target := newScriptedTarget(t, s)
	target.Poll()
	if _, _, ok := target.Refused(); !ok {
		t.Fatal("the first choice's refusal was not latched")
	}
	if got := target.Poll(); got != "second-choice" {
		t.Fatalf("target = %q, want second-choice", got)
	}
	if code, _, ok := target.Refused(); ok {
		t.Errorf("the abandoned model's refusal survived the new choice: %q", code)
	}
}
