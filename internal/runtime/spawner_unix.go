//go:build !windows

package runtime

import (
	"context"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
)

// DefaultSpawner runs commands via os/exec in a separate process group
// so signals sent to waired-agent's group don't leak into ollama
// (and vice versa).
type DefaultSpawner struct {
	// Dir, when non-empty, is the child's working directory. Empty
	// inherits the parent's cwd (the historical behaviour, kept for
	// the engine adapters). The bundled OpenCode coding-agent sets it
	// so `opencode serve` scopes its project to a dedicated scratch
	// workspace instead of waired-agent's cwd (which can be `/`).
	Dir string
}

// Spawn implements Spawner.
func (s DefaultSpawner) Spawn(ctx context.Context, binary string, args, env []string, logW io.Writer) (RunningProcess, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = env
	cmd.Dir = s.Dir
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
	if sig, ok := s.(syscall.Signal); ok {
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
	if err := syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL); err == nil {
		return nil
	}
	return p.cmd.Process.Kill()
}
