package catalog

import (
	"encoding/json"
	"strings"
	"testing"
)

// The field must serialise away when unset, so a consumer built against an
// older proto tag sees a byte-identical manifest. Required by the
// additive-only proto contract (CLAUDE.md §Modules) and not otherwise
// observable from Go.
func TestManualOnlyIsAbsentWhenUnset(t *testing.T) {
	b, err := json.Marshal(Manifest{ModelID: "m"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "manual_only") {
		t.Errorf("manual_only must be omitempty; got %s", b)
	}
	// The whole struct, not just this field: adding it must not have
	// disturbed anything else that was already omitempty.
	if got, want := string(b), `{"model_id":"m","context_length":0,"runtime":{"preferred":""},"variants":null,"security":{"trust_remote_code_required":false,"allow_persistent_kv_cache":false}}`; got != want {
		t.Errorf("empty manifest no longer serialises as before:\n got %s\nwant %s", got, want)
	}
}

// Product contract, issue #520 and
// docs/decisions/20260805/1427-quality-tier-is-a-curated-ladder.md: this is a
// weaker withholding than internal_only. A manual-only model stays in the
// catalog a person browses, because a person cannot select what they cannot
// see — that is the entire difference between the two fields, and the only way
// to state it here is that BundledManifests keeps such an entry.
func TestBundledManifests_keepsManualOnly(t *testing.T) {
	offered, err := BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	all, err := BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatalf("BundledManifestsIncludingInternal: %v", err)
	}
	inOffered := make(map[string]bool, len(offered))
	for _, m := range offered {
		inOffered[m.ModelID] = true
	}
	for _, m := range all {
		if m.ManualOnly == "" || m.InternalOnly != "" {
			continue
		}
		if !inOffered[m.ModelID] {
			t.Errorf("%s is manual_only and was dropped from the offered set; "+
				"manual_only withholds from automatic CHOICE, not from the catalog", m.ModelID)
		}
	}
}

// The two compose and internal_only wins: a model we do not offer at all is
// not one we could have chosen for someone. Product contract, issue #520.
func TestInternalOnlyWinsOverManualOnly(t *testing.T) {
	all, err := BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatalf("BundledManifestsIncludingInternal: %v", err)
	}
	offered, err := BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	inOffered := make(map[string]bool, len(offered))
	for _, m := range offered {
		inOffered[m.ModelID] = true
	}
	for _, m := range all {
		if m.InternalOnly == "" {
			continue
		}
		if inOffered[m.ModelID] {
			t.Errorf("%s is internal_only and still offered; internal_only must win "+
				"whatever manual_only says", m.ModelID)
		}
	}
}

// Same reason discipline as internal_only: an exclusion nobody had to justify
// is one nobody revisits. Vacuous until the first entry is marked (issue #521);
// it exists so the first one cannot land as a bare flag.
func TestManualOnlyEntriesCarryARealReason(t *testing.T) {
	all, err := BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatalf("BundledManifestsIncludingInternal: %v", err)
	}
	for _, m := range all {
		if m.ManualOnly == "" {
			continue
		}
		reason := strings.TrimSpace(m.ManualOnly)
		if len(reason) < 20 {
			t.Errorf("%s: manual_only = %q — say why it is not recommended, in a sentence",
				m.ModelID, m.ManualOnly)
		}
		switch strings.ToLower(reason) {
		case "true", "yes", "1", "manual":
			t.Errorf("%s: manual_only = %q is a flag pretending to be a reason",
				m.ModelID, m.ManualOnly)
		}
	}
}

// A manual-only model ships and serves, so it is held to the same schema as
// anything else — and it must still resolve by id and by alias, which is what
// makes "the person picked it themselves" work at all.
func TestManualOnlyEntriesValidateAndResolve(t *testing.T) {
	all, err := BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatalf("BundledManifestsIncludingInternal: %v", err)
	}
	for i := range all {
		if all[i].ManualOnly == "" {
			continue
		}
		if err := all[i].Validate(); err != nil {
			t.Errorf("%s: %v", all[i].ModelID, err)
		}
		if _, ok := LookupByAlias(all[i].ModelID, all); !ok {
			t.Errorf("%s: manual_only must not affect resolution", all[i].ModelID)
		}
		for _, a := range all[i].ModelAliases {
			if _, ok := LookupByAlias(a, all); !ok {
				t.Errorf("%s: alias %q stopped resolving", all[i].ModelID, a)
			}
		}
	}
}

// The field is a plain string on the wire, and a manifest that carries it
// round-trips unchanged. Pins the JSON tag itself, which proto-guard checks
// structurally but no Go test would otherwise exercise.
func TestManualOnlyRoundTrips(t *testing.T) {
	in := Manifest{ModelID: "m", ManualOnly: "carried for popularity, never our recommendation"}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"manual_only":"carried for popularity, never our recommendation"`) {
		t.Errorf("unexpected encoding: %s", b)
	}
	var out Manifest
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.ManualOnly != in.ManualOnly {
		t.Errorf("round-trip lost the reason: %q", out.ManualOnly)
	}
}
