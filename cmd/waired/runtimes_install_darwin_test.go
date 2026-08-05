//go:build darwin

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// macOS installs the bundled engine exactly the way Linux does since #492,
// so these mirror runtimes_install_linux_test.go. The pair that used to
// live here — "skip when an ollama is already resolvable" and the
// Ollama.app bundle-seal tests — went with the /Applications layout: there
// is no bundle to seal any more, and a foreign Ollama no longer satisfies
// a bundled install (see TestInstallOllamaDarwin_IgnoresAForeignOllama).

// TestInstallOllamaDarwin_Bundled verifies installOllama drives the
// bundled installer seam with the state-dir base and the resolved install
// budget when -y skips the prompt.
func TestInstallOllamaDarwin_Bundled(t *testing.T) {
	orig := installOllamaBundled
	t.Cleanup(func() { installOllamaBundled = orig })

	var gotBaseDir string
	var gotDeadline time.Time
	var hadDeadline bool
	gotSink := true
	called := false
	installOllamaBundled = func(ctx context.Context, baseDir string, sink func(infruntime.OllamaInstallProgress)) error {
		called = true
		gotBaseDir = baseDir
		gotSink = sink != nil
		gotDeadline, hadDeadline = ctx.Deadline()
		return nil
	}

	if err := installOllama(true, "/Library/Application Support/waired", nil); err != nil {
		t.Fatalf("installOllama(-y): %v", err)
	}
	if !called {
		t.Fatal("bundled installer seam was not invoked")
	}
	want := filepath.Join("/Library/Application Support/waired", "runtimes", "ollama")
	if gotBaseDir != want {
		t.Errorf("baseDir = %q, want %q", gotBaseDir, want)
	}
	// `waired runtimes install` is a hand-run command: there is no browser
	// wizard on the other end of a lease to report bytes to.
	if gotSink {
		t.Error("a progress sink reached the installer on the hand-run path")
	}
	// The installer must run under the resolved backstop, not a hardcoded
	// wall clock (#189).
	if !hadDeadline {
		t.Fatal("installer ran with no deadline: the install budget is not being applied")
	}
	budget := ollamaInstallTimeout(func(string) string { return "" })
	if got := time.Until(gotDeadline); got > budget || got < budget-time.Minute {
		t.Errorf("install budget = %s, want ~%s (the resolved backstop)", got.Round(time.Second), budget)
	}
}

func TestInstallOllamaDarwin_BudgetFollowsTheEnvironment(t *testing.T) {
	t.Setenv(ollamaInstallTimeoutEnv, "3h")

	orig := installOllamaBundled
	t.Cleanup(func() { installOllamaBundled = orig })
	var got time.Duration
	var ok bool
	installOllamaBundled = func(ctx context.Context, _ string, _ func(infruntime.OllamaInstallProgress)) error {
		var dl time.Time
		dl, ok = ctx.Deadline()
		got = time.Until(dl)
		return nil
	}

	if err := installOllama(true, t.TempDir(), nil); err != nil {
		t.Fatalf("installOllama(-y): %v", err)
	}
	if !ok {
		t.Fatal("installer ran with no deadline")
	}
	if got > 3*time.Hour || got < 3*time.Hour-time.Minute {
		t.Errorf("install budget = %s, want ~3h from %s", got.Round(time.Second), ollamaInstallTimeoutEnv)
	}
}

// TestInstallOllamaDarwin_Error surfaces an installer failure wrapped.
func TestInstallOllamaDarwin_Error(t *testing.T) {
	orig := installOllamaBundled
	t.Cleanup(func() { installOllamaBundled = orig })
	sentinel := errors.New("download failed")
	installOllamaBundled = func(ctx context.Context, _ string, _ func(infruntime.OllamaInstallProgress)) error {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("installer ran with no deadline on the error path either")
		}
		return sentinel
	}
	err := installOllama(true, t.TempDir(), nil)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want wrapped %v", err, sentinel)
	}
}

// PRODUCT CONTRACT (#488): an Ollama waired did not install is not an
// Ollama waired serves with. Until #492 this function returned early with
// "already present — nothing to do" for anything download.ResolveBinary
// could find, which on macOS includes the user's own /Applications
// Ollama.app and a Homebrew CLI — so `waired runtimes install ollama`
// could complete having installed nothing, and the daemon then served
// through software of an unknown version.
//
// The fixture is a $PATH ollama rather than the old $WAIRED_OLLAMA_BINARY
// one: #493 deleted the resolver that read that variable, and a fixture
// nothing could possibly find would make this assertion vacuous.
func TestInstallOllamaDarwin_IgnoresAForeignOllama(t *testing.T) {
	dir := t.TempDir()
	foreign := filepath.Join(dir, "ollama")
	if err := os.WriteFile(foreign, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	orig := installOllamaBundled
	t.Cleanup(func() { installOllamaBundled = orig })
	called := false
	installOllamaBundled = func(context.Context, string, func(infruntime.OllamaInstallProgress)) error {
		called = true
		return nil
	}

	if err := installOllama(true, t.TempDir(), nil); err != nil {
		t.Fatalf("installOllama: %v", err)
	}
	if !called {
		t.Error("a foreign ollama on this host suppressed the bundled install")
	}
}
