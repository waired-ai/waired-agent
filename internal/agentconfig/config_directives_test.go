package agentconfig

import (
	"flag"
	"testing"
)

// TestClaudeModelRouteDirectives_Plumbing: the #52 opt-in defaults off and is
// settable via both env (WAIRED_INFERENCE_CLAUDE_MODEL_ROUTE_DIRECTIVES) and
// the --inference-claude-model-route-directives flag.
func TestClaudeModelRouteDirectives_Plumbing(t *testing.T) {
	if Defaults().Inference.ClaudeModelRouteDirectives {
		t.Fatal("ClaudeModelRouteDirectives default = true, want false (opt-in)")
	}

	t.Run("env", func(t *testing.T) {
		cfg := Defaults()
		if err := cfg.MergeEnv([]string{"WAIRED_INFERENCE_CLAUDE_MODEL_ROUTE_DIRECTIVES=true"}); err != nil {
			t.Fatalf("MergeEnv: %v", err)
		}
		if !cfg.Inference.ClaudeModelRouteDirectives {
			t.Error("env did not enable ClaudeModelRouteDirectives")
		}
	})

	t.Run("env rejects garbage", func(t *testing.T) {
		cfg := Defaults()
		if err := cfg.MergeEnv([]string{"WAIRED_INFERENCE_CLAUDE_MODEL_ROUTE_DIRECTIVES=maybe"}); err == nil {
			t.Error("MergeEnv accepted a non-bool value")
		}
	})

	t.Run("flag", func(t *testing.T) {
		cfg := Defaults()
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		cfg.RegisterInferenceFlags(fs)
		if err := fs.Parse([]string{"--inference-claude-model-route-directives"}); err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if !cfg.Inference.ClaudeModelRouteDirectives {
			t.Error("flag did not enable ClaudeModelRouteDirectives")
		}
	})
}
