package setup

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
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
			OllamaSource:   agentconfig.OllamaSourceBundled,
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

	t.Run("under-spec-tiny-fits-defers-to-caller", func(t *testing.T) {
		// 2 GB RAM: nothing above the coding-quality floor fits, but a tiny
		// below-floor model (min 2 GB) does. Local inference is disabled by
		// default and UnderSpec/BelowFloorModelID are set so the caller can
		// offer the tiny model as an opt-in; SelectBundledModel emits no
		// generic under-spec note in this case (messaging is the caller's).
		in := baseInputs(cpuProfile(2), manifests)
		in.FreeDiskBytes = fixedDisk(500)
		sel, err := SelectBundledModel(in)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if sel.EnableInference {
			t.Errorf("under-spec host should disable local inference by default")
		}
		if !sel.UnderSpec {
			t.Errorf("expected UnderSpec=true")
		}
		if sel.BelowFloorModelID == "" {
			t.Errorf("expected a below-floor model to be offered on a 2 GB host")
		}
		if len(sel.Notes) != 0 {
			t.Errorf("caller owns the tiny opt-in messaging; expected no notes, got %v", sel.Notes)
		}
	})

	t.Run("nothing-fits-emits-generic-note", func(t *testing.T) {
		// 1 GB RAM: not even the smallest tiny model (min 2 GB) fits → disable
		// with the generic under-spec guidance emitted here.
		in := baseInputs(cpuProfile(1), manifests)
		sel, err := SelectBundledModel(in)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if sel.EnableInference {
			t.Errorf("under-spec host should disable local inference")
		}
		if sel.BelowFloorModelID != "" {
			t.Errorf("nothing should fit a 1 GB host, got %q", sel.BelowFloorModelID)
		}
		if !containsNote(sel.Notes, "gateway/relay") {
			t.Errorf("warning should explain the node still works as gateway/relay; got %v", sel.Notes)
		}
		if !containsNote(sel.Notes, "needs ≥") {
			t.Errorf("warning should state what's needed; got %v", sel.Notes)
		}
	})

	t.Run("under-spec-forced-keeps-enabled", func(t *testing.T) {
		in := baseInputs(cpuProfile(2), manifests)
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

	t.Run("best-effort-note-on-floor-fallback", func(t *testing.T) {
		// 4 GB CPU host: the under-spec rescue keeps coder-3b (32k
		// native) — the note must say the pick is below the floor.
		in := baseInputs(cpuProfile(4), manifests)
		in.FreeDiskBytes = fixedDisk(500)
		sel, err := SelectBundledModel(in)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !sel.EnableInference {
			t.Fatal("floor fallback must not disable inference")
		}
		if !containsNote(sel.Notes, "below the ~200k coding-agent context floor") {
			t.Errorf("notes lack the best-effort floor line: %v", sel.Notes)
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
