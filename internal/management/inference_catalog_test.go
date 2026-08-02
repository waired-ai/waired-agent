package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

func catalogFixture() []catalog.Manifest {
	return []catalog.Manifest{
		{
			ModelID: "qwen3-4b-instruct", DisplayName: "Qwen3 4B Instruct",
			Variants: []catalog.Variant{{
				VariantID: "q4-gguf", Format: catalog.FormatOllamaTag,
				RuntimeSupport: []string{catalog.RuntimeOllama},
				MinRAMGB:       8, QualityTier: 35,
				Source: catalog.VariantSource{Type: catalog.SourceOllama, Tag: "qwen3:4b-q4"},
			}},
		},
		{
			ModelID: "qwen3-8b-instruct", DisplayName: "Qwen3 8B Instruct",
			Variants: []catalog.Variant{
				{
					VariantID: "q4-gguf", Format: catalog.FormatOllamaTag,
					RuntimeSupport: []string{catalog.RuntimeOllama},
					MinRAMGB:       12, QualityTier: 50,
					Source: catalog.VariantSource{Type: catalog.SourceOllama, Tag: "qwen3:8b-q4"},
				},
				{
					VariantID: "fp16", Format: catalog.FormatSafetensors,
					DType:          "float16",
					RuntimeSupport: []string{catalog.RuntimeVLLM},
					MinVRAMMB:      18000, QualityTier: 65,
					Source: catalog.VariantSource{Type: catalog.SourceHuggingFace, RepoID: "Qwen/Qwen3-8B"},
				},
			},
		},
		{
			ModelID: "qwen3-32b-instruct", DisplayName: "Qwen3 32B Instruct",
			Variants: []catalog.Variant{{
				VariantID: "awq-int4", Format: catalog.FormatSafetensors,
				Quantization:   "AWQ-int4",
				RuntimeSupport: []string{catalog.RuntimeVLLM},
				MinVRAMMB:      24576, QualityTier: 80,
				Source: catalog.VariantSource{Type: catalog.SourceHuggingFace, RepoID: "Qwen/Qwen3-32B-AWQ"},
			}},
		},
	}
}

func newCatalogTestServer(t *testing.T, inf *fakeInference, prefDir string) *Server {
	t.Helper()
	cfg := &CatalogConfig{
		PreferencePath: filepath.Join(prefDir, "preferred-model.json"),
		ManifestsFn:    func() ([]catalog.Manifest, error) { return catalogFixture(), nil },
		RestartScheduler: func() {
			t.Errorf("RestartScheduler must not fire on GET /catalog")
		},
	}
	return New(stubStatus{}, stubPinger{}).WithInference(inf).WithCatalog(cfg)
}

func doGet(t *testing.T, s *Server, path string) (*httptest.ResponseRecorder, ModelCatalogResponse) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	var got ModelCatalogResponse
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v body=%s", err, w.Body.String())
		}
	}
	return w, got
}

func TestInferenceCatalog_RAMOnlyHost(t *testing.T) {
	inf := &fakeInference{
		hwProfile: hardware.Profile{RAMTotalGB: 32},
		canned:    InferenceStatus{Active: &ActiveSelection{ModelID: "qwen3-4b-instruct", VariantID: "q4-gguf"}},
		models: []ModelEntry{
			{ModelID: "qwen3-4b-instruct", State: catalog.ModelStateReady},
		},
	}
	s := newCatalogTestServer(t, inf, t.TempDir())

	w, got := doGet(t, s, "/waired/v1/inference/catalog")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got.Engine != catalog.RuntimeOllama {
		t.Errorf("engine: want ollama (no GPU), got %q", got.Engine)
	}
	if got.Active == nil || got.Active.ModelID != "qwen3-4b-instruct" {
		t.Errorf("active: %+v", got.Active)
	}
	if got.Active.DisplayName != "Qwen3 4B Instruct" {
		t.Errorf("display_name: %q", got.Active.DisplayName)
	}
	if len(got.Families) != 3 {
		t.Fatalf("families: want 3, got %d", len(got.Families))
	}
	byID := map[string]CatalogFamily{}
	for _, f := range got.Families {
		byID[f.ModelID] = f
	}
	// 4B fits ollama (8 GB ≤ 32 GB), is active + downloaded.
	four := byID["qwen3-4b-instruct"]
	if !four.Fits || !four.Active || !four.Downloaded {
		t.Errorf("4B family: %+v", four)
	}
	if four.BestFitVariantID != "q4-gguf" {
		t.Errorf("4B best-fit variant: %q", four.BestFitVariantID)
	}
	// 8B fits ollama (12 GB ≤ 32 GB), not active.
	eight := byID["qwen3-8b-instruct"]
	if !eight.Fits || eight.Active {
		t.Errorf("8B family: %+v", eight)
	}
	// 32B is vllm-only — engine is ollama → no variant supports.
	thirtytwo := byID["qwen3-32b-instruct"]
	if thirtytwo.Fits {
		t.Errorf("32B should not fit on RAM-only host: %+v", thirtytwo)
	}
	if thirtytwo.DeficitLabel != "no variant supports ollama" {
		t.Errorf("32B deficit: %q", thirtytwo.DeficitLabel)
	}
}

func TestInferenceCatalog_GPUHost_ShortVRAM(t *testing.T) {
	// 12 GB NVIDIA host (≥ the 8 GB vLLM threshold). vLLM auto-selection is
	// gated off while #557 is unwired, so force it on to exercise the
	// vLLM-engine catalog rendering this test covers (vllm-only families and
	// VRAM-based deficit labels). 8B/fp16 needs 18 GB → doesn't fit; 32B AWQ
	// needs 24 GB → doesn't fit.
	//
	// Nothing is committed in state.json here, so the endpoint falls back to
	// the auto-picker: the profile must therefore name a Linux host with a
	// vLLM venv installed, the two facts that fallback now requires
	// (waired-agent#319).
	old := router.VLLMAutoSelectable
	router.VLLMAutoSelectable = true
	t.Cleanup(func() { router.VLLMAutoSelectable = old })
	inf := &fakeInference{
		hwProfile: hardware.Profile{
			OS:         "linux",
			RAMTotalGB: 32,
			GPUs:       []hardware.GPU{{Vendor: "nvidia", Model: "RTX 3060", VRAMTotalMB: 12288}},
			Engines:    hardware.InstalledEngines{VLLM: hardware.EngineInfo{Installed: true}},
		},
	}
	s := newCatalogTestServer(t, inf, t.TempDir())

	w, got := doGet(t, s, "/waired/v1/inference/catalog")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got.Engine != catalog.RuntimeVLLM {
		t.Fatalf("engine: want vllm (12 GB ≥ 8 GB threshold), got %q", got.Engine)
	}
	if got.Host.VRAMTotalMB != 12288 || got.Host.GPUModel != "RTX 3060" {
		t.Errorf("host: %+v", got.Host)
	}
	byID := map[string]CatalogFamily{}
	for _, f := range got.Families {
		byID[f.ModelID] = f
	}
	// 4B is ollama-only → on vllm engine: no variant supports.
	if byID["qwen3-4b-instruct"].DeficitLabel != "no variant supports vllm" {
		t.Errorf("4B deficit: %q", byID["qwen3-4b-instruct"].DeficitLabel)
	}
	// 8B has fp16 (18 GB) — doesn't fit on 12 GB GPU.
	eight := byID["qwen3-8b-instruct"]
	if eight.Fits {
		t.Errorf("8B should not fit on 12 GB GPU: %+v", eight)
	}
	if eight.DeficitLabel != "needs 18 GB VRAM (have 12 GB)" {
		t.Errorf("8B deficit: %q", eight.DeficitLabel)
	}
	// 32B AWQ needs 24 GB → doesn't fit.
	thirtytwo := byID["qwen3-32b-instruct"]
	if thirtytwo.DeficitLabel != "needs 24 GB VRAM (have 12 GB)" {
		t.Errorf("32B deficit: %q", thirtytwo.DeficitLabel)
	}
}

func TestInferenceCatalog_RecommendedSpecs(t *testing.T) {
	// RAM-only host (ollama engine): fitting families carry the best-fit
	// variant's RAM spec; the vllm-only family has no engine-supported
	// variant so Recommended stays nil.
	t.Run("ollama host", func(t *testing.T) {
		inf := &fakeInference{hwProfile: hardware.Profile{RAMTotalGB: 32}}
		s := newCatalogTestServer(t, inf, t.TempDir())
		_, got := doGet(t, s, "/waired/v1/inference/catalog")
		byID := map[string]CatalogFamily{}
		for _, f := range got.Families {
			byID[f.ModelID] = f
		}
		four := byID["qwen3-4b-instruct"].Recommended
		if four == nil || four.VariantID != "q4-gguf" || four.MinRAMGB != 8 || four.QualityTier != 35 {
			t.Errorf("4B recommended: %+v", four)
		}
		if four != nil && four.MinVRAMMB != 0 {
			t.Errorf("4B recommended should not carry VRAM on ollama: %+v", four)
		}
		if r := byID["qwen3-32b-instruct"].Recommended; r != nil {
			t.Errorf("vllm-only family on ollama host should have nil recommended, got %+v", r)
		}
	})

	// GPU host (vllm engine): even over-capacity families expose the
	// representative variant's VRAM spec so the UI can show what it wants.
	// vLLM auto-selection is gated off while #557 is unwired, so force it on
	// to keep exercising the vLLM-engine rendering this subtest covers.
	t.Run("vllm host over-capacity", func(t *testing.T) {
		old := router.VLLMAutoSelectable
		router.VLLMAutoSelectable = true
		t.Cleanup(func() { router.VLLMAutoSelectable = old })
		inf := &fakeInference{hwProfile: hardware.Profile{
			OS:         "linux",
			RAMTotalGB: 32,
			GPUs:       []hardware.GPU{{Vendor: "nvidia", Model: "RTX 3060", VRAMTotalMB: 12288}},
			Engines:    hardware.InstalledEngines{VLLM: hardware.EngineInfo{Installed: true}},
		}}
		s := newCatalogTestServer(t, inf, t.TempDir())
		_, got := doGet(t, s, "/waired/v1/inference/catalog")
		byID := map[string]CatalogFamily{}
		for _, f := range got.Families {
			byID[f.ModelID] = f
		}
		eight := byID["qwen3-8b-instruct"]
		if eight.Fits {
			t.Fatalf("8B should not fit on 12 GB GPU: %+v", eight)
		}
		if eight.Recommended == nil || eight.Recommended.MinVRAMMB != 18000 || eight.Recommended.VariantID != "fp16" {
			t.Errorf("8B recommended (no-fit representative): %+v", eight.Recommended)
		}
	})
}

func TestInferenceCatalog_PreferredModelMarked(t *testing.T) {
	prefDir := t.TempDir()
	prefPath := filepath.Join(prefDir, "preferred-model.json")
	if err := agentconfig.SavePreference(prefPath, agentconfig.Preference{ModelID: "qwen3-8b-instruct"}); err != nil {
		t.Fatalf("save preference: %v", err)
	}
	inf := &fakeInference{
		hwProfile: hardware.Profile{RAMTotalGB: 32},
		canned:    InferenceStatus{Active: &ActiveSelection{ModelID: "qwen3-4b-instruct", VariantID: "q4-gguf"}},
	}
	s := newCatalogTestServer(t, inf, prefDir)

	w, got := doGet(t, s, "/waired/v1/inference/catalog")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	if got.PreferredModelID != "qwen3-8b-instruct" {
		t.Errorf("preferred_model_id: %q", got.PreferredModelID)
	}
	byID := map[string]CatalogFamily{}
	for _, f := range got.Families {
		byID[f.ModelID] = f
	}
	if !byID["qwen3-8b-instruct"].Preferred {
		t.Errorf("8B should be marked preferred: %+v", byID["qwen3-8b-instruct"])
	}
	if byID["qwen3-4b-instruct"].Preferred {
		t.Errorf("4B should not be marked preferred: %+v", byID["qwen3-4b-instruct"])
	}
	// active and preferred can differ (mid-restart scenario).
	if !byID["qwen3-4b-instruct"].Active {
		t.Errorf("4B should still be active until restart")
	}
}

func TestInferenceCatalog_DownloadingFamilyAnnotated(t *testing.T) {
	inf := &fakeInference{
		hwProfile: hardware.Profile{RAMTotalGB: 32},
		models: []ModelEntry{
			{ModelID: "qwen3-8b-instruct", State: catalog.ModelStateDownloading},
		},
	}
	s := newCatalogTestServer(t, inf, t.TempDir())
	w, got := doGet(t, s, "/waired/v1/inference/catalog")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	for _, f := range got.Families {
		if f.ModelID == "qwen3-8b-instruct" {
			if !f.Downloading || f.Downloaded {
				t.Errorf("8B should be downloading + not downloaded: %+v", f)
			}
		}
	}
}

func TestInferenceCatalog_NotConfiguredReturns404(t *testing.T) {
	inf := &fakeInference{}
	// No WithCatalog → endpoint should not be mounted.
	s := New(stubStatus{}, stubPinger{}).WithInference(inf)
	r := httptest.NewRequest(http.MethodGet, "/waired/v1/inference/catalog", nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 when catalog is unconfigured, got %d", w.Code)
	}
}

func TestInferenceCatalog_MethodNotAllowed(t *testing.T) {
	inf := &fakeInference{}
	s := newCatalogTestServer(t, inf, t.TempDir())
	r := httptest.NewRequest(http.MethodPost, "/waired/v1/inference/catalog", nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 for POST, got %d", w.Code)
	}
}

// The waired-agent#319 regression, reproduced end to end: a Windows host with
// a 16 GB RTX 5080 whose daemon serves Ollama. The endpoint used to recompute
// the engine from hardware alone and answered "vllm", so every ollama-only
// family — including the one actually serving — rendered as
// "no variant supports vllm".
//
// Product contract: the catalog is judged against the SERVING engine.
func TestInferenceCatalog_WindowsNVIDIAHostServingOllama(t *testing.T) {
	old := router.VLLMAutoSelectable
	router.VLLMAutoSelectable = true
	t.Cleanup(func() { router.VLLMAutoSelectable = old })
	inf := &fakeInference{
		hwProfile: hardware.Profile{
			OS:         "windows",
			RAMTotalGB: 64,
			GPUs:       []hardware.GPU{{Vendor: "nvidia", Model: "RTX 5080", VRAMTotalMB: 16303}},
		},
		canned: InferenceStatus{Active: &ActiveSelection{
			Runtime: catalog.RuntimeOllama,
			ModelID: "qwen3-4b-instruct", VariantID: "q4-gguf",
		}},
	}
	s := newCatalogTestServer(t, inf, t.TempDir())

	w, got := doGet(t, s, "/waired/v1/inference/catalog")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got.Engine != catalog.RuntimeOllama {
		t.Fatalf("engine: want ollama (the daemon serves it), got %q", got.Engine)
	}
	byID := map[string]CatalogFamily{}
	for _, f := range got.Families {
		byID[f.ModelID] = f
	}
	// The ollama-only families must be judged against ollama and fit a
	// 64 GB host — not grayed out as unsupported.
	for _, id := range []string{"qwen3-4b-instruct", "qwen3-8b-instruct"} {
		if f := byID[id]; !f.Fits {
			t.Errorf("%s should fit on an ollama host: %+v", id, f)
		}
	}
	for _, f := range got.Families {
		if strings.Contains(f.DeficitLabel, "vllm") {
			t.Errorf("no row may be judged against vllm on this host: %s → %q",
				f.ModelID, f.DeficitLabel)
		}
	}
}

// The mirror case: a Linux host actually serving vLLM keeps being judged
// against vllm. Guards against "fix the Windows symptom by hardcoding ollama".
func TestInferenceCatalog_LinuxHostServingVLLM(t *testing.T) {
	inf := &fakeInference{
		hwProfile: hardware.Profile{
			OS:         "linux",
			RAMTotalGB: 64,
			GPUs:       []hardware.GPU{{Vendor: "nvidia", Model: "RTX 5090", VRAMTotalMB: 32607}},
			Engines:    hardware.InstalledEngines{VLLM: hardware.EngineInfo{Installed: true}},
		},
		canned: InferenceStatus{Active: &ActiveSelection{
			Runtime: catalog.RuntimeVLLM,
			ModelID: "qwen3-32b-instruct", VariantID: "awq-int4",
		}},
	}
	s := newCatalogTestServer(t, inf, t.TempDir())

	_, got := doGet(t, s, "/waired/v1/inference/catalog")
	if got.Engine != catalog.RuntimeVLLM {
		t.Fatalf("engine: want vllm (the daemon serves it), got %q", got.Engine)
	}
	byID := map[string]CatalogFamily{}
	for _, f := range got.Families {
		byID[f.ModelID] = f
	}
	if !byID["qwen3-32b-instruct"].Fits {
		t.Errorf("32B AWQ should fit a 24 GB vllm host: %+v", byID["qwen3-32b-instruct"])
	}
	if byID["qwen3-4b-instruct"].DeficitLabel != "no variant supports vllm" {
		t.Errorf("ollama-only family on a vllm host: %q", byID["qwen3-4b-instruct"].DeficitLabel)
	}
}

// Pre-commit fallback (nothing in state.json yet). Product contract: a Linux
// host large enough for vLLM but WITHOUT a venv will serve ollama, so the
// catalog must say ollama too — the picker's preference is demoted by the
// same presence fact engineViable consults. This is the Linux half of #319,
// which an OS gate alone would not cover.
func TestInferenceCatalog_NoActiveYet_VLLMDemotedWithoutVenv(t *testing.T) {
	old := router.VLLMAutoSelectable
	router.VLLMAutoSelectable = true
	t.Cleanup(func() { router.VLLMAutoSelectable = old })

	hw := func(vllmInstalled bool) hardware.Profile {
		return hardware.Profile{
			OS:         "linux",
			RAMTotalGB: 64,
			GPUs:       []hardware.GPU{{Vendor: "nvidia", Model: "RTX 4090", VRAMTotalMB: 24564}},
			Engines:    hardware.InstalledEngines{VLLM: hardware.EngineInfo{Installed: vllmInstalled}},
		}
	}

	t.Run("no venv", func(t *testing.T) {
		s := newCatalogTestServer(t, &fakeInference{hwProfile: hw(false)}, t.TempDir())
		_, got := doGet(t, s, "/waired/v1/inference/catalog")
		if got.Engine != catalog.RuntimeOllama {
			t.Errorf("engine: want ollama (no vllm venv on this host), got %q", got.Engine)
		}
	})
	t.Run("venv installed", func(t *testing.T) {
		s := newCatalogTestServer(t, &fakeInference{hwProfile: hw(true)}, t.TempDir())
		_, got := doGet(t, s, "/waired/v1/inference/catalog")
		if got.Engine != catalog.RuntimeVLLM {
			t.Errorf("engine: want vllm (venv present, Linux, 24 GB), got %q", got.Engine)
		}
	})
}

// Product contract (waired-agent#321): every row carries the shared
// projection, and the projection agrees with the legacy Fits bit. The
// tray, `waired models ls --detail` and the setup wizard all render this
// one shape; a row without it would silently degrade to the pre-#321
// display on whichever surface reached it first.
func TestInferenceCatalog_EveryRowCarriesTheSharedProjection(t *testing.T) {
	inf := &fakeInference{
		hwProfile: hardware.Profile{RAMTotalGB: 32},
		canned:    InferenceStatus{Active: &ActiveSelection{ModelID: "qwen3-4b-instruct", VariantID: "q4-gguf"}},
	}
	_, got := doGet(t, newCatalogTestServer(t, inf, t.TempDir()), "/waired/v1/inference/catalog")
	if len(got.Families) == 0 {
		t.Fatal("no families")
	}
	for _, f := range got.Families {
		if f.Fit == nil {
			t.Fatalf("%s carries no fit projection", f.ModelID)
		}
		if f.Fit.Runnable != f.Fits {
			t.Errorf("%s: fit.runnable=%v but fits=%v", f.ModelID, f.Fit.Runnable, f.Fits)
		}
		if f.Fit.QualityTier == 0 {
			t.Errorf("%s: quality tier is 0 — it ranks the model, so it is true even for a row that cannot run",
				f.ModelID)
		}
	}
	// The vllm-only family on an ollama host is the F36 row: present,
	// not runnable, and carrying a code a UI can word for itself rather
	// than the wire's "no variant supports ollama".
	for _, f := range got.Families {
		if f.ModelID == "qwen3-32b-instruct" {
			if f.Fit.Reason != hostfit.ReasonNoVariantForEngine {
				t.Errorf("32B fit reason: %q, want %q", f.Fit.Reason, hostfit.ReasonNoVariantForEngine)
			}
		}
	}
}

// Product contract: at most one row is the host's own pick, and it is one
// that actually runs. A badge on an unrunnable row would be a suggestion
// the operator cannot take.
func TestInferenceCatalog_MarksExactlyOneRecommendedPick(t *testing.T) {
	inf := &fakeInference{hwProfile: hardware.Profile{RAMTotalGB: 32}}
	_, got := doGet(t, newCatalogTestServer(t, inf, t.TempDir()), "/waired/v1/inference/catalog")

	var marked []string
	for _, f := range got.Families {
		if f.RecommendedPick {
			marked = append(marked, f.ModelID)
			if !f.Fits {
				t.Errorf("%s is marked recommended but does not run here", f.ModelID)
			}
		}
	}
	if len(marked) != 1 {
		t.Fatalf("recommended rows: %v, want exactly 1", marked)
	}
	// The 8B is the higher tier of the two ollama families that fit 32 GB.
	if marked[0] != "qwen3-8b-instruct" {
		t.Errorf("recommended = %q, want qwen3-8b-instruct (highest tier that fits)", marked[0])
	}
	if want := router.RecommendedFamily(router.PickInput{
		Catalog: catalogFixture(), Hardware: inf.hwProfile, Engine: catalog.RuntimeOllama,
	}); marked[0] != want {
		t.Errorf("endpoint marked %q, router would pick %q — two policies", marked[0], want)
	}
}
