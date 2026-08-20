package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/management"
)

// fakeApplier records the real argument. A fake that dropped it would
// make the "the wrong value reached the engine" case unwritable.
type fakeApplier struct {
	current time.Duration
	present bool
	applied []time.Duration
	err     error
	effect  management.ResidencyEffect
}

func (f *fakeApplier) CurrentResidency() (time.Duration, bool) { return f.current, f.present }

func (f *fakeApplier) ApplyResidency(_ context.Context, idle time.Duration) (management.ResidencyEffect, error) {
	f.applied = append(f.applied, idle)
	if f.err != nil {
		return "", f.err
	}
	f.current, f.present = idle, true
	if f.effect == "" {
		return management.ResidencyEffectLive, nil
	}
	return f.effect, nil
}

func newTestResidencyController(t *testing.T, a residencyApplier) (*residencyController, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.json")
	seed := agentconfig.Defaults()
	// An unrelated field, so a read-modify-write that clobbers the file
	// is caught rather than merely suspected.
	seed.Inference.MaxCacheGB = 7
	if err := seed.Save(path); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return &residencyController{jsonPath: path, applierFn: func() residencyApplier { return a }}, path
}

func reloadIdle(t *testing.T, path string) time.Duration {
	t.Helper()
	cfg := agentconfig.Defaults()
	if err := cfg.MergeJSON(path); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cfg.Inference.MaxCacheGB != 7 {
		t.Errorf("persist clobbered an unrelated field: MaxCacheGB=%d, want 7", cfg.Inference.MaxCacheGB)
	}
	return cfg.Inference.IdleTimeout.Duration()
}

func TestResidencyControllerAppliesLiveAndPersists(t *testing.T) {
	fa := &fakeApplier{present: true}
	c, path := newTestResidencyController(t, fa)

	got, _, err := c.SetResidency(context.Background(), 45*time.Minute)
	if err != nil {
		t.Fatalf("SetResidency: %v", err)
	}
	if got != 45*time.Minute {
		t.Errorf("returned %v, want 45m", got)
	}
	if len(fa.applied) != 1 || fa.applied[0] != 45*time.Minute {
		t.Errorf("engine received %v, want [45m0s]", fa.applied)
	}
	if d := reloadIdle(t, path); d != 45*time.Minute {
		t.Errorf("persisted %v, want 45m", d)
	}
}

// TestResidencyControllerPersistsWhenEngineFails pins the half-success
// the operator has to be told about: the value survives a restart, but
// the model loaded right now is still on the old setting. Reporting only
// the error would hide that it WAS saved; reporting only success would
// hide that it did not take effect.
func TestResidencyControllerPersistsWhenEngineFails(t *testing.T) {
	fa := &fakeApplier{present: true, err: errors.New("engine did not answer")}
	c, path := newTestResidencyController(t, fa)

	got, _, err := c.SetResidency(context.Background(), time.Hour)
	if err == nil {
		t.Fatal("expected the live failure to be reported")
	}
	if !strings.Contains(err.Error(), "saved") {
		t.Errorf("error = %v, want it to say the value was saved", err)
	}
	if got != time.Hour {
		t.Errorf("returned %v, want 1h", got)
	}
	if d := reloadIdle(t, path); d != time.Hour {
		t.Errorf("persisted %v, want 1h — a live failure must not cost the setting", d)
	}
}

// TestResidencyControllerWithoutEngine: a host with no engine can still
// record the choice for the next one.
func TestResidencyControllerWithoutEngine(t *testing.T) {
	c, path := newTestResidencyController(t, nil)

	if _, _, err := c.SetResidency(context.Background(), 2*time.Hour); err != nil {
		t.Fatalf("SetResidency: %v", err)
	}
	if d := reloadIdle(t, path); d != 2*time.Hour {
		t.Errorf("persisted %v, want 2h", d)
	}
}

// TestResidencyControllerNegativeIsIndefinite records today's behaviour:
// a negative duration is the engine's own spelling of "never unload", so
// it is normalized to the zero the surfaces render rather than rejected.
func TestResidencyControllerNegativeIsIndefinite(t *testing.T) {
	fa := &fakeApplier{present: true, current: time.Hour}
	c, path := newTestResidencyController(t, fa)

	got, _, err := c.SetResidency(context.Background(), -5*time.Minute)
	if err != nil {
		t.Fatalf("SetResidency: %v", err)
	}
	if got != 0 {
		t.Errorf("returned %v, want 0", got)
	}
	if len(fa.applied) != 1 || fa.applied[0] != 0 {
		t.Errorf("engine received %v, want [0s]", fa.applied)
	}
	if d := reloadIdle(t, path); d != 0 {
		t.Errorf("persisted %v, want 0", d)
	}
}

// TestResidencyControllerPrefersTheLiveValue: the two sources disagree
// exactly when a write reached agent.json and the engine has not been
// through it, and the live one is the true answer to "what happens to my
// model".
func TestResidencyControllerPrefersTheLiveValue(t *testing.T) {
	fa := &fakeApplier{present: true, current: 30 * time.Minute}
	c, path := newTestResidencyController(t, fa)

	cfg := agentconfig.Defaults()
	if err := cfg.MergeJSON(path); err != nil {
		t.Fatal(err)
	}
	cfg.Inference.IdleTimeout = agentconfig.NewDuration(8 * time.Hour)
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}

	got, err := c.Residency(context.Background())
	if err != nil {
		t.Fatalf("Residency: %v", err)
	}
	if got != 30*time.Minute {
		t.Errorf("Residency() = %v, want the live 30m", got)
	}
}

// TestResidencyControllerFallsBackToTheFile: no engine yet (a daemon
// that has not enrolled, or one whose engine is not installed) still has
// an answer to give.
func TestResidencyControllerFallsBackToTheFile(t *testing.T) {
	c, path := newTestResidencyController(t, nil)

	cfg := agentconfig.Defaults()
	if err := cfg.MergeJSON(path); err != nil {
		t.Fatal(err)
	}
	cfg.Inference.IdleTimeout = agentconfig.NewDuration(8 * time.Hour)
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}

	got, err := c.Residency(context.Background())
	if err != nil {
		t.Fatalf("Residency: %v", err)
	}
	if got != 8*time.Hour {
		t.Errorf("Residency() = %v, want the persisted 8h", got)
	}
}
