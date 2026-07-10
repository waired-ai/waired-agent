package tray

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// selectedClaudeRoute returns the Class of the single selected row, or "" if
// none — the projection must always mark exactly one.
func selectedClaudeRoute(t *testing.T, rows []ClaudeRouteRow) state.ClaudeRouteClass {
	t.Helper()
	var sel state.ClaudeRouteClass
	n := 0
	for _, r := range rows {
		if r.Selected {
			sel = r.Class
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want exactly one selected row, got %d in %+v", n, rows)
	}
	return sel
}

func TestUpdate_ClaudeRouting_HiddenWithoutEndpoint(t *testing.T) {
	got := Update(baseSnapshot()) // ClaudeRouting nil
	if got.ShowClaudeCode {
		t.Errorf("ShowClaudeCode must be false when Snapshot.ClaudeRouting is nil")
	}
}

func TestUpdate_ClaudeRouting_DefaultPolicy(t *testing.T) {
	snap := baseSnapshot()
	snap.ClaudeRouting = &management.ClaudeRoutingState{
		Policy: state.ClaudeRoutingPolicy{Main: state.ClaudeRouteAuto, Sub: state.ClaudeRouteSame},
	}
	got := Update(snap)
	if !got.ShowClaudeCode {
		t.Fatal("ShowClaudeCode should be true")
	}
	if len(got.ClaudeMainRouteRows) != 3 {
		t.Fatalf("want 3 main rows, got %d", len(got.ClaudeMainRouteRows))
	}
	if len(got.ClaudeSubRouteRows) != 4 {
		t.Fatalf("want 4 sub rows, got %d", len(got.ClaudeSubRouteRows))
	}
	if sel := selectedClaudeRoute(t, got.ClaudeMainRouteRows); sel != state.ClaudeRouteAuto {
		t.Errorf("main selected = %q, want auto", sel)
	}
	if sel := selectedClaudeRoute(t, got.ClaudeSubRouteRows); sel != state.ClaudeRouteSame {
		t.Errorf("sub selected = %q, want same", sel)
	}
	if got.ClaudeFallbackNote != "" {
		t.Errorf("no fallback expected, got note %q", got.ClaudeFallbackNote)
	}
}

func TestUpdate_ClaudeRouting_HybridSelection(t *testing.T) {
	snap := baseSnapshot()
	snap.ClaudeRouting = &management.ClaudeRoutingState{
		Policy: state.ClaudeRoutingPolicy{Main: state.ClaudeRouteAnthropic, Sub: state.ClaudeRouteWaired},
	}
	got := Update(snap)
	if sel := selectedClaudeRoute(t, got.ClaudeMainRouteRows); sel != state.ClaudeRouteAnthropic {
		t.Errorf("main selected = %q, want anthropic", sel)
	}
	if sel := selectedClaudeRoute(t, got.ClaudeSubRouteRows); sel != state.ClaudeRouteWaired {
		t.Errorf("sub selected = %q, want waired", sel)
	}
}

// Empty policy fields must coerce to their defaults (main→auto, sub→same) so a
// daemon that never persisted a policy still marks exactly one row per group.
func TestUpdate_ClaudeRouting_EmptyFieldsCoerce(t *testing.T) {
	snap := baseSnapshot()
	snap.ClaudeRouting = &management.ClaudeRoutingState{} // zero policy
	got := Update(snap)
	if sel := selectedClaudeRoute(t, got.ClaudeMainRouteRows); sel != state.ClaudeRouteAuto {
		t.Errorf("main selected = %q, want auto", sel)
	}
	if sel := selectedClaudeRoute(t, got.ClaudeSubRouteRows); sel != state.ClaudeRouteSame {
		t.Errorf("sub selected = %q, want same", sel)
	}
}

func TestUpdate_ClaudeRouting_FallbackNote(t *testing.T) {
	cases := []struct {
		name      string
		direction string
		wantSub   string
	}{
		{"anthropic", "anthropic", "fell back → Anthropic"},
		{"local", "local", "served locally"},
		{"legacy-unset", "", "fell back → Anthropic"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := baseSnapshot()
			snap.ClaudeRouting = &management.ClaudeRoutingState{
				Policy:       state.ClaudeRoutingPolicy{Main: state.ClaudeRouteAuto, Sub: state.ClaudeRouteSame},
				LastFallback: &management.ClaudeRoutingFallbackEvent{Direction: tc.direction, Reason: "x"},
			}
			got := Update(snap)
			if !strings.Contains(got.ClaudeFallbackNote, tc.wantSub) {
				t.Errorf("ClaudeFallbackNote = %q, want substr %q", got.ClaudeFallbackNote, tc.wantSub)
			}
		})
	}
}

// The enable note appears only when Claude Code is not yet routed through
// Waired, and carries the OS-specific hint (built for the test's GOOS).
func TestUpdate_ClaudeRouting_EnableNote(t *testing.T) {
	base := &management.ClaudeRoutingState{
		Policy: state.ClaudeRoutingPolicy{Main: state.ClaudeRouteAuto, Sub: state.ClaudeRouteSame},
	}

	// Not configured (supported OS) → note present with the enable hint.
	snap := baseSnapshot()
	snap.ClaudeRouting = base
	snap.Claude = &management.ClaudeIntegrationStatus{
		Wrapper:         management.ClaudeWrapperView{Reachable: true},
		ManagedSettings: management.ClaudeManagedSettingsView{Supported: true, Configured: false},
	}
	got := Update(snap)
	if got.ClaudeEnableNote == "" {
		t.Fatal("expected an enable note when managed settings are not configured")
	}
	if !strings.Contains(got.ClaudeEnableNote, "not active yet") ||
		!strings.Contains(got.ClaudeEnableNote, claudeEnableHint()) {
		t.Errorf("ClaudeEnableNote = %q, want 'not active yet' + %q", got.ClaudeEnableNote, claudeEnableHint())
	}

	// Configured → no note.
	snap2 := baseSnapshot()
	snap2.ClaudeRouting = base
	snap2.Claude = &management.ClaudeIntegrationStatus{
		Wrapper:         management.ClaudeWrapperView{Reachable: true},
		ManagedSettings: management.ClaudeManagedSettingsView{Supported: true, Configured: true},
	}
	if got := Update(snap2); got.ClaudeEnableNote != "" {
		t.Errorf("no enable note expected when configured, got %q", got.ClaudeEnableNote)
	}
}

// Disconnected (paused) still surfaces the routing submenu, mirroring the
// worker submenu — a route change stays useful while paused.
func TestUpdate_ClaudeRouting_ShownWhileDisconnected(t *testing.T) {
	snap := baseSnapshot()
	snap.Status.Phase = "paused"
	snap.ClaudeRouting = &management.ClaudeRoutingState{
		Policy: state.ClaudeRoutingPolicy{Main: state.ClaudeRouteWaired, Sub: state.ClaudeRouteSame},
	}
	got := Update(snap)
	if got.Kind != MenuDisconnected {
		t.Fatalf("precondition: want MenuDisconnected, got %v", got.Kind)
	}
	if !got.ShowClaudeCode {
		t.Error("routing submenu should stay visible while paused")
	}
}

func TestClaudeRouteRowLabel(t *testing.T) {
	if got := claudeRouteRowLabel(ClaudeRouteRow{Label: "Auto (Waired-first)", Selected: true}); got != "● Auto (Waired-first)" {
		t.Errorf("selected label = %q", got)
	}
	if got := claudeRouteRowLabel(ClaudeRouteRow{Label: "Anthropic", Selected: false}); got != "○ Anthropic" {
		t.Errorf("unselected label = %q", got)
	}
}
