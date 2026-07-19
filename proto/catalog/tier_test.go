package catalog

import "testing"

func tierFixture() []Manifest {
	return []Manifest{
		{
			ModelID: "model-a",
			Variants: []Variant{
				{
					VariantID:      "gguf-q4",
					RuntimeSupport: []string{RuntimeOllama},
					QualityTier:    12,
					Source:         VariantSource{Type: SourceOllama, Tag: "model-a:q4"},
				},
				{
					VariantID:      "awq",
					RuntimeSupport: []string{RuntimeVLLM},
					QualityTier:    30,
					Source:         VariantSource{Type: SourceHuggingFace, RepoID: "org/model-a-awq"},
				},
			},
		},
		{
			ModelID: "model-b",
			Variants: []Variant{
				{
					VariantID:      "gguf-q8",
					RuntimeSupport: []string{RuntimeOllama},
					QualityTier:    45,
					Source:         VariantSource{Type: SourceOllama, Tag: "model-b:q8"},
				},
			},
		},
	}
}

func TestBestTierIn(t *testing.T) {
	ms := tierFixture()
	cases := []struct {
		name   string
		engine string
		models []string
		want   int
	}{
		{"ollama exact match", RuntimeOllama, []string{"model-a:q4"}, 12},
		{"vllm exact match", RuntimeVLLM, []string{"org/model-a-awq"}, 30},
		{"max over models", RuntimeOllama, []string{"model-a:q4", "model-b:q8"}, 45},
		{"unknown model", RuntimeOllama, []string{"unrelated:latest"}, 0},
		{"engine mismatch id space", RuntimeVLLM, []string{"model-a:q4"}, 0},
		{"unknown engine", "llamafile", []string{"model-a:q4"}, 0},
		{"empty models", RuntimeOllama, nil, 0},
		{"empty string model", RuntimeOllama, []string{""}, 0},
		// No prefix/substring matching: an advertised name must equal the
		// source id exactly.
		{"no substring match", RuntimeOllama, []string{"model-a"}, 0},
	}
	for _, tc := range cases {
		if got := BestTierIn(ms, tc.engine, tc.models); got != tc.want {
			t.Errorf("%s: BestTierIn(%q, %v) = %d, want %d", tc.name, tc.engine, tc.models, got, tc.want)
		}
	}
}

// TestBestTier_BundledCatalogResolves pins that the embedded catalog is
// non-empty, valid, and that BestTier resolves a real bundled ollama tag
// to its manifest tier — the contract the control plane's Public Share
// matchmaking depends on.
func TestBestTier_BundledCatalogResolves(t *testing.T) {
	ms, err := BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	if len(ms) == 0 {
		t.Fatal("bundled catalog is empty")
	}
	checked := 0
	for _, m := range ms {
		if err := m.Validate(); err != nil {
			t.Errorf("bundled manifest %s invalid: %v", m.ModelID, err)
		}
		for _, v := range m.Variants {
			if v.Source.Tag == "" {
				continue
			}
			if got := BestTier(RuntimeOllama, []string{v.Source.Tag}); got < v.QualityTier {
				t.Errorf("BestTier(ollama, %q) = %d, want >= %d", v.Source.Tag, got, v.QualityTier)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no bundled ollama-tag variants exercised")
	}
}

func TestValidDottedVersion(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"0.30.0", true},
		{"v0.7.1-rc1", true},
		{"0.6.3.post1", true},
		{"ollama version is 0.24.0", true},
		{"garbage", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := validDottedVersion(tc.in); got != tc.want {
			t.Errorf("validDottedVersion(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
