package main

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/setup"
)

// bootFlagSet registers the REAL boot flag surface — `--disable-inference`
// from main.go plus every `--inference-*` from RegisterInferenceFlags — so
// these tests exercise the flag names the daemon actually parses. A
// hand-listed subset would keep passing after a rename that silently moved
// a flag out of the intent resolver's reach.
func bootFlagSet(t *testing.T) *flag.FlagSet {
	t.Helper()
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Bool("disable-inference", false, "")
	fs.String("state-dir", "", "")
	cfg := agentconfig.Defaults()
	cfg.RegisterInferenceFlags(fs)
	return fs
}

// TestResolveInferenceIntent pins which operator signals speak about the
// MODEL and which merely wire the engine.
//
// The rule it replaced treated any `--inference-*` flag as "the operator
// already chose", so `--inference-ollama-port 11500` on a fresh 4 GB host
// skipped hardware-aware selection AND the below-recommended-spec default, and the
// daemon pre-pulled the compiled default instead. A port number cannot
// say which model belongs on a machine, and the cases below are the line
// between the two kinds of flag.
//
// PRODUCT CONTRACT, not a record of today's behaviour.
func TestResolveInferenceIntent(t *testing.T) {
	cases := []struct {
		name             string
		disableInference bool
		args             []string
		environ          []string
		want             inferenceIntent
	}{
		{name: "pristine fresh install"},
		{name: "unrelated env", environ: []string{"PATH=/usr/bin", "HOME=/root"}},

		// The defect this function exists to fix: engine wiring must not
		// silence model selection.
		{name: "ollama port says nothing about the model", args: []string{"-inference-ollama-port", "11500"}},
		{name: "cache and engine preference say nothing either",
			args: []string{"-inference-max-cache-gb", "50", "-inference-preferred-engine", "ollama"}},
		{name: "vllm knobs say nothing either", args: []string{"-inference-vllm-port", "8001"}},
		{name: "unrelated env with an inference-looking prefix",
			environ: []string{"WAIRED_INFERENCE_MAX_CACHE_GB=50"}},

		{name: "--disable-inference", disableInference: true, want: inferenceIntent{Skip: true}},
		{name: "--inference-enabled=false", args: []string{"-inference-enabled=false"}, want: inferenceIntent{Skip: true}},
		{name: "WAIRED_INFERENCE_ENABLED=false", environ: []string{"WAIRED_INFERENCE_ENABLED=false"},
			want: inferenceIntent{Skip: true}},

		{name: "--inference-enabled=true forces, it does not skip",
			args: []string{"-inference-enabled=true"}, want: inferenceIntent{Forced: true}},
		{name: "WAIRED_INFERENCE_ENABLED=true forces", environ: []string{"WAIRED_INFERENCE_ENABLED=true"},
			want: inferenceIntent{Forced: true}},
		{name: "unreadable enablement defers rather than guesses",
			environ: []string{"WAIRED_INFERENCE_ENABLED=yes-please"}, want: inferenceIntent{Skip: true}},

		{name: "--inference-bundled-model-id pins, it does not skip",
			args: []string{"-inference-bundled-model-id", "qwen2.5-coder-14b"}, want: inferenceIntent{Pinned: true}},
		{name: "WAIRED_INFERENCE_BUNDLED_MODEL_ID pins",
			environ: []string{"WAIRED_INFERENCE_BUNDLED_MODEL_ID=qwen2.5-coder-14b"}, want: inferenceIntent{Pinned: true}},

		// #306: the operator's own preferred model takes responsibility
		// for the serving path, and the bundled pre-pull stands down.
		{name: "--inference-preferred-model-id skips",
			args: []string{"-inference-preferred-model-id", "qwen3.5-9b"}, want: inferenceIntent{Skip: true}},
		{name: "WAIRED_INFERENCE_PREFERRED_MODEL_ID skips",
			environ: []string{"WAIRED_INFERENCE_PREFERRED_MODEL_ID=qwen3.5-9b"}, want: inferenceIntent{Skip: true}},

		{name: "pin and force compose",
			args: []string{"-inference-bundled-model-id", "qwen2.5-coder-14b", "-inference-enabled=true"},
			want: inferenceIntent{Pinned: true, Forced: true}},
		{name: "--disable-inference beats a pin",
			disableInference: true, args: []string{"-inference-bundled-model-id", "qwen2.5-coder-14b"},
			want: inferenceIntent{Skip: true, Pinned: true}},
		// Config precedence is defaults < JSON < env < flags, so the flag
		// wins on the enablement axis.
		{name: "flag beats env on the same axis",
			args: []string{"-inference-enabled=true"}, environ: []string{"WAIRED_INFERENCE_ENABLED=false"},
			want: inferenceIntent{Forced: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := bootFlagSet(t)
			if err := fs.Parse(tc.args); err != nil {
				t.Fatalf("parse %v: %v", tc.args, err)
			}
			// The three DECISION fields only. Enablement is the raw
			// signal they are derived from, and it has its own table in
			// inference_startup_test.go — repeating it in every row here
			// would say nothing about the model/engine split this test
			// is about.
			got := resolveInferenceIntent(tc.disableInference, fs, tc.environ)
			got.Enablement = nil
			if got != tc.want {
				t.Fatalf("resolveInferenceIntent = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestShouldAutoSelectBundledModel covers the waired#756 boot gate: the daemon
// auto-selects the bundled model only on a pristine fresh install that the
// operator has not opted out of. A pin or a force does NOT stop selection —
// they are carried into it as inputs (see TestResolveInferenceIntent).
func TestShouldAutoSelectBundledModel(t *testing.T) {
	cases := []struct {
		name       string
		agentJSON  bool
		preference bool
		intent     inferenceIntent
		want       bool
	}{
		{name: "pristine fresh install", want: true},
		{name: "agent.json already present", agentJSON: true},
		{name: "model preference present", preference: true},
		{name: "operator opted out", intent: inferenceIntent{Skip: true}},
		{name: "pinned still selects", intent: inferenceIntent{Pinned: true}, want: true},
		{name: "forced still selects", intent: inferenceIntent{Forced: true}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldAutoSelectBundledModel(tc.agentJSON, tc.preference, tc.intent)
			if got != tc.want {
				t.Fatalf("shouldAutoSelectBundledModel = %v, want %v", got, tc.want)
			}
		})
	}
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
		applyBundledSelection(&cfg, setup.BundledModelSelection{ModelID: cfg.Inference.BundledModelID, EnableInference: false, BelowRecommendedSpec: true})
		if cfg.Inference.Enabled {
			t.Error("Enabled must be false on a host below the recommended spec")
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
