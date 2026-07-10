package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/router"
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
	old := router.VLLMAutoSelectable
	router.VLLMAutoSelectable = true
	t.Cleanup(func() { router.VLLMAutoSelectable = old })
	inf := &fakeInference{
		hwProfile: hardware.Profile{
			RAMTotalGB: 32,
			GPUs:       []hardware.GPU{{Vendor: "nvidia", Model: "RTX 3060", VRAMTotalMB: 12288}},
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
			RAMTotalGB: 32,
			GPUs:       []hardware.GPU{{Vendor: "nvidia", Model: "RTX 3060", VRAMTotalMB: 12288}},
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
