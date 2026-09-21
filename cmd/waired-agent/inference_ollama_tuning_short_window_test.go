package main

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// A model whose own window is under 200,704, served whole, is not a host
// that ran out of memory: the decision line says the window is the model's,
// and carries no warning that blames memory or claims the window is
// declared to the mesh (below 200,704 nothing is). Found on real hardware
// with a custom model (waired-ai/waired#1481). A record of today's
// behaviour.
func TestModelDecisionReasons_ModelsOwnShortWindow(t *testing.T) {
	m := customTestManifest(t, "small") // 40,960 tokens
	tn := ollamaTuning{ModelTuning: infruntime.ModelTuning{ModelID: m.ModelID, ContextLength: 40960, WindowFits: false}}
	reasons, extra := modelDecisionReasons(agentconfig.InferenceConfig{}, m, tn)
	if extra != "" {
		t.Errorf("a warning for a model served at its own window: %q", extra)
	}
	if len(reasons) != 1 || !strings.Contains(reasons[0], "its own 40960-token context window") ||
		strings.Contains(reasons[0], "declared to the mesh") {
		t.Errorf("reasons = %v", reasons)
	}
}
