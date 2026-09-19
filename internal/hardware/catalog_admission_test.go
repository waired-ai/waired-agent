package hardware

import (
	"fmt"
	"slices"
	"testing"

	"github.com/waired-ai/waired-agent/proto/catalog"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// admissionExemptions are the offered builds that do not meet the
// admission rule yet, each with the change that takes it out of the
// catalog. An entry that starts passing fails the test too, so the table
// cannot outlive the reason it was written for.
//
// Empty since waired-ai/waired#1427 retired glm-5.2 and deepseek-v4-flash,
// whose three vLLM builds sat here.
var admissionExemptions = map[string]string{}

// TestBundledCatalog_EveryBuildFitsTheReferenceHost is the catalog's
// admission rule (decision 3 of
// docs/decisions/20260916/0340-catalog-reference-host-rank-and-admission.md):
// a build ships only if the reference host — a Strix Halo with 128 GB of
// unified memory — holds it fully resident with a ~200k context window.
//
// The host is built through strixHaloUMA, as windowsStrixHalo builds it
// for the #863 measurement: 512 MB carved out, the OS seeing 127 GB, the
// configuration that loaded a 76.3 GB model in 15 s. A hand-written
// budget would keep passing after the producer stopped agreeing with it.
//
// An ollama build must get the recommendation verdict there (decision 10
// of docs/decisions/20260913/2355-catalog-variant-kv-and-residency-rulings.md).
// A vLLM build has no product path on this host (the vLLM engine is
// NVIDIA and Linux only), so its min_vram_mb — sized with a 200,704-token
// window — is held against the same accelerator budget.
func TestBundledCatalog_EveryBuildFitsTheReferenceHost(t *testing.T) {
	h := windowsStrixHalo(127, 0, 512)
	budget := h.OllamaVRAMBudgetMB()
	if budget != 96*1024 {
		t.Fatalf("reference host budget = %d MB, want %d: the Strix Halo rule moved, "+
			"so this test no longer describes the reference host", budget, 96*1024)
	}

	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	seen := map[string]bool{}
	for _, m := range manifests {
		for _, v := range m.Variants {
			key := m.ModelID + "/" + v.VariantID
			seen[key] = true
			miss := admissionMiss(m, v, h, budget)
			reason, exempt := admissionExemptions[key]
			switch {
			case miss != "" && !exempt:
				t.Errorf("%s does not fit the reference host: %s", key, miss)
			case miss == "" && exempt:
				t.Errorf("%s now fits the reference host; remove its exemption (%s)", key, reason)
			}
		}
	}
	for key := range admissionExemptions {
		if !seen[key] {
			t.Errorf("exemption %s names no offered build; remove it", key)
		}
	}
}

// admissionMiss returns why v does not meet the admission rule on h, or
// "" when it does.
func admissionMiss(m catalog.Manifest, v catalog.Variant, h hostfit.Host, budgetMB int) string {
	if m.ContextLength < hostfit.ServingWindow200k {
		return fmt.Sprintf("native window %d is below %d", m.ContextLength, hostfit.ServingWindow200k)
	}
	if slices.Contains(v.RuntimeSupport, catalog.RuntimeOllama) {
		if rec := hostfit.OllamaRecommendModel(m, v, h); !rec.Fits {
			return fmt.Sprintf("ollama recommendation refused: %s (need %d MB, have %d MB)",
				rec.Reason, rec.NeedMB, rec.HaveMB)
		}
	}
	if slices.Contains(v.RuntimeSupport, catalog.RuntimeVLLM) && v.MinVRAMMB > budgetMB {
		return fmt.Sprintf("min_vram_mb %d is over the %d MB budget", v.MinVRAMMB, budgetMB)
	}
	return ""
}
