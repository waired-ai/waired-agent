package catalog

// ServedMaxParallel is the most requests at once a device may serve the
// build it reports: Variant.MaxParallel of the bundled catalog's entry for
// modelID and variantID on engineType. 0 means no limit — the engine is not
// ollama, the model is unknown, or the build sets none.
//
// It reads the cached bundled catalog, so the control plane can call it
// while building every peer entry of a network map.
func ServedMaxParallel(engineType, modelID, variantID string) int {
	return ServedMaxParallelIn(cachedBundled(), engineType, modelID, variantID)
}

// ServedMaxParallelIn is ServedMaxParallel over the given manifests.
//
// A device that does not report its variant, or reports one this catalog
// does not know, still gets a limit when every ollama build of the model
// shares it: which of those builds it serves cannot change the answer. When
// the builds disagree the answer is 0, since holding a build that allows
// more down to one that allows less would under-report it.
func ServedMaxParallelIn(ms []Manifest, engineType, modelID, variantID string) int {
	if engineType != RuntimeOllama {
		return 0
	}
	m, ok := LookupByAlias(modelID, ms)
	if !ok {
		return 0
	}
	agreed, seen := 0, false
	for _, v := range m.Variants {
		if !supportsRuntime(v.RuntimeSupport, RuntimeOllama) {
			continue
		}
		if variantID != "" && v.VariantID == variantID {
			return v.MaxParallel
		}
		if !seen {
			agreed, seen = v.MaxParallel, true
		} else if v.MaxParallel != agreed {
			agreed = 0
		}
	}
	return agreed
}
