package catalog

import (
	"testing"

	protocatalog "github.com/waired-ai/waired-agent/proto/catalog"
)

// A withheld model must stay fully RESOLVABLE. Withholding it is a
// statement about what we offer, not about what exists, and every path
// that takes a model id somebody already has must still find it.
//
// This is not a hypothetical. Two separate call sites were classified
// as "offering" when they were really "resolution", and each one only
// surfaced when a per-PR CI gate went red:
//
//   - the preferred-model endpoint, which `waired init` uses to apply
//     an operator's --inference-bundled-model-id, answered
//     "no bundled manifest with that model_id" for a model the build
//     ships;
//   - catalog-tool's --import resolved an engine tag against the
//     offered set, refusing the very measurement that justified
//     withholding the model.
//
// The rule the tree follows: taking a model id as INPUT and looking it
// up is resolution and takes the complete set; enumerating models to
// show or to choose among is offering and takes the filtered default.
// These tests pin the resolution half at the data layer, where every
// such path bottoms out.
func TestWithheldModelsStayResolvable(t *testing.T) {
	all, err := BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatalf("BundledManifestsIncludingInternal: %v", err)
	}
	var withheld []Manifest
	for _, m := range all {
		if m.InternalOnly != "" {
			withheld = append(withheld, m)
		}
	}
	if len(withheld) == 0 {
		t.Skip("no manifest is withheld")
	}

	for _, m := range withheld {
		t.Run(m.ModelID, func(t *testing.T) {
			// By model id.
			if _, ok := LookupByAlias(m.ModelID, all); !ok {
				t.Errorf("%s does not resolve by its own model_id", m.ModelID)
			}
			// By every alias it advertises. The routing sentinel reaches
			// its fixture through one of these.
			for _, a := range m.ModelAliases {
				if _, ok := LookupByAlias(a, all); !ok {
					t.Errorf("%s does not resolve by alias %q", m.ModelID, a)
				}
			}
			// By engine tag — how a verdict is filed against it, and how
			// a peer names it on the wire.
			for _, v := range m.Variants {
				if v.Source.Tag == "" {
					continue
				}
				found := false
				for _, cand := range all {
					for _, cv := range cand.Variants {
						if cv.Source.Tag == v.Source.Tag {
							found = true
						}
					}
				}
				if !found {
					t.Errorf("%s: engine tag %q resolves to nothing", m.ModelID, v.Source.Tag)
				}
			}
			// And a quality tier from the engine tag it advertises —
			// the usage-ingest path. A withheld model still generates
			// traffic, and traffic that resolves to tier 0 is traffic
			// nobody can attribute.
			for _, v := range m.Variants {
				if v.Source.Tag == "" {
					continue
				}
				if tier := protocatalog.BestTier(RuntimeOllama, []string{v.Source.Tag}); tier == 0 {
					t.Errorf("%s: engine tag %q resolves to no quality tier", m.ModelID, v.Source.Tag)
				}
			}
		})
	}
}

// The offered accessor is the one that must NOT find them — the other
// half of the same contract, so a future change cannot satisfy the test
// above by quietly un-withholding everything.
func TestWithheldModelsAreNotOffered(t *testing.T) {
	all, err := BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatalf("BundledManifestsIncludingInternal: %v", err)
	}
	offered, err := BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	for _, m := range all {
		if m.InternalOnly == "" {
			continue
		}
		if _, ok := LookupByAlias(m.ModelID, offered); ok {
			t.Errorf("%s is withheld but resolves in the offered set", m.ModelID)
		}
		for _, a := range m.ModelAliases {
			if _, ok := LookupByAlias(a, offered); ok {
				t.Errorf("%s is withheld but its alias %q resolves in the offered set", m.ModelID, a)
			}
		}
	}
}
