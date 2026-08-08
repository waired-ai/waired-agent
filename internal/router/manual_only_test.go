package router

import (
	"errors"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/runtime"
)

// manualOnlyFixture is three ollama models where the BEST one is the one
// under test, so every assertion below is "the winner changed", not "a
// loser moved". ContextLength is deliberately sub-floor on all three
// (like fixtureCatalog): that makes the #624 gate fall through for the
// whole set, which keeps these cases about manual_only and nothing else.
//
// withheld reports whether "withheld-top" carries manual_only; the two
// catalogs are otherwise byte-identical, which is what makes each case a
// controlled A/B rather than two separate scenarios.
func manualOnlyFixture(withheld bool) []catalog.Manifest {
	reason := ""
	if withheld {
		reason = "test fixture: offered but never chosen automatically"
	}
	return []catalog.Manifest{
		{
			ModelID: "plain-low", ContextLength: 32768,
			Capabilities: []string{"chat", "tool_use"},
			Variants: []catalog.Variant{{
				VariantID: "q4-gguf", Format: "ollama-tag",
				Quantization: "Q4_K_M", RuntimeSupport: []string{"ollama"},
				EstimatedWeightGB: 2.0, MinRAMGB: 4, QualityTier: 30,
				ParamCount: 3_000_000_000,
				Source:     catalog.VariantSource{Type: "ollama", Tag: "low:3b"},
			}},
		},
		{
			ModelID: "plain-mid", ContextLength: 32768,
			Capabilities: []string{"chat", "tool_use"},
			Variants: []catalog.Variant{{
				VariantID: "q4-gguf", Format: "ollama-tag",
				Quantization: "Q4_K_M", RuntimeSupport: []string{"ollama"},
				EstimatedWeightGB: 6.0, MinRAMGB: 12, QualityTier: 50,
				ParamCount: 9_000_000_000,
				Source:     catalog.VariantSource{Type: "ollama", Tag: "mid:9b"},
			}},
		},
		{
			ModelID: "withheld-top", ContextLength: 32768,
			ManualOnly:   reason,
			Capabilities: []string{"chat", "tool_use"},
			Variants: []catalog.Variant{{
				VariantID: "q4-gguf", Format: "ollama-tag",
				Quantization: "Q4_K_M", RuntimeSupport: []string{"ollama"},
				EstimatedWeightGB: 12.0, MinRAMGB: 20, QualityTier: 80,
				ParamCount: 20_000_000_000,
				Source:     catalog.VariantSource{Type: "ollama", Tag: "top:20b"},
			}},
		},
	}
}

func manualOnlyHost() hardware.Profile {
	return hardware.Profile{OS: "linux", Arch: "x86_64", RAMTotalGB: 32}
}

func manualOnlyInput(withheld bool) PickInput {
	return PickInput{
		Catalog:       manualOnlyFixture(withheld),
		Hardware:      manualOnlyHost(),
		Engine:        catalog.RuntimeOllama,
		EngineVersion: runtime.OllamaPinnedVersion,
	}
}

// TestManualOnly_WithheldFromEveryAutomaticPath is the product contract
// from #521: a manual_only model is never the answer to an automatic
// question. Ratifying source:
// docs/decisions/20260805/1427-quality-tier-is-a-curated-ladder.md.
//
// Every path is driven twice with the same input and only manual_only
// flipped, so a case that fails is a path that forgot the field rather
// than a path that never picked the model anyway. All five reach the
// catalog through RankModels; they are enumerated here because the
// point of the contract is that no CALLER can be the one that forgets.
func TestManualOnly_WithheldFromEveryAutomaticPath(t *testing.T) {
	cases := []struct {
		name string
		// pick returns the model_id this path chooses, or "" for none.
		pick func(t *testing.T, in PickInput) string
	}{
		{
			// The serving-time ranking. Everything else is built on it.
			name: "RankModels",
			pick: func(t *testing.T, in PickInput) string {
				ranked, err := RankModels(in)
				if err != nil {
					t.Fatalf("RankModels: %v", err)
				}
				return ranked[0].Manifest.ModelID
			},
		},
		{
			// The install-time commitment, at the shipped floor.
			name: "SelectInstallModel",
			pick: func(t *testing.T, in PickInput) string {
				above, ok, err := SelectInstallModel(in)
				if err != nil {
					t.Fatalf("SelectInstallModel: %v", err)
				}
				if !ok || len(above) == 0 {
					return ""
				}
				return above[0].Manifest.ModelID
			},
		},
		{
			// The badge a catalog UI shows. Delegates to
			// SelectInstallModel and falls back to RankModels for
			// under-spec hosts (family_picker.go) — both covered.
			name: "RecommendedFamily",
			pick: func(t *testing.T, in PickInput) string {
				return RecommendedFamily(in)
			},
		},
		{
			// The upgrade proposal, from the lowest rung with plenty
			// of measured headroom so the prediction clears the bar
			// for every candidate above it.
			name: "UpgradeCandidate",
			pick: func(t *testing.T, in PickInput) string {
				p, _, ok := UpgradeCandidate(UpgradeInput{
					Pick:            in,
					ActiveModelID:   "plain-low",
					ActiveVariantID: "q4-gguf",
					MeasuredTokps:   400,
					FloorTokps:      10,
				})
				if !ok {
					return ""
				}
				return p.Manifest.ModelID
			},
		},
		{
			// The lighter-model proposal. It steps down from the
			// ACTIVE variant, so the case needs an active heavier
			// than every candidate — otherwise the answer is the
			// rung below the top and the A/B tests nothing. Appended
			// to both catalogs, so the comparison stays controlled.
			name: "LighterCandidate",
			pick: func(t *testing.T, in PickInput) string {
				in.Catalog = append(in.Catalog, catalog.Manifest{
					ModelID: "plain-huge", ContextLength: 32768,
					Capabilities: []string{"chat", "tool_use"},
					Variants: []catalog.Variant{{
						VariantID: "q4-gguf", Format: "ollama-tag",
						Quantization: "Q4_K_M", RuntimeSupport: []string{"ollama"},
						EstimatedWeightGB: 20.0, MinRAMGB: 24, QualityTier: 90,
						ParamCount: 32_000_000_000,
						Source:     catalog.VariantSource{Type: "ollama", Tag: "huge:32b"},
					}},
				})
				p, ok := LighterCandidate(in, "plain-huge", "q4-gguf")
				if !ok {
					return ""
				}
				return p.Manifest.ModelID
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			offered := tc.pick(t, manualOnlyInput(false))
			if offered != "withheld-top" {
				t.Fatalf("with manual_only unset this path chose %q, want %q — "+
					"the A/B below proves nothing unless the model wins when it is allowed to",
					offered, "withheld-top")
			}
			got := tc.pick(t, manualOnlyInput(true))
			if got == "withheld-top" {
				t.Errorf("%s chose the manual_only model; want any other candidate "+
					"(#521: a withheld model is never an automatic answer)", tc.name)
			}
			if got == "" {
				t.Errorf("%s returned nothing; want it to fall through to the next "+
					"candidate, not to give up (plain-mid and plain-low both fit)", tc.name)
			}
		})
	}
}

// TestManualOnly_ExplicitPinStillWins is the other half of the contract:
// withholding a model from automatic choice must not break a pin
// somebody already wrote into agent.json, preferred-model.json, or a
// control-plane desired model (proto/catalog/manifest.go).
func TestManualOnly_ExplicitPinStillWins(t *testing.T) {
	in := manualOnlyInput(true)
	in.PreferredModelID = "withheld-top"

	ranked, err := RankModels(in)
	if err != nil {
		t.Fatalf("RankModels with an explicit pin: %v — a pinned manual_only "+
			"model must still resolve, not 404", err)
	}
	if ranked[0].Manifest.ModelID != "withheld-top" {
		t.Errorf("pinned pick = %q, want withheld-top", ranked[0].Manifest.ModelID)
	}
}

// TestManualOnly_EverythingWithheldIsHardwareShaped pins the ERROR KIND,
// which decides whether an install stops or degrades: SelectInstallModel
// turns ErrHardwareInsufficient into "no pick" and anything else into a
// failed install (internal/setup/modelselect.go). Reporting a capability
// failure here would take local inference off the host with a message
// about capabilities nobody asked for.
func TestManualOnly_EverythingWithheldIsHardwareShaped(t *testing.T) {
	in := manualOnlyInput(true)
	for i := range in.Catalog {
		in.Catalog[i].ManualOnly = "test fixture: everything withheld"
	}

	_, err := RankModels(in)
	if !errors.Is(err, ErrHardwareInsufficient) {
		t.Fatalf("RankModels error = %v, want ErrHardwareInsufficient", err)
	}
	if errors.Is(err, ErrCapabilityNotMet) {
		t.Errorf("reported as a capability failure; nothing was asked of capabilities")
	}

	above, ok, err := SelectInstallModel(in)
	if err != nil {
		t.Errorf("SelectInstallModel error = %v, want nil (no pick is not a failure)", err)
	}
	if ok || len(above) != 0 {
		t.Errorf("SelectInstallModel offered %v, want nothing", above)
	}
}

// --- the shipped catalog -------------------------------------------------

// realCatalogHosts spans the bands the bundled catalog serves, on both
// engines. The vLLM rows give the card the same size as system RAM,
// which is how the picker tests elsewhere synthesise a discrete GPU.
func realCatalogHosts() []struct {
	name   string
	hw     hardware.Profile
	engine string
} {
	var out []struct {
		name   string
		hw     hardware.Profile
		engine string
	}
	for _, gb := range []int{4, 8, 16, 24, 32, 48, 64, 96, 128, 192, 256} {
		out = append(out, struct {
			name   string
			hw     hardware.Profile
			engine string
		}{
			name:   "ollama-" + itoa(gb) + "gb",
			hw:     hardware.Profile{OS: "linux", Arch: "x86_64", RAMTotalGB: gb},
			engine: catalog.RuntimeOllama,
		})
		out = append(out, struct {
			name   string
			hw     hardware.Profile
			engine string
		}{
			name: "vllm-" + itoa(gb) + "gb",
			hw: hardware.Profile{OS: "linux", Arch: "x86_64", RAMTotalGB: gb,
				GPUs: []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: gb * 1024}}},
			engine: catalog.RuntimeVLLM,
		})
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestManualOnly_ShippedCatalogNeverOffersGptOss drives the REAL bundled
// catalog, because the property under test is not "RankModels honours
// the field" — the fixture above proves that — but "the manifests we
// ship carry it". A synthetic catalog would pass while gpt-oss went out
// unmarked, which is the whole failure mode (#518, #521).
func TestManualOnly_ShippedCatalogNeverOffersGptOss(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}

	var withheld []string
	for _, m := range manifests {
		if m.ManualOnly != "" {
			withheld = append(withheld, m.ModelID)
		}
	}
	if len(withheld) == 0 {
		t.Fatal("no bundled manifest is marked manual_only; #521 marks both gpt-oss entries")
	}
	for _, want := range []string{"gpt-oss-20b", "gpt-oss-120b"} {
		if !contains(withheld, want) {
			t.Errorf("%s is not marked manual_only; #518 withholds gpt-oss from recommendation", want)
		}
	}

	for _, h := range realCatalogHosts() {
		t.Run(h.name, func(t *testing.T) {
			in := PickInput{Catalog: manifests, Hardware: h.hw, Engine: h.engine,
				EngineVersion: runtime.OllamaPinnedVersion}

			if got := RecommendedFamily(in); contains(withheld, got) {
				t.Errorf("RecommendedFamily = %q, a manual_only model", got)
			}
			if above, ok, err := SelectInstallModel(in); err == nil && ok {
				for _, p := range above {
					if contains(withheld, p.Manifest.ModelID) {
						t.Errorf("SelectInstallModel offered %q, a manual_only model", p.Manifest.ModelID)
					}
				}
			}
			if ranked, err := RankModels(in); err == nil {
				for _, p := range ranked {
					if contains(withheld, p.Manifest.ModelID) {
						t.Errorf("RankModels ranked %q, a manual_only model", p.Manifest.ModelID)
					}
				}
			}
		})
	}
}

// TestManualOnly_NoHostLosesItsPick guards the one way this change could
// take local inference away: a host whose only fitting model is the one
// being withheld. It compares against the same catalog with the field
// cleared rather than asserting a fixed list of hosts, so it keeps
// meaning the same thing as the catalog moves under it.
func TestManualOnly_NoHostLosesItsPick(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	allowed := make([]catalog.Manifest, len(manifests))
	copy(allowed, manifests)
	for i := range allowed {
		allowed[i].ManualOnly = ""
	}

	for _, h := range realCatalogHosts() {
		t.Run(h.name, func(t *testing.T) {
			base := PickInput{Hardware: h.hw, Engine: h.engine, EngineVersion: runtime.OllamaPinnedVersion}
			with, withOK, _ := func() ([]Pick, bool, error) {
				in := base
				in.Catalog = manifests
				return SelectInstallModel(in)
			}()
			without, withoutOK, _ := func() ([]Pick, bool, error) {
				in := base
				in.Catalog = allowed
				return SelectInstallModel(in)
			}()

			if withoutOK && !withOK {
				t.Errorf("this host had a pick before gpt-oss was withheld and has none now — "+
					"withholding a model must never disable local inference (had %d candidates)",
					len(without))
			}
			_ = with
		})
	}
}

// TestManualOnly_StaysListedAndResolvable is the difference from
// internal_only, asserted where it can fail. internal_only removes the
// entry from BundledManifests; this one leaves it there so a person can
// still find it, and leaves every name it answers to working so an
// explicit pin keeps resolving.
func TestManualOnly_StaysListedAndResolvable(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	for _, want := range []string{"gpt-oss-20b", "gpt-oss-120b"} {
		m, ok := catalog.LookupByAlias(want, manifests)
		if !ok {
			t.Errorf("%s is not in the offered catalog; manual_only withholds it from "+
				"automatic CHOICE, not from the catalog (that is internal_only)", want)
			continue
		}
		if m.ManualOnly == "" {
			t.Errorf("%s resolved but carries no manual_only reason", want)
		}
		for _, alias := range m.ModelAliases {
			if _, ok := catalog.LookupByAlias(alias, manifests); !ok {
				t.Errorf("%s: alias %q stopped resolving", want, alias)
			}
		}
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want || strings.EqualFold(x, want) {
			return true
		}
	}
	return false
}
