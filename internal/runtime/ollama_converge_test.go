package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The policy, stated once and table-tested, because three call sites act
// on it: the CLI the installers invoke, the daemon's reconcile, and the
// installer scripts' own log line. #826.
func TestDecideOllamaConverge(t *testing.T) {
	cases := []struct {
		name    string
		facts   OllamaConvergeFacts
		want    bool
		wantWhy string // substring
	}{
		{
			// #138: `waired init` is the only thing that installs an
			// engine, because it is the only thing that asks whether
			// this computer should run models. An update must not
			// answer that question by downloading 1.4 GB.
			name:    "absent stays absent",
			facts:   OllamaConvergeFacts{Installed: false, Pin: "0.32.13"},
			want:    false,
			wantWhy: "no bundled engine",
		},
		{
			name:    "already at the pin",
			facts:   OllamaConvergeFacts{Installed: true, Version: "0.32.13", Pin: "0.32.13"},
			want:    false,
			wantWhy: "already",
		},
		{
			// The spelling may differ without the release differing.
			name:  "pin equality is not string equality",
			facts: OllamaConvergeFacts{Installed: true, Version: "v0.32.13", Pin: "0.32.13"},
			want:  false,
		},
		{
			name:    "behind the pin",
			facts:   OllamaConvergeFacts{Installed: true, Version: "0.31.1", Pin: "0.32.13"},
			want:    true,
			wantWhy: "0.31.1",
		},
		{
			// A pin can move backwards (a bad engine release gets
			// reverted). Exact-match is the rule the agent serves by
			// — #489's consequence line — so converging DOWN is as
			// necessary as converging up.
			name:  "ahead of the pin",
			facts: OllamaConvergeFacts{Installed: true, Version: "0.33.0", Pin: "0.32.13"},
			want:  true,
		},
		{
			// An installed binary that will not say what it is cannot
			// be reasoned about, and it is not serving anything
			// either. Re-installing the pin is the repair.
			name:    "installed but unreadable",
			facts:   OllamaConvergeFacts{Installed: true, Version: "", Pin: "0.32.13"},
			want:    true,
			wantWhy: "does not report",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DecideOllamaConverge(c.facts)
			if got.Install != c.want {
				t.Errorf("Install = %v, want %v (reason %q)", got.Install, c.want, got.Reason)
			}
			if got.Reason == "" {
				t.Error("every decision carries a reason; this one is empty")
			}
			if c.wantWhy != "" && !strings.Contains(got.Reason, c.wantWhy) {
				t.Errorf("Reason = %q, want it to mention %q", got.Reason, c.wantWhy)
			}
		})
	}
}

// ConvergeOllama must not download when the decision says not to. A fake
// installer that records whether it ran is the only way to assert that;
// a fake that just returns nil would let a spurious download pass.
func TestConvergeOllama_InstallsOnlyWhenDecided(t *testing.T) {
	cases := []struct {
		name        string
		present     bool
		version     string
		wantInstall bool
	}{
		{name: "absent", present: false, wantInstall: false},
		{name: "current", present: true, version: OllamaPinnedVersion, wantInstall: false},
		{name: "stale", present: true, version: "0.1.0", wantInstall: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			installed := false
			probedPath := ""
			got, err := ConvergeOllama(context.Background(), OllamaConvergeDeps{
				Present: func() bool { return c.present },
				BinaryPath: func() string {
					return "/state/runtimes/ollama/bin/ollama"
				},
				Probe: func(_ context.Context, path string) (bool, string) {
					probedPath = path
					return c.present, c.version
				},
				Install: func(context.Context) error {
					installed = true
					return nil
				},
			})
			if err != nil {
				t.Fatalf("ConvergeOllama: %v", err)
			}
			if installed != c.wantInstall {
				t.Errorf("installed = %v, want %v (decision: %+v)", installed, c.wantInstall, got)
			}
			if got.Install != c.wantInstall {
				t.Errorf("decision.Install = %v, want %v", got.Install, c.wantInstall)
			}
			// The probe must be given the bundled binary's path, not
			// whatever is on PATH: the whole point is the engine waired
			// manages, and a foreign ollama on PATH would answer for it.
			if c.present && probedPath != "/state/runtimes/ollama/bin/ollama" {
				t.Errorf("probed %q, want the bundled binary path", probedPath)
			}
		})
	}
}

// A failed install surfaces as an error AND keeps the decision, so a
// caller can say what it was trying to do when it failed.
func TestConvergeOllama_InstallFailureIsReported(t *testing.T) {
	boom := errors.New("network down")
	got, err := ConvergeOllama(context.Background(), OllamaConvergeDeps{
		Present:    func() bool { return true },
		BinaryPath: func() string { return "/x/ollama" },
		Probe:      func(context.Context, string) (bool, string) { return true, "0.1.0" },
		Install:    func(context.Context) error { return boom },
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap %v", err, boom)
	}
	if !got.Install {
		t.Error("the decision to install is lost on failure; the caller cannot explain itself")
	}
}
