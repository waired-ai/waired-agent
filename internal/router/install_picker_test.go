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
			// 4 GB CPU box: only 3b/2b/0.8b fit, and the sole tier-30+
			// fit (coder-3b) is a 32k-window model. The #624 under-spec
			// rescue re-ranks without the context floor rather than newly
			// disabling inference — coder-3b stays the pick, flagged
			// ContextFloorSatisfied=false.
			name: "cpu-4gb-picks-3b-coder", hw: cpu(4), engine: catalog.RuntimeOllama,
			wantOK: true, wantTop: "qwen2.5-coder-3b-instruct",
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
