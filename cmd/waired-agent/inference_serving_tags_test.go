package main

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

// activeServingTags decides whether the status surfaces may say "not the
// model this computer serves" (waired-agent#837). The failure that matters is
// asymmetric: an empty answer costs a missing clause, a WRONG answer puts
// "model not loaded" on the footer of a perfectly warm machine. So every case
// that cannot be resolved must come back empty.
func servingTagsProvider(t *testing.T, seed func(*catalog.State)) *agentInferenceProvider {
	t.Helper()
	p := &agentInferenceProvider{store: catalog.NewStore(filepath.Join(t.TempDir(), "state.json"))}
	if seed != nil {
		if err := p.store.Update(seed); err != nil {
			t.Fatalf("seed store: %v", err)
		}
	}
	return p
}

func TestActiveServingTags(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(*catalog.State)
		want []string
	}{
		{name: "no active selection", seed: nil},
		{
			name: "active, but serving on another engine",
			seed: func(s *catalog.State) {
				s.Active = &catalog.ActiveSelection{Runtime: catalog.RuntimeVLLM, ModelID: "m1"}
				s.Models = map[string]catalog.ModelState{"m1": {OllamaTag: "m1:q4"}}
			},
		},
		{
			name: "active model is not in the store",
			seed: func(s *catalog.State) {
				s.Active = &catalog.ActiveSelection{Runtime: catalog.RuntimeOllama, ModelID: "m1"}
			},
		},
		{
			name: "active model has no engine-native name yet",
			seed: func(s *catalog.State) {
				s.Active = &catalog.ActiveSelection{Runtime: catalog.RuntimeOllama, ModelID: "m1"}
				s.Models = map[string]catalog.ModelState{"m1": {}}
			},
		},
		{
			name: "the ordinary case",
			seed: func(s *catalog.State) {
				s.Active = &catalog.ActiveSelection{Runtime: catalog.RuntimeOllama, ModelID: "m1"}
				s.Models = map[string]catalog.ModelState{"m1": {OllamaTag: "m1:q4"}}
			},
			want: []string{"m1:q4"},
		},
		{
			// #642: the serving tag is a model waired derived from the
			// catalog one. Before it has been built, the engine holds the
			// base — and a machine holding the base is not a machine with
			// the wrong model loaded.
			name: "a derived batch model counts its base too",
			seed: func(s *catalog.State) {
				s.Active = &catalog.ActiveSelection{Runtime: catalog.RuntimeOllama, ModelID: "m1"}
				s.Models = map[string]catalog.ModelState{"m1": {OllamaTag: "m1-waired:q4", BaseOllamaTag: "m1:q4"}}
			},
			want: []string{"m1-waired:q4", "m1:q4"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := servingTagsProvider(t, tc.seed).activeServingTags()
			if !slices.Equal(got, tc.want) {
				t.Errorf("activeServingTags = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestActiveServingTags_NilProvider(t *testing.T) {
	var p *agentInferenceProvider
	if got := p.activeServingTags(); got != nil {
		t.Errorf("nil provider = %v, want nil", got)
	}
	if got := (&agentInferenceProvider{}).activeServingTags(); got != nil {
		t.Errorf("provider with no store = %v, want nil", got)
	}
}

func TestResidencyTagOrNone(t *testing.T) {
	if got := residencyTagOrNone(""); got != "none" {
		t.Errorf("empty tag = %q, want %q — an empty log field reads as one nobody set", got, "none")
	}
	if got := residencyTagOrNone("m1:q4"); got != "m1:q4" {
		t.Errorf("tag = %q", got)
	}
}
