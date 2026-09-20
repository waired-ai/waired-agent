//go:build linux || darwin

package detach

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestConfigureKeepsASysProcAttrTheCallerSet: the same hole
// internal/platform/logdump/group_unix.go avoids. Replacing the struct would
// silently drop a Credential or a Chroot a caller had set, and the child would
// run as somebody else.
func TestConfigureKeepsASysProcAttrTheCallerSet(t *testing.T) {
	t.Run("sets the process group when there is nothing to keep", func(t *testing.T) {
		cmd := exec.Command("true")
		configure(cmd)
		if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
			t.Fatalf("Setpgid not set: %+v", cmd.SysProcAttr)
		}
	})
	t.Run("keeps what the caller already put there", func(t *testing.T) {
		cmd := exec.Command("true")
		cmd.SysProcAttr = &syscall.SysProcAttr{Noctty: true}
		configure(cmd)
		if !cmd.SysProcAttr.Setpgid {
			t.Error("Setpgid not set")
		}
		if !cmd.SysProcAttr.Noctty {
			t.Error("the caller's SysProcAttr was replaced rather than added to")
		}
	})
}

// TestStartSurvivesAGroupKillOfItsParent is the load-bearing one: everything
// else here is about flags, and this is about whether the child survives what
// kills its parent.
//
// It reproduces the shape Claude Code has. The helper stands in for the
// SessionStart hook: it is put in a process group of its own, calls Start on a
// command that waits and then writes a sentinel, and then signals its own
// process group — which is what a hook timeout does. A grandchild left in that
// group dies with it and never writes anything.
func TestStartSurvivesAGroupKillOfItsParent(t *testing.T) {
	sentinel := filepath.Join(t.TempDir(), "written-by-the-grandchild")

	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()

	helper := exec.Command(os.Args[0], "-test.run=TestDetachHelperProcess")
	helper.Env = append(os.Environ(), "DETACH_HELPER_SENTINEL="+sentinel)
	helper.Stdin, helper.Stdout, helper.Stderr = null, null, null
	// Its own group, so the helper's group kill reaches the helper and its
	// children and nothing else — certainly not the test runner.
	helper.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	// The helper kills itself, so a signal exit is the expected outcome and a
	// clean one means it never got that far.
	if err := helper.Wait(); err == nil {
		t.Fatal("the helper exited cleanly: it did not signal its own process group")
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("the grandchild finished before its parent died — the test proves nothing")
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sentinel); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the grandchild never wrote its sentinel: the group kill took it with its parent")
}

// TestDetachHelperProcess is the helper half of the test above, and a no-op in
// an ordinary run. It is a test rather than a separate program so the child is
// this binary, which is the shape spawnPickerRepublish uses.
func TestDetachHelperProcess(t *testing.T) {
	sentinel := os.Getenv("DETACH_HELPER_SENTINEL")
	if sentinel == "" {
		t.Skip("not the helper process")
	}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	cmd := exec.Command("/bin/sh", "-c", "sleep 1; printf ok > "+sentinel)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = null, null, null
	if err := Start(cmd); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// What a hook timeout does. No Wait and no sleep first: returning without
	// the child is the behaviour under test.
	_ = syscall.Kill(0, syscall.SIGKILL)
}
