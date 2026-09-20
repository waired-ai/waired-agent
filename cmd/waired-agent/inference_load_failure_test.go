package main

import (
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

// seedFailedLoad puts one record in the provider's store.
func seedFailedLoad(t *testing.T, p *agentInferenceProvider, sha string, f catalog.VariantLoadFailure) {
	t.Helper()
	if err := p.store.Update(func(s *catalog.State) {
		if s.FailedLoads == nil {
			s.FailedLoads = map[string]catalog.VariantLoadFailure{}
		}
		s.FailedLoads[sha] = f
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// refFailure is the reference host of waired-agent#1443 and the load that
// failed there.
func refFailure() catalog.VariantLoadFailure {
	return catalog.VariantLoadFailure{
		ModelID: "qwen3.5-122b-a10b", VariantID: "q5-k-s-gguf",
		Reason: "this computer ran out of memory putting the model in memory",
		Detail: `llama-server process has terminated: exit status 0xe06d7363 ` +
			`(C:\Users\Public\ollama-models-1443\blobs\sha256-0fbaae8d)`,
		Context: catalog.LoadContext{
			EngineKind: "ollama", EngineVersion: "0.34.0",
			GPUModel: "AMD Radeon 8060S Graphics", DriverVersion: "32.0.21029.1001",
			VRAMTotalMB: 98304, RAMTotalMB: 130199,
		},
		Shape: catalog.LoadShape{
			ContextLength: 131072, KVCacheType: "f16", NumParallel: 1, Backend: "vulkan",
		},
		FailedAt: time.Date(2026, 9, 20, 4, 21, 26, 0, time.UTC),
	}
}

// TestPublishedLoadFailures_CarriesTheFactsAndNotTheProse is the contract
// waired-agent#1453's field table settled.
//
// PRODUCT CONTRACT: the facts of the attempt travel to the control plane;
// the words do not. The engine's own sentence names blob paths and other
// things particular to one machine, and a consumer ranking builds has no use
// for prose. There is no field on the wire to put it in, and this test is
// what keeps anyone from adding one by reflex.
func TestPublishedLoadFailures_CarriesTheFactsAndNotTheProse(t *testing.T) {
	e := &warmEngine{}
	p := warmProvider(t, e, "model-a", "a:q4")
	rec := refFailure()
	seedFailedLoad(t, p, "sha256:0fbaae8d", rec)

	got := p.PublishedLoadFailures()
	if len(got) != 1 {
		t.Fatalf("published %d records, want 1: %+v", len(got), got)
	}
	f := got[0]

	for _, tc := range []struct {
		name      string
		got, want any
	}{
		{"model id", f.ModelID, rec.ModelID},
		{"variant id", f.VariantID, rec.VariantID},
		{"variant sha", f.VariantSHA, "sha256:0fbaae8d"},
		{"engine kind", f.EngineKind, rec.Context.EngineKind},
		{"engine version", f.EngineVersion, rec.Context.EngineVersion},
		{"gpu model", f.GPUModel, rec.Context.GPUModel},
		{"driver version", f.DriverVersion, rec.Context.DriverVersion},
		{"vram total", f.VRAMTotalMB, rec.Context.VRAMTotalMB},
		{"ram total", f.RAMTotalMB, rec.Context.RAMTotalMB},
		{"context length", f.ContextLength, rec.Shape.ContextLength},
		{"kv cache type", f.KVCacheType, rec.Shape.KVCacheType},
		{"num parallel", f.NumParallel, rec.Shape.NumParallel},
		{"backend", f.Backend, rec.Shape.Backend},
		{"failed at", f.FailedAt, "2026-09-20T04:21:26Z"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}

	// CONTRACT: no field on the wire carries the sentence or the engine's
	// words. Checked by looking for them anywhere in the published record,
	// so a future field would have to be deliberate about it.
	rendered := strings.Join([]string{
		f.ModelID, f.VariantID, f.VariantSHA, f.EngineKind, f.EngineVersion,
		f.GPUModel, f.DriverVersion, f.KVCacheType, f.Backend, f.FailedAt,
	}, " ")
	for _, leaked := range []string{"ran out of memory", "0xe06d7363", `C:\Users\Public`} {
		if strings.Contains(rendered, leaked) {
			t.Errorf("the published record carries %q; the facts travel and the words do not", leaked)
		}
	}
}

// A record that cannot be keyed is not published. The ledger writer already
// refuses these; this is the second reader of that rule.
func TestPublishedLoadFailures_RefusesAnUnkeyableRecord(t *testing.T) {
	for _, tc := range []struct {
		name string
		sha  string
		mut  func(*catalog.VariantLoadFailure)
	}{
		{"no digest to key it by", "", nil},
		{"no model", "sha256:aaa", func(f *catalog.VariantLoadFailure) { f.ModelID = "" }},
		{"no variant", "sha256:aaa", func(f *catalog.VariantLoadFailure) { f.VariantID = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &warmEngine{}
			p := warmProvider(t, e, "model-a", "a:q4")
			rec := refFailure()
			if tc.mut != nil {
				tc.mut(&rec)
			}
			seedFailedLoad(t, p, tc.sha, rec)
			if got := p.PublishedLoadFailures(); len(got) != 0 {
				t.Errorf("published %+v, want nothing", got)
			}
		})
	}
}

// Nothing to say is nil, not an empty slice: a fresh install pushes the same
// bytes it did before the field existed.
func TestPublishedLoadFailures_NothingToSayIsNil(t *testing.T) {
	e := &warmEngine{}
	p := warmProvider(t, e, "model-a", "a:q4")
	if got := p.PublishedLoadFailures(); got != nil {
		t.Errorf("a host that has failed nothing published %+v, want nil", got)
	}
}

// The order is stable, so an unchanged set pushes identical bytes every
// probe tick rather than re-ordering a map on each one.
func TestPublishedLoadFailures_OrderIsStable(t *testing.T) {
	e := &warmEngine{}
	p := warmProvider(t, e, "model-a", "a:q4")
	for _, sha := range []string{"sha256:ccc", "sha256:aaa", "sha256:bbb"} {
		rec := refFailure()
		rec.ModelID = "model-" + sha
		seedFailedLoad(t, p, sha, rec)
	}
	first := p.PublishedLoadFailures()
	if len(first) != 3 {
		t.Fatalf("published %d, want 3", len(first))
	}
	for i, want := range []string{"sha256:aaa", "sha256:bbb", "sha256:ccc"} {
		if first[i].VariantSHA != want {
			t.Errorf("record %d = %s, want %s", i, first[i].VariantSHA, want)
		}
	}
	for range 5 {
		again := p.PublishedLoadFailures()
		for i := range first {
			if again[i] != first[i] {
				t.Fatalf("the published set re-ordered between calls at %d", i)
			}
		}
	}
}
