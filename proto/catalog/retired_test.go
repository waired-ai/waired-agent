package catalog

import (
	"strings"
	"testing"
)

// Product contract (waired-ai/waired-agent#200): the table is the only
// record of what a deleted entry's names now mean, so a malformed row is
// a migration that silently does nothing. Sibling of
// TestInternalOnlyEntriesCarryARealReason — same argument, same shape.
func TestRetirementTableIsWellFormed(t *testing.T) {
	seen := map[string]string{} // name -> the successor that already claimed it
	for _, r := range Retirements() {
		if len(r.Names) == 0 {
			t.Errorf("retirement to %q lists no names — it can never be looked up",
				r.SuccessorModelID)
		}
		for _, n := range r.Names {
			if strings.TrimSpace(n) == "" {
				t.Errorf("retirement to %q has a blank name", r.SuccessorModelID)
				continue
			}
			// Across the WHOLE table, not within a row: two rows claiming
			// one name makes the answer depend on table order.
			if prev, dup := seen[n]; dup {
				t.Errorf("name %q is claimed twice (-> %q and -> %q)",
					n, prev, r.SuccessorModelID)
			}
			seen[n] = r.SuccessorModelID
		}
		if strings.TrimSpace(r.SuccessorModelID) == "" {
			t.Errorf("retirement of %v has no successor — deleting an entry with "+
				"nowhere to send its users is what this table exists to prevent", r.Names)
		}
		reason := strings.TrimSpace(r.Reason)
		if len(reason) < 20 {
			t.Errorf("retirement of %v: reason = %q — say why it went, in a sentence",
				r.Names, r.Reason)
		}
		switch strings.ToLower(reason) {
		case "true", "yes", "1", "retired", "superseded":
			t.Errorf("retirement of %v: reason = %q is a flag pretending to be a reason",
				r.Names, r.Reason)
		}
	}
}

// Product contract (#200). Three separate ways a successor can be
// unusable, and each one turns the migration into a different silent
// failure.
func TestEveryRetirementSuccessorResolves(t *testing.T) {
	all, err := BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatalf("BundledManifestsIncludingInternal: %v", err)
	}
	for _, r := range Retirements() {
		m, ok := LookupByAlias(r.SuccessorModelID, all)
		if !ok {
			t.Errorf("successor %q for %v is not in the catalog — every site that "+
				"substitutes it would fall through to model-not-found",
				r.SuccessorModelID, r.Names)
			continue
		}
		// Migrating somebody onto a model we refuse to offer is not a
		// migration. Without this, granite4-350m — withheld, resolvable,
		// and 352M — would be a legal successor.
		if m.InternalOnly != "" {
			t.Errorf("successor %q for %v is internal_only (%q): a withheld model is "+
				"not something to hand anyone", r.SuccessorModelID, r.Names, m.InternalOnly)
		}
		// A chain is a migration nobody wrote. Re-point the first entry at
		// the final destination instead.
		if chained, isRetired := LookupRetirement(r.SuccessorModelID); isRetired {
			t.Errorf("successor %q for %v is itself retired (-> %q); re-point this row "+
				"at the final destination rather than chaining",
				r.SuccessorModelID, r.Names, chained.SuccessorModelID)
		}
	}
}

// Product contract (#200). The two halves of a retirement — the table
// entry and the deleted manifest — must stay welded. If a name is in
// both, LookupByAlias and LookupRetirement disagree and which one wins
// depends on the order a caller happens to ask in.
//
// This is also the guard that fires if someone re-adds a retired
// manifest.
func TestRetiredNamesAreGoneFromTheCatalog(t *testing.T) {
	all, err := BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatalf("BundledManifestsIncludingInternal: %v", err)
	}
	for _, r := range Retirements() {
		for _, n := range r.Names {
			if m, ok := LookupByAlias(n, all); ok {
				t.Errorf("retired name %q still resolves to manifest %q — a name is "+
					"either live or retired, never both", n, m.ModelID)
			}
		}
	}
}

// Product contract for the qwen2.5-coder-0.5b -> qwen3.5-0.8b mapping:
// this issue's own proposal ("0.5b — replaceable by qwen3.5-0.8b (tier 12
// > 10)", #200) plus the agent-harness evidence recorded on it ("qwen3.5-0.8b
// passes the harness — 3 of 72, bound 0.028", #467/#479).
//
// The three names are the id and both aliases the deleted manifest
// carried. A user who pinned the short form is exactly as migrated as one
// who pinned the id.
func TestLookupRetirement(t *testing.T) {
	live, err := BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatalf("BundledManifestsIncludingInternal: %v", err)
	}
	for _, name := range []string{
		"qwen2.5-coder-0.5b-instruct",
		"qwen2.5-coder-0.5b",
		"Qwen/Qwen2.5-Coder-0.5B-Instruct",
	} {
		r, ok := LookupRetirement(name)
		if !ok {
			t.Errorf("LookupRetirement(%q) miss; want the qwen3.5-0.8b retirement", name)
			continue
		}
		if r.SuccessorModelID != "qwen3.5-0.8b" {
			t.Errorf("LookupRetirement(%q).SuccessorModelID = %q, want qwen3.5-0.8b",
				name, r.SuccessorModelID)
		}
	}

	// Empty and unknown both miss, and neither may panic: LookupRetirement
	// is called on every resolution that failed, including hostile input
	// arriving as a request's `model` field.
	for _, name := range []string{"", "nonesuch", "waired/small", "../../etc/passwd"} {
		if r, ok := LookupRetirement(name); ok {
			t.Errorf("LookupRetirement(%q) = %q, want a miss", name, r.SuccessorModelID)
		}
	}

	// A live name must never be answered by the table. Cheap here, and it
	// is what lets every call site consult the table without first
	// checking whether the name was live.
	for _, m := range live {
		if r, ok := LookupRetirement(m.ModelID); ok {
			t.Errorf("live model %q is in the retirement table (-> %q)",
				m.ModelID, r.SuccessorModelID)
		}
		for _, a := range m.ModelAliases {
			if r, ok := LookupRetirement(a); ok {
				t.Errorf("live alias %q (of %s) is in the retirement table (-> %q)",
					a, m.ModelID, r.SuccessorModelID)
			}
		}
	}
}

// Record of today's behaviour: the table is package data that outlives
// every caller, so handing out the backing array would let one caller's
// append or sort change what the next one reads.
func TestRetirementsReturnsACopy(t *testing.T) {
	first := Retirements()
	if len(first) == 0 {
		t.Skip("empty table")
	}
	first[0].SuccessorModelID = "clobbered"
	first[0].Names = append(first[0].Names, "clobbered")

	second := Retirements()
	if second[0].SuccessorModelID == "clobbered" {
		t.Error("Retirements() hands out the package's own slice")
	}
	for _, n := range second[0].Names {
		if n == "clobbered" {
			t.Error("Retirements() shares the Names backing array")
		}
	}
}
