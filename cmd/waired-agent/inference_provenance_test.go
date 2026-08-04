package main

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

func TestOllamaVersionWarning(t *testing.T) {
	pin := infruntime.OllamaPinnedVersion
	cases := []struct {
		name string
		live string
		warn bool
	}{
		{"live matches pin", pin, false},
		{"live differs", "0.24.0", true},
		// Since #489 the serving engine is always waired's own, so a
		// version below the old reuse floor is a mismatch like any other
		// — there is no "the user's engine is merely old" case left.
		{"live far below the pin", "0.5.0", true},
		{"unknown live", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ollamaVersionWarning(tc.live)
			if (got != "") != tc.warn {
				t.Errorf("ollamaVersionWarning(live=%q) = %q, want warning=%v",
					tc.live, got, tc.warn)
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
	a := newAdoptedTestAdapter(t)

	ec := newEngineController(context.Background(), a, nil)
	power, managed := ec.EngineState()
	if power != management.EnginePowerRunning || managed {
		t.Errorf("EngineState = %s managed=%v, want running/false", power, managed)
	}
	if err := ec.StopEngine(context.Background()); !errors.Is(err, infruntime.ErrEngineNotOwned) {
		t.Errorf("StopEngine on adopted engine = %v, want ErrEngineNotOwned", err)
	}
}
