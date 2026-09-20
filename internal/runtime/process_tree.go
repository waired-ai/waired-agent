package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"syscall"
	"time"
)

// ErrPreviousEngineStillExiting is returned by a start that gave up waiting
// for the processes of an engine it retired earlier to exit. The start did
// not spawn: a new engine beside a runner that still holds a large model's
// memory is how a unified-memory host ran out of it
// (waired-ai/waired-agent#1443).
var ErrPreviousEngineStillExiting = errors.New("processes started by a previous engine are still running")

// processTreeFacts is what a spawner could read about its child's
// process tree at one moment. Each platform fills the fields it has:
//
//   - linux and darwin: GroupProbeErr is kill(-pgid, 0). Linux also lists
//     the group's members, because a member that has exited but was never
//     reaped still answers the probe. That happens when waired-agent runs
//     as PID 1 of a container (build/Dockerfile.waired-agent execs it), where
//     the engine's orphaned runners are re-parented to waired-agent and
//     nothing waits for them.
//   - windows: JobActive is the Job Object's ActiveProcesses.
type processTreeFacts struct {
	GroupProbeErr error
	Members       []treeMember
	MembersErr    error
	JobActive     uint32
	JobQueryErr   error
}

// treeMember is one process of a Unix process group.
type treeMember struct {
	PID    int
	Zombie bool
}

// treeAliveFrom decides whether the tree is still running. An error means
// the facts do not answer the question.
//
// Where the members could not be listed after the probe found some, the
// answer is "alive": the probe is certain that a process exists, and the
// listing only exists to discount zombies.
func treeAliveFrom(goos string, f processTreeFacts) (bool, error) {
	if goos == "windows" {
		if f.JobQueryErr != nil {
			return false, f.JobQueryErr
		}
		return f.JobActive > 0, nil
	}
	switch {
	case errors.Is(f.GroupProbeErr, syscall.ESRCH):
		return false, nil
	case f.GroupProbeErr != nil && !errors.Is(f.GroupProbeErr, syscall.EPERM):
		return false, f.GroupProbeErr
	}
	if goos != "linux" {
		// darwin: no container runs waired-agent as PID 1 there, and
		// launchd reaps orphans at once, so the probe alone is the answer.
		return true, nil
	}
	if f.MembersErr != nil {
		return true, nil
	}
	for _, m := range f.Members {
		if !m.Zombie {
			return true, nil
		}
	}
	return false, nil
}

// PendingExits records engine processes that were retired (stopped or
// killed) while their process trees may still be running, and makes a
// start wait until they are gone.
//
// One instance is shared by every engine on a host. The engines share the
// memory, so an ollama runner that is still exiting blocks a vLLM start as
// much as an ollama one, and the vLLM adapter is rebuilt on every
// bootstrap, so a record kept on an adapter would be forgotten with it.
//
// The time limit counts from when a process was retired, not from each
// wait. Every start after the limit fails at once, so the failures add up
// to the engine's give-up latch instead of each retry waiting afresh.
type PendingExits struct {
	limit time.Duration
	poll  time.Duration
	now   func() time.Time

	mu      sync.Mutex
	entries map[RunningProcess]*pendingExit
}

type pendingExit struct {
	engine    string
	pid       int
	retiredAt time.Time
	killed    bool
	logged    bool
}

// NewPendingExits returns a record with the given limit and poll interval;
// zero picks DefaultTreeExitTimeout and one second.
func NewPendingExits(limit, poll time.Duration) *PendingExits {
	if limit <= 0 {
		limit = DefaultTreeExitTimeout
	}
	if poll <= 0 {
		poll = treeExitPoll
	}
	return &PendingExits{limit: limit, poll: poll, now: time.Now, entries: map[RunningProcess]*pendingExit{}}
}

// Add records proc, retired by engine. A process that cannot report on
// its tree is not recorded. Adding a process already recorded keeps the
// first time: the adapters stop the same child twice (Stop, then the reap
// before a respawn), and the limit counts from the first.
func (e *PendingExits) Add(engine string, proc RunningProcess) {
	if e == nil || proc == nil {
		return
	}
	if _, ok := proc.(ProcessTree); !ok {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.entries[proc]; ok {
		return
	}
	e.entries[proc] = &pendingExit{engine: engine, pid: proc.PID(), retiredAt: e.now()}
}

// Wait blocks until no recorded tree is still running.
//
// It finishes the kill before it waits. A graceful stop ends once the child
// exits, and on Unix that can leave the child's own children alive with
// only a SIGTERM delivered. Kill is idempotent on both spawners.
//
// It runs on a start's context, never a stop's: the limit is minutes, and
// no caller with a short budget (the tray, the management API, shutdown)
// reaches it. A Stop or Park cancels the start, and Wait returns ctx.Err().
//
// A tree whose state cannot be read is dropped from the record. That is
// the behaviour before this wait existed, and blocking a start on an answer
// nobody can give would be worse.
func (e *PendingExits) Wait(ctx context.Context) error {
	if e == nil {
		return nil
	}
	tick := time.NewTicker(e.poll)
	defer tick.Stop()
	for {
		stuck, err := e.sweep()
		if err != nil || !stuck {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// sweep asks every recorded tree once. It reports whether any is still
// running, and an error once one has been running past the limit.
//
// The whole sweep holds e.mu, including the TreeAlive and Kill calls. Two
// starts can wait on this record at once — the engines share it — and each
// entry is read and then written here, so anything less is a race. It also
// keeps the kill and the "still running" warning to one per entry rather
// than one per waiter. The calls under the lock are a process-group probe,
// a /proc read or a Job Object query, none of which block.
func (e *PendingExits) sweep() (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	stuck := false
	for p, ent := range e.entries {
		alive, err := p.(ProcessTree).TreeAlive()
		if err != nil {
			slog.Warn(ent.engine+": cannot tell whether the previous engine's processes have exited; starting anyway",
				"pid", ent.pid, "err", err)
			delete(e.entries, p)
			continue
		}
		if !alive {
			if ent.logged {
				slog.Info(ent.engine+": the previous engine's processes have exited",
					"pid", ent.pid, "since_retired", e.now().Sub(ent.retiredAt).Round(time.Second))
			}
			delete(e.entries, p)
			continue
		}
		if !ent.killed {
			if kerr := p.Kill(); kerr != nil {
				slog.Warn(ent.engine+": kill of the previous engine's processes failed; waiting for them anyway",
					"pid", ent.pid, "err", kerr)
			}
			ent.killed = true
		}
		since := e.now().Sub(ent.retiredAt)
		if since >= e.limit {
			return true, fmt.Errorf("%s: %w (pid %d, retired %s ago); not starting a new engine while they may still hold memory",
				ent.engine, ErrPreviousEngineStillExiting, ent.pid, since.Round(time.Second))
		}
		if !ent.logged {
			slog.Warn(ent.engine+": processes started by the previous engine are still running; waiting for them to exit before starting an engine",
				"pid", ent.pid, "limit", e.limit)
			ent.logged = true
		}
		stuck = true
	}
	return stuck, nil
}

// pending reports how many processes are recorded, for tests.
func (e *PendingExits) pending() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.entries)
}
