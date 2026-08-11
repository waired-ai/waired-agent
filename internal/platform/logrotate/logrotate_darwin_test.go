//go:build darwin

package logrotate

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// borrowFD opens path and returns a descriptor onto it that the test
// owns, standing in for the one launchd hands the daemon. Using a
// dup'd descriptor rather than fd 1/2 keeps the test from redirecting
// the test binary's own output.
func borrowFD(t *testing.T, path string) int {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	fd, err := syscall.Dup(int(f.Fd()))
	if err != nil {
		t.Fatalf("dup: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	return fd
}

func writeFD(t *testing.T, fd int, s string) {
	t.Helper()
	if _, err := syscall.Write(fd, []byte(s)); err != nil {
		t.Fatalf("write to fd %d: %v", fd, err)
	}
}

// TestRotate_RealOpsKeepTheDescriptorPointedAtTheLiveFile is #331 with
// the actual syscalls: a descriptor is opened onto the log file, the
// file is rotated underneath it, and writes made afterwards must land in
// the new live file rather than in the renamed inode.
//
// It also demonstrates the defect being fixed: the write made between
// the rename and the reopen is the one newsyslog used to lose forever,
// and here it is preserved in the archive.
//
// Product contract. Runs on darwin only — locally and on CI's
// "unit tests (darwin)" leg.
func TestRotate_RealOpsKeepTheDescriptorPointedAtTheLiveFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "waired-agent.err.log")
	fd := borrowFD(t, path)
	writeFD(t, fd, strings.Repeat("before the rotation\n", 100))

	rotated, err := rotate(Target{Path: path, FD: fd}, Policy{MaxBytes: 16, Keep: 5}, defaultOps())
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if !rotated {
		t.Fatal("rotate reported no rotation for a file over the cap")
	}

	writeFD(t, fd, "after the rotation\n")

	if got := readFile(t, path); got != "after the rotation\n" {
		t.Errorf("live file = %q, want only the post-rotation write; the descriptor was not re-pointed", got)
	}
	archive := readGz(t, archiveName(path, 0))
	if !strings.HasPrefix(archive, "before the rotation\n") {
		t.Errorf("archive does not start with the pre-rotation content: %.60q", archive)
	}
	if strings.Contains(archive, "after the rotation") {
		t.Error("the post-rotation write landed in the archive")
	}
}

// TestSameFile pins the guard that decides whether this process is the
// one writing the file at path.
//
// Product contract: it is what keeps a foreground run (fd 2 on a tty)
// and a wrong home-directory guess from renaming somebody else's file.
func TestSameFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "waired-agent.err.log")
	other := filepath.Join(dir, "someone-elses.log")
	writeFile(t, other, "not ours\n")
	fd := borrowFD(t, path)

	ok, err := sameFile(fd, path)
	if err != nil {
		t.Fatalf("sameFile on the descriptor's own path: %v", err)
	}
	if !ok {
		t.Error("sameFile said no for the file the descriptor was opened on")
	}

	ok, err = sameFile(fd, other)
	if err != nil {
		t.Fatalf("sameFile on an unrelated path: %v", err)
	}
	if ok {
		t.Error("sameFile said yes for a file the descriptor does not refer to")
	}
}

// TestSameFile_AfterRenameSaysNo is the state the process is in between
// an external rotator's rename and a reopen: the descriptor still refers
// to the renamed inode, and the path now holds a different (or no) file.
// The guard must not claim ownership of the new one.
func TestSameFile_AfterRenameSaysNo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "waired-agent.err.log")
	fd := borrowFD(t, path)
	writeFD(t, fd, "some lines\n")

	if err := os.Rename(path, path+".rotated"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	writeFile(t, path, "a fresh file an external rotator created\n")

	ok, err := sameFile(fd, path)
	if err != nil {
		t.Fatalf("sameFile: %v", err)
	}
	if ok {
		t.Error("sameFile said yes after the file was renamed out from under the descriptor")
	}
}

// TestReopen_CreatesTheFileWhenMissing covers the ordinary rotation
// case, where the live path does not exist at reopen time because the
// rename just moved it away.
func TestReopen_CreatesTheFileWhenMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "waired-agent.err.log")
	fd := borrowFD(t, path)
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}

	if err := reopen(path, fd); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	writeFD(t, fd, "recreated\n")
	if got := readFile(t, path); got != "recreated\n" {
		t.Errorf("live file = %q, want %q", got, "recreated\n")
	}
}

// TestManage_RotatesThroughRealDescriptors exercises the entry point the
// daemon and the tray actually call, with the real per-OS ops and a real
// descriptor — the layer above rotate(), where a mis-wired Manage would
// simply never rotate anything and no other test would notice.
//
// Manage's first sweep runs before the first tick, so a file that is
// already over the cap rotates immediately rather than 60s later.
//
// Product contract.
func TestManage_RotatesThroughRealDescriptors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "waired-agent.err.log")
	fd := borrowFD(t, path)
	writeFD(t, fd, strings.Repeat("before\n", 200))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	Manage(ctx, []Target{{Path: path, FD: fd}},
		func() Policy { return Policy{MaxBytes: 16, Keep: 5} },
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	deadline := time.Now().Add(5 * time.Second)
	for !exists(archiveName(path, 0)) {
		if time.Now().After(deadline) {
			t.Fatal("Manage did not rotate an over-cap file within 5s")
		}
		time.Sleep(20 * time.Millisecond)
	}

	writeFD(t, fd, "after\n")
	if got := readFile(t, path); got != "after\n" {
		t.Errorf("live file = %q, want only the post-rotation write", got)
	}
}

// TestManage_NoTargetsIsANoOp: on a host with nothing to rotate (and on
// every non-darwin build) Manage must return without starting a
// goroutine or touching anything.
func TestManage_NoTargetsIsANoOp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	Manage(ctx, nil, DefaultPolicy, slog.New(slog.NewTextHandler(io.Discard, nil)))
	Manage(ctx, AgentTargets("linux"), DefaultPolicy, nil)
	// A nil policy is the same no-op: nothing to ask for a bound with.
	Manage(ctx, AgentTargets("darwin"), nil, nil)
}
