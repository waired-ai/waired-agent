package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

func TestOllamaVersionWarning(t *testing.T) {
	pin := infruntime.OllamaPinnedVersion
	cases := []struct {
		name     string
		borrowed bool
		live     string
		warn     bool
	}{
		{"bundled live matches pin", false, pin, false},
		{"bundled live differs", false, "0.24.0", true},
		{"bundled unknown live", false, "", false},
		{"reuse above floor", true, "0.24.0", false},
		{"reuse below floor", true, "0.5.0", true},
		{"reuse unknown live", true, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ollamaVersionWarning(tc.borrowed, tc.live)
			if (got != "") != tc.warn {
				t.Errorf("ollamaVersionWarning(borrowed=%v, live=%q) = %q, want warning=%v",
					tc.borrowed, tc.live, got, tc.warn)
			}
		})
	}
}

// deadSpawner's child exits immediately, as `ollama serve` does when
// the port is already bound.
type deadSpawner struct{}

func (deadSpawner) Spawn(context.Context, string, []string, []string, io.Writer) (infruntime.RunningProcess, error) {
	p := newFakeProc()
	_ = p.Kill()
	return p, nil
}

// TestEngineController_AdoptedNotManaged: an adopted orphan has no
// process handle, so the power axis must report it unmanaged and
// refuse to park it.
func TestEngineController_AdoptedNotManaged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if r.URL.Path == "/api/version" {
			_, _ = w.Write([]byte(`{"version":"9.9.9"}`))
			return
		}
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	t.Cleanup(srv.Close)
	host, port := hostPort(t, srv.URL)
	a := infruntime.NewOllamaAdapter(infruntime.OllamaConfig{
		Binary: "/fake/ollama", Host: host, Port: port,
		Spawner: deadSpawner{}, HTTPClient: srv.Client(),
		ExpectedVersion: "9.9.9",
		HealthInterval:  5 * time.Millisecond, HealthSuccess: 2, HealthMaxFails: 50,
		StopTimeout: 50 * time.Millisecond,
	})
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning should adopt the exact-pin orphan: %v", err)
	}
	if got := a.Mode(); got != infruntime.EngineModeAdopted {
		t.Fatalf("Mode() = %s, want adopted", got)
	}

	ec := newEngineController(context.Background(), a, nil)
	power, managed := ec.EngineState()
	if power != management.EnginePowerRunning || managed {
		t.Errorf("EngineState = %s managed=%v, want running/false", power, managed)
	}
	if err := ec.StopEngine(context.Background()); !errors.Is(err, infruntime.ErrEngineNotOwned) {
		t.Errorf("StopEngine on adopted engine = %v, want ErrEngineNotOwned", err)
	}
}
