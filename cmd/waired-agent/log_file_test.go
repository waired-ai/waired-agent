package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/platform/logrotate"
)

// fakeOpener records the real arguments it is handed, so a test can assert
// on the path and the policy the daemon asked for rather than only on
// whether something was opened.
type fakeOpener struct {
	paths    []string
	policies []logrotate.Policy
	err      error
	closer   *countingCloser
}

func (f *fakeOpener) open(path string, policy func() logrotate.Policy) (io.WriteCloser, error) {
	f.paths = append(f.paths, path)
	f.policies = append(f.policies, policy())
	if f.err != nil {
		return nil, f.err
	}
	f.closer = &countingCloser{}
	return f.closer, nil
}

type countingCloser struct {
	written strings.Builder
	closes  int
}

func (c *countingCloser) Write(b []byte) (int, error) { return c.written.Write(b) }
func (c *countingCloser) Close() error                { c.closes++; return nil }

// TestOpenAgentLogFile_PerOS pins which OS the daemon opens a log file on.
// Product contract, ratified by #636.
func TestOpenAgentLogFile_PerOS(t *testing.T) {
	for _, tc := range []struct {
		name, goos, stateDir string
		wantPath             string
	}{
		{"windows", "windows", `C:\ProgramData\waired`, `C:\ProgramData\waired\logs\waired-agent.log`},
		{"linux", "linux", "/var/lib/waired", ""},
		{"darwin", "darwin", "/Library/Application Support/waired", ""},
		{"windows without a state dir", "windows", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeOpener{}
			w, path, err := openAgentLogFile(tc.goos, tc.stateDir, logrotate.DefaultPolicy, f.open)
			if err != nil {
				t.Fatalf("openAgentLogFile: %v", err)
			}
			if path != tc.wantPath {
				t.Errorf("path = %q, want %q", path, tc.wantPath)
			}
			if tc.wantPath == "" {
				if w != nil {
					t.Errorf("writer = %v, want nil on %s", w, tc.goos)
				}
				if len(f.paths) != 0 {
					t.Errorf("opener called with %v, want no call on %s", f.paths, tc.goos)
				}
				return
			}
			if w == nil {
				t.Fatal("writer = nil, want an open file")
			}
			if len(f.paths) != 1 || f.paths[0] != tc.wantPath {
				t.Errorf("opener paths = %v, want [%q]", f.paths, tc.wantPath)
			}
			if got, want := f.policies[0], logrotate.DefaultPolicy(); got != want {
				t.Errorf("policy = %+v, want %+v", got, want)
			}
		})
	}
}

// TestOpenAgentLogFile_OpenFailureIsReportedNotFatal covers the locked-ACL
// case: the daemon must get a nil writer (not a non-nil interface wrapping
// nil) plus an error naming the path, so it can warn and carry on with
// stderr.
func TestOpenAgentLogFile_OpenFailureIsReportedNotFatal(t *testing.T) {
	f := &fakeOpener{err: errors.New("access is denied")}
	w, path, err := openAgentLogFile("windows", `C:\ProgramData\waired`, logrotate.DefaultPolicy, f.open)
	if err == nil {
		t.Fatal("err = nil, want the open failure")
	}
	if w != nil {
		t.Errorf("writer = %v, want a nil writer on failure", w)
	}
	if !strings.Contains(err.Error(), `C:\ProgramData\waired\logs\waired-agent.log`) {
		t.Errorf("err = %v, want it to name the path", err)
	}
	if path == "" {
		t.Error("path = \"\", want the attempted path so the caller can name it")
	}
}

// TestOpenRotatingLogFile exercises the production opener itself — without
// this the seam above would be the only thing any test ever calls
// (CLAUDE.md §Test discipline).
func TestOpenRotatingLogFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "waired-agent.log")
	w, err := openRotatingLogFile(path, logrotate.DefaultPolicy)
	if err != nil {
		t.Fatalf("openRotatingLogFile: %v", err)
	}
	if _, err := io.WriteString(w, "a record\n"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(b) != "a record\n" {
		t.Errorf("file = %q, want %q", b, "a record\n")
	}
}

// TestOpenRotatingLogFile_FailureReturnsANilWriter is the typed-nil trap:
// `return logrotate.OpenFile(...)` would hand back a non-nil io.WriteCloser
// wrapping a nil *logrotate.File, and the daemon would write into it.
func TestOpenRotatingLogFile_FailureReturnsANilWriter(t *testing.T) {
	// A path whose parent cannot be created: an existing regular file
	// standing where the directory would go.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	w, err := openRotatingLogFile(filepath.Join(blocker, "logs", "waired-agent.log"), logrotate.DefaultPolicy)
	if err == nil {
		t.Fatal("err = nil, want a failure")
	}
	if w != nil {
		t.Errorf("writer = %#v, want an untyped nil", w)
	}
}
