package agentconfig

import (
	"flag"
	"testing"
)

// TestClaudeModelRouteDirectives_Plumbing: the #52 feature defaults ON (so both
// /waired-route and the /model directives work with no config) and is
// opt-out-able via both env (WAIRED_INFERENCE_CLAUDE_MODEL_ROUTE_DIRECTIVES)
// and the --inference-claude-model-route-directives flag.
func TestClaudeModelRouteDirectives_Plumbing(t *testing.T) {
	if !Defaults().Inference.ClaudeModelRouteDirectives {
		t.Fatal("ClaudeModelRouteDirectives default = false, want true (on by default)")
	}

	t.Run("env opts out", func(t *testing.T) {
		cfg := Defaults()
		if err := cfg.MergeEnv([]string{"WAIRED_INFERENCE_CLAUDE_MODEL_ROUTE_DIRECTIVES=false"}); err != nil {
			t.Fatalf("MergeEnv: %v", err)
		}
		if cfg.Inference.ClaudeModelRouteDirectives {
			t.Error("env=false did not disable ClaudeModelRouteDirectives")
		}
	})

	t.Run("env rejects garbage", func(t *testing.T) {
		cfg := Defaults()
		if err := cfg.MergeEnv([]string{"WAIRED_INFERENCE_CLAUDE_MODEL_ROUTE_DIRECTIVES=maybe"}); err == nil {
			t.Error("MergeEnv accepted a non-bool value")
		}
	})

	t.Run("flag opts out", func(t *testing.T) {
		cfg := Defaults()
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		cfg.RegisterInferenceFlags(fs)
		if err := fs.Parse([]string{"--inference-claude-model-route-directives=false"}); err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if cfg.Inference.ClaudeModelRouteDirectives {
			t.Error("flag=false did not disable ClaudeModelRouteDirectives")
		}
	})
}
