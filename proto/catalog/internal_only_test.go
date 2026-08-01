package catalog

import (
	"encoding/json"
	"strings"
	"testing"
)

// The offered set is the default and the complete set is opt-in. This
// pins that direction, because the value of the split is entirely in
// which way a caller fails when it picks wrong: a surface that reaches
// for the default shows too little, which is recoverable, rather than
// offering a model nobody should be given.
func TestBundledManifests_excludesInternalOnly(t *testing.T) {
	offered, err := BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	all, err := BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatalf("BundledManifestsIncludingInternal: %v", err)
	}

	if len(offered) > len(all) {
		t.Fatalf("offered (%d) cannot exceed the complete set (%d)", len(offered), len(all))
	}
	for _, m := range offered {
		if m.InternalOnly != "" {
			t.Errorf("%s is internal_only but was offered", m.ModelID)
		}
	}

	// Every entry the complete set has and the offered set lacks must be
	// missing BECAUSE it is internal — never for any other reason.
	inOffered := make(map[string]bool, len(offered))
	for _, m := range offered {
		inOffered[m.ModelID] = true
	}
	for _, m := range all {
		if inOffered[m.ModelID] {
			continue
		}
		if m.InternalOnly == "" {
			t.Errorf("%s was dropped from the offered set without being internal_only", m.ModelID)
		}
	}
}

// The reason is the field's point: an exemption nobody has to justify
// is an exemption nobody revisits. Also guards the obvious slip of
// writing `"internal_only": "true"`, which says nothing.
func TestInternalOnlyEntriesCarryARealReason(t *testing.T) {
	all, err := BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatalf("BundledManifestsIncludingInternal: %v", err)
	}
	for _, m := range all {
		if m.InternalOnly == "" {
			continue
		}
		reason := strings.TrimSpace(m.InternalOnly)
		if len(reason) < 20 {
			t.Errorf("%s: internal_only = %q — say why it is withheld, in a sentence",
				m.ModelID, m.InternalOnly)
		}
		switch strings.ToLower(reason) {
		case "true", "yes", "1", "internal":
			t.Errorf("%s: internal_only = %q is a flag pretending to be a reason",
				m.ModelID, m.InternalOnly)
		}
	}
}

// An internal model is withheld, not exempt: it ships, so it is held to
// the same schema as anything else.
func TestInternalOnlyEntriesStillValidate(t *testing.T) {
	all, err := BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatalf("BundledManifestsIncludingInternal: %v", err)
	}
	for i := range all {
		if all[i].InternalOnly == "" {
			continue
		}
		if err := all[i].Validate(); err != nil {
			t.Errorf("%s: %v", all[i].ModelID, err)
		}
	}
}

// The field must serialise away when unset, so an old consumer sees a
// byte-identical manifest. Required by the additive-only proto contract
// and not otherwise observable from Go.
func TestInternalOnlyIsAbsentWhenUnset(t *testing.T) {
	b, err := json.Marshal(Manifest{ModelID: "m"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "internal_only") {
		t.Errorf("internal_only must be omitempty; got %s", b)
	}
}

// Tier resolution reads the COMPLETE set on purpose: it answers "what
// is this device already serving", and a model withheld from the offer
// surfaces still generates traffic that has to be attributable.
func TestTierResolutionSeesInternalModels(t *testing.T) {
	all, err := BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatalf("BundledManifestsIncludingInternal: %v", err)
	}
	cached := cachedBundled()
	if len(cached) != len(all) {
		t.Errorf("tier resolver sees %d manifests, the complete set has %d — "+
			"an internal model's usage would resolve to no tier", len(cached), len(all))
	}
}

// This change ships the MECHANISM with nothing using it yet, so that it
// can be reviewed, tagged and consumed by the control plane while
// provably changing nothing. The flag goes on a manifest in the next
// change, and this test is what makes "provably" true rather than
// asserted.
//
// It is expected to be DELETED by that change, not weakened: once an
// entry is internal the two sets legitimately differ, and a reviewer
// seeing this test edited instead of removed should ask why.
func TestNoManifestIsInternalYet(t *testing.T) {
	all, err := BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatalf("BundledManifestsIncludingInternal: %v", err)
	}
	for _, m := range all {
		if m.InternalOnly != "" {
			t.Fatalf("%s is marked internal_only (%q). That is the NEXT change — "+
				"delete this test as part of it, and add the coverage that proves "+
				"the model stays out of the pickers and the under-spec fallback.",
				m.ModelID, m.InternalOnly)
		}
	}
}
