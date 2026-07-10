package main

import (
	"context"
	"testing"

	"github.com/waired-ai/waired-agent/internal/observability"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

func TestWorkerController_StartsFromInitial(t *testing.T) {
	dir := t.TempDir()
	wc := newWorkerController(dir, state.RoutingPreference{Mode: state.RoutingModeAuto}, nil)
	got := wc.Routing()
	if got.Mode != state.RoutingModeAuto {
		t.Errorf("initial Routing = %q, want auto", got.Mode)
	}
}

func TestWorkerController_EmptyInitialBecomesAuto(t *testing.T) {
	dir := t.TempDir()
	wc := newWorkerController(dir, state.RoutingPreference{}, nil)
	got := wc.Routing()
	if got.Mode != state.RoutingModeAuto {
		t.Errorf("empty initial Routing = %q, want auto (normalised)", got.Mode)
	}
}

func TestWorkerController_SetModeTransitions(t *testing.T) {
	dir := t.TempDir()
	wc := newWorkerController(dir, state.RoutingPreference{Mode: state.RoutingModeAuto}, nil)

	if err := wc.SetMode(context.Background(), state.RoutingModeLocalOnly); err != nil {
		t.Fatal(err)
	}
	if wc.Routing().Mode != state.RoutingModeLocalOnly {
		t.Errorf("after SetMode(local-only) live = %q", wc.Routing().Mode)
	}
	persisted, err := state.ReadDesiredWorker(dir)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Mode != state.RoutingModeLocalOnly {
		t.Errorf("persisted = %q, want local-only", persisted.Mode)
	}

	if err := wc.SetMode(context.Background(), state.RoutingModePeerPreferred); err != nil {
		t.Fatal(err)
	}
	if wc.Routing().Mode != state.RoutingModePeerPreferred {
		t.Errorf("after SetMode(peer-preferred) live = %q", wc.Routing().Mode)
	}
}

func TestWorkerController_SetPinFlipsToPinned(t *testing.T) {
	dir := t.TempDir()
	wc := newWorkerController(dir, state.RoutingPreference{Mode: state.RoutingModeAuto}, nil)

	if err := wc.SetPin(context.Background(), "dev_linux_gpu_1"); err != nil {
		t.Fatal(err)
	}
	got := wc.Routing()
	if got.Mode != state.RoutingModePinned {
		t.Errorf("mode after SetPin = %q, want pinned", got.Mode)
	}
	if got.PinnedPeerDeviceID != "dev_linux_gpu_1" {
		t.Errorf("peer after SetPin = %q, want dev_linux_gpu_1", got.PinnedPeerDeviceID)
	}
}

func TestWorkerController_SetModePinnedRejected(t *testing.T) {
	dir := t.TempDir()
	wc := newWorkerController(dir, state.RoutingPreference{Mode: state.RoutingModeAuto}, nil)
	if err := wc.SetMode(context.Background(), state.RoutingModePinned); err == nil {
		t.Error("SetMode(pinned) must reject — use SetPin instead")
	}
}

func TestWorkerController_SetPinEmptyRejected(t *testing.T) {
	dir := t.TempDir()
	wc := newWorkerController(dir, state.RoutingPreference{Mode: state.RoutingModeAuto}, nil)
	if err := wc.SetPin(context.Background(), ""); err == nil {
		t.Error("SetPin(\"\") must reject")
	}
}

func TestWorkerController_ClearReturnsToAuto(t *testing.T) {
	dir := t.TempDir()
	wc := newWorkerController(dir, state.RoutingPreference{Mode: state.RoutingModeAuto}, nil)
	if err := wc.SetPin(context.Background(), "dev_x"); err != nil {
		t.Fatal(err)
	}
	if err := wc.Clear(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := wc.Routing()
	if got.Mode != state.RoutingModeAuto {
		t.Errorf("after Clear mode = %q, want auto", got.Mode)
	}
	if got.PinnedPeerDeviceID != "" {
		t.Errorf("after Clear pin = %q, want empty", got.PinnedPeerDeviceID)
	}
}

func TestWorkerController_SwitchingModeClearsStaleP(t *testing.T) {
	dir := t.TempDir()
	wc := newWorkerController(dir, state.RoutingPreference{Mode: state.RoutingModeAuto}, nil)
	if err := wc.SetPin(context.Background(), "dev_a"); err != nil {
		t.Fatal(err)
	}
	if err := wc.SetMode(context.Background(), state.RoutingModeLocalOnly); err != nil {
		t.Fatal(err)
	}
	got := wc.Routing()
	if got.PinnedPeerDeviceID != "" {
		t.Errorf("switching to local-only must clear pinned peer, got %q", got.PinnedPeerDeviceID)
	}
}

func TestWorkerController_EmitsRoutingModeChangeEvent(t *testing.T) {
	dir := t.TempDir()
	ring := observability.NewRing(observability.DefaultRingCapacity)
	wc := newWorkerController(dir, state.RoutingPreference{Mode: state.RoutingModeAuto}, nil).
		WithObservability(ring)

	if err := wc.SetPin(context.Background(), "dev_xyz"); err != nil {
		t.Fatal(err)
	}
	events, _, _ := ring.Since(0, []observability.Kind{observability.KindRoutingModeChange}, 10)
	if len(events) != 1 {
		t.Fatalf("want 1 event after SetPin, got %d", len(events))
	}
	e := events[0]
	if e.RoutingModeChange == nil {
		t.Fatal("RoutingModeChange payload should be populated")
	}
	if e.RoutingModeChange.To != string(state.RoutingModePinned) {
		t.Errorf("To = %q, want pinned", e.RoutingModeChange.To)
	}
	if e.RoutingModeChange.PinnedPeerDeviceID != "dev_xyz" {
		t.Errorf("PinnedPeerDeviceID = %q", e.RoutingModeChange.PinnedPeerDeviceID)
	}
}

func TestWorkerController_NoEventWhenStateUnchanged(t *testing.T) {
	dir := t.TempDir()
	ring := observability.NewRing(observability.DefaultRingCapacity)
	wc := newWorkerController(dir, state.RoutingPreference{Mode: state.RoutingModeAuto}, nil).
		WithObservability(ring)

	// Auto → auto: no-op should NOT emit.
	if err := wc.SetMode(context.Background(), state.RoutingModeAuto); err != nil {
		t.Fatal(err)
	}
	events, _, _ := ring.Since(0, []observability.Kind{observability.KindRoutingModeChange}, 10)
	if len(events) != 0 {
		t.Errorf("no-op transition should not emit, got %d events", len(events))
	}
}

func TestWorkerController_StateReportsDesiredFromDisk(t *testing.T) {
	dir := t.TempDir()
	wc := newWorkerController(dir, state.RoutingPreference{Mode: state.RoutingModeAuto}, nil)
	// Simulate operator-hand-edited desired-worker.json that the daemon
	// has not yet applied. State() should surface the disk truth in
	// desired while leaving current at the live in-memory value.
	if err := state.WriteDesiredWorker(dir, state.RoutingPreference{
		Mode: state.RoutingModePeerPreferred,
	}); err != nil {
		t.Fatal(err)
	}
	cur, desired := wc.State()
	if cur.Mode != state.RoutingModeAuto {
		t.Errorf("current mode = %q, want auto (live hasn't flipped yet)", cur.Mode)
	}
	if desired.Mode != state.RoutingModePeerPreferred {
		t.Errorf("desired mode = %q, want peer-preferred", desired.Mode)
	}
}
