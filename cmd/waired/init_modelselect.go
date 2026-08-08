package main

import (
	"strings"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/router"
)

// bundledModelLabel returns a short human-facing label for a bundled model
// id/alias — the display name with any trailing parenthetical dropped (e.g.
// "Qwen3.5 0.8B (Hybrid Linear+Full Attention)" → "Qwen3.5 0.8B"), else the id.
func bundledModelLabel(manifests []catalog.Manifest, modelID string) string {
	if m, ok := catalog.LookupByAlias(modelID, manifests); ok && m.DisplayName != "" {
		return strings.TrimSpace(strings.SplitN(m.DisplayName, " (", 2)[0])
	}
	return modelID
}

// The helpers below all take a model id somebody already has and look
// it up, so they resolve against EVERY shipped manifest rather than the
// offered subset. A withheld model is one an operator can still pin,
// and a lookup that cannot find it degrades quietly: the wrong label
// printed, no below-floor warning — for a model this build ships.
//
// The rule across the tree: taking a model id as input and looking it
// up is RESOLUTION and takes the complete set; enumerating models to
// show or to choose among is OFFERING and takes the filtered default.

// bundledModelLabelDefault is bundledModelLabel over the embedded catalog,
// falling back to the raw id when the catalog is unreadable.
func bundledModelLabelDefault(modelID string) string {
	manifests, err := catalog.BundledManifestsIncludingInternal()
	if err != nil {
		return modelID
	}
	return bundledModelLabel(manifests, modelID)
}

// canonicalBundledModelID resolves an id OR alias to the catalog id the
// agent keys its own model state by, so a name from outside this process
// can be compared against /inference/status.
//
// It matters because the two ends of that comparison are resolved
// differently today. PullModel runs the same LookupByAlias and then writes
// state.Models under manifest.ModelID, which is what models.ready reports;
// but desired_model_id arrives from the control plane and is folded into
// the setup state without ever being resolved. A raw compare would
// therefore miss for an alias — and the catalog does ship aliases that
// differ from the id (qwen3.6-35b-a3b.json declares "qwen3.6-35b" among
// others). A miss here is not a cosmetic wrong label; it is a wait for a
// string that never appears.
//
// A RETIRED name resolves to its successor (#200), for the same reason:
// the daemon's own switch publishes the successor's id, so a compare
// against the raw name would be the same wait for a string that never
// appears. The daemon-side twin is setupCanonicalModelID.
//
// An unknown name is returned unchanged, which degrades to exactly the
// compare the caller would have done anyway.
//
// The COMPLETE set, per the resolution/offering rule above: the agent
// keys its model state off catalog.BundledManifestsIncludingInternal
// (cmd/waired-agent/inference.go), so resolving against the offered
// subset left an internal model's alias unresolved — a wait for a string
// that never appears, which is the exact failure this function exists to
// prevent.
func canonicalBundledModelID(modelID string) string {
	manifests, err := catalog.BundledManifestsIncludingInternal()
	if err != nil {
		return modelID
	}
	if m, _, ok := catalog.ResolveModel(modelID, manifests); ok && m.ModelID != "" {
		return m.ModelID
	}
	return modelID
}

// The benchmark and recommendation lines used to render
// "<label> (quality 30)" so the user could weigh the speed/quality
// trade-off of a switch (waired#773). #537 removed the figure: after
// #518 the tier is arithmetic over two catalog fields, and a number
// labelled "quality" beside a model claims a measurement behind a
// composite.
//
// Nothing replaces it in the label, because a coarse size class would
// not answer the question those lines ask either. What the user needs
// there is the DIRECTION of the swap, and each flow already knows which
// direction it is offering — so it says so in its own prose rather than
// leaving the reader to compare two numbers. See benchmarkWithScanner.

// isBundledModelBelowFloor reports whether modelID (id or alias) resolves to a
// bundled model whose best variant sits below the install quality floor —
// a model Waired does not choose for anyone.
//
// Not "very low quality", which is what the prompt used to call it. The
// floor is not a measurement, and #537 gives `small` a meaning that
// reaches models this flow recommends without hesitation, so one word for
// the two lines would have been a product saying two things.
//
// Which model that is moves with the catalog, so this does not name one:
// it was qwen2.5-coder-0.5b (tier 10) until #200 retired it, and the
// smallest offered entry is qwen3.5-0.8b (tier 12) today. Best-effort:
// false when the catalog is unreadable or the id is unknown.
func isBundledModelBelowFloor(modelID string) bool {
	manifests, err := catalog.BundledManifestsIncludingInternal()
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
