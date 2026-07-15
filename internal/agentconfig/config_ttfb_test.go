package agentconfig

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// TestClaudeTTFBBudget_Defaults pins the #757 backstop defaults and the
// invariant that subagents get the tighter budget.
func TestClaudeTTFBBudget_Defaults(t *testing.T) {
	cfg := Defaults()
	if cfg.Inference.ClaudeTTFBBudgetMainMs != 60000 {
		t.Errorf("ClaudeTTFBBudgetMainMs default = %d, want 60000", cfg.Inference.ClaudeTTFBBudgetMainMs)
	}
	if cfg.Inference.ClaudeTTFBBudgetSubMs != 20000 {
		t.Errorf("ClaudeTTFBBudgetSubMs default = %d, want 20000", cfg.Inference.ClaudeTTFBBudgetSubMs)
	}
	if cfg.Inference.ClaudeTTFBBudgetSubMs >= cfg.Inference.ClaudeTTFBBudgetMainMs {
		t.Errorf("sub budget (%d) must be tighter than main (%d)",
			cfg.Inference.ClaudeTTFBBudgetSubMs, cfg.Inference.ClaudeTTFBBudgetMainMs)
	}
}

func TestClaudeTTFBBudget_JSONOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.json")
	if err := os.WriteFile(path, []byte(`{"inference":{"claude_ttfb_budget_main_ms":45000,"claude_ttfb_budget_sub_ms":8000}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Defaults()
	if err := cfg.MergeJSON(path); err != nil {
		t.Fatalf("MergeJSON: %v", err)
	}
	if cfg.Inference.ClaudeTTFBBudgetMainMs != 45000 {
		t.Errorf("main = %d, want 45000", cfg.Inference.ClaudeTTFBBudgetMainMs)
	}
	if cfg.Inference.ClaudeTTFBBudgetSubMs != 8000 {
		t.Errorf("sub = %d, want 8000", cfg.Inference.ClaudeTTFBBudgetSubMs)
	}
}

func TestClaudeTTFBBudget_EnvOverride(t *testing.T) {
	cfg := Defaults()
	if err := cfg.MergeEnv([]string{
		"WAIRED_INFERENCE_CLAUDE_TTFB_BUDGET_MAIN_MS=30000",
		"WAIRED_INFERENCE_CLAUDE_TTFB_BUDGET_SUB_MS=0", // 0 = disable the sub deadline
	}); err != nil {
		t.Fatalf("MergeEnv: %v", err)
	}
	if cfg.Inference.ClaudeTTFBBudgetMainMs != 30000 {
		t.Errorf("main = %d, want 30000", cfg.Inference.ClaudeTTFBBudgetMainMs)
	}
	if cfg.Inference.ClaudeTTFBBudgetSubMs != 0 {
		t.Errorf("sub = %d, want 0 (disabled)", cfg.Inference.ClaudeTTFBBudgetSubMs)
	}
}

func TestClaudeTTFBBudget_FlagOverride(t *testing.T) {
	cfg := Defaults()
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	cfg.RegisterInferenceFlags(fs)
	if err := fs.Parse([]string{
		"--inference-claude-ttfb-budget-main-ms=90000",
		"--inference-claude-ttfb-budget-sub-ms=15000",
	}); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Inference.ClaudeTTFBBudgetMainMs != 90000 {
		t.Errorf("main = %d, want 90000", cfg.Inference.ClaudeTTFBBudgetMainMs)
	}
	if cfg.Inference.ClaudeTTFBBudgetSubMs != 15000 {
		t.Errorf("sub = %d, want 15000", cfg.Inference.ClaudeTTFBBudgetSubMs)
	}
}
