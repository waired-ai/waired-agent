package tray

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestClassifySystemDir pins that "absent" and "unreadable" are different
// answers.
//
// Product contract, waired-agent#1269: an elevated action must be pointed at
// the system state dir when this process cannot read it, because "cannot read"
// is what a locked-down system install looks like from the desktop user — on
// every OS. The old code asked os.Stat and kept only err == nil, so a
// permission error and a missing directory produced the same answer, and the
// wrong one.
func TestClassifySystemDir(t *testing.T) {
	for _, tc := range []struct {
		name  string
		names []string
		err   error
		want  systemDirAnswer
	}{
		{
			name: "unreadable is a system install, not an absent one",
			err:  &fs.PathError{Op: "open", Path: "/x", Err: fs.ErrPermission},
			want: systemDirUnreadable,
		},
		{
			name:  "readable and enrolled",
			names: []string{"agent.json", "identity.json", "secrets"},
			want:  systemDirEnrolled,
		},
		{
			name:  "readable, laid down by the installer, nobody signed in yet",
			names: []string{"agent.env", "runtimes"},
			want:  systemDirAbsent,
		},
		{
			name: "missing",
			err:  &fs.PathError{Op: "open", Path: "/x", Err: fs.ErrNotExist},
			want: systemDirAbsent,
		},
		{
			// Fail to today's answer rather than invent a failure mode —
			// resolveSystemFallbackAt's default arm makes the same choice.
			name: "some other error",
			err:  errors.New("i/o error"),
			want: systemDirAbsent,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifySystemDir(tc.names, tc.err); got != tc.want {
				t.Errorf("classifySystemDir = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestResolveStateDir walks the precedence with each OS's real directory
// strings.
//
// resolveStateDir takes no GOOS argument on purpose: nothing in it branches on
// the operating system. What varies per OS is the two candidate PATHS, and
// internal/platform/paths owns those with its own tests. The rows are still
// named per OS so all three sit on one screen — the reported defect is darwin,
// and its two siblings behave identically.
func TestResolveStateDir(t *testing.T) {
	const (
		darwinSys  = "/Library/Application Support/waired"
		darwinUser = "/Users/someone/Library/Application Support/waired"
		linuxSys   = "/var/lib/waired"
		linuxUser  = "/home/someone/.local/state/waired"
		winSys     = `C:\ProgramData\waired`
		winUser    = `C:\Users\someone\AppData\Local\waired`
	)
	for _, tc := range []struct {
		name       string
		facts      stateDirFacts
		wantDir    string
		wantSource stateDirSource
	}{
		// The reported case, on all three OSes: a system install the desktop
		// user is locked out of, and a daemon that did not name a directory.
		{
			name:       "darwin/system-unreadable/daemon-silent",
			facts:      stateDirFacts{SystemDir: darwinSys, UserDir: darwinUser, System: systemDirUnreadable},
			wantDir:    darwinSys,
			wantSource: sourceSystem,
		},
		{
			name:       "linux/system-unreadable/daemon-silent",
			facts:      stateDirFacts{SystemDir: linuxSys, UserDir: linuxUser, System: systemDirUnreadable},
			wantDir:    linuxSys,
			wantSource: sourceSystem,
		},
		{
			name:       "windows/system-unreadable/daemon-silent",
			facts:      stateDirFacts{SystemDir: winSys, UserDir: winUser, System: systemDirUnreadable},
			wantDir:    winSys,
			wantSource: sourceSystem,
		},

		// An elevated or developer run can see the directory.
		{
			name:       "darwin/system-enrolled/daemon-silent",
			facts:      stateDirFacts{SystemDir: darwinSys, UserDir: darwinUser, System: systemDirEnrolled},
			wantDir:    darwinSys,
			wantSource: sourceSystem,
		},
		{
			name:       "linux/system-enrolled/daemon-silent",
			facts:      stateDirFacts{SystemDir: linuxSys, UserDir: linuxUser, System: systemDirEnrolled},
			wantDir:    linuxSys,
			wantSource: sourceSystem,
		},
		{
			name:       "windows/system-enrolled/daemon-silent",
			facts:      stateDirFacts{SystemDir: winSys, UserDir: winUser, System: systemDirEnrolled},
			wantDir:    winSys,
			wantSource: sourceSystem,
		},

		// A per-user install: no system directory to point at.
		{
			name:       "darwin/no-system-dir",
			facts:      stateDirFacts{SystemDir: darwinSys, UserDir: darwinUser, System: systemDirAbsent},
			wantDir:    darwinUser,
			wantSource: sourceUser,
		},
		{
			name:       "linux/no-system-dir",
			facts:      stateDirFacts{SystemDir: linuxSys, UserDir: linuxUser, System: systemDirAbsent},
			wantDir:    linuxUser,
			wantSource: sourceUser,
		},
		{
			name:       "windows/no-system-dir",
			facts:      stateDirFacts{SystemDir: winSys, UserDir: winUser, System: systemDirAbsent},
			wantDir:    winUser,
			wantSource: sourceUser,
		},

		// The daemon's own answer outranks both local candidates: it is the
		// only one that knows about a daemon started with its own --state-dir.
		{
			name: "darwin/daemon-named-its-own-dir",
			facts: stateDirFacts{
				DaemonDir: "/opt/waired-instance-2",
				SystemDir: darwinSys, UserDir: darwinUser, System: systemDirUnreadable,
			},
			wantDir:    "/opt/waired-instance-2",
			wantSource: sourceDaemon,
		},
		{
			name: "linux/daemon-answers-even-when-no-system-dir-exists",
			facts: stateDirFacts{
				DaemonDir: linuxSys,
				SystemDir: linuxSys, UserDir: linuxUser, System: systemDirAbsent,
			},
			wantDir:    linuxSys,
			wantSource: sourceDaemon,
		},

		// An operator who named a directory gets it, whatever anyone else says.
		{
			name: "windows/override-beats-the-daemon",
			facts: stateDirFacts{
				Override: `D:\waired-test`, DaemonDir: winSys,
				SystemDir: winSys, UserDir: winUser, System: systemDirUnreadable,
			},
			wantDir:    `D:\waired-test`,
			wantSource: sourceOverride,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotDir, gotSrc := resolveStateDir(tc.facts)
			if gotDir != tc.wantDir {
				t.Errorf("dir = %q, want %q", gotDir, tc.wantDir)
			}
			// The source is asserted too: a row that lands on the right
			// directory for the wrong reason is a rule that will misfire on
			// the next host.
			if gotSrc != tc.wantSource {
				t.Errorf("source = %v, want %v", gotSrc, tc.wantSource)
			}
		})
	}
}

// TestOsReadStateDirNames_AgainstRealDirectory drives the real reader, which
// the seam would otherwise leave uncalled by any test (CLAUDE.md §Test
// discipline: "a `var xFn = realFn` seam needs a table test on realFn").
//
// The permission arm is deliberately NOT re-derived here. Staging it needs a
// directory this process cannot open, which is unstageable as root (CI runs
// containers as root) and meaningless on Windows, where os.Chmod toggles the
// read-only attribute rather than denying traversal. What matters is that
// classifySystemDir routes fs.ErrPermission, and TestClassifySystemDir feeds
// it that error value directly. Do not add a chmod-0 case here: it would skip
// on exactly the machines that run this suite.
func TestOsReadStateDirNames_AgainstRealDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "identity.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed identity.json: %v", err)
	}
	names, err := osReadStateDirNames(dir)
	if err != nil {
		t.Fatalf("osReadStateDirNames(%q) = %v", dir, err)
	}
	if !slices.Contains(names, "identity.json") {
		t.Errorf("names = %v, want it to contain identity.json", names)
	}
	if got := classifySystemDir(names, err); got != systemDirEnrolled {
		t.Errorf("classify of a real enrolled dir = %v, want %v", got, systemDirEnrolled)
	}

	_, missingErr := osReadStateDirNames(filepath.Join(dir, "no-such-dir"))
	if !errors.Is(missingErr, fs.ErrNotExist) {
		t.Fatalf("reading a missing dir = %v, want fs.ErrNotExist", missingErr)
	}
	if got := classifySystemDir(nil, missingErr); got != systemDirAbsent {
		t.Errorf("classify of a missing dir = %v, want %v", got, systemDirAbsent)
	}
}
