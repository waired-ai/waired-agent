//go:build linux || darwin

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSpawnPickerRepublishReturnsWithoutWaiting.
//
// The hook's whole budget is about a second — Claude Code holds the session
// until it returns and bounds it with a timeout — while the write that reaches
// that session happens seconds later. So the one thing this must not do is
// wait for the child. A mutant that called Run or Wait fails the deadline
// below; one that got the arguments wrong fails the argv check.
//
// The stand-in is a shell script rather than the test binary, because the test
// binary would reject `claude _picker republish` as flags and exit at once,
// which is exactly the behaviour a broken spawn would also show.
func TestSpawnPickerRepublishReturnsWithoutWaiting(t *testing.T) {
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	sentinel := filepath.Join(dir, "the-child-kept-running")
	script := filepath.Join(dir, "stand-in-for-waired")
	body := "#!/bin/sh\n" +
		"printf '%s' \"$*\" > " + argvFile + "\n" +
		"sleep 1\n" +
		"printf ok > " + sentinel + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	restore := pickerRepublishExeFn
	pickerRepublishExeFn = func() (string, error) { return script, nil }
	t.Cleanup(func() { pickerRepublishExeFn = restore })

	start := time.Now()
	if err := spawnPickerRepublish(); err != nil {
		t.Fatalf("spawnPickerRepublish: %v", err)
	}
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Errorf("spawnPickerRepublish took %v: it waited for the child", took)
	}

	waitFor := func(path string) []byte {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if b, err := os.ReadFile(path); err == nil {
				return b
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("%s never appeared", filepath.Base(path))
		return nil
	}

	gotArgv := strings.TrimSpace(string(waitFor(argvFile)))
	wantArgv := strings.Join(pickerRepublishArgv(), " ")
	if gotArgv != wantArgv {
		t.Errorf("the child was started with %q, want %q", gotArgv, wantArgv)
	}
	waitFor(sentinel)
}
