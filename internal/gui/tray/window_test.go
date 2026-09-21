package tray

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// waired-ai/waired#1359 (owner ruling 2026-09-20): the tray gets the same
// window choice as the console and the CLI. At 1M a model that cannot be had
// at 1M is greyed with the reason; nothing is greyed for want of memory.

const long = hostfit.ServingWindow1M

func windowFamilies() []management.CatalogFamily {
	return []management.CatalogFamily{
		{ModelID: "reaches", DisplayName: "Reaches 1M", Fits: true, ContextLength: 262144,
			ServingWindows: []int{hostfit.ServingWindow200k, long}},
		{ModelID: "stops", DisplayName: "Stops short", Fits: true, ContextLength: 262144,
			ServingWindows: []int{hostfit.ServingWindow200k}},
		// Reaches 1M but does not fit this computer: the capacity case. It
		// must stay choosable at 1M, with its shortfall, like at any window.
		{ModelID: "heavy", DisplayName: "Too heavy", Fits: false, ContextLength: 262144,
			DeficitLabel: "needs 96 GB", ServingWindows: []int{hostfit.ServingWindow200k, long}},
	}
}

// project runs the two builders in the order Update runs them — inference
// first, so the catalog can read the window in force.
func project(engine, activeModel string, windowInForce int) MenuModel {
	var m MenuModel
	applyInference(&m, &management.InferenceStatus{
		Active:   &management.ActiveSelection{Runtime: engine, ModelID: activeModel},
		Runtimes: map[string]management.RuntimeStatus{engine: {ContextLength: windowInForce}},
	})
	var active *management.CatalogActive
	if activeModel != "" {
		active = &management.CatalogActive{ModelID: activeModel}
	}
	applyCatalog(&m, &management.ModelCatalogResponse{
		Engine: engine, Families: windowFamilies(), Active: active,
	})
	return m
}

func entry(t *testing.T, m MenuModel, id string) CatalogEntryView {
	t.Helper()
	for _, e := range m.CatalogEntries {
		if e.ModelID == id {
			return e
		}
	}
	t.Fatalf("no catalog row for %s", id)
	return CatalogEntryView{}
}

func TestWindow_AtTheCodingWindowNothingIsWalled(t *testing.T) {
	m := project("ollama", "reaches", hostfit.ServingWindow200k)
	for _, e := range m.CatalogEntries {
		if e.WindowWall != WindowReachable {
			t.Errorf("%s is walled at the coding window", e.ModelID)
		}
	}
}

func TestWindow_At1MOnlyModelsThatCannotReachItAreWalled(t *testing.T) {
	m := project("ollama", "reaches", long)

	stops := entry(t, m, "stops")
	if stops.WindowWall != WindowWallModel {
		t.Fatalf("stops: wall = %v, want the model's own wall", stops.WindowWall)
	}
	// The reason is in the LABEL, visible on every platform — a menu item's
	// tooltip is not shown everywhere, and a grey row with no visible reason
	// is not "greyed out with the reason".
	if stops.Label != "Stops short — doesn't reach 1M" {
		t.Errorf("stops: label %q, want the reason visible in it", stops.Label)
	}
	// The whole sentence, with the length grouped the way a person reads a
	// token count, stays in the tooltip.
	if !strings.Contains(stops.Tooltip, "262,144") || !strings.Contains(stops.Tooltip, "1M") {
		t.Errorf("stops: tooltip %q does not name 1M and the model's length", stops.Tooltip)
	}
	if got := entry(t, m, "reaches").WindowWall; got != WindowReachable {
		t.Errorf("reaches: wall = %v, want reachable", got)
	}
}

// PRODUCT CONTRACT (waired's docs/decisions/20260808/2325; this repo's
// docs/decisions/20260920/2345-…): nothing is greyed for want of memory. A
// model that does not fit this computer but reaches 1M stays choosable at
// 1M and carries its shortfall. This is the line that keeps the window wall
// from growing back into the capacity block the tray removed.
func TestWindow_ACapacityShortfallIsNeverAWall(t *testing.T) {
	m := project("ollama", "reaches", long)
	heavy := entry(t, m, "heavy")
	if heavy.WindowWall != WindowReachable {
		t.Fatalf("heavy: a model that reaches 1M but does not fit was walled (%v)", heavy.WindowWall)
	}
	if heavy.UnfitReason == "" {
		t.Error("heavy: the shortfall is gone — it should still ask, not block")
	}
}

func TestWindow_PresetsNameTheWindowInForce(t *testing.T) {
	for _, tc := range []struct {
		inForce  int
		header   string
		selected string
	}{
		{hostfit.ServingWindow200k, "Context window: 200K", "200K"},
		{long, "Context window: 1M", "1M"},
	} {
		m := project("ollama", "reaches", tc.inForce)
		if m.WindowHeader != tc.header {
			t.Errorf("in force %d: header %q, want %q", tc.inForce, m.WindowHeader, tc.header)
		}
		var got []string
		for _, r := range m.WindowRows {
			if r.Selected {
				got = append(got, r.Label)
			}
		}
		if len(got) != 1 || got[0] != tc.selected {
			t.Errorf("in force %d: selected %v, want exactly [%s]", tc.inForce, got, tc.selected)
		}
		if m.WindowActiveModelID != "reaches" {
			t.Errorf("preset re-serves %q, want the model in use", m.WindowActiveModelID)
		}
	}
}

func TestWindow_The1MPresetIsWalledForAModelThatCannotReachIt(t *testing.T) {
	m := project("ollama", "stops", hostfit.ServingWindow200k)
	if len(m.WindowRows) != 2 {
		t.Fatalf("rows = %d, want 2", len(m.WindowRows))
	}
	coding, oneM := m.WindowRows[0], m.WindowRows[1]
	if coding.Wall != WindowReachable {
		t.Errorf("the coding preset is walled (%v); every model reaches it", coding.Wall)
	}
	if oneM.Wall != WindowWallModel || !strings.Contains(oneM.Tooltip, "262,144") {
		t.Errorf("1M preset: wall %v, tooltip %q — want the model's wall and its length", oneM.Wall, oneM.Tooltip)
	}
}

// On vLLM the long rung is not served, so the 1M preset names the ENGINE —
// saying the model stops at 262,144 would name the wrong wall, since the
// same model reaches 1M on ollama.
func TestWindow_OnVLLMThe1MPresetNamesTheEngine(t *testing.T) {
	m := project("vllm", "reaches", hostfit.ServingWindow200k)
	oneM := m.WindowRows[1]
	if oneM.Wall != WindowWallEngine {
		t.Fatalf("1M preset on vLLM: wall %v, want the engine's", oneM.Wall)
	}
	if oneM.Tooltip != "vLLM doesn't serve 1M yet." {
		t.Errorf("tooltip %q", oneM.Tooltip)
	}
}

// A preset re-serves the model in use at the window it names. With no model
// in use there is nothing to re-serve, so no presets are drawn.
func TestWindow_NoModelInUseMeansNoPresets(t *testing.T) {
	m := project("ollama", "", hostfit.ServingWindow200k)
	if m.WindowHeader != "" || len(m.WindowRows) != 0 {
		t.Errorf("drew presets with no model in use: header %q, %d rows", m.WindowHeader, len(m.WindowRows))
	}
}

func TestGroupThousands(t *testing.T) {
	for n, want := range map[int]string{
		0: "0", 999: "999", 1000: "1,000", 262144: "262,144", 1048576: "1,048,576", -1234: "-1,234",
	} {
		if got := groupThousands(n); got != want {
			t.Errorf("groupThousands(%d) = %q, want %q", n, got, want)
		}
	}
}

// The faster-model suggestion keeps 1M when the model it names can be had at
// 1M, and falls back to the coding window only when it cannot — it used to
// drop a person to 200K every time.
func TestWindowToKeep(t *testing.T) {
	entries := []CatalogEntryView{
		{ModelID: "reaches", WindowWall: WindowReachable},
		{ModelID: "stops", WindowWall: WindowWallModel},
	}
	for _, tc := range []struct {
		model   string
		inForce int
		want    int
	}{
		{"reaches", long, long},
		{"stops", long, 0},
		{"unknown", long, 0},
		{"reaches", 0, 0},
	} {
		if got := windowToKeep(entries, tc.model, tc.inForce); got != tc.want {
			t.Errorf("windowToKeep(%s, %d) = %d, want %d", tc.model, tc.inForce, got, tc.want)
		}
	}
}
