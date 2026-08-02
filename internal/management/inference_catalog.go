package management

import (
	"context"
	"errors"
	"net/http"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// CatalogConfig wires the dependencies for the model-catalog endpoints
// (GET /waired/v1/inference/catalog and POST /waired/v1/inference/preferred-model).
// Provided by the agent's main.go via WithCatalog; nil keeps both
// endpoints unmounted so the tray sees a clean 404 on older builds.
type CatalogConfig struct {
	// PreferencePath is the on-disk preferred-model.json. Empty
	// disables the catalog endpoints.
	PreferencePath string

	// RestartScheduler is invoked by /preferred-model after the response
	// is sent. nil falls back to the default SIGTERM-to-self behaviour
	// (defined where the Server is constructed). Tests inject a counter
	// channel here. Since #812 this is the FALLBACK path (wedged engine /
	// cross-engine / unenrolled); the common case applies the switch in
	// process via ApplyModelSwitch with no restart.
	RestartScheduler func()

	// ApplyModelSwitch applies an operator's preferred-model switch in
	// process (#812) — no whole-agent restart — and reports whether a
	// background pull was started. nil (or a non-nil error return) makes
	// /preferred-model fall back to RestartScheduler and answer
	// WillRestart:true; a nil error means the switch is applying live and
	// the response carries WillRestart:false. The one error the fallback
	// is wrong for is ErrModelSwitchUnavailable — see below. Tests inject
	// a stub here.
	ApplyModelSwitch func(ctx context.Context, modelID string) (downloading bool, err error)

	// ManifestsFn returns the bundled manifests. nil falls back to
	// catalog.BundledManifests. Tests inject a synthetic catalog.
	ManifestsFn func() ([]catalog.Manifest, error)
}

// ErrModelSwitchUnavailable is ApplyModelSwitch reporting that the
// switch did not happen AND that restarting would not make it happen:
// this host declined to fetch the weights (pulls turned off, a state
// store that could not be written).
//
// It exists because the restart fallback is the wrong answer for
// exactly this case, and the handler cannot tell it from the case the
// fallback is right for ("this daemon cannot apply it in process") by
// looking at an ordinary error. Restarting would bounce the whole agent
// — management API, gateway, and mesh — to re-run a bootstrap that
// fails for the same reason it just failed here.
//
// Implementations wrap it around the cause rather than replacing the
// cause, so a caller that classifies the failure still sees what went
// wrong underneath (waired-agent#257).
var ErrModelSwitchUnavailable = errors.New("management: this host cannot apply that model switch")

// ModelCatalogResponse is the body of GET /waired/v1/inference/catalog.
//
// It is the only payload the tray consumes for the catalog submenu —
// the tray does not call /inference/status or /models in addition.
// Designed so a single poll fully describes "what the agent serves
// today, what the user picked, what each family looks like on this
// host".
type ModelCatalogResponse struct {
	// Active mirrors the catalog state (currently running). nil when
	// the agent has not committed a selection yet.
	Active *CatalogActive `json:"active,omitempty"`

	// PreferredModelID is the user's persisted choice from preferred-model.json.
	// Empty when no manual selection has been made.
	PreferredModelID string `json:"preferred_model_id,omitempty"`

	// Engine is the auto-detected engine for this host (vllm or ollama).
	// The tray uses this only for diagnostic display; fit calculations
	// happen server-side.
	Engine string `json:"engine,omitempty"`

	Host     CatalogHost     `json:"host"`
	Families []CatalogFamily `json:"families"`

	// BenchmarkRecommendation mirrors InferenceStatus.BenchmarkRecommendation
	// so the tray's single catalog poll learns about a pending #133
	// step-down suggestion without a second round-trip. nil when none.
	// Lighter direction only — see InferenceStatus for why upgrades
	// travel separately.
	BenchmarkRecommendation *BenchmarkRecommendation `json:"benchmark_recommendation,omitempty"`

	// BenchmarkUpgrade mirrors InferenceStatus.BenchmarkUpgrade (the
	// headroom-driven higher-tier suggestion). nil when none.
	BenchmarkUpgrade *BenchmarkRecommendation `json:"benchmark_upgrade,omitempty"`
}

// CatalogActive mirrors the relevant fields from catalog.ActiveSelection
// plus the manifest's display_name for ergonomic tray rendering.
type CatalogActive struct {
	ModelID     string `json:"model_id"`
	VariantID   string `json:"variant_id,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
}

// CatalogHost summarises the relevant host capacity for the deficit
// labels the tray renders next to over-capacity rows.
type CatalogHost struct {
	RAMTotalGB  int    `json:"ram_total_gb"`
	VRAMTotalMB int    `json:"vram_total_mb,omitempty"`
	GPUModel    string `json:"gpu_model,omitempty"`
}

// CatalogFamily is one row in the tray's catalog submenu. Tray-side
// rendering walks the slice in order and applies annotations
// (active / preferred / downloading / deficit) without re-evaluating
// fit logic.
type CatalogFamily struct {
	ModelID          string `json:"model_id"`
	DisplayName      string `json:"display_name,omitempty"`
	BestFitVariantID string `json:"best_fit_variant_id,omitempty"`
	Fits             bool   `json:"fits"`
	Active           bool   `json:"active,omitempty"`
	Preferred        bool   `json:"preferred,omitempty"`
	Downloaded       bool   `json:"downloaded,omitempty"`
	Downloading      bool   `json:"downloading,omitempty"`

	// DeficitLabel is English prose composed HERE, which is what
	// waired-agent#321 is unwinding: the control plane emits machine
	// codes and lets the UI own every word, and the tray's wire did the
	// opposite, so one catalog looked like two depending on which picker
	// you opened.
	//
	// Kept beside Fit rather than removed. It is additive-only discipline
	// for a tray that predates Fit, and it is still the ONLY answer for
	// the engine-version floor — hostfit does not model that, so Fit has
	// no code for it. A renderer reads Fit.Reason first and falls back
	// here.
	DeficitLabel string `json:"deficit_label,omitempty"`

	// Fit is the shared projection (proto/hostfit.Presentation) — the
	// same shape and the same JSON names the control plane's onboarding
	// catalog emits. It carries what this wire could not say: a machine
	// reason code, the honest required-VRAM figure for a row that RUNS,
	// the quality tier, and the "runs but is not the right choice here"
	// demotion (waired-ai/waired#988) that had no way to reach a user at
	// all.
	//
	// A pointer so an older consumer sees the field absent rather than a
	// zero value it would read as "nothing fits".
	Fit *hostfit.Presentation `json:"fit,omitempty"`

	// RecommendedPick marks the one family this host would choose for
	// ITSELF. It is router.SelectInstallModel's answer — the same
	// function the installer commits to — so the badge cannot drift from
	// the machine's own pick.
	//
	// At most one row carries it. Absent on every row only when nothing
	// fits this host at all.
	//
	// Not spelled `recommended`: that name is taken below by the
	// recommended-SPECS projection, which shipped tray and CLI builds
	// already decode. Two meanings of one word is worse than a longer
	// name for the newer one.
	RecommendedPick bool `json:"recommended_pick,omitempty"`

	// Recommended carries the recommended specs of the family's
	// representative variant on this host — the best-fit variant when
	// Fits=true, else the least-demanding engine-supported variant the
	// deficit is measured against. Lets the CLI / tray show explicit
	// min RAM/VRAM, quality tier, and parameter counts from the single
	// catalog poll. nil only when no variant supports the host's engine.
	Recommended *CatalogSpec `json:"recommended,omitempty"`
}

// CatalogSpec is the recommended-spec projection of one variant, shared
// by the CLI's `models ls --detail` view and the tray's Models submenu
// so both render the same numbers without re-reading the manifests.
type CatalogSpec struct {
	VariantID    string `json:"variant_id,omitempty"`
	Quantization string `json:"quantization,omitempty"`
	MinRAMGB     int    `json:"min_ram_gb,omitempty"`
	MinVRAMMB    int    `json:"min_vram_mb,omitempty"`
	QualityTier  int    `json:"quality_tier,omitempty"`
	ParamCount   int64  `json:"param_count,omitempty"`
	ActiveParams int64  `json:"active_params,omitempty"`
}

// WithCatalog attaches a CatalogConfig so the server exposes
// GET /waired/v1/inference/catalog and POST /waired/v1/inference/preferred-model.
// Pass nil-valued config to disable.
func (s *Server) WithCatalog(c *CatalogConfig) *Server {
	s.catalog = c
	return s
}

func (s *Server) handleInferenceCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody("method_not_allowed", "GET only"))
		return
	}
	if s.inference == nil || s.catalog == nil || s.catalog.PreferencePath == "" {
		http.Error(w, "catalog not configured", http.StatusNotFound)
		return
	}

	manifests, err := s.loadManifests()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody("manifest_load_failed", err.Error()))
		return
	}

	hw := s.inference.Hardware(r.Context())
	status := s.inference.Status(r.Context())
	engine, err := catalogEngine(status, hw)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody("engine_pick_failed", err.Error()))
		return
	}

	models := s.inference.ListModels(r.Context())

	pref, _, _ := agentconfig.LoadPreference(s.catalog.PreferencePath)

	downloaded := make(map[string]bool, len(models))
	downloading := make(map[string]bool, len(models))
	for _, m := range models {
		switch m.State {
		case catalog.ModelStateReady:
			downloaded[m.ModelID] = true
		case catalog.ModelStateQueued, catalog.ModelStateDownloading, catalog.ModelStateVerifying:
			downloading[m.ModelID] = true
		}
	}

	resp := ModelCatalogResponse{
		PreferredModelID:        pref.ModelID,
		Engine:                  engine,
		Host:                    hostFromProfile(hw),
		Families:                make([]CatalogFamily, 0, len(manifests)),
		BenchmarkRecommendation: status.BenchmarkRecommendation,
		BenchmarkUpgrade:        status.BenchmarkUpgrade,
	}

	var activeModelID string
	if status.Active != nil {
		activeModelID = status.Active.ModelID
		resp.Active = &CatalogActive{
			ModelID:     status.Active.ModelID,
			VariantID:   status.Active.VariantID,
			DisplayName: displayNameFor(manifests, status.Active.ModelID),
		}
	}

	// Serving-engine version for the per-variant MinEngineVersion gate:
	// live /api/version when the engine has been ready once, else the
	// boot-time binary probe. Unknown ("") excludes floored variants
	// (fail closed).
	engineVersion := ""
	if rt, ok := status.Runtimes[engine]; ok {
		engineVersion = rt.LiveVersion
		if engineVersion == "" {
			engineVersion = rt.Version
		}
	}

	// The host's own pick, resolved ONCE for the whole catalog: it is a
	// property of the list, not of a row, and asking per family would
	// re-rank the catalog for every manifest in it.
	recommendedID := router.RecommendedFamily(router.PickInput{
		Catalog:       manifests,
		Hardware:      hw,
		Engine:        engine,
		EngineVersion: engineVersion,
	})

	for _, m := range manifests {
		fit := router.FamilyBestFit(m, engine, engineVersion, hw)
		presentation := fit.Fit
		f := CatalogFamily{
			ModelID:         m.ModelID,
			DisplayName:     m.DisplayName,
			Fits:            fit.Fits,
			Active:          m.ModelID == activeModelID,
			Preferred:       pref.ModelID != "" && m.ModelID == pref.ModelID,
			Downloaded:      downloaded[m.ModelID],
			Downloading:     downloading[m.ModelID],
			Fit:             &presentation,
			RecommendedPick: recommendedID != "" && m.ModelID == recommendedID,
		}
		if fit.Fits {
			f.BestFitVariantID = fit.Variant.VariantID
		} else {
			f.DeficitLabel = fit.DeficitLabel
		}
		// Recommended specs come from the representative variant
		// (best-fit when it fits, else the deficit's reference variant).
		// VariantID is empty only when no variant supports the engine.
		if fit.Variant.VariantID != "" {
			f.Recommended = &CatalogSpec{
				VariantID:    fit.Variant.VariantID,
				Quantization: fit.Variant.Quantization,
				MinRAMGB:     fit.Variant.MinRAMGB,
				MinVRAMMB:    fit.Variant.MinVRAMMB,
				QualityTier:  fit.Variant.QualityTier,
				ParamCount:   fit.Variant.ParamCount,
				ActiveParams: fit.Variant.ActiveParams,
			}
		}
		resp.Families = append(resp.Families, f)
	}

	writeJSON(w, http.StatusOK, resp)
}

// catalogEngine resolves the engine the catalog must judge families
// against: the one this host actually SERVES, not the one its hardware
// would justify buying.
//
// The endpoint used to recompute the engine from hardware alone
// (router.PickEngine). On a host whose daemon serves ollama but whose
// GPU votes vllm, that made every ollama-only family — including the
// one currently serving — render as "no variant supports vllm"
// (waired-agent#319).
//
// Resolution order:
//
//  1. status.Active.Runtime — the committed selection in state.json, i.e.
//     the outcome of chooseEngine/engineViable. Authoritative, and the
//     same source upgradeFromBench already prefers for the same reason.
//  2. Nothing committed yet (fresh install, pre-bootstrap): fall back to
//     the auto-picker, which since waired-agent#319 also refuses vllm on
//     a non-Linux host. A vllm answer is additionally demoted when no
//     venv is installed here — hardware.Profile.Engines resolves through
//     the daemon's injected engineVersionOnHost, so it reports the same
//     presence engineViable would, and a Linux host with a big NVIDIA card
//     and no venv will serve ollama regardless of what the picker prefers.
func catalogEngine(status InferenceStatus, hw hardware.Profile) (string, error) {
	if status.Active != nil && status.Active.Runtime != "" {
		return status.Active.Runtime, nil
	}
	pick, err := router.PickEngine(router.EnginePickInput{Hardware: hw})
	if err != nil {
		return "", err
	}
	if pick.Engine == catalog.RuntimeVLLM && !hw.Engines.VLLM.Installed {
		return catalog.RuntimeOllama, nil
	}
	return pick.Engine, nil
}

// loadManifests returns the models this device may OFFER — the list the
// tray renders and a person browses. Withheld entries are absent.
func (s *Server) loadManifests() ([]catalog.Manifest, error) {
	if s.catalog != nil && s.catalog.ManifestsFn != nil {
		return s.catalog.ManifestsFn()
	}
	return catalog.BundledManifests()
}

// loadManifestsForResolve returns every shipped model, including
// withheld ones.
//
// Listing and naming are different questions. A browsable catalog must
// not show a model nobody should be given; resolving a model_id somebody
// has explicitly asked for must still find it, or an operator who pins
// one gets "no bundled manifest with that model_id" for a model this
// build ships. That is not hypothetical — it is how the routing
// sentinel's pin reaches the daemon, through `waired init`'s
// select-model step.
//
// The ManifestsFn seam is honoured either way: a caller that injects its
// own list has already decided what this server may see.
func (s *Server) loadManifestsForResolve() ([]catalog.Manifest, error) {
	if s.catalog != nil && s.catalog.ManifestsFn != nil {
		return s.catalog.ManifestsFn()
	}
	return catalog.BundledManifestsIncludingInternal()
}

func hostFromProfile(hw hardware.Profile) CatalogHost {
	host := CatalogHost{RAMTotalGB: hw.RAMTotalGB}
	if len(hw.GPUs) > 0 {
		host.VRAMTotalMB = hw.GPUs[0].VRAMTotalMB
		host.GPUModel = hw.GPUs[0].Model
	}
	return host
}

func displayNameFor(manifests []catalog.Manifest, modelID string) string {
	for _, m := range manifests {
		if m.ModelID == modelID {
			return m.DisplayName
		}
	}
	return ""
}
