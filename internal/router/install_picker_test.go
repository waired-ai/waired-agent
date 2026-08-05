package router

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/runtime"
)

// TestSelectInstallModel_RealCatalog drives the install-time selector
// against the REAL bundled catalog with synthetic hardware profiles, so
// the assertions track the shipped quality_tiers / resource floors and
// the tier-30 coding floor (#517).
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
			// 8 GB CPU box: 14b (min 16) is out. The #624 context floor
			// excludes the 32k-window coder-7b (tier 45) from
			// auto-selection; the best 262144-native fit is qwen3.5-4b
			// (tier 42) — a small tier cost for a real coding window.
			name: "cpu-8gb-picks-7b", hw: cpu(8), engine: catalog.RuntimeOllama,
			wantOK: true, wantTop: "qwen3.5-4b",
		},
		{
			// 4 GB CPU box: only 3b/2b/0.8b fit, and the sole tier-30+ fit
			// (coder-3b) is a 32k-window model. That used to be rescued by
			// re-ranking without the context floor; waired#1031 removed the
			// rescue, because the window is a contract now and a 32k node
			// has no way to say so that a requester could route on. The
			// host is below the recommended spec: it enrols, routes to peers, and runs no
			// local engine.
			name: "cpu-4gb-under-spec", hw: cpu(4), engine: catalog.RuntimeOllama,
			wantOK: false,
		},
		{
			// 2 GB CPU box: only qwen3.5-0.8b (tier 12) fits — below the
			// floor → under-spec, no auto-selection.
			name: "cpu-2gb-under-spec", hw: cpu(2), engine: catalog.RuntimeOllama,
			wantOK: false,
		},
		{
			// 16 GB Apple-Silicon UMA Mac (12 GB GPU budget): a coder fits
			// resident; selection clears the floor.
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
			}, InstallQualityFloorTier)
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
			// Every returned candidate must clear the floor and be ordered
			// best-first (quality_tier non-increasing).
			for i, p := range above {
				if p.Variant.QualityTier < InstallQualityFloorTier {
					t.Errorf("candidate %d (%s) tier %d below floor %d",
						i, p.Manifest.ModelID, p.Variant.QualityTier, InstallQualityFloorTier)
				}
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
	}, InstallQualityFloorTier); err == nil || ok {
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
//   - hostfit.OllamaPlannedWindow's rule 3 — a host's window is never
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
		}, InstallQualityFloorTier)
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
