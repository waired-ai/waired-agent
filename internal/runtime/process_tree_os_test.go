package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// The real-OS half of the process-tree contract, on every CI leg. The
// helper processes are this test binary itself, dispatched from TestMain
// by treeHelperRoleEnv, so the tests need nothing installed.

const (
	treeHelperRoleEnv    = "WAIRED_TREE_HELPER_ROLE"
	treeHelperPIDFileEnv = "WAIRED_TREE_HELPER_PIDFILE"
)

// runTreeHelper plays one role and returns the exit code.
//
//   - exit-now: exit at once.
//   - sleeper: sleep for two minutes (a runner that is still loading).
//   - leader-exits: start a sleeper, write its pid, exit at once (a server
//     that died and left its runner behind).
//   - leader-stays: the same, then sleep (a server that is still up).
//
// The sleeper is started without a process group or job of its own, as
// ollama starts its runners.
func runTreeHelper(role string) int {
	switch role {
	case "exit-now":
		return 0
	case "sleeper":
		time.Sleep(2 * time.Minute)
		return 0
	case "leader-exits", "leader-stays":
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), treeHelperRoleEnv+"=sleeper")
		if err := cmd.Start(); err != nil {
			return 2
		}
		if err := os.WriteFile(os.Getenv(treeHelperPIDFileEnv), []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
			return 3
		}
		if role == "leader-stays" {
			time.Sleep(2 * time.Minute)
		}
		return 0
	}
	return 1
}

func spawnTreeHelper(t *testing.T, role string) (RunningProcess, ProcessTree) {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	env := append(os.Environ(), treeHelperRoleEnv+"="+role, treeHelperPIDFileEnv+"="+pidFile)
	proc, err := DefaultSpawner{}.Spawn(context.Background(), os.Args[0], nil, env, nil)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = proc.Kill() })
	tree, ok := proc.(ProcessTree)
	if !ok {
		t.Fatalf("DefaultSpawner's process does not implement ProcessTree")
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if b, err := os.ReadFile(pidFile); err == nil && len(b) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the helper never started its grandchild")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return proc, tree
}

func waitTreeGone(t *testing.T, tree ProcessTree) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		alive, err := tree.TreeAlive()
		if err != nil {
			t.Fatalf("TreeAlive after Kill: %v", err)
		}
		if !alive {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the tree was still alive 30 s after Kill")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// PRODUCT CONTRACT (waired-ai/waired-agent#1443): the tree outlives its
// leader. A server that exited and left a runner behind still has a live
// tree, and Done() — which tracks the leader only — says nothing about it.
// Kill then ends the runner too, and TreeAlive sees it go.
func TestDefaultSpawner_TreeAliveCoversAGrandchildAfterItsLeaderExits(t *testing.T) {
	proc, tree := spawnTreeHelper(t, "leader-exits")
	select {
	case <-proc.Done():
	case <-time.After(30 * time.Second):
		t.Fatalf("the leader did not exit")
	}
	alive, err := tree.TreeAlive()
	if err != nil {
		t.Fatalf("TreeAlive: %v", err)
	}
	if !alive {
		t.Fatalf("TreeAlive = false with the grandchild still sleeping: the leader's exit was taken for the tree's")
	}
	if err := proc.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	waitTreeGone(t, tree)
	// Settled: later calls keep saying gone, and Kill is a no-op.
	if alive, err := tree.TreeAlive(); alive || err != nil {
		t.Errorf("TreeAlive after the tree was seen gone = %v, %v; want false, nil", alive, err)
	}
	if err := proc.Kill(); err != nil {
		t.Errorf("Kill after the tree was seen gone = %v, want nil", err)
	}
}

// A live leader is part of its own tree, and Kill ends leader and
// grandchild together.
func TestDefaultSpawner_KillEndsALiveTree(t *testing.T) {
	proc, tree := spawnTreeHelper(t, "leader-stays")
	if alive, err := tree.TreeAlive(); err != nil || !alive {
		t.Fatalf("TreeAlive with leader and grandchild running = %v, %v; want true, nil", alive, err)
	}
	if err := proc.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	select {
	case <-proc.Done():
	case <-time.After(30 * time.Second):
		t.Fatalf("the leader did not exit after Kill")
	}
	waitTreeGone(t, tree)
}
