//go:build linux || darwin

package tray

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakeWaired writes a stand-in CLI that records its argv and exits with
// the given status, so the REAL runWairedLink is what is under test —
// not a seam over it (CLAUDE.md §Test discipline).
func fakeWaired(t *testing.T, exit int, stdout string) (bin, argvFile string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "waired")
	argvFile = filepath.Join(dir, "argv")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argvFile + "\n" +
		"printf '%s' '" + stdout + "'\nexit " + strconv.Itoa(exit) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argvFile
}

func TestRunWairedLinkPassesTheArgvAndSucceeds(t *testing.T) {
	bin, argvFile := fakeWaired(t, 0, "Coding-agent integration updated.\n")

	if err := runWairedLink(context.Background(), bin, "opencode"); err != nil {
		t.Fatalf("runWairedLink: %v", err)
	}
	got, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	want := "link\n--force\n--no-prompt\nopencode\n"
	if string(got) != want {
		t.Errorf("argv = %q, want %q", got, want)
	}
}

// TestRunWairedLinkCarriesTheCLIsOwnWords: the tray shows what the CLI
// said, because "exit status 1" tells the user nothing they can act on.
func TestRunWairedLinkCarriesTheCLIsOwnWords(t *testing.T) {
	bin, _ := fakeWaired(t, 1, "warming up\nError: integration: opencode: permission denied\n")

	err := runWairedLink(context.Background(), bin, "opencode")
	if err == nil {
		t.Fatal("runWairedLink returned nil for a failing CLI")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("err = %v, want it to carry the CLI's last line", err)
	}
}
