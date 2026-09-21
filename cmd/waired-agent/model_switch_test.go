package main

import (
	"context"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/management"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// The outcome decides which approved sentence `waired models use`, the
// Waired app and `waired inference status` print (waired-agent#1515), so
// every row here is a case one of them words differently. Record of today's
// behaviour; the sentences themselves are pinned in cmd/waired and the tray.
func TestSwitchOutcome(t *testing.T) {
	const ollama, vllm = catalog.RuntimeOllama, catalog.RuntimeVLLM
	for _, c := range []struct {
		name string
		f    switchFacts
		want management.ModelSwitchOutcome
	}{
		{
			name: "ollama, on disk: applied in place",
			f:    switchFacts{Engine: ollama, TargetModel: "b", EngineUp: true, ServingModel: "a"},
			want: management.ModelSwitchOutcome{},
		},
		{
			name: "ollama, downloading, the old model answers",
			f:    switchFacts{Engine: ollama, TargetModel: "b", Downloading: true, EngineUp: true, ServingModel: "a"},
			want: management.ModelSwitchOutcome{Downloading: true},
		},
		{
			name: "ollama, downloading, no model on this computer yet",
			f:    switchFacts{Engine: ollama, TargetModel: "b", Downloading: true, EngineUp: true},
			want: management.ModelSwitchOutcome{Downloading: true, NothingAnswers: true},
		},
		{
			name: "ollama never reports a vLLM fit",
			f: switchFacts{Engine: ollama, TargetModel: "b", EngineUp: true, ServingModel: "a",
				NeedVRAMMB: 36864, HaveVRAMMB: 24463},
			want: management.ModelSwitchOutcome{},
		},
		{
			name: "vLLM, on disk, another model up: the engine restarts onto it",
			f:    switchFacts{Engine: vllm, TargetModel: "b", EngineUp: true, ServingModel: "a"},
			want: management.ModelSwitchOutcome{EngineRestarts: true},
		},
		{
			name: "vLLM, the chosen build is already up: nothing to do",
			f: switchFacts{Engine: vllm, TargetModel: "b", TargetVariant: "bf16", EngineUp: true,
				ServingModel: "b", ServingVariant: "bf16"},
			want: management.ModelSwitchOutcome{},
		},
		{
			name: "vLLM, another build of the same model is up: restart",
			f: switchFacts{Engine: vllm, TargetModel: "b", TargetVariant: "fp8", EngineUp: true,
				ServingModel: "b", ServingVariant: "bf16"},
			want: management.ModelSwitchOutcome{EngineRestarts: true},
		},
		{
			name: "vLLM, downloading, the old model answers until it lands",
			f:    switchFacts{Engine: vllm, TargetModel: "b", Downloading: true, EngineUp: true, ServingModel: "a"},
			want: management.ModelSwitchOutcome{Downloading: true, EngineRestarts: true},
		},
		{
			name: "vLLM, downloading, nothing up but the previous model will be started",
			f: switchFacts{Engine: vllm, TargetModel: "b", Downloading: true,
				PreviousWillAnswer: true, PreviousModel: "a"},
			want: management.ModelSwitchOutcome{Downloading: true, EngineRestarts: true},
		},
		{
			// The case the #1515 host was in: a retired previous model, so
			// nothing to start meanwhile.
			name: "vLLM, downloading, nothing up and nothing to start",
			f:    switchFacts{Engine: vllm, TargetModel: "b", Downloading: true},
			want: management.ModelSwitchOutcome{Downloading: true, EngineRestarts: true, NothingAnswers: true},
		},
		{
			name: "vLLM, a build not expected to fit",
			f: switchFacts{Engine: vllm, TargetModel: "b", Downloading: true,
				NeedVRAMMB: 36864, HaveVRAMMB: 24463},
			want: management.ModelSwitchOutcome{Downloading: true, EngineRestarts: true, NothingAnswers: true,
				NeedVRAMMB: 36864, HaveVRAMMB: 24463},
		},
		{
			name: "vLLM, a build that fits reports no figures",
			f:    switchFacts{Engine: vllm, TargetModel: "b", EngineUp: true, ServingModel: "a", NeedVRAMMB: 8192, HaveVRAMMB: 24463},
			want: management.ModelSwitchOutcome{EngineRestarts: true},
		},
		{
			name: "vLLM, unknown budget reports no figures",
			f:    switchFacts{Engine: vllm, TargetModel: "b", EngineUp: true, ServingModel: "a", NeedVRAMMB: 36864},
			want: management.ModelSwitchOutcome{EngineRestarts: true},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := switchOutcome(c.f); got != c.want {
				t.Errorf("switchOutcome = %+v, want %+v", got, c.want)
			}
		})
	}
}

// The status line names what answers meanwhile.
func TestSwitchFacts_AnsweringModel(t *testing.T) {
	for _, c := range []struct {
		name string
		f    switchFacts
		want string
	}{
		{"the running engine's model", switchFacts{Engine: catalog.RuntimeVLLM, TargetModel: "b", EngineUp: true, ServingModel: "a"}, "a"},
		{"the previous model a vLLM host will start", switchFacts{Engine: catalog.RuntimeVLLM, TargetModel: "b", PreviousWillAnswer: true, PreviousModel: "a"}, "a"},
		{"nothing", switchFacts{Engine: catalog.RuntimeVLLM, TargetModel: "b"}, ""},
		{"the chosen model itself is not a stand-in", switchFacts{Engine: catalog.RuntimeOllama, TargetModel: "b", EngineUp: true, ServingModel: "b"}, ""},
		{"ollama's previous-model start does not exist", switchFacts{Engine: catalog.RuntimeOllama, TargetModel: "b", PreviousWillAnswer: true, PreviousModel: "a"}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := c.f.answeringModel(); got != c.want {
				t.Errorf("answeringModel = %q, want %q", got, c.want)
			}
		})
	}
}

// What a vLLM switch says about the meantime is what this computer can start
// (waired-agent#1515). On the fleet host the model it had been running was a
// 35B its 24 GB card could not hold; the engine was failing on it when the
// next model was chosen, and `waired models use` said the current model kept
// answering. Record of today's behaviour.
func TestSwitchFactsFor_OnlyAModelThatCanStartAnswers(t *testing.T) {
	vllmBuild := func(id string, minMB int) catalog.Manifest {
		return catalog.Manifest{ModelID: id, Variants: []catalog.Variant{{
			VariantID: "st", RuntimeSupport: []string{catalog.RuntimeVLLM}, MinVRAMMB: minMB,
			Source: catalog.VariantSource{Type: catalog.SourceHuggingFace, RepoID: "acme/" + id},
		}}}
	}
	provider := func(t *testing.T, previous string) *agentInferenceProvider {
		t.Helper()
		p := activeReaderProvider(t)
		p.manifests = []catalog.Manifest{vllmBuild("big", 36864), vllmBuild("fits", 8192), vllmBuild("chosen", 8192)}
		p.setServingEngine(catalog.RuntimeVLLM)
		p.profiler = hardware.NewProfiler(t.TempDir(),
			hardware.WithRAM(func(context.Context) (int, int, error) { return 128, 120, nil }),
			hardware.WithGPU(func(context.Context) ([]hardware.GPU, hardware.Accelerators, error) {
				return []hardware.GPU{{Vendor: "nvidia", Model: "card", VRAMTotalMB: 24576}}, hardware.Accelerators{}, nil
			}),
			hardware.WithEngineVersion(func(context.Context, string) (bool, string) { return false, "" }),
			// A 24 GB card on Linux, whatever runs the test: on an Apple
			// Silicon runner the default UMA step makes the profile unified
			// memory (the GPU's wired limit, else 3/4 of RAM — 96 GB of the
			// 128 here), and the 35B fitted the runner.
			hardware.WithOSArch(func() (string, string) { return "linux", "amd64" }),
			hardware.WithUMA(func(context.Context, *hardware.Profile) {}),
		)
		if err := p.store.Update(func(s *catalog.State) {
			s.Active = &catalog.ActiveSelection{Runtime: catalog.RuntimeVLLM, ModelID: previous, VariantID: "st"}
			s.Models = map[string]catalog.ModelState{
				previous: {State: catalog.ModelStateReady, VariantID: "st", LocalPath: t.TempDir()},
			}
		}); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ctx := context.Background()

	t.Run("nothing is up", func(t *testing.T) {
		for previous, wantAnswers := range map[string]bool{"big": false, "fits": true} {
			f := provider(t, previous).switchFactsFor(ctx, "chosen", true)
			if f.PreviousWillAnswer != wantAnswers {
				t.Errorf("previous %s: PreviousWillAnswer = %v, want %v", previous, f.PreviousWillAnswer, wantAnswers)
			}
			if got := switchOutcome(f).NothingAnswers; got == wantAnswers {
				t.Errorf("previous %s: NothingAnswers = %v", previous, got)
			}
		}
	})

	t.Run("a start is under way", func(t *testing.T) {
		for _, c := range []struct {
			serving, health string
			wantAnswers     bool
		}{
			{"big", infruntime.StateStarting, false},
			{"fits", infruntime.StateStarting, true},
			// Up is up: the estimate is not asked of an engine answering.
			{"big", infruntime.StateReady, true},
		} {
			p := provider(t, c.serving)
			p.setVLLM(&recordingAdapter{name: "vllm", health: c.health})
			p.vllmServing.Store(&vllmServingModel{ModelID: c.serving, VariantID: "st"})
			f := p.switchFactsFor(ctx, "chosen", true)
			if got := f.answeringModel() != ""; got != c.wantAnswers {
				t.Errorf("%s %s: answering %q, want answering %v", c.serving, c.health, f.answeringModel(), c.wantAnswers)
			}
			if got := switchOutcome(f).NothingAnswers; got == c.wantAnswers {
				t.Errorf("%s %s: NothingAnswers = %v", c.serving, c.health, got)
			}
		}
	})
}
