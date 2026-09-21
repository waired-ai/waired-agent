package runtime

import (
	"slices"
	"testing"
)

// The two flags a custom model's engine gets (waired-ai/waired#1480): its
// own reasoning parser, and safetensors-only loading. A record of today's
// behaviour; both are omitted when unset, so a catalog model's command line
// is unchanged.
func TestVLLMCommandArgs_CustomModelFlags(t *testing.T) {
	base := VLLMConfig{Python: "/venv/bin/python", Host: "127.0.0.1", Port: 8000, Model: "/models/m"}
	args := NewVLLMAdapter(base).commandArgs()
	if slices.Contains(args, "--reasoning-parser") || slices.Contains(args, "--load-format") {
		t.Fatalf("flags present with nothing set: %v", args)
	}
	cfg := base
	cfg.ReasoningParser = "qwen3"
	cfg.LoadFormat = "safetensors"
	args = NewVLLMAdapter(cfg).commandArgs()
	for flag, want := range map[string]string{"--reasoning-parser": "qwen3", "--load-format": "safetensors"} {
		i := slices.Index(args, flag)
		if i < 0 || i+1 >= len(args) || args[i+1] != want {
			t.Errorf("%s: %v, want %s", flag, args, want)
		}
	}
}
