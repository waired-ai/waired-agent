package main

import (
	"io"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/setup"
)

func cpuProf(ramGB int) hardware.Profile {
	return hardware.Profile{OS: "linux", Arch: "x86_64", RAMTotalGB: ramGB}
}

// TestApplyBundledModelSelection exercises the waired-init wiring end to
// end through the config mutation: capable host overrides the model and
// keeps inference on; under-spec disables local inference; --inference-
// enabled=true forces it back on; a pin is honoured verbatim (#517).
func TestApplyBundledModelSelection(t *testing.T) {
	truePtr := true

	newCfg := func() *agentconfig.Config {
		c := agentconfig.Defaults()
		c.Inference.OllamaSource = agentconfig.OllamaSourceBundled
		return &c
	}

	t.Run("capable-host-overrides-and-enables", func(t *testing.T) {
		cfg := newCfg()
		applyBundledModelSelection(cfg, cpuProf(8), setup.OllamaDetection{},
			t.TempDir(), t.TempDir(), "", nil, false, strings.NewReader(""), io.Discard)
		if !cfg.Inference.Enabled {
			t.Errorf("8 GB host should keep inference enabled")
		}
		// #624: the 32k-native coder-7b is excluded by the coding-agent
		// context floor; qwen3.5-4b is the best floor-passing 8 GB fit.
		if cfg.Inference.BundledModelID != "qwen3.5-4b" {
			t.Errorf("model = %q, want qwen3.5-4b", cfg.Inference.BundledModelID)
		}
	})

	// A truly tiny host (nothing fits, not even the 0.5B) still disables
	// silently — no opt-in dialog, generic under-spec note.
	t.Run("nothing-fits-disables", func(t *testing.T) {
		cfg := newCfg()
		var out strings.Builder
		applyBundledModelSelection(cfg, cpuProf(1), setup.OllamaDetection{},
			t.TempDir(), t.TempDir(), "", nil, false, strings.NewReader(""), &out)
		if cfg.Inference.Enabled {
			t.Errorf("1 GB host should disable local inference")
		}
	})

	t.Run("under-spec-forced-stays-enabled", func(t *testing.T) {
		cfg := newCfg()
		applyBundledModelSelection(cfg, cpuProf(2), setup.OllamaDetection{},
			t.TempDir(), t.TempDir(), "", &truePtr, false, strings.NewReader(""), io.Discard)
		if !cfg.Inference.Enabled {
			t.Errorf("--inference-enabled=true must force inference on under-spec")
		}
	})

	t.Run("pin-honoured", func(t *testing.T) {
		cfg := newCfg()
		applyBundledModelSelection(cfg, cpuProf(32), setup.OllamaDetection{},
			t.TempDir(), t.TempDir(), "qwen2.5-coder-3b-instruct", nil, false, strings.NewReader(""), io.Discard)
		if cfg.Inference.BundledModelID != "qwen2.5-coder-3b-instruct" {
			t.Errorf("pin not honoured: got %q", cfg.Inference.BundledModelID)
		}
		if !cfg.Inference.Enabled {
			t.Errorf("pinned model should keep inference enabled")
		}
	})

	// A 3 GB host clears nothing above the coding-quality floor (the 3B needs
	// 4 GB) but a tiny below-floor model (min 2 GB) fits, so the under-spec
	// opt-in dialog fires instead of a silent disable. Opting in enables
	// inference on whichever below-floor model the picker chose.
	t.Run("tiny-fits-opt-in-yes-enables-below-floor", func(t *testing.T) {
		cfg := newCfg()
		var out strings.Builder
		applyBundledModelSelection(cfg, cpuProf(3), setup.OllamaDetection{},
			t.TempDir(), t.TempDir(), "", nil, false, strings.NewReader("y\n"), &out)
		if !cfg.Inference.Enabled {
			t.Fatalf("opting in should enable local inference; out=%q", out.String())
		}
		if !isBundledModelBelowFloor(cfg.Inference.BundledModelID) {
			t.Errorf("model = %q, want a below-floor model", cfg.Inference.BundledModelID)
		}
		if !strings.Contains(out.String(), "very small, low-quality model") {
			t.Errorf("expected the tiny-model warning, got: %q", out.String())
		}
	})

	t.Run("tiny-fits-opt-in-no-stays-disabled", func(t *testing.T) {
		cfg := newCfg()
		var out strings.Builder
		applyBundledModelSelection(cfg, cpuProf(3), setup.OllamaDetection{},
			t.TempDir(), t.TempDir(), "", nil, false, strings.NewReader("n\n"), &out)
		if cfg.Inference.Enabled {
			t.Errorf("declining the tiny model must leave local inference off")
		}
	})

	t.Run("tiny-fits-non-interactive-stays-disabled", func(t *testing.T) {
		cfg := newCfg()
		var out strings.Builder
		// stdin must NOT be consulted.
		applyBundledModelSelection(cfg, cpuProf(3), setup.OllamaDetection{},
			t.TempDir(), t.TempDir(), "", nil, true, strings.NewReader(""), &out)
		if cfg.Inference.Enabled {
			t.Errorf("non-interactive under-spec must leave local inference off")
		}
		if !strings.Contains(out.String(), "left disabled") {
			t.Errorf("expected a non-interactive left-disabled note, got: %q", out.String())
		}
	})
}
