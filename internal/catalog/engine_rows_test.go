package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Each engine's records are its own (waired-agent#1520). Record of today's
// behaviour.
func TestEngineRows_Accessors(t *testing.T) {
	var s State
	s.SetModel(RuntimeOllama, "m", ModelState{VariantID: "q8"})
	s.SetModel(RuntimeVLLM, "m", ModelState{VariantID: "bf16"})
	s.SetModel("mlx", "m", ModelState{VariantID: "x"}) // an engine that keeps no records

	if ms, ok := s.ModelFor(RuntimeOllama, "m"); !ok || ms.VariantID != "q8" {
		t.Errorf("ollama's record = %+v %v, want q8", ms, ok)
	}
	if ms, ok := s.ModelFor(RuntimeVLLM, "m"); !ok || ms.VariantID != "bf16" {
		t.Errorf("vLLM's record = %+v %v, want bf16", ms, ok)
	}
	if _, ok := s.ModelFor("mlx", "m"); ok {
		t.Error("an engine that keeps no records answered with one")
	}
	if got := s.RecordsFor("m"); len(got) != 2 || got[RuntimeOllama].VariantID != "q8" || got[RuntimeVLLM].VariantID != "bf16" {
		t.Errorf("RecordsFor = %+v, want both engines' records", got)
	}

	s.RemoveModel(RuntimeVLLM, "m")
	if _, ok := s.ModelFor(RuntimeVLLM, "m"); ok {
		t.Error("vLLM's record survived its removal")
	}
	if _, ok := s.ModelFor(RuntimeOllama, "m"); !ok {
		t.Error("removing vLLM's record took ollama's")
	}
}

// state.json: vLLM's records go under their own key, left out while there
// are none, and survive a save and load.
func TestEngineRows_StateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := NewStore(path)
	if err := store.Update(func(s *State) {
		s.SetModel(RuntimeOllama, "m", ModelState{VariantID: "q8", State: ModelStateReady})
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "vllm_models") {
		t.Errorf("a state with no vLLM records wrote the key:\n%s", raw)
	}

	if err := store.Update(func(s *State) {
		s.SetModel(RuntimeVLLM, "m", ModelState{VariantID: "bf16", State: ModelStateReady, LocalPath: "/models/hf/acme__m"})
	}); err != nil {
		t.Fatal(err)
	}
	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if ms, _ := st.ModelFor(RuntimeVLLM, "m"); ms.VariantID != "bf16" || ms.LocalPath != "/models/hf/acme__m" {
		t.Errorf("vLLM's record after a load = %+v", ms)
	}
	if ms, _ := st.ModelFor(RuntimeOllama, "m"); ms.VariantID != "q8" {
		t.Errorf("ollama's record after a load = %+v", ms)
	}
}
