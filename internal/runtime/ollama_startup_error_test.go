package runtime

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTailEngineLog(t *testing.T) {
	t.Run("unset path", func(t *testing.T) {
		if got := tailEngineLog("", 4096); got != "" {
			t.Errorf(`tailEngineLog("") = %q, want ""`, got)
		}
	})
	t.Run("missing file", func(t *testing.T) {
		if got := tailEngineLog(filepath.Join(t.TempDir(), "nope.log"), 4096); got != "" {
			t.Errorf("missing file = %q, want empty", got)
		}
	})
	t.Run("empty file", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "engine.log")
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if got := tailEngineLog(p, 4096); got != "" {
			t.Errorf("empty file = %q, want empty", got)
		}
	})
	t.Run("nonpositive max", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "engine.log")
		if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := tailEngineLog(p, 0); got != "" {
			t.Errorf("maxBytes=0 = %q, want empty", got)
		}
	})
	t.Run("shorter than max returns trimmed all", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "engine.log")
		if err := os.WriteFile(p, []byte("  line one\nline two\n  "), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := tailEngineLog(p, 4096); got != "line one\nline two" {
			t.Errorf("tail = %q, want trimmed full content", got)
		}
	})
	t.Run("longer than max returns last bytes", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "engine.log")
		body := strings.Repeat("A", 100) + "TAILMARKER"
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := tailEngineLog(p, 10); got != "TAILMARKER" {
			t.Errorf("tail = %q, want the last 10 bytes %q", got, "TAILMARKER")
		}
	})
}

func TestStartupExitError(t *testing.T) {
	t.Run("with engine.log tail", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "engine.log")
		if err := os.WriteFile(p, []byte("ggml_metal_init: no Metal device found\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		msg := startupExitError("ollama", p, errors.New("exit status 1")).Error()
		for _, want := range []string{
			"ollama: process exited during startup: exit status 1",
			"no Metal device found", // the real reason, folded in from engine.log
			p,                       // the full-log path for follow-up
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("error %q missing %q", msg, want)
			}
		}
	})
	t.Run("no log content falls back to a bare exit", func(t *testing.T) {
		msg := startupExitError("vllm", "", errors.New("exit status 2")).Error()
		if msg != "vllm: process exited during startup: exit status 2" {
			t.Errorf("error = %q, want the bare exit message", msg)
		}
		if strings.Contains(msg, "stderr (tail") {
			t.Errorf("error %q should not carry a tail section when there is no log", msg)
		}
	})
}

// crashingLogSpawner writes canned "stderr" to the engine.log capture
// writer, then exits the child immediately — as `ollama serve` does when
// it prints a fatal startup error and dies (#22).
type crashingLogSpawner struct {
	content string
	exitErr error
}

func (s *crashingLogSpawner) Spawn(_ context.Context, _ string, _, _ []string, logW io.Writer) (RunningProcess, error) {
	if logW != nil && s.content != "" {
		_, _ = io.WriteString(logW, s.content)
	}
	p := newFakeProcess()
	p.exit(s.exitErr)
	return p, nil
}

// TestOllamaAdapter_EnsureRunning_CrashError_IncludesEngineLogTail proves
// the end-to-end #22 wiring: a spawned ollama that dies during startup
// surfaces its engine.log stderr through the returned error AND through the
// adapter's Health.LastErr (which runtimeStatusFor maps to the mgmt-API
// last_error), instead of a bare "exit status 1".
func TestOllamaAdapter_EnsureRunning_CrashError_IncludesEngineLogTail(t *testing.T) {
	// A closed port: nothing answers, so there is no survivor to adopt and
	// the original startup error is the one that surfaces.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	host, port := splitHostPort(t, srv.URL)
	srv.Close()

	dir := t.TempDir()
	const stderrLine = "ggml_metal_init: error: no Metal device; giving up"
	spawner := &crashingLogSpawner{content: stderrLine + "\n", exitErr: errors.New("exit status 1")}
	a := NewOllamaAdapter(OllamaConfig{
		Binary: "/fake/ollama", Host: host, Port: port,
		Spawner: spawner, HTTPClient: &http.Client{Timeout: 50 * time.Millisecond},
		ExpectedVersion: "0.30.7",
		HealthInterval:  5 * time.Millisecond, HealthSuccess: 3, HealthMaxFails: 50,
		StopTimeout: 100 * time.Millisecond,
		LogDir:      dir,
	})

	err := a.EnsureRunning(context.Background())
	if err == nil {
		t.Fatal("EnsureRunning should fail when the spawned engine dies at startup")
	}
	if !strings.Contains(err.Error(), "process exited during startup") {
		t.Errorf("error %q should carry the startup-exit wrapper", err.Error())
	}
	if !strings.Contains(err.Error(), stderrLine) {
		t.Errorf("error %q should include the engine.log stderr tail %q", err.Error(), stderrLine)
	}
	if h := a.Health(context.Background()); !strings.Contains(h.LastErr, stderrLine) {
		t.Errorf("Health.LastErr = %q, should include the engine.log tail %q", h.LastErr, stderrLine)
	}
	_ = a.Stop(context.Background())
}
