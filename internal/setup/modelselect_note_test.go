package setup

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// TestSelectionNoteSeparatesTheTwoFloorCauses is the regression guard for
// waired-agent#798 (c).
//
// Product contract (waired-agent#798, owner-filed from the rc9 3-OS
// verification): the note must not assert something its own parenthetical
// refutes. ContextFloorSatisfied is false for two independent reasons
// (internal/router/coding_floor.go: a short native window, or a host that
// will not serve a long one), and the note used to render both as "below the
// ~200k coding-agent context floor (native window N tokens)" -- which on the
// reported host printed N = 262144, a number above the floor it claimed to be
// below.
func TestSelectionNoteSeparatesTheTwoFloorCauses(t *testing.T) {
	hw := hardware.Profile{RAMTotalGB: 121}

	longWindow := router.Pick{
		Manifest: catalog.Manifest{ModelID: "qwen3.6-27b", ContextLength: 262144},
		Variant:  catalog.Variant{QualityTier: 72},
		// Host gate failed: the model's own window clears the floor.
		ContextFloorSatisfied: false,
	}
	shortWindow := router.Pick{
		Manifest:              catalog.Manifest{ModelID: "small-4b", ContextLength: 32768},
		Variant:               catalog.Variant{QualityTier: 20},
		ContextFloorSatisfied: false,
	}

	// Guard the premise: if the catalog floor ever moved past these
	// fixtures, the two cases below would collapse into one and this test
	// would pass while testing nothing.
	if !router.MeetsNativeContextFloor(longWindow.Manifest) {
		t.Fatalf("fixture drift: %d tokens no longer clears the native floor (%d)",
			longWindow.Manifest.ContextLength, hostfit.NativeContextFloorTokens)
	}
	if router.MeetsNativeContextFloor(shortWindow.Manifest) {
		t.Fatalf("fixture drift: %d tokens now clears the native floor (%d)",
			shortWindow.Manifest.ContextLength, hostfit.NativeContextFloorTokens)
	}

	gotLong := selectionNote(longWindow, hw, "vllm")
	if !strings.Contains(gotLong, "this host cannot serve the ~200k coding-agent context") {
		t.Errorf("host-gate note does not name the host as the cause:\n%s", gotLong)
	}
	if strings.Contains(gotLong, "below the ~200k coding-agent context floor") {
		t.Errorf("host-gate note still claims the model is below the floor while printing a 262144-token window:\n%s", gotLong)
	}

	gotShort := selectionNote(shortWindow, hw, "ollama")
	if !strings.Contains(gotShort, "this model's own window is below the ~200k coding-agent context floor") {
		t.Errorf("short-window note does not name the model as the cause:\n%s", gotShort)
	}
}

// TestSelectionNoteBoundedSpillArmUnchanged pins that splitting the
// below-floor case left the satisfied-with-spill wording alone.
func TestSelectionNoteBoundedSpillArmUnchanged(t *testing.T) {
	p := router.Pick{
		Manifest:              catalog.Manifest{ModelID: "m", ContextLength: 262144},
		Variant:               catalog.Variant{QualityTier: 70},
		ContextFloorSatisfied: true,
		ExpectedSpillFraction: 0.26,
	}
	got := selectionNote(p, hardware.Profile{RAMTotalGB: 32}, "ollama")
	if !strings.Contains(got, "serves a ~200k coding context with ~26% of the model expected in system RAM") {
		t.Errorf("bounded-spill arm changed:\n%s", got)
	}
}
