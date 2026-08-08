package router

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/runtime"
)

// TestSelectInstallModel_RealCatalog drives the install-time selector
// against the REAL bundled catalog with synthetic hardware profiles, so
// the assertions track the shipped resource floors.
//
// There is no quality floor to track any more (#522, owner decision
// 2026-08-08). What decides a host now is capacity — certain OOM, priced
// at the window the product serves since #552 — and the #624 native
// window. Both are hardware or manifest facts; neither is a threshold on
// quality_tier.
func TestSelectInstallModel_RealCatalog(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}

	cpu := func(ramGB int) hardware.Profile {
		return hardware.Profile{OS: "linux", Arch: "x86_64", RAMTotalGB: ramGB}
	}

	cases := []struct {
		name    string
		hw      hardware.Profile
		engine  string
		wantOK  bool
		wantTop string // expected best-fit model_id when wantOK
	}{
		{
			// 32 GB CPU box: a large model fits by the RAM gate. The exact
			// winner (a high-tier MoE such as qwen3.6-35b-a3b, whose 3B
			// active params fit) tracks the evolving catalog, so we only
			// assert a fitting above-floor pick rather than pin the id.
			name: "cpu-32gb-fits", hw: cpu(32), engine: catalog.RuntimeOllama,
			wantOK: true,
		},
		{
			// 8 GB CPU box: THIS CASE IS INVERTED BY #522. It expected
			// wantOK=false, because qwen3.5-2b is tier 27 and the floor
			// was 30 — a host that could hold a model was told it was
			// below the recommended spec and got none.
			//
			// The #624 context floor still excludes the 32k-window coder
			// entries, and #552's capacity gate still prices at the served
			// window: qwen3.5-4b needs 7403 MiB and this host has 6144, so
			// it is out. qwen3.5-2b needs 4012 MiB, holds its full 262,144
			// native window here, and is now the pick.
			//
			// The same verdict as an 8 GB Mac, and that is the point: the
			// arithmetic branches on Host.Class(), never on an operating
			// system.
			name: "cpu-8gb-picks-the-lightest-that-holds-its-window",
			hw:   cpu(8), engine: catalog.RuntimeOllama,
			wantOK: true, wantTop: "qwen3.5-2b",
		},
		{
			// 4 GB CPU box: nothing fits, and it is capacity that says so
			// rather than quality. After the OS allowance this host has
			// 2048 MiB, and the lightest catalog entry needs 3154 MiB to
			// hold its window (#552's 6 GB CPU-only cutoff). The 32k-window
			// coder entries that used to fit here are excluded by #624
			// regardless — waired#1031 removed the re-rank that rescued
			// them, because the window is a contract now and a 32k node has
			// no way to say so that a requester could route on.
			name: "cpu-4gb-nothing-fits", hw: cpu(4), engine: catalog.RuntimeOllama,
			wantOK: false,
		},
		{
			// 2 GB CPU box: the OS allowance alone accounts for it.
			name: "cpu-2gb-nothing-fits", hw: cpu(2), engine: catalog.RuntimeOllama,
			wantOK: false,
		},
		{
			// 16 GB Apple-Silicon UMA Mac (12 GB GPU budget): a model fits
			// resident and holds its window.
			name: "apple-uma-16gb-ok", hw: syntheticAppleUMA(16, 12*1024),
			engine: catalog.RuntimeOllama, wantOK: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			above, ok, err := SelectInstallModel(PickInput{
				Catalog:       manifests,
				Hardware:      tc.hw,
				Engine:        tc.engine,
				EngineVersion: runtime.OllamaPinnedVersion,
			})
			if err != nil {
				t.Fatalf("SelectInstallModel: unexpected error %v", err)
			}
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (above=%d)", ok, tc.wantOK, len(above))
			}
			if !ok {
				if len(above) != 0 {
					t.Errorf("under-spec: above should be empty, got %d", len(above))
				}
				return
			}
			if len(above) == 0 {
				t.Fatal("ok=true but no candidates returned")
			}
			// Candidates must be ordered best-first (quality_tier
			// non-increasing) so a caller stepping down for disk space
			// gets progressively lighter models without re-ranking.
			// quality_tier is the ordering here and nothing more — there
			// is no threshold left to clear (#522).
			for i, p := range above {
				if i > 0 && above[i-1].Variant.QualityTier < p.Variant.QualityTier {
					t.Errorf("candidates not tier-desc at %d: %d < %d",
						i, above[i-1].Variant.QualityTier, p.Variant.QualityTier)
				}
			}
			if tc.wantTop != "" && above[0].Manifest.ModelID != tc.wantTop {
				t.Errorf("best fit = %q, want %q", above[0].Manifest.ModelID, tc.wantTop)
			}
		})
	}
}

// TestSelectInstallModel_ErrorsSurface confirms a real misconfiguration
// (empty Engine) is returned as an error, NOT silently reported as
// under-spec — the caller must be able to tell the two apart.
func TestSelectInstallModel_ErrorsSurface(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	if _, ok, err := SelectInstallModel(PickInput{
		Catalog:  manifests,
		Hardware: hardware.Profile{RAMTotalGB: 32},
		// Engine deliberately empty.
	}); err == nil || ok {
		t.Fatalf("empty engine: want (ok=false, err!=nil), got ok=%v err=%v", ok, err)
	}
}

// TestSelectInstallModel_ASmallCardMustNotMakeAHostUnderSpec asserts
// that fitting a graphics card never leaves a host with no local model.
//
// Ratifying source: the owner decision of 2026-08-03 on
// waired-ai/waired#1056 — 「dGPU を挿して採用モデルが下がるべきではない
// (CPU/iGPU 環境は概して遅いのだから)」— and its decision 1, which
// reserves refusal for certain OOM. The reasoning is about hardware, not
// fairness: GPU inference is generally faster than CPU inference, so a
// machine that has one should be offered at least as good a model as the
// same machine without it. waired#988 item 5's "a LOWER pick is the
// accepted trade" is explicitly NOT permanent policy (same decision).
//
// Two mechanisms keep it true, and both are needed:
//
//   - hostfit.OllamaPlannedRung's rule 3 — a host's window is never
//     sized below what it would be with the accelerator removed. Without
//     it, an 8 GB card shrinks the sizing budget from tens of GB of
//     system RAM to 8 GB, the carded host declares a smaller window than
//     the card-less one, and the pick drops with it.
//   - SelectInstallModel standing the recommendation gate down before it
//     concludes a host has no local model.
//
// The test used to carry a knownSmallCardBreach fence for two hosts
// (8 GB and 16 GB RAM with a 2 GB card) that the #229 decode floor
// excluded at 19.96 tok/s while admitting the card-less host at 17.65 —
// the faster machine being the one refused. That pass no longer excludes
// (waired-ai/waired-agent#464), so the fence is gone and the sweep is
// asserted for every host again.
//
// A LOWER pick is still possible and is logged, not failed. The
// recommendation requires the weights to be GPU-resident, and a host with
// no accelerator has nothing to be resident in and is exempt — so a
// 32 GB machine with no card is offered a 22.6 GB mixture of experts and
// the same machine with a 16 GB card is offered a 6.6 GB model whose
// weights actually fit the card. Both configurations work; the second is
// faster per token and lower tier. Closing that gap means giving the
// CPU-only arm a real speed bound instead of the population constant it
// rests on now, which needs a measurement (waired-ai/waired-agent#466 and
// the install-time probe it depends on). Record of today's behaviour, not
// a rule.
func TestSelectInstallModel_ASmallCardMustNotMakeAHostUnderSpec(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	pick := func(hw hardware.Profile) (string, int, bool) {
		t.Helper()
		above, ok, err := SelectInstallModel(PickInput{
			Catalog: manifests, Hardware: hw, Engine: catalog.RuntimeOllama,
		})
		if err != nil {
			t.Fatalf("SelectInstallModel: %v", err)
		}
		if !ok {
			return "", 0, false
		}
		return above[0].Manifest.ModelID, above[0].Variant.QualityTier, true
	}

	var exercised bool
	for _, ramGB := range []int{8, 16, 32, 64, 128} {
		bare := hardware.Profile{OS: "linux", Arch: "x86_64", RAMTotalGB: ramGB}
		bareID, bareTier, bareOK := pick(bare)
		if !bareOK {
			continue // no local model without a card either; nothing to protect
		}
		for _, vramMB := range []int{2048, 4096, 8192, 12288, 16303, 24564} {
			carded := bare
			carded.GPUs = []hardware.GPU{{Vendor: "nvidia", Model: "test", VRAMTotalMB: vramMB}}
			id, tier, ok := pick(carded)
			if !ok {
				t.Errorf("RAM %d GB + %d MB card: no local model, while the same host with no "+
					"card installs %s (tier %d). Adding a graphics card must never remove "+
					"local inference", ramGB, vramMB, bareID, bareTier)
				continue
			}
			if tier < bareTier {
				t.Logf("RAM %3d GB + %5d MB card: %s (tier %d) vs %s (tier %d) with no card — "+
					"the card holds the smaller model's weights, the card-less host is exempt "+
					"from residency (waired-ai/waired-agent#466)",
					ramGB, vramMB, id, tier, bareID, bareTier)
				exercised = true
			}
		}
	}
	if !exercised {
		t.Error("no host in the sweep took a lower-tier pick for owning a card, so this " +
			"test never exercised the non-monotone case it exists to bound")
	}
}
