package setup

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/router"
)

func cpuProfile(ramGB int) hardware.Profile {
	return hardware.Profile{OS: "linux", Arch: "x86_64", RAMTotalGB: ramGB}
}

// fixedDisk returns a FreeDiskBytes seam reporting a constant free figure.
func fixedDisk(gb float64) func(string) (int64, error) {
	return func(string) (int64, error) { return int64(gb * 1e9), nil }
}

func baseInputs(hw hardware.Profile, manifests []catalog.Manifest) BundledModelInputs {
	return BundledModelInputs{
		Hardware:  hw,
		Manifests: manifests,
		Inference: agentconfig.InferenceConfig{
			BundledModelID: "qwen2.5-coder-7b-instruct",
		},
		StateDir: "/var/lib/waired",
	}
}

func TestSelectBundledModel(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}

	t.Run("32gb-memory-fit-picks-above-the-floor", func(t *testing.T) {
		// 8 GB until #552, which is now below the recommended spec: no
		// above-floor model holds a 200,704 window there.
		in := baseInputs(cpuProfile(32), manifests)
		in.FreeDiskBytes = fixedDisk(500) // plenty
		sel, err := SelectBundledModel(in)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !sel.EnableInference || sel.SkipPull {
			t.Fatalf("want enabled+pull, got enable=%v skip=%v", sel.EnableInference, sel.SkipPull)
		}
		// #624: the 32k-native coder entries are excluded by the
		// coding-agent context floor. Which of the survivors wins tracks
		// the evolving catalog, so the assertion is that an above-floor
		// pick was made at all — the id was pinned when this case ran on
		// an 8 GB host and there was only one candidate.
		if sel.ModelID == "" || sel.BelowRecommendedSpec {
			t.Errorf("ModelID = %q belowSpec=%v, want an above-floor pick",
				sel.ModelID, sel.BelowRecommendedSpec)
		}
	})

	t.Run("disk-short-steps-down", func(t *testing.T) {
		// 16 GB RAM fits qwen3.5-9b (≈6.6 GB weights) by memory, but only
		// ~8 GB free disk (< 6.6 + 3 headroom) forces a step-down to a
		// smaller floor-passing model (qwen3.5-4b, ≈3.4 GB).
		in := baseInputs(cpuProfile(16), manifests)
		in.FreeDiskBytes = fixedDisk(8)
		sel, err := SelectBundledModel(in)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !sel.EnableInference || sel.SkipPull {
			t.Fatalf("want enabled+pull, got enable=%v skip=%v", sel.EnableInference, sel.SkipPull)
		}
		if sel.ModelID == "qwen3.5-9b" {
			t.Errorf("expected a step-down from the 9b, still got it")
		}
		if !containsNote(sel.Notes, "stepped down") {
			t.Errorf("expected a step-down note, got %v", sel.Notes)
		}
	})

	t.Run("disk-too-small-skips-pull", func(t *testing.T) {
		// Enough RAM for an above-floor pick, but < headroom free disk:
		// even the smallest above-floor model won't fit → keep it
		// configured but skip the pull. 32 GB rather than 8 since #552.
		in := baseInputs(cpuProfile(32), manifests)
		in.FreeDiskBytes = fixedDisk(1)
		sel, err := SelectBundledModel(in)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !sel.EnableInference {
			t.Errorf("inference should stay enabled (disk freeable later)")
		}
		if !sel.SkipPull {
			t.Errorf("want SkipPull=true on disk-too-small")
		}
		if !containsNote(sel.Notes, "waired models pull") {
			t.Errorf("expected a retry hint, got %v", sel.Notes)
		}
	})

	t.Run("8gb-installs-the-model-it-can-hold", func(t *testing.T) {
		// THIS CASE IS INVERTED BY #522 (owner decision 2026-08-08). It
		// asserted that an 8 GB host starts with local inference OFF and
		// exposes a "below-floor" model the caller may offer as an opt-in.
		// The host could hold qwen3.5-2b's full 262,144 window the whole
		// time; the only thing stopping it was that 2b is tier 27 and the
		// floor was 30.
		//
		// Nothing about the machine changed. What changed is that a tier
		// threshold stopped being treated as a statement about whether a
		// model is usable — within one generation quality_tier is
		// 10*log10(params) - 5*log10(footprint) (#518), so the floor was a
		// size cutoff written the long way round, and the agent-grade
		// harness is not monotone in size across that generation.
		//
		// Product contract, ratified in #522: refusal is capacity and the
		// #624 window, nothing else.
		in := baseInputs(cpuProfile(8), manifests)
		in.FreeDiskBytes = fixedDisk(500)
		sel, err := SelectBundledModel(in)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !sel.EnableInference {
			t.Errorf("an 8 GB host holds a model's full window; local inference must start on (notes: %v)", sel.Notes)
		}
		if sel.BelowRecommendedSpec {
			t.Errorf("BelowRecommendedSpec = true on a host that fits a model")
		}
		// Which model tracks the catalog, so the assertion is that a real
		// one was chosen. Today it is qwen3.5-2b — the ladder across every
		// host size is in TestSelectBundledModel_TheBottomRung.
		if sel.ModelID == "" {
			t.Errorf("no model selected on an 8 GB host; notes: %v", sel.Notes)
		}
	})

	t.Run("nothing-fits-emits-generic-note", func(t *testing.T) {
		// 2 GB RAM: the OS allowance leaves nothing, so not even the
		// smallest tiny model fits → disable with the generic under-spec
		// guidance emitted here.
		in := baseInputs(cpuProfile(2), manifests)
		sel, err := SelectBundledModel(in)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if sel.EnableInference {
			t.Errorf("under-spec host should disable local inference")
		}
		if sel.ModelID != "qwen2.5-coder-7b-instruct" {
			t.Errorf("nothing fits a 2 GB host, so the configured id stays; got %q", sel.ModelID)
		}
		if !containsNote(sel.Notes, "gateway/relay") {
			t.Errorf("warning should explain the node still works as gateway/relay; got %v", sel.Notes)
		}
		if !containsNote(sel.Notes, "needs ≥") {
			t.Errorf("warning should state what's needed; got %v", sel.Notes)
		}
	})

	t.Run("under-spec-forced-keeps-enabled", func(t *testing.T) {
		in := baseInputs(cpuProfile(4), manifests)
		in.Forced = true
		sel, err := SelectBundledModel(in)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !sel.EnableInference {
			t.Errorf("forced inference must stay enabled even under-spec")
		}
		if sel.ModelID != "qwen2.5-coder-7b-instruct" {
			t.Errorf("forced should keep the configured model, got %q", sel.ModelID)
		}
		if !containsNote(sel.Notes, "forced") {
			t.Errorf("expected a forced-on warning, got %v", sel.Notes)
		}
	})

	// An 8 GB host with nothing configured used to reach the forced
	// under-spec branch and inherit its "below-floor fit" so the daemon
	// would not boot inference with nothing to serve. #522 makes it an
	// ordinary host: it fits a model, so it is selected the ordinary way
	// and Forced changes nothing about the outcome.
	t.Run("8gb-forced-with-nothing-configured-selects-normally", func(t *testing.T) {
		in := baseInputs(cpuProfile(8), manifests)
		in.Forced = true
		in.Inference.BundledModelID = "" // agentconfig.Defaults()
		sel, err := SelectBundledModel(in)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !sel.EnableInference {
			t.Errorf("forced inference must stay enabled")
		}
		if sel.BelowRecommendedSpec {
			t.Errorf("BelowRecommendedSpec = true on a host that fits a model")
		}
		if sel.ModelID == "" {
			t.Fatalf("forced install left no model to serve; notes: %v", sel.Notes)
		}
	})

	t.Run("nothing-fits-forced-leaves-the-model-unset", func(t *testing.T) {
		// 2 GB: nothing fits at any tier, so there is no honest id to
		// invent. Forced keeps inference on; the model stays unchosen.
		in := baseInputs(cpuProfile(2), manifests)
		in.Forced = true
		in.Inference.BundledModelID = ""
		sel, err := SelectBundledModel(in)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !sel.EnableInference {
			t.Errorf("forced inference must stay enabled")
		}
		if sel.ModelID != "" {
			t.Errorf("ModelID = %q, want empty — nothing fits this host", sel.ModelID)
		}
	})

	t.Run("pinned-skips-autoselection", func(t *testing.T) {
		in := baseInputs(cpuProfile(32), manifests) // capable host
		in.Pinned = true
		in.Inference.BundledModelID = "qwen2.5-coder-3b-instruct"
		sel, err := SelectBundledModel(in)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if sel.ModelID != "qwen2.5-coder-3b-instruct" {
			t.Errorf("pinned id should be used verbatim, got %q", sel.ModelID)
		}
		if !sel.EnableInference {
			t.Errorf("pinned should keep inference enabled")
		}
	})

	t.Run("no-disk-seam-takes-best-fit", func(t *testing.T) {
		in := baseInputs(cpuProfile(32), manifests)
		in.FreeDiskBytes = nil // disk pre-flight disabled
		sel, err := SelectBundledModel(in)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		// The id tracks the evolving catalog; what this case is about is
		// that the disk seam being absent does not suppress the pull.
		if sel.ModelID == "" || sel.SkipPull || !sel.EnableInference {
			t.Errorf("want an above-floor pick with no skip; got %q skip=%v enable=%v",
				sel.ModelID, sel.SkipPull, sel.EnableInference)
		}
	})
}

func containsNote(notes []string, sub string) bool {
	for _, n := range notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}

// #624: install notes must state the context-floor status the pick
// carries — a bounded spill on the anchor-class host, the best-effort
// line when nothing serves the floor, and the pin escape hatch.
func TestSelectBundledModel_ContextFloorNotes(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}

	t.Run("a-small-host-is-given-a-model-that-can-declare-a-window", func(t *testing.T) {
		// 8 GB CPU host. The subject is waired#1031's trade — tier flexes,
		// the window does not — and #522 makes it read directly: the model
		// this host SELECTS declares a servable window. It used to have to
		// be read off the below-floor opt-in, because the quality floor
		// stopped the same model from being selected outright.
		//
		// The 32k-window coder entries that fit here are excluded by #624
		// and stay excluded: waired#1031 removed the re-rank that rescued
		// them, because the window is a contract a node either declares or
		// does not, and 32k is not one of the two windows it can declare.
		in := baseInputs(cpuProfile(8), manifests)
		in.FreeDiskBytes = fixedDisk(500)
		sel, err := SelectBundledModel(in)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !sel.EnableInference {
			t.Fatalf("an 8 GB host holds a model's window; it must auto-enable local "+
				"inference (notes: %v)", sel.Notes)
		}
		if sel.ModelID == "" {
			t.Fatal("no model selected on an 8 GB host")
		}
		m, found := catalog.LookupByAlias(sel.ModelID, manifests)
		if !found {
			t.Fatalf("selected %q is not in the catalog", sel.ModelID)
		}
		if !router.MeetsNativeContextFloor(m) {
			t.Errorf("selected %s (%d-token window); installing a model that cannot "+
				"declare a window puts the host back where the rescue left it",
				sel.ModelID, m.ContextLength)
		}
	})

	t.Run("pinned-subfloor-notes-escape-hatch", func(t *testing.T) {
		in := baseInputs(cpuProfile(8), manifests)
		in.Pinned = true
		in.Inference.BundledModelID = "qwen2.5-coder-7b-instruct"
		sel, err := SelectBundledModel(in)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !containsNote(sel.Notes, "not enforced for pins") {
			t.Errorf("notes lack the pin floor note: %v", sel.Notes)
		}
	})
}
