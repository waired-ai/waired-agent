package main

import (
	"io"
	"log/slog"
	"testing"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

func newTestRoutingController(t *testing.T) (*claudeRoutingController, string) {
	t.Helper()
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return newClaudeRoutingController(dir, state.DefaultClaudeRoutingPolicy(), logger), dir
}

func TestRoutingController_DefaultAndRouteFor(t *testing.T) {
	c, _ := newTestRoutingController(t)
	if got := c.Policy(); got.Main != state.ClaudeRouteAuto || got.Sub != state.ClaudeRouteSame {
		t.Fatalf("default policy = %+v", got)
	}
	if r := c.RouteFor(state.ClaudeClassMain); r != "auto" {
		t.Errorf("RouteFor(main) = %q want auto", r)
	}
	if r := c.RouteFor(state.ClaudeClassSub); r != "auto" {
		t.Errorf("RouteFor(sub) = %q want auto (same → main)", r)
	}
}

func TestRoutingController_SetClassPersistsAndReloads(t *testing.T) {
	c, dir := newTestRoutingController(t)
	if err := c.SetClass(t.Context(), state.ClaudeClassMain, state.ClaudeRouteAnthropic); err != nil {
		t.Fatal(err)
	}
	if err := c.SetClass(t.Context(), state.ClaudeClassSub, state.ClaudeRouteWaired); err != nil {
		t.Fatal(err)
	}
	if r := c.RouteFor(state.ClaudeClassSub); r != "waired" {
		t.Errorf("RouteFor(sub) = %q want waired", r)
	}
	// A fresh controller reading the same dir must see the persisted policy.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pol, err := state.ReadDesiredClaudeRouting(dir)
	if err != nil {
		t.Fatal(err)
	}
	c2 := newClaudeRoutingController(dir, pol, logger)
	if got := c2.Policy(); got.Main != state.ClaudeRouteAnthropic || got.Sub != state.ClaudeRouteWaired {
		t.Fatalf("reloaded policy = %+v", got)
	}
}

func TestRoutingController_SetClassRejectsUnknownClass(t *testing.T) {
	c, _ := newTestRoutingController(t)
	if err := c.SetClass(t.Context(), "bogus", state.ClaudeRouteAuto); err == nil {
		t.Fatal("expected error for unknown class")
	}
}

func TestRoutingController_RecordFallbacks(t *testing.T) {
	c, _ := newTestRoutingController(t)
	// auto → Anthropic (served upstream).
	c.RecordFallback("local_status_400")
	st := c.State()
	if st.LastFallback == nil || st.LastFallback.Direction != "anthropic" || st.LastFallback.Count != 1 {
		t.Fatalf("after RecordFallback: %+v", st.LastFallback)
	}
	// anthropic/peer → local degrade (served locally).
	c.RecordNodeFallback(state.ClaudeClassMain, "peer-X", "unreachable")
	st = c.State()
	if st.LastFallback == nil || st.LastFallback.Direction != "local" ||
		st.LastFallback.Class != state.ClaudeClassMain || st.LastFallback.Peer != "peer-X" ||
		st.LastFallback.Count != 2 {
		t.Fatalf("after RecordNodeFallback: %+v", st.LastFallback)
	}
}

func TestRoutingController_RecordServed(t *testing.T) {
	c, _ := newTestRoutingController(t)
	c.RecordServed("small-local", "peer-X")
	st := c.State()
	if st.LastLocalModel != "small-local" || st.LastServedBy != "peer-X" {
		t.Fatalf("served = model=%q peer=%q", st.LastLocalModel, st.LastServedBy)
	}
}
