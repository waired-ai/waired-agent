package main

import (
	"strings"
	"testing"
)

// Both `refresh` commands are reporting-only, and their closing line is
// the one thing that tells an operator what to do next. Both used to
// name an internal roadmap step ("Step 12") and an endpoint that was
// never built (`/waired/v1/models/refresh`, `/waired/v1/runtimes/refresh`).
//
// A record of today's behaviour, not a product contract: no issue
// ratifies these exact sentences. What IS pinned below is that neither
// line may name a roadmap step or an unbuilt endpoint again, and that
// the two differ on the restart — because that difference is real
// (waired#812: the in-process swap is same-engine only, so a
// cross-engine change still restarts).

func TestFormatRefreshApplyHint(t *testing.T) {
	tests := []struct {
		name    string
		modelID string
		want    string
	}{
		{
			name:    "names the suggested model in the command to run",
			modelID: "qwen3.5-4b",
			want:    `To apply, run "waired models use qwen3.5-4b".`,
		},
		{
			// available_update without a usable model_id: still print a
			// runnable command rather than "waired models use <nil>".
			name:    "falls back to the placeholder when the id is missing",
			modelID: "",
			want:    `To apply, run "waired models use <model-id>".`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatRefreshApplyHint(tt.modelID); got != tt.want {
				t.Errorf("formatRefreshApplyHint(%q) =\n  %q\nwant\n  %q", tt.modelID, got, tt.want)
			}
		})
	}
}

func TestRefreshHintsNameNoRoadmapStepOrUnbuiltEndpoint(t *testing.T) {
	for _, line := range []string{
		formatRefreshApplyHint("qwen3.5-4b"),
		formatRefreshApplyHint(""),
		runtimesRefreshApplyHint,
	} {
		for _, banned := range []string{
			"Step 1",                      // any "Step N" roadmap token
			"/waired/v1/models/refresh",   // never implemented
			"/waired/v1/runtimes/refresh", // never implemented
		} {
			if strings.Contains(line, banned) {
				t.Errorf("%q still contains %q", line, banned)
			}
		}
	}
}

func TestRuntimesRefreshStillAsksForTheRestart(t *testing.T) {
	// The asymmetry is the point: a model switch applies in process
	// (waired#812), an engine switch does not — errSwapNeedsRestart sends
	// a cross-engine target down the supervised-restart fallback. So the
	// models hint must NOT mention a restart and the runtimes one must.
	if !strings.Contains(runtimesRefreshApplyHint, "restart waired-agent") {
		t.Errorf("runtimes hint = %q, want it to still ask for the restart", runtimesRefreshApplyHint)
	}
	if strings.Contains(formatRefreshApplyHint("qwen3.5-4b"), "restart") {
		t.Errorf("models hint = %q, want no restart claim after waired#812",
			formatRefreshApplyHint("qwen3.5-4b"))
	}
}
