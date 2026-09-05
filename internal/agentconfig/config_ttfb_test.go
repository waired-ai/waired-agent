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
	// waired-agent#1040: the main budget is a grace period, and this is the
	// end of the wait that follows it.
	//
	// PIN: product contract — owner ruling 2026-08-28, on the measurement.
	// A 30k-token first turn on the fleet's slowest peer took 9 min 10 s to
	// its first byte, which left 50 seconds against the ten minutes this
	// shipped with. It was deliberately not the local leg's figure — that
	// one was a pure timeout with no signal behind it, and this one is a
	// backstop for a peer whose own liveness claim is wrong. The local
	// figure is gone entirely now: a local leg has nowhere else to send the
	// turn and is held open with a keepalive instead
	// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md).
	if cfg.Inference.ClaudePeerWaitCeilingMs != 1800000 {
		t.Errorf("ClaudePeerWaitCeilingMs default = %d, want 1800000", cfg.Inference.ClaudePeerWaitCeilingMs)
	}
	// A ceiling at or below the grace would be no ceiling at all — the
	// gateway leaves such a class on the flat deadline (peerLivenessFor).
	if cfg.Inference.ClaudePeerWaitCeilingMs <= cfg.Inference.ClaudeTTFBBudgetMainMs {
		t.Errorf("ceiling %d must be longer than the main grace %d",
			cfg.Inference.ClaudePeerWaitCeilingMs, cfg.Inference.ClaudeTTFBBudgetMainMs)
	}
	if cfg.Inference.ClaudeTTFBBudgetSubMs >= cfg.Inference.ClaudeTTFBBudgetMainMs {
		t.Errorf("sub budget (%d) must be tighter than main (%d)",
			cfg.Inference.ClaudeTTFBBudgetSubMs, cfg.Inference.ClaudeTTFBBudgetMainMs)
	}
}

func TestClaudeTTFBBudget_JSONOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.json")
	if err := os.WriteFile(path, []byte(`{"inference":{"claude_ttfb_budget_main_ms":45000,"claude_ttfb_budget_sub_ms":8000,"claude_peer_wait_ceiling_ms":120000}}`), 0o600); err != nil {
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
	if cfg.Inference.ClaudePeerWaitCeilingMs != 120000 {
		t.Errorf("ceiling = %d, want 120000", cfg.Inference.ClaudePeerWaitCeilingMs)
	}
}

func TestClaudeTTFBBudget_EnvOverride(t *testing.T) {
	cfg := Defaults()
	if err := cfg.MergeEnv([]string{
		"WAIRED_INFERENCE_CLAUDE_TTFB_BUDGET_MAIN_MS=30000",
		"WAIRED_INFERENCE_CLAUDE_PEER_WAIT_CEILING_MS=90000",
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
	if cfg.Inference.ClaudePeerWaitCeilingMs != 90000 {
		t.Errorf("ceiling = %d, want 90000", cfg.Inference.ClaudePeerWaitCeilingMs)
	}
}

func TestClaudeTTFBBudget_FlagOverride(t *testing.T) {
	cfg := Defaults()
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	cfg.RegisterInferenceFlags(fs)
	if err := fs.Parse([]string{
		"--inference-claude-ttfb-budget-main-ms=90000",
		"--inference-claude-peer-wait-ceiling-ms=300000",
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
	if cfg.Inference.ClaudePeerWaitCeilingMs != 300000 {
		t.Errorf("ceiling = %d, want 300000", cfg.Inference.ClaudePeerWaitCeilingMs)
	}
}
