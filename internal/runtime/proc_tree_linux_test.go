//go:build linux

package runtime

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestParseProcStat(t *testing.T) {
	cases := []struct {
		raw       string
		wantState byte
		wantPgrp  int
		wantOK    bool
	}{
		{"123 (llama-server) S 1 120 120 0 -1", 'S', 120, true},
		{"124 (a) b (c)) Z 1 99 99 0", 'Z', 99, true}, // comm with spaces and parentheses
		{"125 (x) R", 0, 0, false},
		{"no parenthesis", 0, 0, false},
	}
	for _, c := range cases {
		state, pgrp, ok := parseProcStat([]byte(c.raw))
		if state != c.wantState || pgrp != c.wantPgrp || ok != c.wantOK {
			t.Errorf("parseProcStat(%q) = %q, %d, %v; want %q, %d, %v",
				c.raw, state, pgrp, ok, c.wantState, c.wantPgrp, c.wantOK)
		}
	}
}

// A process that has exited but was never reaped still answers
// kill(-pgid, 0). That is the state of an engine's runners when
// waired-agent is PID 1 of a container, and without the member listing a
// start would wait out the whole limit for processes that no longer run.
func TestGroupMembers_AZombieDoesNotKeepTheTreeAlive(t *testing.T) {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), treeHelperRoleEnv+"=exit-now")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Wait() })

	// Not reaped: wait for the zombie state.
	deadline := time.Now().Add(30 * time.Second)
	for {
		raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			t.Fatalf("read stat: %v", err)
		}
		if state, _, ok := parseProcStat(raw); ok && state == 'Z' {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the helper never became a zombie")
		}
		time.Sleep(10 * time.Millisecond)
	}

	probe := syscall.Kill(-pid, 0)
	if probe != nil {
		t.Fatalf("kill(-pgid, 0) on an unreaped group = %v; the premise of this test is that it succeeds", probe)
	}
	members, err := groupMembers(pid)
	if err != nil {
		t.Fatalf("groupMembers: %v", err)
	}
	if len(members) != 1 || members[0].PID != pid || !members[0].Zombie {
		t.Fatalf("groupMembers = %+v, want the one zombie %d", members, pid)
	}
	alive, err := treeAliveFrom("linux", processTreeFacts{GroupProbeErr: probe, Members: members})
	if err != nil || alive {
		t.Fatalf("treeAliveFrom = %v, %v; want false, nil for a group of zombies", alive, err)
	}
}
