package logrotate

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- (GOOS, facts) -> plan -------------------------------------------------

// TestAgentOwnedLogFile pins which OS makes the agent process own a log
// file of its own. Only Windows does: under the SCM stderr is closed and
// the Event Log takes Warn and above (internal/platform/logsink), so
// nothing carries the INFO/DEBUG stream. Linux has journald and macOS has
// launchd's StandardErrorPath, both of which already hold it.
//
// Product contract, ratified by #636 ("a rolling log file under the state
// dir is the obvious shape").
func TestAgentOwnedLogFile(t *testing.T) {
	for _, tc := range []struct {
		name, goos, stateDir, want string
	}{
		{"windows", "windows", `C:\ProgramData\waired`, `C:\ProgramData\waired\logs\waired-agent.log`},
		{"windows with a trailing separator", "windows", `C:\ProgramData\waired\`, `C:\ProgramData\waired\logs\waired-agent.log`},
		{"windows with forward slashes", "windows", "C:/tmp/waired", `C:\tmp\waired\logs\waired-agent.log`},
		{"windows without a state dir", "windows", "", ""},
		{"linux", "linux", "/var/lib/waired", ""},
		{"darwin", "darwin", "/Library/Application Support/waired", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := AgentOwnedLogFile(tc.goos, tc.stateDir); got != tc.want {
				t.Errorf("AgentOwnedLogFile(%q, %q) = %q, want %q", tc.goos, tc.stateDir, got, tc.want)
			}
		})
	}
}

// TestAgentLogPath covers the other question — where an operator READS the
// agent's own log — which has a different answer from AgentOwnedLogFile on
// macOS, where launchd owns the file but it is still the file to read. On
// Linux there is no file at all; the journal is the source.
func TestAgentLogPath(t *testing.T) {
	for _, tc := range []struct {
		name, goos, stateDir, want string
	}{
		{"windows", "windows", `C:\ProgramData\waired`, `C:\ProgramData\waired\logs\waired-agent.log`},
		{"windows without a state dir", "windows", "", ""},
		{"darwin", "darwin", "/Library/Application Support/waired", AgentErrPath},
		{"darwin without a state dir", "darwin", "", AgentErrPath},
		{"linux", "linux", "/var/lib/waired", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := AgentLogPath(tc.goos, tc.stateDir); got != tc.want {
				t.Errorf("AgentLogPath(%q, %q) = %q, want %q", tc.goos, tc.stateDir, got, tc.want)
			}
		})
	}
}

// --- the writer ------------------------------------------------------------

// smallPolicy keeps the fixtures short. Keep 3 so a fourth rotation has
// something to drop.
func smallPolicy() Policy { return Policy{MaxBytes: 100, Keep: 3} }

func TestOpenFileCreatesTheParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "logs", "waired-agent.log")
	f, err := OpenFile(path, smallPolicy)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer f.Close()
	if _, err := io.WriteString(f, "hello\n"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := readFile(t, path); got != "hello\n" {
		t.Errorf("live file = %q, want %q", got, "hello\n")
	}
}

// TestFileAppendsAcrossOpens is why the size is read from the file rather
// than counted from zero: a restarted daemon must not get a fresh 1 MB of
// headroom on top of a file that is already at the cap.
func TestFileAppendsAcrossOpens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "waired-agent.log")
	first, err := OpenFile(path, smallPolicy)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	mustWrite(t, first, strings.Repeat("a", 60)+"\n")
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := OpenFile(path, smallPolicy)
	if err != nil {
		t.Fatalf("OpenFile (reopen): %v", err)
	}
	defer second.Close()
	// 61 bytes are already there; 60 more crosses the 100-byte cap, so this
	// write rotates rather than landing in the same file.
	mustWrite(t, second, strings.Repeat("b", 60)+"\n")

	if got := readFile(t, path); got != strings.Repeat("b", 60)+"\n" {
		t.Errorf("live file = %q, want only the post-rotation write", got)
	}
	if got := readGz(t, archiveName(path, 0)); got != strings.Repeat("a", 60)+"\n" {
		t.Errorf("archive 0 = %q, want the pre-rotation write", got)
	}
}

// TestFileRotatesAtTheCap is the core behaviour: the live file holds the
// newest records and everything older is in the gzip archives, under the
// same <path>.<n>.gz names the descriptor-based rotation produces — which
// is what lets internal/platform/logdump collect both with one glob.
func TestFileRotatesAtTheCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "waired-agent.log")
	f, err := OpenFile(path, smallPolicy)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer f.Close()

	mustWrite(t, f, strings.Repeat("a", 90)+"\n")
	mustWrite(t, f, strings.Repeat("b", 20)+"\n")

	if got := readFile(t, path); got != strings.Repeat("b", 20)+"\n" {
		t.Errorf("live file = %q, want only the newest record", got)
	}
	if got := readGz(t, archiveName(path, 0)); got != strings.Repeat("a", 90)+"\n" {
		t.Errorf("archive 0 = %q, want the rotated-away record", got)
	}
	if exists(path + stagedSuffix) {
		t.Errorf("%s still exists; the staged file should have been compressed", path+stagedSuffix)
	}
}

// TestFileShiftsAndDropsArchives pins that Keep is honoured and that the
// numbering runs newest-first, matching shiftArchives.
func TestFileShiftsAndDropsArchives(t *testing.T) {
	path := filepath.Join(t.TempDir(), "waired-agent.log")
	f, err := OpenFile(path, smallPolicy)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer f.Close()

	// Four records of 101 bytes: each write after the first rotates.
	for _, c := range []string{"a", "b", "c", "d"} {
		mustWrite(t, f, strings.Repeat(c, 100)+"\n")
	}

	if got := readFile(t, path); got != strings.Repeat("d", 100)+"\n" {
		t.Errorf("live file = %q, want the newest record", got)
	}
	for i, want := range []string{"c", "b", "a"} {
		got := readGz(t, archiveName(path, i))
		if got != strings.Repeat(want, 100)+"\n" {
			t.Errorf("archive %d = %q, want the %q record", i, got, want)
		}
	}
	if exists(archiveName(path, 3)) {
		t.Errorf("archive 3 exists; Keep=3 should have dropped it")
	}
}

// TestFileKeepsAnOversizedRecordWhole records today's behaviour: a single
// record larger than MaxBytes is written whole rather than split, and an
// empty live file is never rotated — otherwise every such record would
// produce an archive holding nothing.
func TestFileKeepsAnOversizedRecordWhole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "waired-agent.log")
	f, err := OpenFile(path, smallPolicy)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer f.Close()

	huge := strings.Repeat("x", 500) + "\n"
	mustWrite(t, f, huge)

	if got := readFile(t, path); got != huge {
		t.Errorf("live file = %d bytes, want the whole %d-byte record", len(got), len(huge))
	}
	if exists(archiveName(path, 0)) {
		t.Errorf("archive 0 exists; an empty live file must not be rotated")
	}
}

// TestFileFollowsALivePolicyChange is why the policy is a function: the
// management API flips the level on a running daemon
// (`waired config log-level debug`), and a bound captured at boot would
// keep the old window for the rest of the process's life — the #658
// failure, since raising verbosity is exactly when the bigger window is
// needed.
func TestFileFollowsALivePolicyChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "waired-agent.log")
	cap := int64(1000)
	f, err := OpenFile(path, func() Policy { return Policy{MaxBytes: cap, Keep: 3} })
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer f.Close()

	mustWrite(t, f, strings.Repeat("a", 200)+"\n")
	mustWrite(t, f, strings.Repeat("b", 200)+"\n")
	if exists(archiveName(path, 0)) {
		t.Fatal("rotated under the roomy policy; 401 bytes is well inside 1000")
	}

	cap = 100 // as if the level went from debug back to info
	mustWrite(t, f, strings.Repeat("c", 20)+"\n")

	if got := readFile(t, path); got != strings.Repeat("c", 20)+"\n" {
		t.Errorf("live file = %q, want only the write after the policy shrank", got)
	}
	if !exists(archiveName(path, 0)) {
		t.Error("no archive; the smaller cap should have taken effect on the next write")
	}
}

// TestOpenFileRecoversAStagedArchive covers the crash window: a process
// killed between the rename and the gzip leaves <path>.0 holding real log
// data. Opening the file must fold it into slot 0 rather than let the next
// rotation's rename clobber it.
func TestOpenFileRecoversAStagedArchive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "waired-agent.log")
	if err := os.WriteFile(path+stagedSuffix, []byte("from the previous process\n"), 0o600); err != nil {
		t.Fatalf("seed staged: %v", err)
	}

	f, err := OpenFile(path, smallPolicy)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer f.Close()

	if exists(path + stagedSuffix) {
		t.Errorf("%s still exists; it should have been compressed on open", path+stagedSuffix)
	}
	if got := readGz(t, archiveName(path, 0)); got != "from the previous process\n" {
		t.Errorf("archive 0 = %q, want the recovered staged content", got)
	}
}

// TestFileNotesALastingRotationFailureOnce is the anti-flood guard. A
// rotation that fails for a lasting reason leaves the file over its cap,
// so every following write retries and fails the same way. Noting each one
// would put a WARN beside every record — turning a bounded log into a
// doubled unbounded one, which is the failure this package exists to
// prevent.
//
// The failure is forced by making the staging step impossible: a directory
// sitting where <path>.0 has to be renamed to cannot be replaced by a
// file, on any OS.
func TestFileNotesALastingRotationFailureOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "waired-agent.log")
	f, err := OpenFile(path, smallPolicy)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer f.Close()

	mustWrite(t, f, strings.Repeat("a", 90)+"\n")
	if err := os.Mkdir(path+stagedSuffix, 0o700); err != nil {
		t.Fatalf("block the staging slot: %v", err)
	}

	for i := range 5 {
		if _, err := io.WriteString(f, strings.Repeat("b", 90)+"\n"); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	got := strings.Count(readFile(t, path), `"msg":"log rotation failed`)
	if got != 1 {
		t.Errorf("rotation-failure notes = %d, want exactly 1 across five failing writes", got)
	}
	// Every record still got through — a log that cannot rotate must still
	// be a log.
	if n := strings.Count(readFile(t, path), strings.Repeat("b", 90)); n != 5 {
		t.Errorf("records written = %d, want 5; a failed rotation must not drop records", n)
	}
}

// TestFileWriteAfterCloseFails guards the daemon shutdown path: a late
// record must get an error rather than a nil-pointer panic.
func TestFileWriteAfterCloseFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "waired-agent.log")
	f, err := OpenFile(path, smallPolicy)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := io.WriteString(f, "late\n"); err == nil {
		t.Error("Write after Close = nil error, want a failure")
	}
	if err := f.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
}

// --- helpers ---------------------------------------------------------------

func mustWrite(t *testing.T, f *File, s string) {
	t.Helper()
	n, err := io.WriteString(f, s)
	if err != nil {
		t.Fatalf("Write(%d bytes): %v", len(s), err)
	}
	if n != len(s) {
		t.Fatalf("Write returned n = %d, want %d", n, len(s))
	}
}

// readFile / readGz live in logrotate_test.go — same package, same
// fixtures.
