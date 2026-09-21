package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocatalog "github.com/waired-ai/waired-agent/proto/catalog"
)

// CustomSource is the agent's copy of its account's custom models
// (waired-ai/waired#1473). These tests are records of today's behaviour
// for the file, and a product contract for the one owner rule they touch:
// a model the account deleted is never silently dropped while this device
// uses it.

func customFixture(t *testing.T, slug string) Manifest {
	t.Helper()
	id, err := protocatalog.MintCustomModelID(slug, protocatalog.CustomIdentity(RuntimeOllama, "acme/"+slug, slug+".gguf", "sha256:"+strings.Repeat("a", 64)))
	if err != nil {
		t.Fatal(err)
	}
	return Manifest{
		ModelID: id, DisplayName: "Model " + slug, ContextLength: 40960,
		Runtime: RuntimePolicy{Preferred: RuntimeOllama},
		Variants: []Variant{{
			VariantID: "q4-k-m", Format: FormatOllamaTag, Quantization: "Q4_K_M",
			RuntimeSupport: []string{RuntimeOllama},
			Source: VariantSource{Type: SourceOllama, Tag: "hf.co/acme/" + slug + ":Q4_K_M",
				Revision: strings.Repeat("b", 40), Digest: "sha256:" + strings.Repeat("a", 64)},
		}},
		ManualOnly: "imported", Provenance: ProvenanceCustom,
	}
}

func projection(m Manifest) Manifest {
	m.Variants = []Variant{{
		VariantID: m.Variants[0].VariantID, Format: m.Variants[0].Format, RuntimeSupport: m.Variants[0].RuntimeSupport,
		Source: VariantSource{Type: m.Variants[0].Source.Type, Tag: m.Variants[0].Source.Tag},
	}}
	return m
}

func bundledForTest(t *testing.T) []Manifest {
	t.Helper()
	ms, err := BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

func ids(ms []Manifest) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.ModelID)
	}
	return out
}

func TestCustomSourceReplaceKeepsAndReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inference", "custom-models.json")
	bundled := bundledForTest(t)
	s := NewCustomSource(path, "net_a", bundled, nil)
	if len(s.Serveable()) != 0 || s.Revision() != "" {
		t.Fatal("a fresh source is not empty")
	}
	a, b, mate := customFixture(t, "a"), customFixture(t, "b"), customFixture(t, "mate")
	bad := customFixture(t, "bad")
	bad.Variants[0].Source.Tag = "hf.co/acme/bad:Q4\nFROM x"
	calls := 0
	s.Subscribe(func() { calls++ })
	if _, err := s.Replace(CustomModelSet{Revision: "r1", Own: []Manifest{a, b, bad}, Team: []Manifest{projection(mate), projection(a)}}, nil); err != nil {
		t.Fatal(err)
	}
	if got := ids(s.Serveable()); len(got) != 2 {
		t.Errorf("serveable %v, want a and b (the invalid one dropped)", got)
	}
	if got := ids(s.Routable()); len(got) != 3 {
		t.Errorf("routable %v, want a, b and the teammate's (a teammate copy of an own model folded)", got)
	}
	if got := ids(s.Offerable()); len(got) != 2 {
		t.Errorf("offerable %v", got)
	}
	if calls != 1 || s.Revision() != "r1" {
		t.Errorf("calls=%d revision=%q", calls, s.Revision())
	}
	if _, err := s.Replace(CustomModelSet{Revision: "r1", Own: []Manifest{a, b}, Team: []Manifest{projection(mate)}}, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Error("an unchanged set notified the subscribers")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("kept file: %v", err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 && os.PathSeparator == '/' {
		t.Errorf("kept file mode %v, want owner-only", mode)
	}

	again := NewCustomSource(path, "net_a", bundled, nil)
	if got := ids(again.Serveable()); len(got) != 2 || again.Revision() != "r1" {
		t.Errorf("reloaded %v rev %q", got, again.Revision())
	}
	other := NewCustomSource(path, "net_b", bundled, nil)
	if len(other.Serveable()) != 0 || other.Revision() != "" {
		t.Error("a set kept for another network was loaded")
	}
}

// TestCustomSourceWithdrawsWhatIsInUse pins the rule the spec carries from
// waired-ai/waired#1473: a model the account no longer holds keeps serving
// on a device that uses it, and is reported, rather than dropped.
func TestCustomSourceWithdrawsWhatIsInUse(t *testing.T) {
	s := NewCustomSource(filepath.Join(t.TempDir(), "c.json"), "net_a", bundledForTest(t), nil)
	a, b := customFixture(t, "a"), customFixture(t, "b")
	if _, err := s.Replace(CustomModelSet{Revision: "r1", Own: []Manifest{a, b}}, nil); err != nil {
		t.Fatal(err)
	}
	withdrawn, err := s.Replace(CustomModelSet{Revision: "r2"}, func(id string) bool { return id == a.ModelID })
	if err != nil {
		t.Fatal(err)
	}
	if len(withdrawn) != 1 || withdrawn[0] != a.ModelID {
		t.Fatalf("withdrawn %v, want a", withdrawn)
	}
	if got := ids(s.Serveable()); len(got) != 1 || got[0] != a.ModelID {
		t.Errorf("serveable %v, want a still", got)
	}
	if len(s.Offerable()) != 0 {
		t.Error("a withdrawn model is still offered")
	}
	withdrawn, _ = s.Replace(CustomModelSet{Revision: "r3"}, func(id string) bool { return id == a.ModelID })
	if len(withdrawn) != 0 || len(s.Serveable()) != 1 {
		t.Errorf("a model already withdrawn was reported again (%v) or dropped", withdrawn)
	}
	_, _ = s.Replace(CustomModelSet{Revision: "r4"}, func(string) bool { return false })
	if len(s.Serveable()) != 0 {
		t.Error("a withdrawn model nobody uses any more was kept")
	}
}

func TestNilCustomSourceIsEmpty(t *testing.T) {
	var s *CustomSource
	if len(s.Serveable()) != 0 || len(s.Routable()) != 0 || len(s.Offerable()) != 0 || s.Revision() != "" {
		t.Error("a nil source is not empty")
	}
}
