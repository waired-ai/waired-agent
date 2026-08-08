package main

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// TestDeclaredWindowMatchesTheTuner is the anti-drift check
// waired-ai/waired-agent#464 asks for: "one predicate, shared … so 'the
// wizard recommends it' and 'the machine serves it at 200k' cannot drift
// apart."
//
// The recommendation is hostfit.OllamaDeclaresWindow; the engine is
// started at computeOllamaTuning's ContextLength. They are the same
// function today, and this walks the whole shipped catalog against a grid
// of real host shapes to keep them that way — a future caller that
// re-derives either side, or a sizing branch that forgets the other, is
// caught here rather than by a user whose node advertises a window it
// never loaded.
//
// It deliberately reads the tuner through computeOllamaTuning rather than
// the shared function it calls, so the check survives the tuner growing
// its own adjustments.
func TestDeclaredWindowMatchesTheTuner(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	card := func(ramGB, vramMB int) hardware.Profile {
		return hardware.Profile{OS: "linux", Arch: "x86_64", RAMTotalGB: ramGB,
			GPUs: []hardware.GPU{{Vendor: "nvidia", Model: "test", VRAMTotalMB: vramMB}}}
	}
	mac := func(ramGB, usableMB int) hardware.Profile {
		return hardware.Profile{OS: "darwin", Arch: "arm64", RAMTotalGB: ramGB,
			UnifiedMemory: true, UsableVRAMMB: usableMB,
			GPUs: []hardware.GPU{{Vendor: "apple", Model: "Apple M4"}}}
	}
	hosts := map[string]hardware.Profile{
		"cpu-8gb":       {OS: "linux", RAMTotalGB: 8},
		"cpu-64gb":      {OS: "linux", RAMTotalGB: 64},
		"cpu-128gb":     {OS: "linux", RAMTotalGB: 128},
		"8gb+2gb card":  card(8, 2048),
		"16gb+2gb card": card(16, 2048),
		"32gb+8gb card": card(32, 8192),
		"64gb+8gb card": card(64, 8192),
		"64gb+16gb":     card(64, 16303),
		"128gb+24gb":    card(128, 24564),
		"256gb+24gb":    card(256, 24564),
		"mac-8gb":       mac(8, 6144),
		"mac-24gb":      mac(24, 18432),
		"mac-64gb":      mac(64, 49152),
	}

	var agreed int
	for name, hw := range hosts {
		for _, m := range manifests {
			for _, v := range m.Variants {
				if !supportsOllama(v) {
					continue
				}
				tuned := computeOllamaTuning(m, v, hw, ollamaKVAuto)
				declared := hostfit.OllamaDeclaresWindow(m, v, hw.HostFit(), hostfit.ServingWindow200k)
				// WindowFits joined the predicate at waired-agent#587: a
				// forced rung is started at 200,704 too, and what
				// separates "serves it" from "was given it anyway" is the
				// sizing's own proof — the same bit the declaration gate
				// reads (see DeclaredContextWindow).
				serves := tuned.ContextLength >= hostfit.ServingWindow200k &&
					tuned.WindowFits &&
					m.ContextLength >= hostfit.ServingWindow200k
				if declared != serves {
					t.Errorf("%s / %s/%s: the recommendation says declares-200k=%v while the "+
						"tuner would start the engine at %d tokens (model window %d). "+
						"The two have drifted, which is the failure the shared predicate exists "+
						"to remove",
						name, m.ModelID, v.VariantID, declared, tuned.ContextLength, m.ContextLength)
					continue
				}
				agreed++
			}
		}
	}
	if agreed == 0 {
		t.Fatal("no (host, model) pair was compared; the grid or the catalog stopped " +
			"producing candidates and this test is asserting nothing")
	}
	t.Logf("%d (host, model, variant) combinations agree", agreed)
}

// TestDeclaredWindowMatchesTheTuner_CatchesDrift proves the check above
// can fail. A test that only ever passes is a test that has stopped
// reading its subject, and this one is guarding an equality between two
// call sites that a refactor can quietly break.
func TestDeclaredWindowMatchesTheTuner_CatchesDrift(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	m := manifests[0]
	for _, cand := range manifests {
		if cand.ModelID == "qwen3.5-4b" {
			m = cand
		}
	}
	v := m.Variants[0]
	hw := hardware.Profile{OS: "linux", Arch: "x86_64", RAMTotalGB: 64}

	if !hostfit.OllamaDeclaresWindow(m, v, hw.HostFit(), hostfit.ServingWindow200k) {
		t.Fatalf("fixture: %s does not declare the coding window on a 64 GB host, so "+
			"the drift this checks for cannot be demonstrated", m.ModelID)
	}
	// Halve the memory and the same model stops declaring it. If this
	// ever reports the same answer for both hosts, the predicate has
	// stopped reading the host and the agreement check above is vacuous.
	tiny := hardware.Profile{OS: "linux", Arch: "x86_64", RAMTotalGB: 8}
	if hostfit.OllamaDeclaresWindow(m, v, tiny.HostFit(), hostfit.ServingWindow200k) {
		t.Errorf("%s declares the coding window on an 8 GB host too; the predicate is "+
			"not a function of the machine", m.ModelID)
	}
}

func supportsOllama(v catalog.Variant) bool {
	for _, r := range v.RuntimeSupport {
		if r == catalog.RuntimeOllama {
			return true
		}
	}
	return false
}
