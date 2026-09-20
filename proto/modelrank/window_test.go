package modelrank

import (
	"errors"
	"testing"

	"github.com/waired-ai/waired-agent/proto/catalog"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// longLadder is ladder() with the rope scaling its publisher would document
// put on the LOWEST-tier family, and a per-token KV size real enough for the
// sizing to bite.
//
// The lowest tier on purpose. Put it on the top-tier family instead and the
// window filter becomes unobservable: that family wins the ranking at both
// windows, so a test asserting the winner passes whether the filter runs or
// not. The first draft of this file did exactly that and went green with the
// filter deleted. With the scaling on "small", the coding window is won by
// "big" and the long window can only be answered by "small", so the set
// moves if and only if the filter ran.
func longLadder() []catalog.Manifest {
	ms := ladder()
	// EVERY family gets a per-token KV size, not just the scaled one. Two
	// reasons, both learned from a draft that got this wrong. Without a
	// size the sizing cannot be proved, OllamaDeclaresWindowFrom answers
	// no, and the family is demoted — so a fixture where only one family
	// carries a size is really testing "which family has a KV size", and
	// the quality ladder never gets a say. And without a size the floor
	// test below is permissive at every window and measures nothing.
	// The values are of the order the shipped catalog carries.
	for i, kvb := range []int{65536, 32768, 16384} {
		ms[i].Variants[0].KVBytesPerTokenFP16 = kvb
	}
	last := len(ms) - 1
	ms[last].RopeScaling = &catalog.RopeScaling{
		Type:                      catalog.RopeScalingYaRN,
		Factor:                    4,
		OriginalContextLength:     262144,
		PublisherMaxContextLength: 1010000,
	}
	return ms
}

// longFamily is the one family in longLadder that reaches the long window.
func longFamily() (catalog.Manifest, catalog.Variant) {
	ms := longLadder()
	m := ms[len(ms)-1]
	return m, m.Variants[0]
}

func windowInput(window int) PickInput {
	return PickInput{
		Catalog: longLadder(),
		Host:    roomyHost(),
		Engine:  catalog.RuntimeOllama,
		Window:  window,
	}
}

// PRODUCT CONTRACT (owner ruling 2026-09-20 on waired-ai/waired#1359,
// implemented for waired-ai/waired#1456): a person picks an engine, then a
// window, and the tiers and the recommendation are compared WITHIN that
// window. A model that cannot reach the window is not a worse candidate —
// it is not a candidate.
func TestRankModels_LongWindowRanksOnlyWhatReachesIt(t *testing.T) {
	ranked, err := RankModels(windowInput(hostfit.ServingWindow1M))
	if err != nil {
		t.Fatalf("RankModels: %v", err)
	}
	if len(ranked) != 1 {
		var got []string
		for _, p := range ranked {
			got = append(got, p.Manifest.ModelID)
		}
		t.Fatalf("ranked %v at the long window, want only the family that reaches it", got)
	}
	if ranked[0].Manifest.ModelID != "small" {
		t.Errorf("ranked %q, want \"small\" — the only family that reaches the long window",
			ranked[0].Manifest.ModelID)
	}
	// The mutation this test exists to catch: delete the step-1.6 filter and
	// "big" wins here, exactly as it does at the coding window, because
	// nothing else in RankModels asks whether a model reaches the window.
	coding, err := RankModels(windowInput(0))
	if err != nil {
		t.Fatalf("RankModels(0): %v", err)
	}
	if coding[0].Manifest.ModelID == ranked[0].Manifest.ModelID {
		t.Fatalf("the same family (%q) won at both windows: this fixture cannot "+
			"tell the filter from a no-op", coding[0].Manifest.ModelID)
	}
}

// PRODUCT CONTRACT: the window filter is hard. It must not fall through to
// the coding window the way the three narrowing passes do — serving 200,704
// to someone who asked for 1,048,576, and saying nothing, is the failure the
// two-rung contract exists to remove.
func TestRankModels_LongWindowDoesNotFallThroughToTheCodingWindow(t *testing.T) {
	in := windowInput(hostfit.ServingWindow1M)
	in.Catalog = ladder() // nothing here documents any scaling
	_, err := RankModels(in)
	if !errors.Is(err, ErrWindowNotServed) {
		t.Fatalf("err = %v, want ErrWindowNotServed", err)
	}
	if errors.Is(err, ErrHardwareInsufficient) {
		t.Error("answered ErrHardwareInsufficient: a bigger machine would not change this")
	}
}

// PRODUCT CONTRACT: an explicit model pin does NOT bypass the window, and
// this is the one place the window differs from manual_only (which a pin
// does bypass). The pin says which model; the window is a separate and
// equally explicit statement made at the same moment.
func TestRankModels_PreferredModelIDDoesNotBypassTheWindow(t *testing.T) {
	in := windowInput(hostfit.ServingWindow1M)
	in.PreferredModelID = "mid" // real, fits, reaches only its trained length
	_, err := RankModels(in)
	if !errors.Is(err, ErrWindowNotServed) {
		t.Fatalf("err = %v, want ErrWindowNotServed", err)
	}
	// Sanity: the same pin at the coding window is honoured, so the failure
	// above is the window and not the pin.
	in.Window = 0
	if got := top(t, in); got != "mid" {
		t.Errorf("at the coding window the pin gave %q, want \"mid\"", got)
	}
}

// A RECORD of today's behaviour (hostfit.EngineServesWindow): the long rung
// is ollama-only until a vLLM host is measured at it.
func TestRankModels_TheLongWindowIsNotOfferedOnVLLM(t *testing.T) {
	in := windowInput(hostfit.ServingWindow1M)
	in.Engine = catalog.RuntimeVLLM
	_, err := RankModels(in)
	if !errors.Is(err, ErrWindowNotServed) {
		t.Fatalf("err = %v, want ErrWindowNotServed", err)
	}
}

// PRODUCT CONTRACT: 0 and ServingWindow200k are the same request, as
// PickInput.Window's doc says. A caller that spells the coding window out
// must get exactly what a caller that left it at zero gets.
func TestRankModels_CodingWindowSpelledOutMatchesZero(t *testing.T) {
	zero, err := RankModels(windowInput(0))
	if err != nil {
		t.Fatalf("RankModels(0): %v", err)
	}
	named, err := RankModels(windowInput(hostfit.ServingWindow200k))
	if err != nil {
		t.Fatalf("RankModels(200k): %v", err)
	}
	if len(zero) != len(named) {
		t.Fatalf("ranked %d at 0 and %d at 200,704", len(zero), len(named))
	}
	for i := range zero {
		if zero[i].Manifest.ModelID != named[i].Manifest.ModelID ||
			zero[i].Variant.VariantID != named[i].Variant.VariantID ||
			zero[i].Recommendation != named[i].Recommendation {
			t.Errorf("rank %d differs: %+v vs %+v", i, zero[i], named[i])
		}
	}
}

// The shipped catalog, so the filter is exercised against the real rope
// scaling rather than only against a fixture written to pass it.
func TestRankModels_LongWindowOverTheShippedCatalog(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	ranked, err := RankModels(PickInput{
		Catalog: manifests,
		// Large enough that memory is not what decides the set, so a
		// change to the window filter is what moves this test.
		Host:   hostfit.Host{RAMTotalGB: 512},
		Engine: catalog.RuntimeOllama,
		Window: hostfit.ServingWindow1M,
	})
	if err != nil {
		t.Fatalf("RankModels: %v", err)
	}
	if len(ranked) == 0 {
		t.Fatal("nothing ranked at the long window; the catalog should offer some")
	}
	for _, p := range ranked {
		if !hostfit.ReachesWindow(p.Manifest, hostfit.ServingWindow1M) {
			t.Errorf("%s was ranked at the long window but does not reach it",
				p.Manifest.ModelID)
		}
	}
}

// PRODUCT CONTRACT: the floor a host is judged against is the window that
// was ASKED for. A host that lands on 200,704 after someone asked for
// 1,048,576 has not met the request.
func TestOllamaServesContextFloorAt_JudgesTheWindowAsked(t *testing.T) {
	m, v := longFamily()
	// 12 GB and 16 GB are MEASURED against this fixture, not chosen: the
	// boundary for it sits between them, so at 12 the ladder settles on
	// 200,704 after being asked for 1,048,576 and at 16 it reaches the long
	// rung. The pair is what makes this test bite — one host alone cannot
	// tell "judged the window asked" from "always says no", which is the
	// mistake a single-host draft of this test made.
	tight := hostfit.Host{RAMTotalGB: 12}
	roomy := hostfit.Host{RAMTotalGB: 16}

	if ok, _ := OllamaServesContextFloorAt(m, v, tight, 0); !ok {
		t.Fatal("the coding window should be served on the tight host; the fixture is wrong")
	}
	if ok, _ := OllamaServesContextFloorAt(m, v, tight, hostfit.ServingWindow1M); ok {
		t.Error("tight host: reported the long window served, but the ladder settles at 200,704")
	}
	if ok, _ := OllamaServesContextFloorAt(m, v, roomy, hostfit.ServingWindow1M); !ok {
		t.Error("roomy host: reported the long window NOT served, but the ladder reaches it")
	}
}
