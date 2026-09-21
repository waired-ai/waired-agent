//go:build linux

package runtime

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestParseProcStatCPU(t *testing.T) {
	// "pid (comm) state ppid pgrp session tty tpgid flags minflt cminflt
	// majflt cmajflt utime stime cutime cstime priority …"
	cases := []struct {
		raw       string
		wantPgrp  int
		wantTicks int64
		wantOK    bool
	}{
		{"500 (python3) R 1 500 500 0 -1 4194560 10 0 0 0 150 40 7 3 20 0", 500, 200, true},
		{"501 (VLLM::EngineCore (x)) S 500 500 500 0 -1 0 0 0 0 0 5 5 0 0 20", 500, 10, true},
		{"502 (short) S 1 500 500", 0, 0, false},
		{"503 (bad) S 1 500 500 0 -1 0 0 0 0 0 x 1 1 1 20", 0, 0, false},
	}
	for _, c := range cases {
		pgrp, ticks, ok := parseProcStatCPU([]byte(c.raw))
		if pgrp != c.wantPgrp || ticks != c.wantTicks || ok != c.wantOK {
			t.Errorf("parseProcStatCPU(%q) = %d, %d, %v; want %d, %d, %v",
				c.raw, pgrp, ticks, ok, c.wantPgrp, c.wantTicks, c.wantOK)
		}
	}
}

// The real kernel's answer for a busy child and an idle one: the signal
// that keeps a silent nvcc compile from reading as a stalled start
// (waired-agent#1508).
func TestTreeCPUTime_BusyGrowsIdleDoesNot(t *testing.T) {
	measure := func(role string) time.Duration {
		env := append(os.Environ(), treeHelperRoleEnv+"="+role)
		proc, err := DefaultSpawner{}.Spawn(context.Background(), os.Args[0], nil, env, nil)
		if err != nil {
			t.Fatalf("Spawn: %v", err)
		}
		t.Cleanup(func() { _ = proc.Kill() })
		timer, ok := proc.(treeCPUTimer)
		if !ok {
			t.Fatalf("DefaultSpawner's process does not read its group's CPU time")
		}
		time.Sleep(300 * time.Millisecond) // past the test binary's own start-up
		before, err := timer.TreeCPUTime()
		if err != nil {
			t.Fatalf("TreeCPUTime: %v", err)
		}
		time.Sleep(700 * time.Millisecond)
		after, err := timer.TreeCPUTime()
		if err != nil {
			t.Fatalf("TreeCPUTime: %v", err)
		}
		return after - before
	}
	if busy := measure("spin"); busy < 300*time.Millisecond {
		t.Errorf("a spinning child used %s of CPU in 700 ms; want most of a core", busy)
	}
	if idle := measure("sleeper"); idle > 150*time.Millisecond {
		t.Errorf("a sleeping child used %s of CPU in 700 ms; want next to none", idle)
	}
}
