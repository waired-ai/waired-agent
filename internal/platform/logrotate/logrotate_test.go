package logrotate

import (
	"compress/gzip"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- (GOOS, facts) -> plan -------------------------------------------------

// TestAgentTargets pins the product contract that only darwin has log
// files Manage rotates: systemd/journald and the Windows Event Log bound
// their own streams, and neither hands the process a descriptor onto a
// plain file that something else may rename (#331). The Windows agent log
// file is not one of these — the process opens it itself, so File bounds
// it from the inside rather than Manage from the outside (#687).
func TestAgentTargets(t *testing.T) {
	for _, tc := range []struct {
		goos string
		want []Target
	}{
		{"darwin", []Target{
			{Path: "/Library/Logs/waired-agent.out.log", FD: 1},
			{Path: "/Library/Logs/waired-agent.err.log", FD: 2},
		}},
		{"linux", nil},
		{"windows", nil},
	} {
		t.Run(tc.goos, func(t *testing.T) {
			got := AgentTargets(tc.goos)
			if len(got) != len(tc.want) {
				t.Fatalf("AgentTargets(%q) = %+v, want %+v", tc.goos, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("target %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestTrayTargets covers the same table for the per-user tray, plus the
// empty-home case: without a home directory there is no path to rotate
// and guessing one would point at somebody else's file.
func TestTrayTargets(t *testing.T) {
	for _, tc := range []struct {
		name, goos, home string
		want             []Target
	}{
		{"darwin", "darwin", "/Users/example", []Target{
			{Path: "/Users/example/Library/Logs/waired-tray.out.log", FD: 1},
			{Path: "/Users/example/Library/Logs/waired-tray.err.log", FD: 2},
		}},
		{"darwin without a home", "darwin", "", nil},
		{"linux", "linux", "/home/example", nil},
		{"windows", "windows", `C:\Users\example`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := TrayTargets(tc.goos, tc.home)
			if len(got) != len(tc.want) {
				t.Fatalf("TrayTargets(%q, %q) = %+v, want %+v", tc.goos, tc.home, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("target %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// --- rotation mechanics ----------------------------------------------------

// fakeOps records every argument it is handed (CLAUDE.md §Test
// discipline) and, for reopen, also records what the filesystem looked
// like at the moment it was called — which is how the ordering contract
// (reopen before compression) becomes assertable at all.
type fakeOps struct {
	reopenPaths []string
	reopenFDs   []int
	// liveAtReopen / stagedAtReopen are the existence of <path> and
	// <path>.0 as reopen saw them.
	liveAtReopen   []bool
	stagedAtReopen []bool
	// reopenWrites is appended to <path> by reopen, standing in for the
	// process continuing to log through the re-pointed descriptor.
	reopenWrites string
	reopenErr    error

	sameFilePaths []string
	sameFileFDs   []int
	sameFileRet   bool
	sameFileErr   error
}

func (f *fakeOps) ops() ops {
	return ops{
		reopen: func(path string, fd int) error {
			f.reopenPaths = append(f.reopenPaths, path)
			f.reopenFDs = append(f.reopenFDs, fd)
			f.liveAtReopen = append(f.liveAtReopen, exists(path))
			f.stagedAtReopen = append(f.stagedAtReopen, exists(path+stagedSuffix))
			if f.reopenErr != nil {
				return f.reopenErr
			}
			if f.reopenWrites == "" {
				return nil
			}
			fh, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
			if err != nil {
				return err
			}
			defer fh.Close()
			_, err = io.WriteString(fh, f.reopenWrites)
			return err
		},
		sameFile: func(fd int, path string) (bool, error) {
			f.sameFilePaths = append(f.sameFilePaths, path)
			f.sameFileFDs = append(f.sameFileFDs, fd)
			return f.sameFileRet, f.sameFileErr
		},
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func writeGz(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()
	zw := gzip.NewWriter(f)
	if _, err := io.WriteString(zw, content); err != nil {
		t.Fatalf("gzip write %s: %v", path, err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close %s: %v", path, err)
	}
}

func readGz(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip reader %s: %v", path, err)
	}
	defer zr.Close()
	data, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip %s: %v", path, err)
	}
	return string(data)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// TestRotate_KeepsEveryLine is the #331 regression in portable form: the
// content that was live before the rotation ends up in the archive, and
// what the process writes after the descriptor is re-pointed ends up in
// the new live file. Neither is lost, which is exactly what the
// newsyslog arrangement could not promise.
//
// Product contract, not a record of today's behaviour.
func TestRotate_KeepsEveryLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "waired-agent.err.log")
	before := strings.Repeat("before\n", 100)
	writeFile(t, path, before)

	f := &fakeOps{sameFileRet: true, reopenWrites: "after the rotation\n"}
	rotated, err := rotate(Target{Path: path, FD: 2}, Policy{MaxBytes: 16, Keep: 5}, f.ops())
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if !rotated {
		t.Fatal("rotate reported no rotation for a file over the cap")
	}

	if got := readGz(t, path+".0.gz"); got != before {
		t.Errorf("archive holds %d bytes of unexpected content; want the pre-rotation %d bytes",
			len(got), len(before))
	}
	if got := readFile(t, path); got != "after the rotation\n" {
		t.Errorf("live file = %q, want only the post-reopen write", got)
	}
	if exists(path + stagedSuffix) {
		t.Error("the staged file survived the rotation; it should have been compressed away")
	}
}

// TestRotate_ReopensBeforeCompressing pins the ordering that makes the
// above possible. If compression moved ahead of the reopen, the process
// would spend the gzip's duration writing into a file about to be
// deleted — #331, reproduced from inside the process.
//
// Product contract.
func TestRotate_ReopensBeforeCompressing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "waired-agent.err.log")
	writeFile(t, path, strings.Repeat("x", 100))

	f := &fakeOps{sameFileRet: true}
	if _, err := rotate(Target{Path: path, FD: 2}, Policy{MaxBytes: 16, Keep: 5}, f.ops()); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	if len(f.reopenPaths) != 1 {
		t.Fatalf("reopen called %d times, want 1", len(f.reopenPaths))
	}
	if f.reopenPaths[0] != path || f.reopenFDs[0] != 2 {
		t.Errorf("reopen(%q, %d), want (%q, 2)", f.reopenPaths[0], f.reopenFDs[0], path)
	}
	if !f.stagedAtReopen[0] {
		t.Error("at reopen time the staged file was gone — compression ran too early")
	}
	if f.liveAtReopen[0] {
		t.Error("at reopen time the live path already existed — the rename did not happen first")
	}
}

// TestRotate_SkipsWhenTheDescriptorPointsElsewhere covers the guard that
// keeps a foreground run (fd 2 is the developer's terminal) or a wrong
// guess at the tray's home from renaming a file this process does not
// own.
//
// Product contract.
func TestRotate_SkipsWhenTheDescriptorPointsElsewhere(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "waired-agent.err.log")
	writeFile(t, path, strings.Repeat("x", 100))

	f := &fakeOps{sameFileRet: false}
	rotated, err := rotate(Target{Path: path, FD: 2}, Policy{MaxBytes: 16, Keep: 5}, f.ops())
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rotated {
		t.Error("rotated a file the process does not write through")
	}
	if len(f.sameFilePaths) != 1 || f.sameFilePaths[0] != path || f.sameFileFDs[0] != 2 {
		t.Errorf("sameFile calls = %v/%v, want one (%q, 2)", f.sameFilePaths, f.sameFileFDs, path)
	}
	if len(f.reopenPaths) != 0 {
		t.Error("reopen ran despite the guard refusing the rotation")
	}
	if !exists(path) || exists(path+".0.gz") {
		t.Error("the file was rotated anyway")
	}
}

// TestRotate_UnderTheCapDoesNothing / _MissingFileIsNotAnError cover the
// two quiet paths the 60s ticker spends almost all of its life in.
//
// Product contract.
func TestRotate_UnderTheCapDoesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "waired-agent.err.log")
	writeFile(t, path, "small\n")

	f := &fakeOps{sameFileRet: true}
	rotated, err := rotate(Target{Path: path, FD: 2}, Policy{MaxBytes: 1 << 20, Keep: 5}, f.ops())
	if err != nil || rotated {
		t.Fatalf("rotate = (%v, %v), want (false, nil)", rotated, err)
	}
	if len(f.sameFilePaths) != 0 {
		t.Error("the guard was consulted for a file that did not need rotating")
	}
}

func TestRotate_MissingFileIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	f := &fakeOps{sameFileRet: true}
	rotated, err := rotate(
		Target{Path: filepath.Join(dir, "never-written.log"), FD: 2},
		Policy{MaxBytes: 16, Keep: 5}, f.ops())
	if err != nil || rotated {
		t.Fatalf("rotate = (%v, %v), want (false, nil)", rotated, err)
	}
}

// TestRotate_AgesArchivesAndDropsTheOldest pins the retention the
// retired newsyslog drop-in provided: 5 gzip'd archives, oldest dropped.
//
// Product contract.
func TestRotate_AgesArchivesAndDropsTheOldest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "waired-agent.err.log")
	writeFile(t, path, strings.Repeat("live", 100))
	for i := range 5 {
		writeGz(t, archiveName(path, i), fmt.Sprintf("archive %d", i))
	}

	f := &fakeOps{sameFileRet: true}
	if _, err := rotate(Target{Path: path, FD: 2}, Policy{MaxBytes: 16, Keep: 5}, f.ops()); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	// Slot 0 is the rotated-away live content; 1..4 are the former 0..3;
	// the former slot 4 fell off the end.
	if got := readGz(t, archiveName(path, 0)); got != strings.Repeat("live", 100) {
		t.Errorf("slot 0 = %q, want the rotated-away live content", got)
	}
	for i := 1; i < 5; i++ {
		want := fmt.Sprintf("archive %d", i-1)
		if got := readGz(t, archiveName(path, i)); got != want {
			t.Errorf("slot %d = %q, want %q", i, got, want)
		}
	}
	if exists(archiveName(path, 5)) {
		t.Error("a sixth archive exists; Keep=5 must drop the oldest")
	}
}

// TestRecoverStaged_CompressesALeftover covers the crash window between
// the rename and the gzip: the staged file holds real log lines, so a
// later start must fold it into the archives rather than drop it.
//
// Product contract.
func TestRecoverStaged_CompressesALeftover(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "waired-agent.err.log")
	writeFile(t, path+stagedSuffix, "lines from before the crash\n")

	if err := recoverStaged(path, Policy{MaxBytes: 16, Keep: 5}); err != nil {
		t.Fatalf("recoverStaged: %v", err)
	}
	if exists(path + stagedSuffix) {
		t.Error("the staged file is still there")
	}
	if got := readGz(t, archiveName(path, 0)); got != "lines from before the crash\n" {
		t.Errorf("archive = %q, want the staged content", got)
	}
}

func TestRecoverStaged_NothingToDo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "waired-agent.err.log")
	if err := recoverStaged(path, Policy{MaxBytes: 16, Keep: 5}); err != nil {
		t.Fatalf("recoverStaged with no leftover: %v", err)
	}
	if exists(archiveName(path, 0)) {
		t.Error("an archive appeared out of nothing")
	}
}

// TestDefaultPolicy pins the info-level bound, so a change to it is a
// deliberate edit rather than a drift.
//
// It used to pin 1 MB x 5 — the values the retired newsyslog drop-in
// used — and this test is what made changing them deliberate. Measurement
// on the rc8 macOS host retired those: INFO records alone ran ~0.96 MB/h,
// so six windows held about six hours, which is not enough to look into
// something noticed the next morning (#658). Raised on the owner's call,
// with the observation that a host running Waired has already downloaded
// a multi-gigabyte model, so the disk was never the scarce thing.
func TestDefaultPolicy(t *testing.T) {
	p := DefaultPolicy()
	if p.MaxBytes != 32<<20 || p.Keep != 10 {
		t.Errorf("DefaultPolicy() = %+v, want {MaxBytes:33554432 Keep:10}", p)
	}
}

// TestPolicyForLevel pins the two bounds. Debug gets the larger one
// because the standard bug-report advice — raise verbosity, reproduce,
// then collect — otherwise shrank the usable window to about 90 minutes
// at the rate the rc8 macOS host measured, which is how two separate
// investigations there lost evidence only an hour old (#658).
//
// Product contract, ratified by #658 and the owner's sizing call.
func TestPolicyForLevel(t *testing.T) {
	for _, tc := range []struct {
		name string
		lvl  slog.Level
		want Policy
	}{
		{"debug", slog.LevelDebug, Policy{MaxBytes: 128 << 20, Keep: 10}},
		{"below debug", slog.LevelDebug - 4, Policy{MaxBytes: 128 << 20, Keep: 10}},
		{"info", slog.LevelInfo, Policy{MaxBytes: 32 << 20, Keep: 10}},
		{"warn", slog.LevelWarn, Policy{MaxBytes: 32 << 20, Keep: 10}},
		{"error", slog.LevelError, Policy{MaxBytes: 32 << 20, Keep: 10}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := PolicyForLevel(tc.lvl); got != tc.want {
				t.Errorf("PolicyForLevel(%v) = %+v, want %+v", tc.lvl, got, tc.want)
			}
		})
	}
	if PolicyForLevel(slog.LevelInfo) != DefaultPolicy() {
		t.Error("info level and DefaultPolicy disagree; they are meant to be the same bound")
	}
}
