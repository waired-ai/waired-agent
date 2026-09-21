//go:build linux

package runtime

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// The adapter half of the start wait (waired-agent#1508). Linux-only like
// the rest of the vLLM adapter tests: vLLM runs only there, and the fake
// server lives in vllm_test.go.

// busyProcess is a fakeProcess whose group burns a whole core, as the
// flashinfer compile does.
type busyProcess struct {
	*fakeProcess
	since time.Time
}

func (p *busyProcess) TreeCPUTime() (time.Duration, error) { return time.Since(p.since), nil }

type busySpawner struct {
	fakeSpawner
	proc *busyProcess
}

func (s *busySpawner) Spawn(ctx context.Context, binary string, args, env []string, logW io.Writer) (RunningProcess, error) {
	if _, err := s.fakeSpawner.Spawn(ctx, binary, args, env, logW); err != nil {
		return nil, err
	}
	s.proc = &busyProcess{fakeProcess: s.process, since: time.Now()}
	return s.proc, nil
}

// The adapter keeps waiting past the stall window while the engine writes,
// and while it only burns CPU; with neither, it gives up and says why
// (waired-agent#1508).
func TestVLLMAdapter_StartWaitsWhileTheEngineWorks(t *testing.T) {
	const stall = 80 * time.Millisecond
	newAdapter := func(t *testing.T, sp Spawner, logDir string, server *vllmFakeServer) *VLLMAdapter {
		host, port := server.hostPort(t)
		return NewVLLMAdapter(VLLMConfig{
			Python: "/venv/bin/python", Host: host, Port: port,
			Model: "/m", ServedModelName: "x",
			Spawner: sp, HTTPClient: vllmHTTPClient(),
			HealthInterval: 10 * time.Millisecond, HealthSuccess: 1,
			StartStallTimeout: stall, StartTimeout: time.Minute,
			StopTimeout: 50 * time.Millisecond,
			LogDir:      logDir,
		})
	}

	t.Run("writing", func(t *testing.T) {
		server := newVLLMFakeServer("x")
		defer server.srv.Close()
		spawner := &fakeSpawner{}
		a := newAdapter(t, spawner, t.TempDir(), server)
		errc := make(chan error, 1)
		go func() { errc <- a.EnsureRunning(context.Background()) }()
		var logW io.Writer
		for deadline := time.Now().Add(5 * time.Second); logW == nil; time.Sleep(time.Millisecond) {
			spawner.mu.Lock()
			if spawner.lastLogW != nil {
				logW = spawner.lastLogW
			}
			spawner.mu.Unlock()
			if time.Now().After(deadline) {
				t.Fatal("the engine was never spawned")
			}
		}
		for end := time.Now().Add(6 * stall); time.Now().Before(end); time.Sleep(stall / 4) {
			_, _ = logW.Write([]byte("Loading safetensors checkpoint shards\n"))
		}
		server.healthy.Store(true)
		if err := <-errc; err != nil {
			t.Fatalf("EnsureRunning = %v; the engine wrote throughout and then answered", err)
		}
		spawner.process.exit(nil)
	})

	t.Run("busy but silent", func(t *testing.T) {
		server := newVLLMFakeServer("x")
		defer server.srv.Close()
		spawner := &busySpawner{}
		a := newAdapter(t, spawner, "", server)
		time.AfterFunc(6*stall, func() { server.healthy.Store(true) })
		if err := a.EnsureRunning(context.Background()); err != nil {
			t.Fatalf("EnsureRunning = %v; the engine kept a core busy and then answered", err)
		}
		spawner.proc.exit(nil)
	})

	t.Run("nothing moves", func(t *testing.T) {
		server := newVLLMFakeServer("x")
		defer server.srv.Close()
		a := newAdapter(t, &fakeSpawner{}, t.TempDir(), server)
		err := a.EnsureRunning(context.Background())
		if err == nil || !strings.Contains(err.Error(), "no output and no CPU use") {
			t.Fatalf("EnsureRunning = %v, want the stall named", err)
		}
		if a.Health(context.Background()).State != StateFailed {
			t.Errorf("state = %+v, want StateFailed", a.Health(context.Background()))
		}
	})

	t.Run("working past the ceiling", func(t *testing.T) {
		server := newVLLMFakeServer("x")
		defer server.srv.Close()
		host, port := server.hostPort(t)
		spawner := &busySpawner{}
		a := NewVLLMAdapter(VLLMConfig{
			Python: "/venv/bin/python", Host: host, Port: port,
			Model: "/m", ServedModelName: "x",
			Spawner: spawner, HTTPClient: vllmHTTPClient(),
			HealthInterval: 10 * time.Millisecond, HealthSuccess: 1,
			StartStallTimeout: time.Minute, StartTimeout: 100 * time.Millisecond,
			StopTimeout: 50 * time.Millisecond,
		})
		err := a.EnsureRunning(context.Background())
		if err == nil || !strings.Contains(err.Error(), "although still working") {
			t.Fatalf("EnsureRunning = %v, want the ceiling named", err)
		}
	})

	t.Run("state is starting during the wait", func(t *testing.T) {
		server := newVLLMFakeServer("x")
		defer server.srv.Close()
		spawner := &busySpawner{}
		a := newAdapter(t, spawner, "", server)
		errc := make(chan error, 1)
		go func() { errc <- a.EnsureRunning(context.Background()) }()
		time.Sleep(3 * stall)
		if st := a.Health(context.Background()).State; st != StateStarting {
			t.Errorf("state after 3 stall windows of work = %v, want StateStarting", st)
		}
		server.healthy.Store(true)
		if err := <-errc; err != nil {
			t.Fatalf("EnsureRunning = %v", err)
		}
		spawner.proc.exit(nil)
	})
}
