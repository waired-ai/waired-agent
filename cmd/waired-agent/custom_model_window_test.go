package main

import (
	"path/filepath"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// CustomModelWindow is what a device serving a custom model under 200,704
// tokens tells its peers it can take (waired-ai/waired#1481), where
// DeclaredContextWindow declares nothing. A record of today's behaviour.
func TestCustomModelWindow(t *testing.T) {
	bundled, err := catalog.BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatal(err)
	}
	small := customTestManifest(t, "small") // 40,960 tokens
	src := catalog.NewCustomSource(filepath.Join(t.TempDir(), "c.json"), "net_a", bundled, nil)
	if _, err := src.Replace(catalog.CustomModelSet{Revision: "r1", Own: []catalog.Manifest{small}}, nil); err != nil {
		t.Fatal(err)
	}
	prov := func(t *testing.T, active string, applied int) *agentInferenceProvider {
		t.Helper()
		a := newTestAdapter(t)
		if applied > 0 {
			a.SetAppliedTuning(infruntime.ModelTuning{ModelID: active, ContextLength: applied})
		}
		store := catalog.NewStore(filepath.Join(t.TempDir(), "state.json"))
		if err := store.Update(func(s *catalog.State) {
			s.Models = map[string]catalog.ModelState{active: {State: catalog.ModelStateReady, VariantID: "q4-k-m"}}
			s.Active = &catalog.ActiveSelection{Runtime: catalog.RuntimeOllama, ModelID: active, VariantID: "q4-k-m"}
		}); err != nil {
			t.Fatal(err)
		}
		return &agentInferenceProvider{manifests: bundled, custom: src, ollama: a, store: store}
	}
	if got := prov(t, small.ModelID, 40960).CustomModelWindow(); got != 40960 {
		t.Errorf("custom model at 40,960: %d", got)
	}
	if got := prov(t, small.ModelID, 40960).DeclaredContextWindow(); got != 0 {
		t.Errorf("the declared window is %d; below 200,704 nothing is declared", got)
	}
	// The engine was told more than the model reaches: the model's own
	// window is the answer.
	if got := prov(t, small.ModelID, 65536).CustomModelWindow(); got != 40960 {
		t.Errorf("clamped to the model: %d", got)
	}
	if got := prov(t, small.ModelID, 0).CustomModelWindow(); got != 0 {
		t.Errorf("nothing tuned yet: %d", got)
	}
	if got := prov(t, bundled[0].ModelID, 40960).CustomModelWindow(); got != 0 {
		t.Errorf("a catalog model: %d", got)
	}
}
