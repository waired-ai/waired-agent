package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// speedFakeDaemon serves /inference/status with a configurable snapshot
// and records the enable/disable writes step 6 may take.
type speedFakeDaemon struct {
	status     map[string]any
	noSpeed    bool
	enables    atomic.Int32
	disables   atomic.Int32
	remeasures atomic.Int32
	// noRemeasureRoute makes the daemon 404 the remeasure route, which is
	// what an older one does.
	noRemeasureRoute bool

	// The rest is what happens AFTER the daemon accepts the ask. The zero
	// value is a daemon that takes the fresh measurement and publishes it
	// before the next status read, which is the shape step 6's wait is
	// written against (waired-agent#703): a daemon that says it started
	// one and then never changes its figure is a daemon that did not.
	//
	// declineRemeasure answers {"started":false} — a fresh install, where
	// this process measured seconds ago.
	declineRemeasure bool
	// remeasureDelay is how many status reads serve the OLD figure before
	// the fresh one appears.
	remeasureDelay int32
	// remeasureStage is the host_speed_stage reported while the delay
	// runs. A give-up stage there is a measurement that stopped without
	// producing a figure.
	remeasureStage string
	// remeasureFresh is the whole status once the delay is up. nil keeps
	// the figure and moves measured_at, which is what a re-measurement of
	// an unchanged host produces.
	remeasureFresh map[string]any

	started atomic.Bool
	reads   atomic.Int32
}

const (
	speedMeasuredBefore = "2026-08-01T00:00:00Z"
	speedMeasuredAfter  = "2026-08-12T00:00:00Z"
)

func slowStatus(turn, budget float64, desired string, cutoffOff bool) map[string]any {
	return map[string]any{
		"subsystem_state": "ready",
		"desired_state":   desired,
		"host_speed": map[string]any{
			"turn_seconds":         turn,
			"budget_seconds":       budget,
			"turned_inference_off": cutoffOff,
			"measured_at":          speedMeasuredBefore,
		},
	}
}

// withMeasuredAt is the same status with the measurement stamped at a
// different moment — a re-measurement of a host that has not changed.
func withMeasuredAt(st map[string]any, at string) map[string]any {
	out := make(map[string]any, len(st)+1)
	for k, v := range st {
		out[k] = v
	}
	if hs, ok := st["host_speed"].(map[string]any); ok {
		fresh := make(map[string]any, len(hs))
		for k, v := range hs {
			fresh[k] = v
		}
		fresh["measured_at"] = at
		out["host_speed"] = fresh
	}
	return out
}

func (f *speedFakeDaemon) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/waired/v1/inference/status":
			st := f.status
			if f.noSpeed {
				st = map[string]any{"subsystem_state": "ready", "desired_state": "enabled"}
			}
			if f.started.Load() && !f.noSpeed {
				if f.reads.Add(1) > f.remeasureDelay {
					if f.remeasureFresh != nil {
						st = f.remeasureFresh
					} else {
						st = withMeasuredAt(st, speedMeasuredAfter)
					}
				} else if f.remeasureStage != "" {
					st = withMeasuredAt(st, speedMeasuredBefore)
					st["host_speed_stage"] = f.remeasureStage
				}
			}
			_ = json.NewEncoder(w).Encode(st)
		case "/waired/v1/inference/enable":
			f.enables.Add(1)
			_, _ = w.Write([]byte(`{}`))
		case "/waired/v1/inference/disable":
			f.disables.Add(1)
			_, _ = w.Write([]byte(`{}`))
		case "/waired/v1/inference/host-speed/remeasure":
			if f.noRemeasureRoute {
				http.NotFound(w, r)
				return
			}
			f.remeasures.Add(1)
			if f.declineRemeasure {
				_, _ = w.Write([]byte(`{"started":false}`))
				return
			}
			f.started.Store(true)
			_, _ = w.Write([]byte(`{"started":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func shrinkHostSpeedAsk(t *testing.T) {
	t.Helper()
	prevWait, prevPoll := hostSpeedAskWait, hostSpeedAskPoll
	hostSpeedAskWait, hostSpeedAskPoll = 50*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { hostSpeedAskWait, hostSpeedAskPoll = prevWait, prevPoll })
}

// setToggle marks the status as one somebody actually wrote, which is the
// distinction waired#1142 turns on.
func setToggle(st map[string]any) map[string]any {
	st["desired_state_set"] = true
	return st
}

func mine() bool { return true }

// TestConfirmHostSpeedBudget pins install-flow step 6 as the product
// contract from the 2026-08-08 owner rulings (waired-ai/waired#1067;
// waired-agent#585), with the copy owner-approved 2026-08-09 (this
// session): a measurement over budget asks, defaulting off; every other
// outcome changes nothing.
func TestConfirmHostSpeedBudget(t *testing.T) {
	boolp := func(b bool) *bool { return &b }

	t.Run("within budget stays silent", func(t *testing.T) {
		shrinkHostSpeedAsk(t)
		f := &speedFakeDaemon{status: slowStatus(30, 45, "enabled", false)}
		var out strings.Builder
		keptOn := confirmHostSpeedBudget(f.server(t).URL, daemonInitInference{}, false, eofLineReader(), &out, mine)
		if out.Len() != 0 || f.disables.Load() != 0 || f.enables.Load() != 0 {
			t.Fatalf("out=%q disables=%d enables=%d, want nothing", out.String(), f.disables.Load(), f.enables.Load())
		}
		if !keptOn {
			t.Errorf("within budget must report local AI kept on (#586 picker gate)")
		}
	})

	t.Run("over budget defaults to off", func(t *testing.T) {
		shrinkHostSpeedAsk(t)
		f := &speedFakeDaemon{status: slowStatus(68.4, 45, "enabled", false)}
		var out strings.Builder
		keptOn := confirmHostSpeedBudget(f.server(t).URL, daemonInitInference{}, false, eofLineReader(), &out, mine)
		if f.disables.Load() != 1 {
			t.Fatalf("disables = %d, want the default decline recorded", f.disables.Load())
		}
		if keptOn {
			t.Errorf("the default off must report keptOn=false — the model picker keys on it (#586)")
		}
		got := out.String()
		// Copy owner-approved 2026-08-09 (waired-agent#579), replacing
		// "This computer looks slow for everyday coding work: one coding
		// question takes about 68.4 s here (comfortable is <= 45 s)".
		// The figure and the standard it is judged against now sit on
		// their own rows, because an adjective on the figure inside a
		// sentence reads as a requirement floor.
		for _, want := range []string{
			"This computer is below the recommended spec for running AI locally.",
			"one coding question   68.4 s",
			"comfortable           45 s or less",
			"Running AI locally is not recommended here.",
			"Keep local inference on anyway?",
			"No turns local inference off — Waired still works as a gateway/relay.",
			"(default: No)",
			"Local inference disabled — Waired keeps working as a gateway/relay.",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("output missing %q:\n%s", want, got)
			}
		}
	})

	t.Run("keeping it on overturns the cutoff default", func(t *testing.T) {
		shrinkHostSpeedAsk(t)
		// The daemon already applied its silent default (desired off,
		// cutoff's own claim) — the whole point of asking is that now
		// there is somebody to ask.
		f := &speedFakeDaemon{status: slowStatus(68.4, 45, "disabled", true)}
		var out strings.Builder
		keptOn := confirmHostSpeedBudget(f.server(t).URL, daemonInitInference{}, false, linesOf("y\n"), &out, mine)
		if f.enables.Load() != 1 || f.disables.Load() != 0 {
			t.Fatalf("enables=%d disables=%d, want the default overturned once", f.enables.Load(), f.disables.Load())
		}
		if !keptOn {
			t.Errorf("Yes must report keptOn=true so the model picker still runs (#586)")
		}
	})

	t.Run("a person's own off is an answer, not a question", func(t *testing.T) {
		shrinkHostSpeedAsk(t)
		// desired off but NOT the cutoff's claim — `waired inference off`
		// or the step-4 decline. Asking would re-litigate their choice.
		f := &speedFakeDaemon{status: slowStatus(68.4, 45, "disabled", false)}
		var out strings.Builder
		keptOn := confirmHostSpeedBudget(f.server(t).URL, daemonInitInference{}, false, eofLineReader(), &out, mine)
		if out.Len() != 0 || f.disables.Load() != 0 || f.enables.Load() != 0 {
			t.Fatalf("out=%q, want nothing on a deliberate off", out.String())
		}
		if keptOn {
			t.Errorf("a deliberate off must report keptOn=false — no model question after it (#586)")
		}
	})

	t.Run("explicit --inference-enabled=true suppresses the ask", func(t *testing.T) {
		shrinkHostSpeedAsk(t)
		f := &speedFakeDaemon{status: slowStatus(68.4, 45, "enabled", false)}
		var out strings.Builder
		confirmHostSpeedBudget(f.server(t).URL, daemonInitInference{Enabled: boolp(true)}, false, eofLineReader(), &out, mine)
		if out.Len() != 0 || f.disables.Load() != 0 {
			t.Fatalf("out=%q disables=%d, want the forced arm silent", out.String(), f.disables.Load())
		}
	})

	t.Run("non-interactive applies the default with the reason", func(t *testing.T) {
		shrinkHostSpeedAsk(t)
		f := &speedFakeDaemon{status: slowStatus(68.4, 45, "enabled", false)}
		var out strings.Builder
		keptOn := confirmHostSpeedBudget(f.server(t).URL, daemonInitInference{}, true, eofLineReader(), &out, mine)
		if f.disables.Load() != 1 {
			t.Fatalf("disables = %d, want the non-interactive default", f.disables.Load())
		}
		if keptOn {
			t.Errorf("the non-interactive off must report keptOn=false (#586)")
		}
		for _, want := range []string{
			"This computer is below the recommended spec for running AI locally.",
			"one coding question   68.4 s",
			"comfortable           45 s or less",
			"Running AI locally is not recommended here. Non-interactive: turning local",
			"inference off. Re-enable with `waired inference on`.",
		} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("non-interactive note missing %q: %q", want, out.String())
			}
		}
	})

	// PRODUCT CONTRACT (waired#1142, owner ruling this session; the rule it
	// restores is #465 / waired#1056 — the daemon's cutoff stands down on any
	// written toggle, and until now this step could not tell one from the
	// live default). Non-interactive has nobody to ask, so it must not
	// overrule a choice somebody made.
	t.Run("non-interactive leaves a toggle somebody wrote alone", func(t *testing.T) {
		shrinkHostSpeedAsk(t)
		f := &speedFakeDaemon{status: setToggle(slowStatus(68.4, 45, "enabled", false))}
		var out strings.Builder
		keptOn := confirmHostSpeedBudget(f.server(t).URL, daemonInitInference{}, true, eofLineReader(), &out, mine)
		if f.disables.Load() != 0 {
			t.Fatalf("disables = %d, want none — the operator turned this on here", f.disables.Load())
		}
		if !keptOn {
			t.Error("a toggle left alone must report keptOn=true")
		}
		for _, want := range []string{
			"This computer is below the recommended spec for running AI locally.",
			"one coding question   68.4 s",
			"Running AI locally is not recommended here. Non-interactive: leaving local",
			"inference on, because it was turned on here. Turn it off with `waired inference off`.",
		} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("output missing %q: %q", want, out.String())
			}
		}
	})

	// The carve-out, and the reason the guard reads TurnedInferenceOff: the
	// cutoff's own silent default is a written toggle that nobody chose.
	// Treating it as an answer would make step 6 unable to do the one thing
	// it exists for.
	t.Run("the cutoff's own off is not somebody's answer", func(t *testing.T) {
		shrinkHostSpeedAsk(t)
		f := &speedFakeDaemon{status: setToggle(slowStatus(68.4, 45, "disabled", true))}
		var out strings.Builder
		keptOn := confirmHostSpeedBudget(f.server(t).URL, daemonInitInference{}, false, linesOf("y\n"), &out, mine)
		if f.enables.Load() != 1 {
			t.Fatalf("enables = %d, want the question asked and answered yes", f.enables.Load())
		}
		if !keptOn {
			t.Error("Yes must still report keptOn=true")
		}
	})

	// Interactive is unchanged by all of the above: a re-run replays the
	// whole install conversation, gates included (owner ruling 2026-08-09,
	// waired-agent#599), so a written "on" still gets asked about when there
	// is somebody there to ask.
	t.Run("interactive still asks even when the toggle was written", func(t *testing.T) {
		shrinkHostSpeedAsk(t)
		f := &speedFakeDaemon{status: setToggle(slowStatus(68.4, 45, "enabled", false))}
		var out strings.Builder
		confirmHostSpeedBudget(f.server(t).URL, daemonInitInference{}, false, linesOf("y\n"), &out, mine)
		if !strings.Contains(out.String(), "Keep local inference on anyway?") {
			t.Errorf("a re-run must still ask: %q", out.String())
		}
	})

	// waired-agent#599: the re-run asks for a fresh figure rather than
	// reading whatever the last boot left behind. waired#1140 is what
	// reading the leftovers costs.
	t.Run("step 6 asks for a fresh measurement first", func(t *testing.T) {
		shrinkHostSpeedAsk(t)
		f := &speedFakeDaemon{status: slowStatus(30, 45, "enabled", false)}
		var out strings.Builder
		confirmHostSpeedBudget(f.server(t).URL, daemonInitInference{}, false, eofLineReader(), &out, mine)
		if got := f.remeasures.Load(); got != 1 {
			t.Fatalf("remeasure requests = %d, want exactly 1", got)
		}
	})

	// An older daemon 404s the route, and the step behaves exactly as it did
	// before it existed.
	t.Run("an older daemon without the remeasure route still works", func(t *testing.T) {
		shrinkHostSpeedAsk(t)
		f := &speedFakeDaemon{
			status:           slowStatus(68.4, 45, "enabled", false),
			noRemeasureRoute: true,
		}
		var out strings.Builder
		keptOn := confirmHostSpeedBudget(f.server(t).URL, daemonInitInference{}, true, eofLineReader(), &out, mine)
		if f.disables.Load() != 1 || keptOn {
			t.Fatalf("disables=%d keptOn=%v, want the pre-#599 behaviour intact", f.disables.Load(), keptOn)
		}
	})

	// waired-agent#623: a one-shot line in front of a wait that ran 6m45s on
	// a real install cannot tell a working measurement from a hung one.
	t.Run("the wait keeps saying it is still going", func(t *testing.T) {
		shrinkHostSpeedAsk(t)
		prev := hostSpeedNarrateEvery
		hostSpeedNarrateEvery = 5 * time.Millisecond
		t.Cleanup(func() { hostSpeedNarrateEvery = prev })

		f := &speedFakeDaemon{noSpeed: true}
		var out strings.Builder
		confirmHostSpeedBudget(f.server(t).URL, daemonInitInference{}, false, eofLineReader(), &out, mine)

		got := out.String()
		if !strings.Contains(got, "Measuring how fast this computer runs AI") {
			t.Fatalf("the wait must still say what it is waiting for: %q", got)
		}
		if n := strings.Count(got, "still measuring — "); n < 2 {
			t.Errorf("progress lines = %d, want the wait to keep reporting: %q", n, got)
		}
	})

	t.Run("no measurement in time gives up silently", func(t *testing.T) {
		shrinkHostSpeedAsk(t)
		f := &speedFakeDaemon{noSpeed: true}
		var out strings.Builder
		confirmHostSpeedBudget(f.server(t).URL, daemonInitInference{}, false, eofLineReader(), &out, mine)
		if f.disables.Load() != 0 {
			t.Fatalf("disables = %d, want none on timeout", f.disables.Load())
		}
		if !strings.Contains(out.String(), "Measuring how fast this computer runs AI") {
			t.Errorf("the wait must say what it is waiting for (#939): %q", out.String())
		}
	})

	t.Run("a browser setup that started owns the question", func(t *testing.T) {
		shrinkHostSpeedAsk(t)
		f := &speedFakeDaemon{status: slowStatus(68.4, 45, "enabled", false)}
		var out strings.Builder
		confirmHostSpeedBudget(f.server(t).URL, daemonInitInference{}, false, eofLineReader(), &out,
			func() bool { return false })
		if strings.Contains(out.String(), "Keep local inference on anyway?") || f.disables.Load() != 0 {
			t.Fatalf("the terminal asked a question the wizard owns: %q", out.String())
		}
	})

	t.Run("an older daemon with no budget figure never misreads over-budget", func(t *testing.T) {
		shrinkHostSpeedAsk(t)
		f := &speedFakeDaemon{status: slowStatus(68.4, 0, "enabled", false)}
		var out strings.Builder
		confirmHostSpeedBudget(f.server(t).URL, daemonInitInference{}, false, eofLineReader(), &out, mine)
		if out.Len() != 0 || f.disables.Load() != 0 {
			t.Fatalf("out=%q, want nothing without a budget to compare against", out.String())
		}
	})
}

// TestConfirmHostSpeedBudget_WaitsForTheFigureItAskedFor pins the second
// half of the re-run ruling.
//
// PRODUCT CONTRACT (owner ruling 2026-08-09, waired-agent#599 — a re-run
// replays the install conversation, benchmarks and gates included). Asking
// for a fresh measurement and then judging the previous one replays
// nothing, and it is how sv-xps15 came to gate on a 12.017 s figure while
// a 39.473 s one landed 44 s later — with the two measurements running at
// once, which is the contention waired-agent#703 is about.
func TestConfirmHostSpeedBudget_WaitsForTheFigureItAskedFor(t *testing.T) {
	t.Run("a stale over-budget figure does not decide while a fresh one is coming", func(t *testing.T) {
		shrinkHostSpeedAsk(t)
		// Over budget before, comfortably within it after — the direction
		// that matters, because deciding on the stale one turns local AI
		// off on a host that is fine.
		f := &speedFakeDaemon{
			status:         slowStatus(68.4, 45, "enabled", false),
			remeasureDelay: 2,
			remeasureFresh: withMeasuredAt(slowStatus(12.0, 45, "enabled", false), speedMeasuredAfter),
		}
		var out strings.Builder
		keptOn := confirmHostSpeedBudget(f.server(t).URL, daemonInitInference{}, false, eofLineReader(), &out, mine)

		if !keptOn || f.disables.Load() != 0 {
			t.Fatalf("keptOn=%v disables=%d — the stale figure decided: %q",
				keptOn, f.disables.Load(), out.String())
		}
		if strings.Contains(out.String(), hostSpeedBelowSpecLine) {
			t.Errorf("announced the stale verdict: %q", out.String())
		}
		if f.remeasures.Load() != 1 {
			t.Errorf("remeasure asked %d times, want exactly 1", f.remeasures.Load())
		}
	})

	t.Run("a declined remeasure decides on what is already there", func(t *testing.T) {
		shrinkHostSpeedAsk(t)
		// The fresh-install shape: the bootstrap measured seconds ago, so
		// the daemon declines and this figure IS this install's own.
		f := &speedFakeDaemon{
			status:           slowStatus(68.4, 45, "enabled", false),
			declineRemeasure: true,
		}
		var out strings.Builder
		keptOn := confirmHostSpeedBudget(f.server(t).URL, daemonInitInference{}, true, eofLineReader(), &out, mine)

		if keptOn || f.disables.Load() != 1 {
			t.Fatalf("keptOn=%v disables=%d, want the over-budget default applied at once: %q",
				keptOn, f.disables.Load(), out.String())
		}
		if strings.Contains(out.String(), "still measuring") {
			t.Errorf("waited for a measurement nobody started: %q", out.String())
		}
	})

	t.Run("a measurement that gives up ends the wait", func(t *testing.T) {
		shrinkHostSpeedAsk(t)
		// Started, then discarded — the reading was taken while this host
		// was serving, or the probe model would not download. There is no
		// fresh figure coming, and spending the rest of the budget in
		// silence is what the stage exists to prevent.
		f := &speedFakeDaemon{
			status:         slowStatus(68.4, 45, "enabled", false),
			remeasureDelay: 1 << 30, // never publishes
			remeasureStage: "measure_failed",
		}
		var out strings.Builder
		keptOn := confirmHostSpeedBudget(f.server(t).URL, daemonInitInference{}, true, eofLineReader(), &out, mine)

		// Counted rather than timed: the budget is a wall-clock bound and
		// this assertion has to hold on a loaded CI runner too. One read
		// after the ask is enough to see the stage.
		if reads := f.reads.Load(); reads > 3 {
			t.Errorf("polled %d times for a measurement that had stopped; the whole budget is %d polls",
				reads, int(hostSpeedAskWait/hostSpeedAskPoll))
		}
		if keptOn || f.disables.Load() != 1 {
			t.Fatalf("keptOn=%v disables=%d, want the stored figure judged: %q",
				keptOn, f.disables.Load(), out.String())
		}
	})

	t.Run("an older daemon that cannot remeasure behaves as it did", func(t *testing.T) {
		shrinkHostSpeedAsk(t)
		f := &speedFakeDaemon{
			status:           slowStatus(68.4, 45, "enabled", false),
			noRemeasureRoute: true,
		}
		var out strings.Builder
		keptOn := confirmHostSpeedBudget(f.server(t).URL, daemonInitInference{}, true, eofLineReader(), &out, mine)

		if keptOn || f.disables.Load() != 1 {
			t.Fatalf("keptOn=%v disables=%d, want the pre-#599 behaviour on a 404: %q",
				keptOn, f.disables.Load(), out.String())
		}
	})
}
