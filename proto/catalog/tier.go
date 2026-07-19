package catalog

import "sync"

// BestTier resolves the highest quality_tier advertised by an inference
// endpoint, given its engine type ("ollama" | "vllm") and the raw model
// names it reports (InferenceState.Type / .Models). Matching is exact,
// per engine — ollama names match Variant.Source.Tag, vLLM names match
// Variant.Source.RepoID — the same convention the agent router uses to
// resolve peer-advertised models. Unresolvable input returns 0
// ("unknown"), never an error: callers treat 0 as "no tier information"
// (a quality floor excludes it; ranking places it last).
func BestTier(engineType string, models []string) int {
	return BestTierIn(cachedBundled(), engineType, models)
}

// BestTierIn is BestTier over an explicit manifest set (tests, or a
// caller that already loaded/filtered manifests).
func BestTierIn(manifests []Manifest, engineType string, models []string) int {
	if len(models) == 0 || (engineType != RuntimeOllama && engineType != RuntimeVLLM) {
		return 0
	}
	want := make(map[string]bool, len(models))
	for _, m := range models {
		if m != "" {
			want[m] = true
		}
	}
	best := 0
	for _, mf := range manifests {
		for _, v := range mf.Variants {
			id := v.Source.Tag
			if engineType == RuntimeVLLM {
				id = v.Source.RepoID
			}
			if id == "" || !want[id] {
				continue
			}
			if !supportsRuntime(v.RuntimeSupport, engineType) {
				continue
			}
			if v.QualityTier > best {
				best = v.QualityTier
			}
		}
	}
	return best
}

func supportsRuntime(support []string, runtime string) bool {
	for _, s := range support {
		if s == runtime {
			return true
		}
	}
	return false
}

var bundledOnce = sync.OnceValue(func() []Manifest {
	// A decode failure of the embedded catalog would be a build defect
	// (the bundled files are validated by tests); degrade to "no tier
	// info" rather than panicking in a server hot path.
	ms, err := BundledManifests()
	if err != nil {
		return nil
	}
	return ms
})

func cachedBundled() []Manifest { return bundledOnce() }
