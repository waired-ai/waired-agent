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
	status   map[string]any
	noSpeed  bool
	enables  atomic.Int32
	disables atomic.Int32
}

func slowStatus(turn, budget float64, desired string, cutoffOff bool) map[string]any {
	return map[string]any{
		"subsystem_state": "ready",
		"desired_state":   desired,
		"host_speed": map[string]any{
			"turn_seconds":         turn,
			"budget_seconds":       budget,
			"turned_inference_off": cutoffOff,
		},
	}
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
			_ = json.NewEncoder(w).Encode(st)
		case "/waired/v1/inference/enable":
			f.enables.Add(1)
			_, _ = w.Write([]byte(`{}`))
		case "/waired/v1/inference/disable":
			f.disables.Add(1)
			_, _ = w.Write([]byte(`{}`))
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
		for _, want := range []string{
			"slow for everyday coding work",
			"one coding question takes about 68.4 s here (comfortable is ≤ 45 s)",
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
		if !strings.Contains(out.String(), "Non-interactive: turning local inference off") ||
			!strings.Contains(out.String(), "`waired inference on`") {
			t.Errorf("non-interactive note missing: %q", out.String())
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
