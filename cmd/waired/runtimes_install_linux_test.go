//go:build linux

package main

import (
	"context"
	"errors"
	"testing"
	"time"

	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// TestInstallOllamaLinux_Bundled verifies installOllama drives the
// bundled installer seam (and never shells out to install.sh /
// systemctl, which were removed in the bundle redesign) when -y skips
// the prompt, and hands the root-written state dir back to the service
// user afterwards (#484).
func TestInstallOllamaLinux_Bundled(t *testing.T) {
	orig := installOllamaBundled
	t.Cleanup(func() { installOllamaBundled = orig })

	var gotBaseDir string
	var gotDeadline time.Time
	var hadDeadline bool
	gotSink := true
	called := false
	// The fake takes every argument it is given and records it. It used to
	// accept `_ context.Context` and drop it, which made the install
	// budget — the whole subject of #189 — unobservable from any test:
	// installOllama derives a deadline two lines above this call and
	// nothing could see it (CLAUDE.md §Test discipline). The same now
	// holds for the progress sink: a hand-run install has no wizard to
	// report to, and a fake that discarded it could not say so.
	installOllamaBundled = func(ctx context.Context, baseDir string, sink func(infruntime.OllamaInstallProgress)) error {
		called = true
		gotBaseDir = baseDir
		gotSink = sink != nil
		gotDeadline, hadDeadline = ctx.Deadline()
		return nil
	}

	origFix := fixStateOwnership
	t.Cleanup(func() { fixStateOwnership = origFix })
	var gotOwnedDir string
	fixCalls := 0
	fixStateOwnership = func(dir string) error {
		fixCalls++
		gotOwnedDir = dir
		return nil
	}

	if err := installOllama(true, "/var/lib/waired", nil); err != nil {
		t.Fatalf("installOllama(-y): %v", err)
	}
	if !called {
		t.Fatal("bundled installer seam was not invoked")
	}
	if gotBaseDir != "/var/lib/waired/runtimes/ollama" {
		t.Errorf("baseDir = %q, want <state-dir>/runtimes/ollama", gotBaseDir)
	}
	// `waired runtimes install` is a hand-run command: there is no browser
	// wizard on the other end of a lease to report bytes to.
	if gotSink {
		t.Error("a progress sink reached the installer on the hand-run path")
	}
	// The whole state dir (not just runtimes/ollama) is handed back, so a
	// root-run install can't leave the daemon locked out of its identity.
	if fixCalls != 1 {
		t.Errorf("fixStateOwnership called %d times, want 1", fixCalls)
	}
	if gotOwnedDir != "/var/lib/waired" {
		t.Errorf("fixStateOwnership dir = %q, want the state dir /var/lib/waired", gotOwnedDir)
	}
	// The installer must run under a deadline — an unbounded install is
	// what left the terminal waiting (#188) — and that deadline must be
	// the resolved backstop, not a hardcoded wall clock (#189).
	if !hadDeadline {
		t.Fatal("installer ran with no deadline: the install budget is not being applied")
	}
	want := ollamaInstallTimeout(func(string) string { return "" })
	if got := time.Until(gotDeadline); got > want || got < want-time.Minute {
		t.Errorf("install budget = %s, want ~%s (the resolved backstop)", got.Round(time.Second), want)
	}
}

// TestInstallOllamaLinux_BudgetFollowsTheEnvironment pins that the
// backstop is the resolved one rather than a fixed wall clock. #189 was
// exactly this: a hardcoded 15 minutes killed healthy slow downloads and
// reported the kill as "exit status 1". The override has to reach the
// context the installer actually runs under, not just the error message.
func TestInstallOllamaLinux_BudgetFollowsTheEnvironment(t *testing.T) {
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

	origFix := fixStateOwnership
	t.Cleanup(func() { fixStateOwnership = origFix })
	fixStateOwnership = func(string) error { return nil }

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

// TestInstallOllamaLinux_Error surfaces an installer failure and skips the
// ownership hand-off: nothing was successfully written, so there is nothing
// to chown back.
func TestInstallOllamaLinux_Error(t *testing.T) {
	orig := installOllamaBundled
	t.Cleanup(func() { installOllamaBundled = orig })
	// Even the error path takes the ctx: #189's failure mode was a KILLED
	// download reported as a plain "exit status 1", so a test that wants
	// to tell "the budget expired" from "the download failed" needs the
	// ctx here too.
	installOllamaBundled = func(ctx context.Context, _ string, _ func(infruntime.OllamaInstallProgress)) error {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("installer ran with no deadline on the error path either")
		}
		return errors.New("download failed")
	}

	origFix := fixStateOwnership
	t.Cleanup(func() { fixStateOwnership = origFix })
	fixCalled := false
	fixStateOwnership = func(string) error { fixCalled = true; return nil }

	if err := installOllama(true, t.TempDir(), nil); err == nil {
		t.Fatal("expected installer error to propagate")
	}
	if fixCalled {
		t.Error("fixStateOwnership should not run when the install failed")
	}
}
