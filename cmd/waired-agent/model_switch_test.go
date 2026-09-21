package main

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/management"
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
