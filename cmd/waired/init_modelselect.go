package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/internal/setup"
)

// applyBundledModelSelection runs the install-time, hardware-aware
// bundled-model selection (#517) and applies the verdict to cfg.Inference:
//
//   - override BundledModelID with the largest catalog model that fits the
//     host above the coding-quality floor;
//   - disable LOCAL inference when the host is under-spec (the node still
//     enrolls and runs as a gateway/relay) — unless the operator pinned a
//     model (--inference-bundled-model-id) or forced it on
//     (--inference-enabled=true);
//   - turn off the startup pull when free disk is too small, so the agent
//     doesn't retry a download that can't land.
//
// It is best-effort: any failure (catalog unreadable, engine-pick error)
// degrades to the already-configured default and prints a warning rather
// than aborting an otherwise-successful enroll. enabledOverride is the
// tri-state --inference-enabled flag value (nil when not passed).
//
// When the host is under-spec for a floor-clearing model but the tiny 0.5B
// still fits, it does NOT silently disable: interactively it confirms whether
// to run local inference on that very-low-quality model at all (default No);
// non-interactively it leaves inference off with a note. in/out carry that
// prompt; nonInteractive suppresses it.
func applyBundledModelSelection(
	cfg *agentconfig.Config,
	prof hardware.Profile,
	det setup.OllamaDetection,
	stateDir, homeDir, pin string,
	enabledOverride *bool,
	nonInteractive bool,
	in io.Reader,
	out io.Writer,
) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: bundled catalog unavailable (%v); keeping default model %s\n",
			err, cfg.Inference.BundledModelID)
		return
	}

	pinned := pin != ""
	if pinned {
		cfg.Inference.BundledModelID = pin
	}

	// A detected version only matters in reuse mode; bundled uses the
	// pinned bundled engine version (resolved inside SelectBundledModel).
	reuseVer := ""
	if cfg.Inference.OllamaSource == agentconfig.OllamaSourceReuse {
		reuseVer = det.Version
	}

	sel, err := setup.SelectBundledModel(setup.BundledModelInputs{
		Hardware:       prof,
		Manifests:      manifests,
		Inference:      cfg.Inference,
		StateDir:       stateDir,
		HomeDir:        homeDir,
		ReuseOllamaVer: reuseVer,
		FloorTier:      router.InstallQualityFloorTier,
		Forced:         enabledOverride != nil && *enabledOverride,
		Pinned:         pinned,
		FreeDiskBytes:  hardware.FreeDiskBytes,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: bundled model selection failed (%v); keeping %s\n",
			err, cfg.Inference.BundledModelID)
		return
	}

	cfg.Inference.BundledModelID = sel.ModelID
	cfg.Inference.Enabled = sel.EnableInference
	if sel.SkipPull {
		cfg.Inference.PullOnStartup = false
	}

	// Under-spec host where only a below-floor (very low quality) model — today
	// the 0.5B — fits. Rather than silently disabling, confirm whether to run
	// local inference on it at all. Skipped when the operator already forced /
	// disabled inference (--inference-enabled) or pinned a model (those are
	// deliberate choices we honour verbatim).
	if sel.UnderSpec && sel.BelowFloorModelID != "" && enabledOverride == nil && pin == "" {
		label := bundledModelLabel(manifests, sel.BelowFloorModelID)
		if nonInteractive {
			writePromptf(out, "  This computer is below the recommended spec for running AI locally: only the %s model fits — local AI left off.\n", label)
			writePrompt(out, "  Re-run interactively, or pass --inference-enabled=true to force it. Waired still works as a gateway to your other devices.")
			return
		}
		if promptTinyModelOptIn(out, in, label) {
			cfg.Inference.Enabled = true
			cfg.Inference.BundledModelID = sel.BelowFloorModelID
			cfg.Inference.PullOnStartup = true
			writePromptf(out, "  Enabling local inference with %s.\n", label)
		} else {
			writePrompt(out, "  Local AI left off — Waired still works as a gateway to your other devices. Re-run `waired init` to change this.")
		}
		return
	}

	for _, n := range sel.Notes {
		writePrompt(out, "  "+n)
	}
}

// bundledModelLabel returns a short human-facing label for a bundled model
// id/alias — the display name with any trailing parenthetical dropped (e.g.
// "Qwen3.5 0.8B (Hybrid Linear+Full Attention)" → "Qwen3.5 0.8B"), else the id.
func bundledModelLabel(manifests []catalog.Manifest, modelID string) string {
	if m, ok := catalog.LookupByAlias(modelID, manifests); ok && m.DisplayName != "" {
		return strings.TrimSpace(strings.SplitN(m.DisplayName, " (", 2)[0])
	}
	return modelID
}

// bundledModelLabelDefault is bundledModelLabel over the embedded catalog,
// falling back to the raw id when the catalog is unreadable.
func bundledModelLabelDefault(modelID string) string {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		return modelID
	}
	return bundledModelLabel(manifests, modelID)
}

// bundledVariantQuality resolves the catalog quality tier (1–100) for
// modelID's variantID, falling back to the model's best variant when
// variantID is empty or not found (the recommendation may name a variant the
// local catalog build doesn't carry). ok is false when the embedded catalog
// is unreadable or modelID is unknown.
func bundledVariantQuality(modelID, variantID string) (int, bool) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		return 0, false
	}
	m, ok := catalog.LookupByAlias(modelID, manifests)
	if !ok {
		return 0, false
	}
	best := 0
	for _, v := range m.Variants {
		if variantID != "" && v.VariantID == variantID && v.QualityTier > 0 {
			return v.QualityTier, true
		}
		if v.QualityTier > best {
			best = v.QualityTier
		}
	}
	if best > 0 {
		return best, true
	}
	return 0, false
}

// modelWithQuality renders "<label> (quality N)" for benchmark and
// recommendation lines, so the user can weigh the speed/quality trade-off of
// a switch (waired#773). Degrades to the bare label when the tier is unknown
// and to the raw id when the catalog can't resolve the model.
func modelWithQuality(modelID, variantID string) string {
	label := bundledModelLabelDefault(modelID)
	if q, ok := bundledVariantQuality(modelID, variantID); ok {
		return fmt.Sprintf("%s (quality %d)", label, q)
	}
	return label
}

// promptTinyModelOptIn shows the "this machine can only run the smallest
// model" confirmation and returns true iff the operator opts in. Default No —
// running a below-floor model locally is not recommended, but the node still
// works as a gateway/relay when declined.
func promptTinyModelOptIn(out io.Writer, in io.Reader, label string) bool {
	writePromptf(out, "\n%s This computer can only run a very small, low-quality model (%s).\n", emo("⚠", "!"), label)
	writePrompt(out, "   At that size local coding help is often broken or unusable, so running")
	writePrompt(out, "   AI models on this computer is not recommended. Waired still works as a")
	writePrompt(out, "   secure gateway to your other devices without it.")
	writePrompt(out, "")
	return ynPrompt(out, bufio.NewScanner(in), "Run AI models on this computer anyway?", false)
}

// isBundledModelBelowFloor reports whether modelID (id or alias) resolves to a
// bundled model whose best variant sits below the install quality floor — the
// "very low quality, not recommended for local use" tier (today the 0.5B).
// Best-effort: false when the catalog is unreadable or the id is unknown.
func isBundledModelBelowFloor(modelID string) bool {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		return false
	}
	m, ok := catalog.LookupByAlias(modelID, manifests)
	if !ok {
		return false
	}
	best := 0
	for _, v := range m.Variants {
		if v.QualityTier > best {
			best = v.QualityTier
		}
	}
	return best > 0 && best < router.InstallQualityFloorTier
}
