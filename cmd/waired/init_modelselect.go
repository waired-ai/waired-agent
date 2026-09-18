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

// isFastestOfferedModel reports whether modelID (id or alias) resolves to
// a bundled model that nothing offered answers faster than — the case
// where the step-down has no next move, and the real question is whether
// to keep local inference on this machine at all.
//
// "Faster" is the daemon's: the seconds recorded for the reference host
// class (catalog.TurnSpeeds) and the same 5% factor
// (router.FasterStepFactor), per decision 2 of
// docs/decisions/20260916/0340-catalog-reference-host-rank-and-admission.md.
// It used to be an ordering by quality_tier ("nothing ranked below this"),
// which the step-down no longer walks (#1400): with the rank decoupled
// from speed, the bottom of the ladder is not the end of the step-down.
//
// The CLI holds a model id, not a variant, so each model is compared by
// its fastest recorded variant.
//
// Which model that is moves with the catalog and the measurements, so
// this does not name one. Best-effort: false when the catalog or the store
// is unreadable, the id is unknown, or the model has no recorded seconds —
// without a figure there is nothing to say "nothing is faster" about, and
// the daemon offers no step-down from such a model either.
func isFastestOfferedModel(modelID string) bool {
	all, err := catalog.BundledManifestsIncludingInternal()
	if err != nil {
		return false
	}
	m, ok := catalog.LookupByAlias(modelID, all)
	if !ok {
		return false
	}
	speeds, err := catalog.TurnSpeeds()
	if err != nil {
		return false
	}
	mine, ok := fastestRecordedSeconds(speeds, m)
	if !ok {
		return false
	}
	offered, err := catalog.BundledManifests()
	if err != nil {
		return false
	}
	for _, o := range offered {
		if o.ModelID == m.ModelID {
			continue
		}
		if s, ok := fastestRecordedSeconds(speeds, o); ok && s <= mine*router.FasterStepFactor {
			return false
		}
	}
	return true
}

// fastestRecordedSeconds is the smallest seconds any variant of m has on
// record, in the lookup order the daemon uses (catalog.TurnSpeedSet.For).
func fastestRecordedSeconds(speeds catalog.TurnSpeedSet, m catalog.Manifest) (float64, bool) {
	best, found := 0.0, false
	for _, v := range m.Variants {
		if s, _, ok := speeds.For(m, v); ok && (!found || s < best) {
			best, found = s, true
		}
	}
	return best, found
}
