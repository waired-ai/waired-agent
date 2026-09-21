package main

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// The whole rule for how a vLLM host reaches the chosen model
// (waired-agent#1515). Product contract: owner decision 2026-09-21, a vLLM
// host keeps answering with the model it was running until the chosen one's
// weights are on disk, and switches then.
func TestPlanVLLMTarget(t *testing.T) {
	base := vllmTargetFacts{TargetModel: "new", TargetVariant: "bf16", AllowPull: true}
	with := func(mut func(*vllmTargetFacts)) vllmTargetFacts {
		f := base
		mut(&f)
		return f
	}
	for _, tc := range []struct {
		name  string
		facts vllmTargetFacts
		want  vllmTargetPlan
	}{
		{"on disk, already serving it", with(func(f *vllmTargetFacts) {
			f.TargetOnDisk, f.EngineUp, f.ServingModel, f.ServingVariant = true, true, "new", "bf16"
		}), vllmTargetPlan{Action: vllmKeep}},
		{"on disk, serving another build of it", with(func(f *vllmTargetFacts) {
			f.TargetOnDisk, f.EngineUp, f.ServingModel, f.ServingVariant = true, true, "new", "fp8"
		}), vllmTargetPlan{Action: vllmSwitchToTarget}},
		{"on disk, serving another model", with(func(f *vllmTargetFacts) {
			f.TargetOnDisk, f.EngineUp, f.ServingModel = true, true, "old"
		}), vllmTargetPlan{Action: vllmSwitchToTarget}},
		{"on disk, nothing up", with(func(f *vllmTargetFacts) {
			f.TargetOnDisk = true
		}), vllmTargetPlan{Action: vllmStartTarget}},
		{"absent, the old model keeps answering while it downloads", with(func(f *vllmTargetFacts) {
			f.EngineUp, f.ServingModel = true, "old"
		}), vllmTargetPlan{Action: vllmKeep, Download: true}},
		{"absent, nothing up, the previous model answers meanwhile", with(func(f *vllmTargetFacts) {
			f.HasPrevious = true
		}), vllmTargetPlan{Action: vllmStartPrevious, Download: true}},
		// The fleet host of #1515: the previous model was retired, so
		// nothing answers until the download lands.
		{"absent, nothing up, nothing to run meanwhile", base,
			vllmTargetPlan{Action: vllmWait, Download: true}},
		{"absent and already downloading", with(func(f *vllmTargetFacts) {
			f.TargetDownloading = true
		}), vllmTargetPlan{Action: vllmWait}},
		// A cancelled download is not restarted by the next trigger.
		{"absent, and this process already started its download", with(func(f *vllmTargetFacts) {
			f.AlreadyDispatched, f.EngineUp = true, true
		}), vllmTargetPlan{Action: vllmKeep}},
		{"absent, downloads off, nothing to run", with(func(f *vllmTargetFacts) {
			f.AllowPull = false
		}), vllmTargetPlan{Action: vllmRefuseNoPull}},
		{"absent, downloads off, the previous model answers", with(func(f *vllmTargetFacts) {
			f.AllowPull, f.HasPrevious = false, true
		}), vllmTargetPlan{Action: vllmStartPrevious}},
		{"absent, downloads off, the old model keeps answering", with(func(f *vllmTargetFacts) {
			f.AllowPull, f.EngineUp = false, true
		}), vllmTargetPlan{Action: vllmKeep}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := planVLLMTarget(tc.facts); got != tc.want {
				t.Errorf("plan = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// Which model may answer while the chosen one downloads.
func TestVLLMPreviousCandidate(t *testing.T) {
	manifests := []catalog.Manifest{{
		ModelID: "qwen3.5-4b",
		Variants: []catalog.Variant{{
			VariantID: "bf16", RuntimeSupport: []string{catalog.RuntimeVLLM},
			Source: catalog.VariantSource{Type: catalog.SourceHuggingFace, RepoID: "Qwen/Qwen3.5-4B"},
		}},
	}}
	active := func(model, variant, runtime string) *catalog.ActiveSelection {
		return &catalog.ActiveSelection{ModelID: model, VariantID: variant, Runtime: runtime}
	}
	ready := catalog.State{Models: map[string]catalog.ModelState{
		"qwen3.5-4b":  {State: catalog.ModelStateReady, VariantID: "bf16", LocalPath: "/models/hf/Qwen__Qwen3.5-4B"},
		"gpt-oss-20b": {State: catalog.ModelStateReady, VariantID: "mxfp4-safetensors", LocalPath: "/models/hf/openai__gpt-oss-20b"},
	}}
	var asked []string
	exists := func(p string) bool { asked = append(asked, p); return true }

	for _, tc := range []struct {
		name   string
		active *catalog.ActiveSelection
		st     catalog.State
		target string
		exists func(string) bool
		want   bool
	}{
		{"the model the engine ran", active("qwen3.5-4b", "bf16", catalog.RuntimeVLLM), ready, "qwen3.6-35b-a3b", exists, true},
		// The fleet host of #1515: gpt-oss-20b is retired, so it is not in
		// this build's catalog, and nothing may start it.
		{"a retired model", active("gpt-oss-20b", "mxfp4-safetensors", catalog.RuntimeVLLM), ready, "qwen3.6-35b-a3b", exists, false},
		{"it is the chosen model", active("qwen3.5-4b", "bf16", catalog.RuntimeVLLM), ready, "qwen3.5-4b", exists, false},
		{"it ran on ollama", active("qwen3.5-4b", "bf16", catalog.RuntimeOllama), ready, "qwen3.6-35b-a3b", exists, false},
		{"another build is what is on disk", active("qwen3.5-4b", "fp8", catalog.RuntimeVLLM), ready, "qwen3.6-35b-a3b", exists, false},
		{"its weights are not ready", active("qwen3.5-4b", "bf16", catalog.RuntimeVLLM),
			catalog.State{Models: map[string]catalog.ModelState{"qwen3.5-4b": {State: catalog.ModelStateDownloading, VariantID: "bf16", LocalPath: "/x"}}},
			"qwen3.6-35b-a3b", exists, false},
		{"its directory is gone", active("qwen3.5-4b", "bf16", catalog.RuntimeVLLM), ready, "qwen3.6-35b-a3b",
			func(string) bool { return false }, false},
		{"nothing recorded", nil, ready, "qwen3.6-35b-a3b", exists, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asked = nil
			_, _, path, ok := vllmPreviousCandidate(tc.active, manifests, tc.st, tc.target, "0.29.0", tc.exists)
			if ok != tc.want {
				t.Fatalf("eligible = %v, want %v", ok, tc.want)
			}
			if ok && (path != "/models/hf/Qwen__Qwen3.5-4B" || len(asked) != 1 || asked[0] != path) {
				t.Errorf("path = %q, asked about %v; want the weights directory, checked", path, asked)
			}
		})
	}
}

// A start asked for while another runs is run once more afterwards instead
// of being dropped (waired-agent#1515): the weights of a vLLM switch can
// land while the previous model is still starting.
func TestRunEngineBootstrap_RunsAgainWhenAskedDuringARun(t *testing.T) {
	gate := make(chan struct{})
	var calls atomic.Int32
	p := &agentInferenceProvider{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		// The first gate every start passes; blocking it holds the first
		// run in flight, and every call is one run.
		isInferenceDisabled: func() bool {
			if calls.Add(1) == 1 {
				<-gate
			}
			return true
		},
	}
	done := make(chan struct{})
	go func() { p.runEngineBootstrap(context.Background(), "first"); close(done) }()
	waitForCond(t, 2*time.Second, "the first run to start", func() bool { return calls.Load() == 1 })
	p.runEngineBootstrap(context.Background(), "second") // returns at once: one is running
	close(gate)
	<-done
	if n := calls.Load(); n != 2 {
		t.Errorf("runs = %d, want 2: the request made during the first run was dropped", n)
	}
	if p.engineStartInFlight.Load() {
		t.Error("the start claim was left set")
	}
}

// A finished download moves Active only when no engine is up. While one is,
// it still serves the previous model, and the switch writes Active once the
// engine is ready on the new one. Product contract: owner decision
// 2026-09-21 on waired-agent#1515.
func TestHFWeightsLanded_ActiveMovesOnlyWithNothingUp(t *testing.T) {
	for _, tc := range []struct {
		name     string
		engineUp bool
		want     string
	}{
		{"an engine is up: Active stays on the model it serves", true, "previous"},
		{"nothing is up: the landed model is recorded", false, "hybrid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := vllmSwapProvider(t)
			if tc.engineUp {
				p.setVLLM(&recordingAdapter{name: "vllm", health: infruntime.StateReady})
			}
			chosen := "hybrid"
			p.preferredOverride.Store(&chosen)
			if err := p.store.Update(func(s *catalog.State) {
				s.Active = &catalog.ActiveSelection{Runtime: catalog.RuntimeVLLM, ModelID: "previous", VariantID: "bf16"}
				s.Models = map[string]catalog.ModelState{
					"hybrid": {State: catalog.ModelStateReady, VariantID: "safetensors", LocalPath: t.TempDir()},
				}
			}); err != nil {
				t.Fatal(err)
			}
			p.hfWeightsLanded(context.Background(), "hybrid", "safetensors")
			st, err := p.store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if st.Active == nil || st.Active.ModelID != tc.want {
				t.Errorf("Active = %+v, want %s", st.Active, tc.want)
			}
		})
	}
}
