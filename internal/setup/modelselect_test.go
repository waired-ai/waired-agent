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
		StateDir:  "/var/lib/waired",
		FloorTier: 30, // mirror router.InstallQualityFloorTier
	}
}

func TestSelectBundledModel(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}

	t.Run("8gb-memory-fit-picks-7b", func(t *testing.T) {
		in := baseInputs(cpuProfile(8), manifests)
		in.FreeDiskBytes = fixedDisk(500) // plenty
		sel, err := SelectBundledModel(in)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !sel.EnableInference || sel.SkipPull {
			t.Fatalf("want enabled+pull, got enable=%v skip=%v", sel.EnableInference, sel.SkipPull)
		}
		// #624: the 32k-native coder-7b is excluded by the coding-agent
		// context floor; qwen3.5-4b (262144-native, tier 42) is the best
		// floor-passing fit on 8 GB.
		if sel.ModelID != "qwen3.5-4b" {
			t.Errorf("ModelID = %q, want qwen3.5-4b", sel.ModelID)
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
		// 8 GB RAM, but < headroom free disk: even the smallest above-floor
		// model won't fit → keep it configured but skip the pull.
		in := baseInputs(cpuProfile(8), manifests)
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

	t.Run("below-recommended-spec-tiny-fits-explains-the-opt-in", func(t *testing.T) {
		// 4 GB RAM: nothing above the coding-quality floor fits, but a tiny
		// below-floor model does. Local inference starts off, and
		// BelowRecommendedSpec / BelowFloorModelID are set so the caller
		// can offer the tiny model as an opt-in.
		//
		// This case used to assert the OPPOSITE of the note check below:
		// no notes at all, "messaging is the caller's". The caller it
		// deferred to — an interactive opt-in dialog — was never built, so
		// the host that could run SOMETHING got less explanation than the
		// host that could run nothing (#465, waired-ai/waired#1056).
		//
		// The 4 GB figure sat at 2 GB while capacity was the hand-authored
		// min_ram_gb. It is a computation now — weights, the window's KV
		// cache and engine overhead against RAM less the OS allowance
		// (waired-ai/waired#1056 decision 1) — and a 2 GB machine has
		// nothing left for a model once the OS is served, so the tiny
		// fit lives at 4 GB. Record of today's arithmetic, not a rule.
		in := baseInputs(cpuProfile(4), manifests)
		in.FreeDiskBytes = fixedDisk(500)
		sel, err := SelectBundledModel(in)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if sel.EnableInference {
			t.Errorf("a host below the recommended spec should start with local inference off")
		}
		if !sel.BelowRecommendedSpec {
			t.Errorf("expected BelowRecommendedSpec=true")
		}
		if sel.BelowFloorModelID == "" {
			t.Errorf("expected a below-floor model to be offered on a 4 GB host")
		}
		if !containsNote(sel.Notes, sel.BelowFloorModelID) {
			t.Errorf("the notes must name the model this host CAN run; got %v", sel.Notes)
		}
		if !containsNote(sel.Notes, "waired inference on") {
			t.Errorf("the notes must name the way to turn it on; got %v", sel.Notes)
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
		if sel.BelowFloorModelID != "" {
			t.Errorf("nothing should fit a 2 GB host, got %q", sel.BelowFloorModelID)
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

	// Forcing inference on says WHETHER it runs, not WHICH model runs, so
	// a forced under-spec host still needs an id — and with no compiled-in
	// default there is nothing to fall back to but the below-floor fit.
	// Without this the daemon would boot inference on and pre-pull nothing.
	t.Run("under-spec-forced-with-nothing-configured-takes-the-below-floor-fit", func(t *testing.T) {
		in := baseInputs(cpuProfile(4), manifests)
		in.Forced = true
		in.Inference.BundledModelID = "" // agentconfig.Defaults()
		sel, err := SelectBundledModel(in)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !sel.EnableInference {
			t.Errorf("forced inference must stay enabled even under-spec")
		}
		if sel.BelowFloorModelID == "" {
			t.Fatal("a 4 GB host should still have a below-floor fit")
		}
		if sel.ModelID != sel.BelowFloorModelID {
			t.Errorf("ModelID = %q, want the below-floor fit %q", sel.ModelID, sel.BelowFloorModelID)
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
		in := baseInputs(cpuProfile(8), manifests)
		in.FreeDiskBytes = nil // disk pre-flight disabled
		sel, err := SelectBundledModel(in)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		// #624: floor-passing best fit on 8 GB is qwen3.5-4b.
		if sel.ModelID != "qwen3.5-4b" || sel.SkipPull {
			t.Errorf("want best-fit qwen3.5-4b, no skip; got %q skip=%v", sel.ModelID, sel.SkipPull)
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

	t.Run("under-spec-when-only-a-subfloor-model-would-fit", func(t *testing.T) {
		// 4 GB CPU host: the only tier-30+ fit is a 32k-window coder.
		// That used to be rescued by re-ranking without the context floor;
		// waired#1031 removed the rescue, because the window is a contract
		// a node either declares or does not, and 32k is not one of the two
		// windows it can declare.
		in := baseInputs(cpuProfile(4), manifests)
		in.FreeDiskBytes = fixedDisk(500)
		sel, err := SelectBundledModel(in)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if sel.EnableInference {
			t.Fatalf("a host that can declare no window must not auto-enable local "+
				"inference (picked %q)", sel.ModelID)
		}
		if !sel.BelowRecommendedSpec {
			t.Error("UnderSpec must be set so the caller can explain the outcome")
		}
		// The opt-in offer is still made, and what it offers now clears the
		// window even though it does not clear the quality floor — which is
		// the whole trade waired#1031 makes: tier flexes, the window does not.
		if sel.BelowFloorModelID == "" {
			t.Fatal("no below-floor opt-in offered; a 4 GB host can still run something")
		}
		m, found := catalog.LookupByAlias(sel.BelowFloorModelID, manifests)
		if !found {
			t.Fatalf("BelowFloorModelID %q is not in the catalog", sel.BelowFloorModelID)
		}
		if !router.MeetsNativeContextFloor(m) {
			t.Errorf("the opt-in offers %s (%d-token window); offering a model that cannot "+
				"declare a window puts the host back where the rescue left it",
				sel.BelowFloorModelID, m.ContextLength)
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
