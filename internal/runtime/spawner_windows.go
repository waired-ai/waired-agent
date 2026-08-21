//go:build windows

package runtime

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
)

// DefaultSpawner runs commands via os/exec and assigns each child to a
// Windows Job Object configured with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE.
// Closing the job handle (on Kill or process exit) terminates every
// process descended from the child — Windows' only reliable equivalent
// of "signal the process group". The kernel reaps grandchildren even
// if the immediate child was killed first, which is what we need for
// Ollama (it spawns model-runner subprocesses that hold GPU memory).
//
// The child inherits waired-agent's cwd. The working-directory override
// went with the bundled coding agent (waired-agent#333); every engine
// adapter always relied on the inherited cwd.
type DefaultSpawner struct{}

// Spawn implements Spawner.
func (s DefaultSpawner) Spawn(ctx context.Context, binary string, args, env []string, logW io.Writer) (RunningProcess, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("runtime: CreateJobObject: %w", err)
	}

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("runtime: SetInformationJobObject: %w", err)
	}

	// The context bounds only the START, never the child's lifetime (#947).
	// exec.CommandContext would bind the two: its cancel is Process.Kill(),
	// which terminates the immediate child only — the Job Object below is
	// what reaps the descendants holding GPU memory, and it is closed by
	// Kill or by the process exiting, not by a caller's context.
	_ = ctx
	cmd := exec.Command(binary, args...)
	cmd.Env = env
	if logW != nil {
		cmd.Stdout = logW
		cmd.Stderr = logW
	}
	if err := cmd.Start(); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}

	ph, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(cmd.Process.Pid),
	)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("runtime: OpenProcess: %w", err)
	}
	if err := windows.AssignProcessToJobObject(job, ph); err != nil {
		_ = windows.CloseHandle(ph)
		_ = cmd.Process.Kill()
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("runtime: AssignProcessToJobObject: %w", err)
	}
	_ = windows.CloseHandle(ph)

	p := &osProcess{cmd: cmd, job: job, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		if err != nil {
			p.errStore.Store(err)
		}
		close(p.done)
	}()
	return p, nil
}

type osProcess struct {
	cmd       *exec.Cmd
	job       windows.Handle
	jobClosed atomic.Bool
	done      chan struct{}
	errStore  atomicErr
}

type atomicErr struct {
	mu  sync.Mutex
	err error
}

func (a *atomicErr) Store(e error) {
	a.mu.Lock()
	a.err = e
	a.mu.Unlock()
}
func (a *atomicErr) Load() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.err
}

func (p *osProcess) PID() int              { return p.cmd.Process.Pid }
func (p *osProcess) Done() <-chan struct{} { return p.done }
func (p *osProcess) Err() error            { return p.errStore.Load() }

// Signal on Windows has no SIGTERM-equivalent for arbitrary processes, so
// it reports ErrSignalUnsupported instead of pretending to have delivered
// one. That distinction is load-bearing: returning nil (the pre-#316
// behaviour) made the adapter wait out the full `StopTimeout` for an exit
// that could never come, and the tray's shorter budget always won that
// race — the stop was cancelled before it ever reached the Kill
// escalation, so the engine kept its VRAM while status reported it
// stopped. With the sentinel, Stop escalates immediately and Kill closes
// the Job Object, reaping the whole tree.
//
// Engine-specific graceful shutdown (e.g. Ollama's POST /api/shutdown)
// remains the adapter's responsibility on Windows, not the spawner's; see
// the Phase W-1 subprocess-management decision (Unix = pgid + SIGTERM,
// Windows = Job Object) in the internal decision log, and
// docs/decisions/20260801/*-engine-stop-commit-to-kill.md here.
func (p *osProcess) Signal(_ os.Signal) error {
	return ErrSignalUnsupported
}

// Kill terminates the entire job (child + every descendant) by closing
// the Job Object handle. Idempotent — repeated calls after the first
// CloseHandle no-op.
func (p *osProcess) Kill() error {
	if p.jobClosed.CompareAndSwap(false, true) {
		if err := windows.CloseHandle(p.job); err != nil {
			// Fall back to a direct TerminateProcess on the leader
			// only — orphaned grandchildren are accepted as a
			// pathological case (Job handle close should never fail).
			return p.cmd.Process.Kill()
		}
	}
	return nil
}
