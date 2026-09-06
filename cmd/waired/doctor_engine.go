package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/waired-ai/waired-agent/internal/management"
)

// engineDoctor is what one observability probe learned about the serving
// engine beyond the line it prints: whether `waired doctor --fix` can do
// anything about it, and the reason it would be acting on.
//
// Shaped like trayDoctor, and for the same reason: the probe happens once per
// run, and the decision it feeds is a pure function so the matrix is
// table-tested without a daemon (CLAUDE.md §Test discipline).
type engineDoctor struct {
	Repair bool
	Reason string
}

// engineRepair decides whether `--fix` should ask the daemon to start the
// engine.
//
// This is the half waired-agent#1170 found missing. The browser wizard tells
// an operator whose engine will not start to run `sudo waired doctor --fix`
// (the `engine_install:engine_not_ready` pair in the console's setup copy),
// and `--fix` ran `waired link all` and reported Done while the engine state
// did not change — because the fix plan had no engine arm at all. Every other
// surface in this repo points at `waired inference engine start`, which is
// exactly the request this repair makes.
//
// The gate is "not running AND the daemon has a stated reason". A hard stop —
// `waired inference engine stop` — frees the memory and records no fault, and
// starting the engine again would undo an operator action nobody asked us to
// undo. A boot still in progress looks the same and wants the same answer:
// wait. An engine that HAS a reason is one where waiting has been tried.
func engineRepair(a management.AgentState) engineDoctor {
	switch {
	case a.Paused:
		// `waired resume` is the finding's own advice and the only thing
		// that helps; an engine start would not lift the pause.
		return engineDoctor{}
	case a.LocalInferenceState == localInferenceDisabled:
		// A setting, not a fault (#465). engineFinding says so at StatusOK.
		return engineDoctor{}
	case a.EngineReady:
		return engineDoctor{}
	case a.EngineFailureReason == "":
		return engineDoctor{}
	}
	return engineDoctor{Repair: true, Reason: a.EngineFailureReason}
}

// repairEngine asks the daemon to start the serving engine — the same request
// `waired inference engine start` and the tray's Start row make.
//
// It reports what happened rather than claiming success. A 409 is the daemon
// refusing on purpose and naming why (local inference off, no venv, no
// vLLM-capable model, weights still downloading); printing that sentence is
// the whole point of running a repair the wizard sent someone to.
//
// No context parameter: httpPostWithin carries engineWriteTimeout, which is
// the budget an engine transition is bounded by everywhere else in this repo.
func repairEngine(mgmtURL, reason string, out io.Writer) error {
	_, _ = fmt.Fprintln(out, "Starting the inference engine...")
	if reason != "" {
		_, _ = fmt.Fprintf(out, "  It isn't running because: %s\n", reason)
	}
	_, err := httpPostWithin(mgmtURL+"/waired/v1/inference/engine/start", nil, engineWriteTimeout)
	if err == nil {
		// Accepted, not started: a cold vLLM engine takes minutes to load, so
		// the daemon dispatches and answers. Saying "started" here would be
		// the same lie this repair exists to stop telling.
		_, _ = fmt.Fprintln(out, "  Asked. `waired inference engine status` reports when it is running.")
		return nil
	}
	var st *mgmtStatusError
	if errors.As(err, &st) && st.StatusCode == http.StatusConflict {
		// The daemon's message already opens with the sentinel's own text,
		// and "declined to start the engine: engine start refused: …" reads
		// as two people saying the same thing. Trimmed against the exported
		// sentinel rather than a literal, so it cannot drift from it.
		msg := strings.TrimPrefix(parseMgmtError(st.StatusCode, []byte(st.Message)).Message,
			management.ErrEngineStartRefused.Error()+": ")
		return fmt.Errorf("the inference engine was not started: %s", msg)
	}
	return fmt.Errorf("asking the daemon to start the engine: %w", err)
}
