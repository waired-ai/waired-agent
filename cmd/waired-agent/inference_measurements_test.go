package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

// storeWithLedger seeds a store with the given per-variant measurements,
// keyed the way the benchmark writer keys them.
func storeWithLedger(t *testing.T, entries ...catalog.VariantMeasurement) *catalog.Store {
	t.Helper()
	store := catalog.NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err := store.Update(func(s *catalog.State) {
		s.MeasuredVariants = map[string]catalog.VariantMeasurement{}
		for i, e := range entries {
			// The key is opaque to this projection; a distinct one per
			// entry is all that matters here.
			s.MeasuredVariants[string(rune('a'+i))] = e
		}
	}); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	return store
}

func measuredAt(s string) time.Time {
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return ts
}

// PRODUCT CONTRACT (waired-agent#970): the ledger reaches the wire with
// its provenance, so a consumer can tell what the figure describes and
// when it stops describing it.
func TestPublishedMeasurements_CarriesTheProvenance(t *testing.T) {
	p := &agentInferenceProvider{store: storeWithLedger(t, catalog.VariantMeasurement{
		ModelID: "qwen3.5-9b", VariantID: "q4-gguf", MeasuredTokps: 11,
		Method: "ollama_eval", EngineKind: "ollama", EngineVersion: "0.32.13",
		MeasuredAt: measuredAt("2026-08-21T00:00:00Z"),
	})}

	got := p.PublishedMeasurements()
	if len(got) != 1 {
		t.Fatalf("published %d entries, want 1: %+v", len(got), got)
	}
	m := got[0]
	if m.ModelID != "qwen3.5-9b" || m.VariantID != "q4-gguf" {
		t.Errorf("subject = %q/%q, want qwen3.5-9b/q4-gguf", m.ModelID, m.VariantID)
	}
	if m.DecodeTokps != 11 {
		t.Errorf("DecodeTokps = %v, want 11", m.DecodeTokps)
	}
	if m.Method != "ollama_eval" {
		t.Errorf("Method = %q, want ollama_eval", m.Method)
	}
	if m.EngineKind != "ollama" || m.EngineVersion != "0.32.13" {
		t.Errorf("engine = %q/%q, want ollama/0.32.13", m.EngineKind, m.EngineVersion)
	}
	if m.MeasuredAt != "2026-08-21T00:00:00Z" {
		t.Errorf("MeasuredAt = %q, want RFC3339Nano UTC", m.MeasuredAt)
	}
}

// PRODUCT CONTRACT (waired-agent#970): the order is stable across ticks.
//
// The ledger is a map, and Go randomises map iteration. An unordered
// slice would make every push differ from the last, and the control
// plane compares pushes by CONTENT to decide whether to store and
// notify — so the churn would be a re-store and a map-changed
// notification per tick, on every host, forever.
func TestPublishedMeasurements_OrderIsStable(t *testing.T) {
	entries := []catalog.VariantMeasurement{
		{ModelID: "mid", VariantID: "q4", MeasuredTokps: 26},
		{ModelID: "big", VariantID: "q8", MeasuredTokps: 11},
		{ModelID: "big", VariantID: "q4", MeasuredTokps: 12},
		{ModelID: "small", VariantID: "q4", MeasuredTokps: 44},
	}
	p := &agentInferenceProvider{store: storeWithLedger(t, entries...)}

	want := []string{"big/q4", "big/q8", "mid/q4", "small/q4"}
	// Repeated because one agreeing run proves nothing about a map: the
	// randomisation has to be given several chances to show itself.
	for range 20 {
		got := p.PublishedMeasurements()
		if len(got) != len(want) {
			t.Fatalf("published %d entries, want %d", len(got), len(want))
		}
		for i, w := range want {
			if got[i].ModelID+"/"+got[i].VariantID != w {
				t.Fatalf("order = %v..., want %v (map iteration reached the wire)",
					got[i].ModelID+"/"+got[i].VariantID, w)
			}
		}
	}
}

// PRODUCT CONTRACT (waired-agent#970): an entry that cannot be keyed by
// a consumer is not published. The receiver resolves the pair against
// its own catalog to compute the key, so a nameless entry is a figure
// nobody can file — and a figure filed wrong excludes a model the host
// never ran.
func TestPublishedMeasurements_DropsWhatCannotBeKeyed(t *testing.T) {
	p := &agentInferenceProvider{store: storeWithLedger(t,
		catalog.VariantMeasurement{ModelID: "ok", VariantID: "q4", MeasuredTokps: 11},
		catalog.VariantMeasurement{ModelID: "", VariantID: "q4", MeasuredTokps: 11},
		catalog.VariantMeasurement{ModelID: "no-variant", MeasuredTokps: 11},
		catalog.VariantMeasurement{ModelID: "zero", VariantID: "q4", MeasuredTokps: 0},
	)}
	got := p.PublishedMeasurements()
	if len(got) != 1 || got[0].ModelID != "ok" {
		t.Errorf("published %+v, want only the keyable entry", got)
	}
}

// A host that has measured nothing reports nothing — not an empty list,
// which would be a different byte string on a signed map.
func TestPublishedMeasurements_NothingMeasuredIsNil(t *testing.T) {
	p := &agentInferenceProvider{store: storeWithLedger(t)}
	if got := p.PublishedMeasurements(); got != nil {
		t.Errorf("published %+v, want nil", got)
	}
}
