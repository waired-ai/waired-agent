package runtime

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// treeAliveFrom over all three platforms. A record of the rule the
// spawners apply (waired-ai/waired-agent#1443), not a platform contract.
func TestTreeAliveFrom(t *testing.T) {
	queryErr := errors.New("query failed")
	listErr := errors.New("proc unreadable")
	other := errors.New("EINVAL-ish")
	cases := []struct {
		name      string
		goos      string
		facts     processTreeFacts
		wantAlive bool
		wantErr   bool
	}{
		{"linux: group gone", "linux", processTreeFacts{GroupProbeErr: syscall.ESRCH}, false, false},
		{"linux: a running member", "linux", processTreeFacts{Members: []treeMember{{PID: 7}}}, true, false},
		{"linux: only zombies left", "linux", processTreeFacts{Members: []treeMember{{PID: 7, Zombie: true}}}, false, false},
		{"linux: a zombie and a runner", "linux", processTreeFacts{Members: []treeMember{{PID: 7, Zombie: true}, {PID: 8}}}, true, false},
		{"linux: EPERM is a live process", "linux", processTreeFacts{GroupProbeErr: syscall.EPERM, Members: []treeMember{{PID: 7}}}, true, false},
		{"linux: members unreadable", "linux", processTreeFacts{MembersErr: listErr}, true, false},
		{"linux: probe failed otherwise", "linux", processTreeFacts{GroupProbeErr: other}, false, true},
		{"darwin: group gone", "darwin", processTreeFacts{GroupProbeErr: syscall.ESRCH}, false, false},
		{"darwin: probe answers", "darwin", processTreeFacts{}, true, false},
		{"darwin: EPERM is a live process", "darwin", processTreeFacts{GroupProbeErr: syscall.EPERM}, true, false},
		{"darwin: members are not consulted", "darwin", processTreeFacts{Members: []treeMember{{PID: 7, Zombie: true}}}, true, false},
		{"windows: job empty", "windows", processTreeFacts{JobActive: 0}, false, false},
		{"windows: one process", "windows", processTreeFacts{JobActive: 1}, true, false},
		{"windows: query failed", "windows", processTreeFacts{JobActive: 3, JobQueryErr: queryErr}, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			alive, err := treeAliveFrom(c.goos, c.facts)
			if alive != c.wantAlive || (err != nil) != c.wantErr {
				t.Errorf("treeAliveFrom = %v, %v; want alive=%v err=%v", alive, err, c.wantAlive, c.wantErr)
			}
		})
	}
}

// treeProc is a RunningProcess whose tree the test holds open.
type treeProc struct {
	pid     int
	alive   atomic.Bool
	treeErr error
	mu      sync.Mutex
	kills   int
	probes  int
	done    chan struct{}
}

func newTreeProc(pid int, alive bool) *treeProc {
	p := &treeProc{pid: pid, done: make(chan struct{})}
	p.alive.Store(alive)
	close(p.done) // the leader is gone; only the tree is in question
	return p
}

func (p *treeProc) PID() int                 { return p.pid }
func (p *treeProc) Done() <-chan struct{}    { return p.done }
func (p *treeProc) Err() error               { return nil }
func (p *treeProc) Signal(_ os.Signal) error { return nil }
func (p *treeProc) Kill() error {
	p.mu.Lock()
	p.kills++
	p.mu.Unlock()
	return nil
}
func (p *treeProc) TreeAlive() (bool, error) {
	p.mu.Lock()
	p.probes++
	p.mu.Unlock()
	if p.treeErr != nil {
		return false, p.treeErr
	}
	return p.alive.Load(), nil
}
func (p *treeProc) killCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.kills
}

// leaderOnlyProc is a RunningProcess that cannot report on its tree.
type leaderOnlyProc struct{ done chan struct{} }

func (p leaderOnlyProc) PID() int                 { return 1 }
func (p leaderOnlyProc) Done() <-chan struct{}    { return p.done }
func (p leaderOnlyProc) Err() error               { return nil }
func (p leaderOnlyProc) Signal(_ os.Signal) error { return nil }
func (p leaderOnlyProc) Kill() error              { return nil }

// A process that cannot report on its tree is not recorded: nothing to
// wait for, exactly as before.
func TestPendingExits_IgnoresAProcessWithoutATree(t *testing.T) {
	e := NewPendingExits(time.Minute, time.Millisecond)
	e.Add("ollama", leaderOnlyProc{done: make(chan struct{})})
	if n := e.pending(); n != 0 {
		t.Fatalf("recorded %d processes that cannot report a tree, want 0", n)
	}
	if err := e.Wait(context.Background()); err != nil {
		t.Fatalf("Wait = %v, want nil", err)
	}
	var nilExits *PendingExits
	nilExits.Add("ollama", newTreeProc(1, true))
	if err := nilExits.Wait(context.Background()); err != nil {
		t.Fatalf("nil PendingExits.Wait = %v, want nil", err)
	}
}

// PRODUCT CONTRACT (waired-ai/waired-agent#1443): a start waits for a
// retired engine's tree, finishing the kill first, and proceeds once the
// tree is gone.
func TestPendingExits_WaitsForTheTreeAndFinishesTheKill(t *testing.T) {
	e := NewPendingExits(time.Minute, time.Millisecond)
	p := newTreeProc(4242, true)
	e.Add("ollama", p)

	done := make(chan error, 1)
	go func() { done <- e.Wait(context.Background()) }()
	select {
	case err := <-done:
		t.Fatalf("Wait returned %v while the tree was alive", err)
	case <-time.After(50 * time.Millisecond):
	}
	if k := p.killCount(); k != 1 {
		t.Errorf("Kill called %d times, want exactly 1 (the kill is finished once, then waited on)", k)
	}
	p.alive.Store(false)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Wait = %v after the tree exited, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Wait did not return after the tree exited")
	}
	if n := e.pending(); n != 0 {
		t.Errorf("%d processes still recorded after their trees exited", n)
	}
}

// The limit counts from the retirement, not from each wait: once it has
// passed, every later start fails at once, so repeated failures reach the
// engine's give-up latch instead of each retry waiting afresh.
func TestPendingExits_LimitCountsFromRetirement(t *testing.T) {
	e := NewPendingExits(10*time.Minute, time.Millisecond)
	clock := time.Unix(1_000_000, 0)
	var mu sync.Mutex
	e.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	advance := func(d time.Duration) { mu.Lock(); clock = clock.Add(d); mu.Unlock() }

	p := newTreeProc(4242, true)
	e.Add("ollama", p)
	advance(9 * time.Minute)
	// A second Add of the same process (Stop, then the reap before a
	// respawn) must not restart the clock.
	e.Add("ollama", p)
	advance(2 * time.Minute)

	start := time.Now()
	err := e.Wait(context.Background())
	if !errors.Is(err, ErrPreviousEngineStillExiting) {
		t.Fatalf("Wait = %v, want ErrPreviousEngineStillExiting", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("Wait took %s past the limit, want an immediate failure", time.Since(start))
	}
	if n := e.pending(); n != 1 {
		t.Errorf("recorded = %d after the limit, want the process kept (it is still running)", n)
	}
	// Still failing at once on the next start.
	if err := e.Wait(context.Background()); !errors.Is(err, ErrPreviousEngineStillExiting) {
		t.Fatalf("second Wait = %v, want ErrPreviousEngineStillExiting", err)
	}
}

// A Stop or Park cancels the start that is waiting.
func TestPendingExits_WaitReturnsOnCancel(t *testing.T) {
	e := NewPendingExits(time.Hour, time.Millisecond)
	e.Add("vllm", newTreeProc(7, true))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Wait(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Wait = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Wait did not return on cancel")
	}
}

// Two starts can wait on this record at once, because the engines share
// it: the ollama adapter, the vLLM adapter the bootstrap rebuilds, and the
// host-speed probe's own. Each entry is read and then written per sweep, so
// the sweep holds the lock throughout — run with -race, this is where a
// per-field lock showed up. The kill stays at one for the entry, not one
// per waiter.
func TestPendingExits_ConcurrentWaitersShareOneKill(t *testing.T) {
	e := NewPendingExits(time.Minute, time.Millisecond)
	p := newTreeProc(4242, true)
	e.Add("ollama", p)

	const waiters = 4
	done := make(chan error, waiters)
	for range waiters {
		go func() { done <- e.Wait(context.Background()) }()
	}
	time.Sleep(50 * time.Millisecond)
	p.alive.Store(false)
	for range waiters {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Wait = %v, want nil once the tree exited", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("a waiter did not return after the tree exited")
		}
	}
	if k := p.killCount(); k != 1 {
		t.Errorf("Kill called %d times, want 1 for the entry however many starts waited", k)
	}
}

// A tree that cannot be read is dropped: a start is not blocked on an
// answer nobody can give.
func TestPendingExits_UnreadableTreeDoesNotBlock(t *testing.T) {
	e := NewPendingExits(time.Hour, time.Millisecond)
	p := newTreeProc(7, true)
	p.treeErr = errors.New("handle closed")
	e.Add("ollama", p)
	if err := e.Wait(context.Background()); err != nil {
		t.Fatalf("Wait = %v, want nil", err)
	}
	if n := e.pending(); n != 0 {
		t.Errorf("recorded = %d, want the unreadable process dropped", n)
	}
}
