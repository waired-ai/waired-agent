//go:build linux || darwin

package runtime

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
)

// DefaultSpawner runs commands via os/exec in a separate process group
// so signals sent to waired-agent's group don't leak into ollama
// (and vice versa).
//
// The child inherits waired-agent's cwd. The working-directory override
// went with the bundled coding agent (waired-agent#333); every engine
// adapter always relied on the inherited cwd.
type DefaultSpawner struct{}

// Spawn implements Spawner.
func (s DefaultSpawner) Spawn(ctx context.Context, binary string, args, env []string, logW io.Writer) (RunningProcess, error) {
	// The context bounds only the START, never the child's lifetime (#947).
	// exec.CommandContext would bind the two: its cancel is Process.Kill(),
	// a single-pid SIGKILL that bypasses the process-group broadcast in
	// osProcess.Kill below — so a caller's cancellation would leave the
	// engine's own children (vLLM's workers, ollama's model runner) alive
	// and still holding VRAM. Termination is the adapter's job, through
	// Stop/Park, which signal the whole group and wait for the reap.
	_ = ctx
	cmd := exec.Command(binary, args...)
	cmd.Env = env
	if logW != nil {
		cmd.Stdout = logW
		cmd.Stderr = logW
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &osProcess{cmd: cmd, done: make(chan struct{})}
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
	// treeGone latches the first observation that the process group is
	// empty. After that the group id is free, and a later probe could find
	// an unrelated group that happens to have been given the same id.
	treeGone atomic.Bool
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

// Signal forwards s to the entire process group (DefaultSpawner sets
// Setpgid:true so the leader pid is also the pgid). vLLM uses Python
// multiprocessing — signaling only the leader leaves worker children
// holding GPU memory; broadcasting to -pgid catches them too.
// Falls back to per-process signal if the syscall fails (e.g. process
// already exited and pgid is reaped).
func (p *osProcess) Signal(s os.Signal) error {
	// Once TreeAlive has seen the group empty its id is free for reuse,
	// and a broadcast could reach an unrelated group.
	if sig, ok := s.(syscall.Signal); ok && !p.treeGone.Load() {
		if err := syscall.Kill(-p.cmd.Process.Pid, sig); err == nil {
			return nil
		}
	}
	return p.cmd.Process.Signal(s)
}

// Kill broadcasts SIGKILL to the process group for the same reason
// Signal does — orphan vLLM workers otherwise survive the leader's
// death and keep VRAM pinned.
func (p *osProcess) Kill() error {
	if p.treeGone.Load() {
		return nil // nothing left, and the group id may belong to someone else
	}
	if err := syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL); err == nil {
		return nil
	}
	return p.cmd.Process.Kill()
}

// TreeAlive reports whether any member of the child's process group is
// still running. DefaultSpawner sets Setpgid, so the group id is the
// child's pid, and ollama's runners and vLLM's workers join it — neither
// engine moves its children to a group of their own (ollama v0.34.0
// llm/llm_linux.go and llm_darwin.go set no Setpgid; vLLM's
// multiprocessing workers inherit the group).
//
// Signal 0 probes without delivering anything: ESRCH means no process is
// left in the group, and EPERM means one exists that this process may not
// signal. When the probe finds members, the group is listed to discount
// the ones that have exited but were never reaped (see processTreeFacts);
// treeAliveFrom makes the call.
func (p *osProcess) TreeAlive() (bool, error) {
	if p.treeGone.Load() {
		return false, nil
	}
	pgid := p.cmd.Process.Pid
	f := processTreeFacts{GroupProbeErr: syscall.Kill(-pgid, 0)}
	if f.GroupProbeErr == nil || errors.Is(f.GroupProbeErr, syscall.EPERM) {
		f.Members, f.MembersErr = groupMembers(pgid)
	}
	alive, err := treeAliveFrom(runtime.GOOS, f)
	if err == nil && !alive {
		p.treeGone.Store(true)
	}
	return alive, err
}
