//go:build integration

package runtime_test

import (
	"context"
	"net"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/runtime"
)

// TestOllamaAdapter_RealSubprocess exercises the production code path:
// it spawns a real `ollama serve`, waits for the readiness probe to
// pass, and then stops it gracefully. The test is opt-in (build tag
// `integration`) so the default test run stays fast and dep-free.
//
// Run with: go test -tags integration ./internal/runtime/...
func TestOllamaAdapter_RealSubprocess(t *testing.T) {
	bin, err := exec.LookPath("ollama")
	if err != nil {
		t.Skipf("ollama not installed: %v", err)
	}

	port := freePort(t)
	a := runtime.NewOllamaAdapter(runtime.OllamaConfig{
		Binary:         bin,
		Host:           "127.0.0.1",
		Port:           port,
		Spawner:        runtime.DefaultSpawner{},
		HealthInterval: 500 * time.Millisecond,
		HealthSuccess:  1,
		HealthMaxFails: 60, // 30s of retries before giving up
		StopTimeout:    5 * time.Second,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := a.EnsureRunning(ctx); err != nil {
		t.Fatalf("EnsureRunning real ollama: %v", err)
	}
	if h := a.Health(ctx); h.State != runtime.StateReady {
		t.Errorf("state after EnsureRunning = %s, want %s (last err=%q)", h.State, runtime.StateReady, h.LastErr)
	}

	if err := a.Stop(context.Background()); err != nil {
		t.Errorf("Stop real ollama: %v", err)
	}
	if h := a.Health(context.Background()); h.State != runtime.StateStopped {
		t.Errorf("state after Stop = %s, want %s", h.State, runtime.StateStopped)
	}
}

// freePort asks the kernel for an unused TCP port so the test doesn't
// collide with a developer's already-running ollama on 11434.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	addr := l.Addr().(*net.TCPAddr)
	return mustAtoi(t, strconv.Itoa(addr.Port))
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("atoi %q: %v", s, err)
	}
	return n
}
