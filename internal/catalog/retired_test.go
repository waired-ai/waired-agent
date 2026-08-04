package catalog

import (
	"strings"
	"testing"
)

// Product contract (waired-ai/waired-agent#200): the four states
// ResolveModel distinguishes. Each one has a caller that acts on it and
// on nothing else, so collapsing any pair is a behaviour change even
// though the signature would still compile.
func TestResolveModel(t *testing.T) {
	// A fixture, not the shipped catalog: the point of these cases is the
	// state machine, and three manifests make "the successor is absent
	// from THIS set" writable at all.
	full := []Manifest{
		{ModelID: "qwen3.5-0.8b", ModelAliases: []string{"Qwen/Qwen3.5-0.8B"}},
		{ModelID: "granite4-350m", ModelAliases: []string{"waired/tiny"}, InternalOnly: "CI fixture"},
	}
	withoutSuccessor := []Manifest{full[1]}

	cases := []struct {
		name          string
		in            string
		manifests     []Manifest
		wantModelID   string
		wantSuccessor string // "" = no retirement in the answer
		wantOK        bool
	}{
		{"live id", "granite4-350m", full, "granite4-350m", "", true},
		{"live alias", "waired/tiny", full, "granite4-350m", "", true},
		{"live successor by its own name", "qwen3.5-0.8b", full, "qwen3.5-0.8b", "", true},

		{"retired id substitutes", "qwen2.5-coder-0.5b-instruct", full,
			"qwen3.5-0.8b", "qwen3.5-0.8b", true},
		{"retired short alias substitutes", "qwen2.5-coder-0.5b", full,
			"qwen3.5-0.8b", "qwen3.5-0.8b", true},
		{"retired HF alias substitutes", "Qwen/Qwen2.5-Coder-0.5B-Instruct", full,
			"qwen3.5-0.8b", "qwen3.5-0.8b", true},

		// Retired, and this manifest set cannot serve the successor. Not
		// "unknown": the caller can still say WHICH model went away.
		{"successor absent from this set", "qwen2.5-coder-0.5b-instruct", withoutSuccessor,
			"", "qwen3.5-0.8b", false},

		{"unknown", "nonesuch", full, "", "", false},
		{"empty", "", full, "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, r, ok := ResolveModel(c.in, c.manifests)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v (model=%q successor=%q)",
					ok, c.wantOK, m.ModelID, r.SuccessorModelID)
			}
			if m.ModelID != c.wantModelID {
				t.Errorf("model = %q, want %q", m.ModelID, c.wantModelID)
			}
			if r.SuccessorModelID != c.wantSuccessor {
				t.Errorf("successor = %q, want %q", r.SuccessorModelID, c.wantSuccessor)
			}
			// A retirement in the answer always carries its reason, because
			// the reason is what a caller logs or shows.
			if r.SuccessorModelID != "" && strings.TrimSpace(r.Reason) == "" {
				t.Error("substituted without a reason to report")
			}
		})
	}
}

// Product contract (#200): a live name is answered by the catalog and the
// table is never consulted for it. This is what lets every call site
// reach for ResolveModel unconditionally instead of branching first.
func TestResolveModelPrefersTheLiveCatalog(t *testing.T) {
	// A manifest that claims a retired name would be a catalog bug — the
	// proto weld test rejects it — but if one ever existed, resolution
	// must not depend on which lookup a caller happened to run first.
	shadow := []Manifest{{ModelID: "shadow", ModelAliases: []string{"qwen2.5-coder-0.5b"}}}
	m, r, ok := ResolveModel("qwen2.5-coder-0.5b", shadow)
	if !ok || m.ModelID != "shadow" {
		t.Fatalf("ResolveModel = (%q, %v); want the live manifest to win", m.ModelID, ok)
	}
	if r.SuccessorModelID != "" {
		t.Errorf("reported a retirement for a name the catalog answered: %q", r.SuccessorModelID)
	}
}

// Record of today's behaviour: the notice is one string so every surface
// that reports a substitution reports it identically, and docs-site can
// quote it verbatim (CLAUDE.md §Documentation).
func TestRetirementNotice(t *testing.T) {
	r, ok := LookupRetirement("qwen2.5-coder-0.5b")
	if !ok {
		t.Fatal("the 0.5b retirement is missing from the table")
	}
	got := RetirementNotice("qwen2.5-coder-0.5b", r)
	want := `"qwen2.5-coder-0.5b" was retired; using "qwen3.5-0.8b" instead`
	if got != want {
		t.Errorf("RetirementNotice = %q, want %q", got, want)
	}
}

// Product contract (#200): the agent resolves against the COMPLETE set,
// so every retired name must reach a servable model on the real catalog.
// The proto test pins the successor's presence; this pins that the
// agent's own resolver, with the agent's own manifest set, actually
// completes the migration.
func TestResolveModelMigratesEveryRetiredNameOnTheShippedCatalog(t *testing.T) {
	all, err := BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatalf("BundledManifestsIncludingInternal: %v", err)
	}
	checked := 0
	for _, ret := range Retirements() {
		for _, n := range ret.Names {
			m, r, ok := ResolveModel(n, all)
			if !ok {
				t.Errorf("retired name %q does not resolve on the shipped catalog", n)
				continue
			}
			if m.ModelID != ret.SuccessorModelID {
				t.Errorf("ResolveModel(%q) = %q, want the successor %q",
					n, m.ModelID, ret.SuccessorModelID)
			}
			if r.SuccessorModelID == "" {
				t.Errorf("ResolveModel(%q) substituted without reporting the retirement", n)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no retired names to check — this test is asserting nothing")
	}
}
