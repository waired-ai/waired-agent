package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

func convergeLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// The daemon's job here is to run the policy and SAY what happened. The
// three outcomes have to be distinguishable in the log, because this runs
// where nobody is watching and the log is the only account of it (#826).
func TestConvergeBundledEngine_SaysWhatItDid(t *testing.T) {
	cases := []struct {
		name        string
		present     bool
		version     string
		installErr  error
		wantLog     string
		wantLevel   string
		wantInstall bool
	}{
		{
			name: "absent", present: false,
			wantLog: "needs no converge", wantLevel: "DEBUG",
		},
		{
			name: "already at the pin", present: true, version: infruntime.OllamaPinnedVersion,
			wantLog: "needs no converge", wantLevel: "DEBUG",
		},
		{
			name: "stale", present: true, version: "0.1.0",
			wantLog: "converged to the pin", wantLevel: "INFO", wantInstall: true,
		},
		{
			name: "stale and the download fails", present: true, version: "0.1.0",
			installErr: errors.New("dial tcp: no route to host"),
			wantLog:    "converge failed", wantLevel: "WARN", wantInstall: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			installed := false
			var buf bytes.Buffer
			convergeBundledEngine(context.Background(), convergeLogger(&buf), infruntime.OllamaConvergeDeps{
				Present:    func() bool { return c.present },
				BinaryPath: func() string { return "/state/runtimes/ollama/bin/ollama" },
				Probe: func(context.Context, string) (bool, string) {
					return c.present, c.version
				},
				Install: func(context.Context) error {
					installed = true
					return c.installErr
				},
			})
			out := buf.String()
			if !strings.Contains(out, c.wantLog) {
				t.Errorf("log = %q, want it to contain %q", out, c.wantLog)
			}
			if !strings.Contains(out, "level="+c.wantLevel) {
				t.Errorf("log = %q, want level %s", out, c.wantLevel)
			}
			if installed != c.wantInstall {
				t.Errorf("installed = %v, want %v", installed, c.wantInstall)
			}
		})
	}
}

// A failure must not propagate: the daemon has to finish starting on a
// host that cannot reach GitHub, and the mismatch it leaves behind is the
// state the product already reports.
func TestConvergeBundledEngine_FailureIsNotFatal(t *testing.T) {
	var buf bytes.Buffer
	// Would panic the process if convergeBundledEngine returned or
	// re-raised; the assertion is that we reach the line after it.
	convergeBundledEngine(context.Background(), convergeLogger(&buf), infruntime.OllamaConvergeDeps{
		Present:    func() bool { return true },
		BinaryPath: func() string { return "/x" },
		Probe:      func(context.Context, string) (bool, string) { return true, "0.1.0" },
		Install:    func(context.Context) error { return errors.New("boom") },
	})
	if !strings.Contains(buf.String(), "boom") {
		t.Errorf("the underlying error is not in the log: %q", buf.String())
	}
}
