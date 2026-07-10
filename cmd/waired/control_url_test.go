package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestNormalizeControlURL(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{"  ", "", false},
		{"dev.waired.net", "https://dev.waired.net", false},
		{"dev.waired.net/", "https://dev.waired.net", false},
		{"  dev.waired.net  ", "https://dev.waired.net", false},
		{"https://cp.example.com/", "https://cp.example.com", false},
		{"http://cp.example.com", "http://cp.example.com", false},
		{"127.0.0.1:9477", "http://127.0.0.1:9477", false},
		{"localhost:9477", "http://localhost:9477", false},
		{"localhost", "http://localhost", false},
		{"[::1]:9477", "http://[::1]:9477", false},
		{"ftp://cp.example.com", "", true},
		{"https://", "", true},
	}
	for _, c := range cases {
		got, err := normalizeControlURL(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("normalizeControlURL(%q) = %q, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalizeControlURL(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("normalizeControlURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

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
