package management

import (
	"net/http"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/router"
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
	// channel here.
	RestartScheduler func()

	// ManifestsFn returns the bundled manifests. nil falls back to
	// catalog.BundledManifests. Tests inject a synthetic catalog.
	ManifestsFn func() ([]catalog.Manifest, error)
}

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
	DeficitLabel     string `json:"deficit_label,omitempty"`

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
	enginePick, err := router.PickEngine(router.EnginePickInput{Hardware: hw})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody("engine_pick_failed", err.Error()))
		return
	}

	status := s.inference.Status(r.Context())
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
		Engine:                  enginePick.Engine,
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
	if rt, ok := status.Runtimes[enginePick.Engine]; ok {
		engineVersion = rt.LiveVersion
		if engineVersion == "" {
			engineVersion = rt.Version
		}
	}

	for _, m := range manifests {
		fit := router.FamilyBestFit(m, enginePick.Engine, engineVersion, hw)
		f := CatalogFamily{
			ModelID:     m.ModelID,
			DisplayName: m.DisplayName,
			Fits:        fit.Fits,
			Active:      m.ModelID == activeModelID,
			Preferred:   pref.ModelID != "" && m.ModelID == pref.ModelID,
			Downloaded:  downloaded[m.ModelID],
			Downloading: downloading[m.ModelID],
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

func (s *Server) loadManifests() ([]catalog.Manifest, error) {
	if s.catalog != nil && s.catalog.ManifestsFn != nil {
		return s.catalog.ManifestsFn()
	}
	return catalog.BundledManifests()
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
