package setup

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/router"
)

// TestSelectionNoteNamesTheHostWhenTheFloorFails is the regression guard for
// waired-agent#798 (c).
//
// Product contract (waired-agent#798, owner-filed from the rc9 3-OS
// verification): the note must not assert something its own parenthetical
// refutes. It used to render a failed floor as "below the ~200k
// coding-agent context floor (native window N tokens)" -- which on the
// reported host printed N = 262144, a number above the floor it claimed to be
// below.
//
// ContextFloorSatisfied had two causes then: a short native window, or a
// host that will not serve a long one. Only the second is left since
// waired-ai/waired-agent#1400 — the catalog admits only builds whose own
// window reaches the floor (decisions 3 and 4 of
// docs/decisions/20260916/0340) — so the note names the host.
func TestSelectionNoteNamesTheHostWhenTheFloorFails(t *testing.T) {
	hw := hardware.Profile{RAMTotalGB: 121}

	longWindow := router.Pick{
		Manifest: catalog.Manifest{ModelID: "qwen3.6-27b", ContextLength: 262144},
		Variant:  catalog.Variant{QualityTier: 72},
		// Host gate failed: the model's own window clears the floor.
		ContextFloorSatisfied: false,
	}

	gotLong := selectionNote(longWindow, hw, "vllm")
	if !strings.Contains(gotLong, "this host cannot serve the ~200k coding-agent context") {
		t.Errorf("host-gate note does not name the host as the cause:\n%s", gotLong)
	}
	if strings.Contains(gotLong, "below the ~200k coding-agent context floor") {
		t.Errorf("host-gate note still claims the model is below the floor while printing a 262144-token window:\n%s", gotLong)
	}
	if strings.Contains(gotLong, "own window is below") {
		t.Errorf("the note blames the model's own window, which nothing in the catalog is below:\n%s", gotLong)
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
