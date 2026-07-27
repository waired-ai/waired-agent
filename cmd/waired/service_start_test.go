package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestStartAgentServiceBestEffort(t *testing.T) {
	savedInstalled, savedStart, savedHint := serviceInstalledFn, serviceStartFn, serviceStartHintFn
	t.Cleanup(func() {
		serviceInstalledFn, serviceStartFn, serviceStartHintFn = savedInstalled, savedStart, savedHint
	})
	serviceStartHintFn = func() string { return "sudo systemctl start waired-agent" }

	t.Run("installed starts successfully", func(t *testing.T) {
		started := false
		serviceInstalledFn = func() bool { return true }
		serviceStartFn = func() error { started = true; return nil }
		out := &bytes.Buffer{}
		startAgentServiceBestEffort(out)
		if !started {
			t.Fatal("expected StartInstalled to be called")
		}
		if !strings.Contains(out.String(), "Started waired-agent") {
			t.Errorf("missing success line, got %q", out.String())
		}
	})

	t.Run("not installed prints hint, does not start", func(t *testing.T) {
		serviceInstalledFn = func() bool { return false }
		serviceStartFn = func() error { t.Fatal("must not start when not installed"); return nil }
		out := &bytes.Buffer{}
		startAgentServiceBestEffort(out)
		if !strings.Contains(out.String(), "sudo systemctl start waired-agent") {
			t.Errorf("missing manual hint, got %q", out.String())
		}
	})

	t.Run("start error falls back to warning + hint", func(t *testing.T) {
		serviceInstalledFn = func() bool { return true }
		serviceStartFn = func() error { return errors.New("boom") }
		out := &bytes.Buffer{}
		startAgentServiceBestEffort(out)
		s := out.String()
		if !strings.Contains(s, "could not auto-start") || !strings.Contains(s, "sudo systemctl start waired-agent") {
			t.Errorf("missing warning/hint on start error, got %q", s)
		}
	})
}
