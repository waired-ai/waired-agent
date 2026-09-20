//go:build windows

package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// DefaultSpawner runs commands via os/exec and assigns each child to a
// Windows Job Object configured with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE.
// Kill terminates every process in the job — Windows' only reliable
// equivalent of "signal the process group". The kernel reaches
// grandchildren even if the immediate child was killed first, which is
// what we need for Ollama (it spawns model-runner subprocesses that hold
// GPU memory).
//
// The job is NOT ended by the child exiting on its own. If ollama serve
// crashes, its runners stay alive in the job until something calls Kill,
// which the adapter does when it reaps the dead child before a respawn.
// The handle stays open after Kill until TreeAlive has seen the job
// empty, because the handle is the only way to ask
// (waired-ai/waired-agent#1443). If waired-agent itself exits, the OS
// closes the handle and KILL_ON_JOB_CLOSE ends whatever is left.
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
	// what reaps the descendants holding GPU memory, and it is terminated
	// by Kill, not by a caller's context.
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
	cmd      *exec.Cmd
	done     chan struct{}
	errStore atomicErr

	// jobMu guards the three fields below. The handle is closed exactly
	// once: by TreeAlive when it sees the job empty, or by Kill when
	// terminating the job failed and closing the handle is the fallback.
	jobMu     sync.Mutex
	job       windows.Handle
	jobClosed bool
	// treeGone records that TreeAlive saw the job empty. Only then is a
	// closed handle an answer rather than a lost question.
	treeGone bool
}

// jobBasicAccounting is JOBOBJECT_BASIC_ACCOUNTING_INFORMATION, which
// golang.org/x/sys/windows does not define. The four times are
// LARGE_INTEGERs.
type jobBasicAccounting struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

// errJobHandleClosed is TreeAlive's answer when the handle was closed by
// Kill's fallback before anything saw the job empty.
var errJobHandleClosed = errors.New("runtime: job handle already closed; the process tree cannot be observed")

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
// stopped. With the sentinel, Stop escalates immediately and Kill
// terminates the Job Object, reaching the whole tree.
//
// Engine-specific graceful shutdown (e.g. Ollama's POST /api/shutdown)
// remains the adapter's responsibility on Windows, not the spawner's; see
// the Phase W-1 subprocess-management decision (Unix = pgid + SIGTERM,
// Windows = Job Object) in the internal decision log, and
// docs/decisions/20260801/*-engine-stop-commit-to-kill.md here.
func (p *osProcess) Signal(_ os.Signal) error {
	return ErrSignalUnsupported
}

// Kill terminates every process in the job (child + every descendant)
// with TerminateJobObject. The handle stays open so TreeAlive can tell
// when the processes are actually gone: TerminateJobObject only starts the
// termination, and a process whose thread is inside a driver call does not
// finish exiting until the call returns (waired-ai/waired-agent#1443).
// Idempotent, and a no-op once the tree is gone.
func (p *osProcess) Kill() error {
	p.jobMu.Lock()
	defer p.jobMu.Unlock()
	if p.jobClosed {
		return nil
	}
	if err := windows.TerminateJobObject(p.job, 1); err != nil {
		// Closing the handle ends the job too (KILL_ON_JOB_CLOSE), but
		// leaves nothing to ask afterwards; TreeAlive then reports an
		// error, which callers treat as "nothing left to wait for". The
		// leader is killed directly as well, as before this change.
		p.closeJobLocked()
		if kerr := p.cmd.Process.Kill(); kerr != nil && !errors.Is(kerr, os.ErrProcessDone) {
			return fmt.Errorf("runtime: TerminateJobObject: %w; TerminateProcess: %v", err, kerr)
		}
	}
	return nil
}

// TreeAlive reports whether any process is still in the job. The first
// time it sees the job empty it closes the handle, so a job whose
// processes have all exited does not hold a handle for the rest of the
// agent's life.
func (p *osProcess) TreeAlive() (bool, error) {
	p.jobMu.Lock()
	defer p.jobMu.Unlock()
	if p.treeGone {
		return false, nil
	}
	if p.jobClosed {
		return false, errJobHandleClosed
	}
	var info jobBasicAccounting
	f := processTreeFacts{}
	if err := windows.QueryInformationJobObject(p.job,
		windows.JobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err != nil {
		f.JobQueryErr = fmt.Errorf("runtime: QueryInformationJobObject: %w", err)
	}
	f.JobActive = info.ActiveProcesses
	alive, err := treeAliveFrom("windows", f)
	if err != nil || alive {
		return alive, err
	}
	p.treeGone = true
	p.closeJobLocked()
	return false, nil
}

func (p *osProcess) closeJobLocked() {
	if !p.jobClosed {
		_ = windows.CloseHandle(p.job)
		p.jobClosed = true
	}
}
