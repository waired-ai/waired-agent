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
type DefaultSpawner struct {
	// Dir, when non-empty, is the child's working directory. Empty
	// inherits the parent's cwd (the historical behaviour, kept for
	// the engine adapters). The bundled OpenCode coding-agent sets it
	// so `opencode serve` scopes its project to a dedicated scratch
	// workspace instead of waired-agent's cwd.
	Dir string
}

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

	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = env
	cmd.Dir = s.Dir
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

// Signal on Windows has no SIGTERM-equivalent for arbitrary processes.
// Returning nil keeps the adapter's Stop loop intact: the loop waits
// `StopTimeout` for the child to exit on its own (it won't, absent a
// graceful HTTP-shutdown probe driven by the adapter), then escalates
// to Kill which closes the Job Object and reaps the tree.
//
// Engine-specific graceful shutdown (e.g. Ollama's POST /api/shutdown)
// is the adapter's responsibility on Windows, not the spawner's.
// Documented as a Phase W-1 trade-off in docs/decisions.md (20260514).
func (p *osProcess) Signal(_ os.Signal) error {
	return nil
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
