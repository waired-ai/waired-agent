package router

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

// The serving engine's records are what this device answers with
// (waired-agent#1520): a model only vLLM fetched — a host-speed probe
// records one Ready — is no local route on a device serving with ollama.
func TestSelectK_AnotherEnginesRecordIsNoLocalRoute(t *testing.T) {
	vllmOnly := readyState()
	vllmOnly.VLLMModels, vllmOnly.Models = vllmOnly.Models, map[string]catalog.ModelState{}
	s := NewSelector(Inputs{
		Manifests:  []catalog.Manifest{qwen()},
		LocalState: vllmOnly,
		Hardware:   goodHardware(),
		Runtimes:   registryWithOllama(),
	})
	cands, _ := s.SelectK(t.Context(), Request{Model: "waired/default"}, 3)
	for _, c := range cands {
		if c.ExecutionMode == "local" {
			t.Errorf("a record only vLLM holds routed locally on an ollama device: %+v", c)
		}
	}
}

// Which records Inputs reads as this device's own.
func TestInputs_LocalModels(t *testing.T) {
	st := catalog.State{
		Models:     map[string]catalog.ModelState{"o": {}},
		VLLMModels: map[string]catalog.ModelState{"v": {}},
	}
	for engine, want := range map[string]string{"": "o", catalog.RuntimeOllama: "o", catalog.RuntimeVLLM: "v"} {
		got := Inputs{LocalState: st, ServingEngine: engine}.localModels()
		if _, ok := got[want]; !ok || len(got) != 1 {
			t.Errorf("ServingEngine %q: localModels = %v, want only %q", engine, got, want)
		}
	}
}
