package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// The daemon's job here is to run the policy and SAY what happened, the
// same as the ollama backstop — this runs where nobody is watching and
// the log is the only account of it. vLLM adds a fourth outcome the
// ollama side does not have: "needed, and cannot run", which must not be
// filed under the debug line that means "nothing to do" (#843).
func TestConvergeVLLMVenv_SaysWhatItDid(t *testing.T) {
	atPin := infruntime.WantedVLLMPins()

	cases := []struct {
		name        string
		installed   bool
		version     string
		pins        infruntime.VLLMPinSet
		hasPins     bool
		freeBytes   int64
		installErr  error
		wantLog     string
		wantLevel   string
		wantInstall bool
	}{
		{
			// Every macOS and Windows host, and every Linux host that
			// never installed vLLM — which is most of them.
			name: "absent", installed: false,
			wantLog: "needs no converge", wantLevel: "DEBUG",
		},
		{
			name: "already at the pin set", installed: true, version: infruntime.VLLMPinnedVersion,
			pins: atPin, hasPins: true,
			wantLog: "needs no converge", wantLevel: "DEBUG",
		},
		{
			name: "stale", installed: true, version: "0.20.0",
			pins: infruntime.VLLMPinSet{VLLM: "0.20.0"}, hasPins: true,
			wantLog: "converged to the pin", wantLevel: "INFO", wantInstall: true,
		},
		{
			name: "stale and the download fails", installed: true, version: "0.20.0",
			pins: infruntime.VLLMPinSet{VLLM: "0.20.0"}, hasPins: true,
			installErr: errors.New("dial tcp: no route to host"),
			wantLog:    "converge failed", wantLevel: "WARN", wantInstall: true,
		},
		{
			// The rebuild is needed and nothing will change until
			// somebody frees space. A debug line would bury that.
			name: "stale and the disk is too full", installed: true, version: "0.20.0",
			pins: infruntime.VLLMPinSet{VLLM: "0.20.0"}, hasPins: true,
			freeBytes: 1 << 30,
			wantLog:   "cannot run now", wantLevel: "WARN",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			installed := false
			var buf bytes.Buffer
			convergeVLLMVenv(context.Background(), convergeLogger(&buf), infruntime.VLLMConvergeDeps{
				Active:    func() (string, bool) { return c.version, c.installed },
				Pins:      func() (infruntime.VLLMPinSet, bool) { return c.pins, c.hasPins },
				FreeBytes: func() int64 { return c.freeBytes },
				Install: func(context.Context) error {
					installed = true
					return c.installErr
				},
				Prune: func() ([]string, error) { return nil, nil },
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

// Reclaiming ~6 GB is worth an INFO line — it is a large, silent change
// to the host's disk — and failing to reclaim it is worth a WARN, but
// neither turns the converge into a failure.
func TestConvergeVLLMVenv_ReportsThePruneSeparately(t *testing.T) {
	var buf bytes.Buffer
	convergeVLLMVenv(context.Background(), convergeLogger(&buf), infruntime.VLLMConvergeDeps{
		Active:    func() (string, bool) { return "0.20.0", true },
		Pins:      func() (infruntime.VLLMPinSet, bool) { return infruntime.VLLMPinSet{VLLM: "0.20.0"}, true },
		FreeBytes: func() int64 { return 500 << 30 },
		Install:   func(context.Context) error { return nil },
		Prune:     func() ([]string, error) { return []string{"0.20.0"}, errors.New("permission denied") },
	})
	out := buf.String()
	for _, want := range []string{"converged to the pin", "removed the superseded", "permission denied"} {
		if !strings.Contains(out, want) {
			t.Errorf("log = %q, want it to contain %q", out, want)
		}
	}
	if strings.Contains(out, "converge failed") {
		t.Errorf("a prune failure was reported as a failed converge: %q", out)
	}
}

// A failure must not propagate: the daemon has to finish starting on a
// host that cannot reach PyPI.
func TestConvergeVLLMVenv_FailureIsNotFatal(t *testing.T) {
	var buf bytes.Buffer
	convergeVLLMVenv(context.Background(), convergeLogger(&buf), infruntime.VLLMConvergeDeps{
		Active:  func() (string, bool) { return "0.20.0", true },
		Pins:    func() (infruntime.VLLMPinSet, bool) { return infruntime.VLLMPinSet{VLLM: "0.20.0"}, true },
		Install: func(context.Context) error { return errors.New("boom") },
	})
	if !strings.Contains(buf.String(), "boom") {
		t.Errorf("the underlying error is not in the log: %q", buf.String())
	}
}
