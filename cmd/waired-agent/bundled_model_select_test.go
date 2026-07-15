package main

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/setup"
)

// TestShouldAutoSelectBundledModel covers the waired#756 boot gate: the daemon
// auto-selects the bundled model only on a pristine fresh install with no
// operator inference preference expressed through any channel.
func TestShouldAutoSelectBundledModel(t *testing.T) {
	cases := []struct {
		name       string
		disableInf bool
		agentJSON  bool
		preference bool
		flagSet    bool
		environ    []string
		want       bool
	}{
		{name: "pristine fresh install", want: true},
		{name: "pristine with unrelated env", environ: []string{"PATH=/usr/bin", "HOME=/root"}, want: true},
		{name: "agent.json already present", agentJSON: true, want: false},
		{name: "model preference present", preference: true, want: false},
		{name: "--disable-inference passed", disableInf: true, want: false},
		{name: "inference flag set", flagSet: true, want: false},
		{name: "WAIRED_INFERENCE_ env override", environ: []string{"WAIRED_INFERENCE_BUNDLED_MODEL_ID=x"}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldAutoSelectBundledModel(tc.disableInf, tc.agentJSON, tc.preference, tc.flagSet, tc.environ)
			if got != tc.want {
				t.Fatalf("shouldAutoSelectBundledModel = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAnyInferenceFlagSet only fires for explicitly-set inference-controlling
// flags, not for registered-but-unset ones or unrelated flags.
func TestAnyInferenceFlagSet(t *testing.T) {
	newFS := func() *flag.FlagSet {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.String("inference-bundled-model-id", "", "")
		fs.Bool("disable-inference", false, "")
		fs.String("state-dir", "", "")
		return fs
	}
	t.Run("nothing set", func(t *testing.T) {
		fs := newFS()
		_ = fs.Parse(nil)
		if anyInferenceFlagSet(fs) {
			t.Fatal("no flags set should be false")
		}
	})
	t.Run("unrelated flag set", func(t *testing.T) {
		fs := newFS()
		_ = fs.Parse([]string{"-state-dir", "/tmp/x"})
		if anyInferenceFlagSet(fs) {
			t.Fatal("unrelated flag must not count")
		}
	})
	t.Run("inference flag set", func(t *testing.T) {
		fs := newFS()
		_ = fs.Parse([]string{"-inference-bundled-model-id", "m"})
		if !anyInferenceFlagSet(fs) {
			t.Fatal("inference flag must count")
		}
	})
	t.Run("disable-inference set", func(t *testing.T) {
		fs := newFS()
		_ = fs.Parse([]string{"-disable-inference"})
		if !anyInferenceFlagSet(fs) {
			t.Fatal("disable-inference must count")
		}
	})
}

// TestApplyBundledSelection folds the selection verdict into the config.
func TestApplyBundledSelection(t *testing.T) {
	t.Run("capable host: model set, inference on", func(t *testing.T) {
		cfg := agentconfig.Defaults()
		applyBundledSelection(&cfg, setup.BundledModelSelection{ModelID: "qwen2.5-coder-3b-instruct", EnableInference: true})
		if cfg.Inference.BundledModelID != "qwen2.5-coder-3b-instruct" {
			t.Errorf("BundledModelID = %q", cfg.Inference.BundledModelID)
		}
		if !cfg.Inference.Enabled {
			t.Error("Enabled should stay true on a capable host")
		}
		if !cfg.Inference.PullOnStartup {
			t.Error("PullOnStartup should be unchanged when SkipPull is false")
		}
	})
	t.Run("under-spec host: inference disabled", func(t *testing.T) {
		cfg := agentconfig.Defaults()
		applyBundledSelection(&cfg, setup.BundledModelSelection{ModelID: cfg.Inference.BundledModelID, EnableInference: false, UnderSpec: true})
		if cfg.Inference.Enabled {
			t.Error("Enabled must be false on an under-spec host")
		}
	})
	t.Run("disk-short host: pull skipped", func(t *testing.T) {
		cfg := agentconfig.Defaults()
		applyBundledSelection(&cfg, setup.BundledModelSelection{ModelID: cfg.Inference.BundledModelID, EnableInference: true, SkipPull: true})
		if cfg.Inference.PullOnStartup {
			t.Error("PullOnStartup must be false when SkipPull is set")
		}
	})
}

// TestMaybeSelect_SkipsWhenAgentJSONExists is the one-shot guarantee: a boot
// with an already-written agent.json must not re-run selection or rewrite the
// file (so a prior choice — local init's, or an earlier daemon boot's — stands).
func TestMaybeSelect_SkipsWhenAgentJSONExists(t *testing.T) {
	stateDir := t.TempDir()
	agentJSONPath := filepath.Join(stateDir, "agent.json")
	const sentinel = `{"inference":{"bundled_model_id":"user-picked-model"}}`
	if err := os.WriteFile(agentJSONPath, []byte(sentinel), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := agentconfig.Defaults()
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	// gate sees agent.json present ⇒ returns before touching hardware / the file.
	maybeSelectBundledModelForFreshInstall(&cfg, false, agentJSONPath, stateDir, fs)

	got, err := os.ReadFile(agentJSONPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != sentinel {
		t.Fatalf("agent.json was rewritten despite existing; got:\n%s", got)
	}
}
