package main

import (
	"github.com/waired-ai/waired-agent/internal/management"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// The engine installer already produces everything the browser wizard
// needs — `1.4 GB / 1.4 GB (72.8 MB/s)` is on the operator's terminal
// while the wizard shows "Working on it…" — and the only thing missing
// was a second consumer for it. This file is that consumer
// (waired-agent#197).

// ollamaProgressStage reports which §7 row an OllamaInstallProgress
// event belongs to. The installer's stages are its own vocabulary
// (download / download-rocm / extract / verify / …); the wizard's rows
// are the settled five. Only the transfer stages are byte-denominated,
// and everything after them is the install proper.
func ollamaProgressStage(stage string) string {
	switch stage {
	case "download", "download-rocm":
		return management.SetupStepEngineDownload
	default:
		return management.SetupStepEngineInstall
	}
}

// newExecutorProgressSink returns an OllamaInstaller progress callback
// that republishes the installer's own figures to the daemon, so the
// browser wizard draws the same download the terminal is drawing.
//
// Returns nil for an inert session (no daemon, or one predating the
// executor routes) so callers can pass the result straight through: a nil
// callback is what "render to stdout only" already looks like everywhere
// else in the installer.
func newExecutorProgressSink(s *executorSession, engine string) func(infruntime.OllamaInstallProgress) {
	if !s.Supported() {
		return nil
	}
	return func(p infruntime.OllamaInstallProgress) {
		step := ollamaProgressStage(p.Stage)
		if step == management.SetupStepEngineDownload && p.Total == 0 && p.Completed == 0 {
			// The stage-opening event, which carries the URL rather than
			// any bytes. Reporting it would open the row with a bar of
			// unknown size that the very next event replaces.
			return
		}
		s.Progress(step, engine, p.Completed, p.Total, wireRateBps(p.BytesPerSec))
	}
}

// wireRateBps translates the installer's rate sentinel to the wire's.
// The renderer uses -1 for "no sample yet" and 0 for "stalled"; the wire
// has one value for both (`omitempty` collapses 0 into absent) and the
// control plane rejects a negative outright. A stall is derived from the
// byte counters not advancing, so nothing is lost by flattening them.
func wireRateBps(bps int64) int64 {
	if bps < 0 {
		return 0
	}
	return bps
}

// vllmProgressStage reports which §7 row an InstallProgress event
// belongs to. Same shape as ollamaProgressStage one engine over: the
// installer's stages are its own vocabulary (resolve-uv / create-venv /
// pip-install / verify / activate), the wizard's rows are the settled
// five, and only the stage that transfers anything is byte-denominated.
//
// pip-install is ~90% of the wall clock and all ~4 GB of the download;
// the four stages around it are a uv resolve, a venv creation, a CUDA
// import check and a symlink swap (waired-agent#255).
func vllmProgressStage(stage infruntime.InstallStage) string {
	if stage == infruntime.StagePipInstall {
		return management.SetupStepEngineDownload
	}
	return management.SetupStepEngineInstall
}

// newVLLMProgressSink returns a VLLMInstaller progress callback that
// republishes the installer's figures to the daemon, so the browser
// wizard draws the same download the terminal is drawing.
//
// Returns nil for an inert session, exactly as newExecutorProgressSink
// does — a nil callback is what "render to stdout only" already looks
// like everywhere else in the installer.
func newVLLMProgressSink(s *executorSession, engine string) func(infruntime.InstallProgress) {
	if !s.Supported() {
		return nil
	}
	return func(p infruntime.InstallProgress) {
		step := vllmProgressStage(p.Stage)
		if step == management.SetupStepEngineDownload && p.TotalBytes == 0 {
			// pip-install has started but uv has not announced a download
			// yet (it resolves first, and a fully cached host announces
			// nothing at all). Reporting it would open the row with a bar
			// of unknown size, which is the same thing the ollama sink
			// refuses to do for the stage-opening event.
			return
		}
		s.Progress(step, engine, p.CompletedBytes, p.TotalBytes, wireRateBps(p.BytesPerSec))
	}
}

// teeProgress fans one installer callback out to several, skipping nil
// ones. The stdout renderer and the daemon sink are peers: neither may
// suppress the other, and the terminal must keep its bar even on a host
// where the daemon routes are absent.
//
// Generic over the event type because the two installers report
// different shapes — ollama is byte-denominated end to end, vLLM is
// staged with one byte-denominated stage — and the fan-out is the same
// either way.
func teeProgress[T any](fns ...func(T)) func(T) {
	live := make([]func(T), 0, len(fns))
	for _, f := range fns {
		if f != nil {
			live = append(live, f)
		}
	}
	if len(live) == 0 {
		return nil
	}
	if len(live) == 1 {
		return live[0]
	}
	return func(p T) {
		for _, f := range live {
			f(p)
		}
	}
}

// teeOllamaProgress is teeProgress at the ollama event type. Kept as a
// named function so the three per-OS installers keep reading as prose.
func teeOllamaProgress(fns ...func(infruntime.OllamaInstallProgress)) func(infruntime.OllamaInstallProgress) {
	return teeProgress(fns...)
}
