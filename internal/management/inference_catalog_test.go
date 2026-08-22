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
	if thirtytwo.DeficitLabel != "no Ollama variant" {
		t.Errorf("32B deficit: %q", thirtytwo.DeficitLabel)
	}
}

func TestInferenceCatalog_GPUHost_ShortVRAM(t *testing.T) {
	// 12 GB NVIDIA host. The subject is the vLLM-engine catalog rendering —
	// vllm-only families and VRAM-based deficit labels — so the engine has to
	// be vllm: 8B/fp16 needs 18 GB → doesn't fit; 32B AWQ needs 24 GB →
	// doesn't fit.
	//
	// The engine is COMMITTED here rather than auto-picked. Since #522 the
	// auto-picker will not name an engine no catalog variant fits, and on
	// this fixture nothing vllm does — so the fallback path answers ollama
	// and this rendering would never be exercised. A committed
	// Active.Runtime is how a real device reaches this state: the daemon
	// chose vllm when something fitted, and the hardware or the catalog
	// moved underneath it. TestInferenceCatalog_EngineFallsBackWhenNothingFitsVLLM
	// covers the auto-picked half.
	inf := &fakeInference{
		hwProfile: hardware.Profile{
			OS:         "linux",
			RAMTotalGB: 32,
			GPUs:       []hardware.GPU{{Vendor: "nvidia", Model: "RTX 3060", VRAMTotalMB: 12288}},
			Engines:    hardware.InstalledEngines{VLLM: hardware.EngineInfo{Installed: true}},
		},
		canned: InferenceStatus{Active: &ActiveSelection{Runtime: catalog.RuntimeVLLM}},
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
	if byID["qwen3-4b-instruct"].DeficitLabel != "no vLLM variant" {
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

// The catalog endpoint and the install pick must judge models against the
// same engine. On a host that qualifies for vLLM on hardware but has no
// vllm variant it can fit, the auto-picker answers ollama, so this endpoint
// renders the ollama view — the one the device will actually install
// against.
//
// Product contract, ratified in waired-agent#522 (owner decision
// 2026-08-08): the engine auto-pick requires a model the engine can serve.
// Before it, this endpoint reported vllm and rendered every family as
// not-fitting while `waired init` on the same machine reported below the
// recommended spec.
//
// The fixture is the pre-existing 12 GB RTX 3060 one: its two vllm variants
// need 18 GB and 24 GB.
func TestInferenceCatalog_EngineFallsBackWhenNothingFitsVLLM(t *testing.T) {
	inf := &fakeInference{hwProfile: hardware.Profile{
		OS:         "linux",
		RAMTotalGB: 32,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", Model: "RTX 3060", VRAMTotalMB: 12288}},
		Engines:    hardware.InstalledEngines{VLLM: hardware.EngineInfo{Installed: true}},
	}}
	s := newCatalogTestServer(t, inf, t.TempDir())

	w, got := doGet(t, s, "/waired/v1/inference/catalog")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got.Engine != catalog.RuntimeOllama {
		t.Fatalf("engine = %q, want ollama: the host clears the %d MB vLLM VRAM "+
			"threshold but no vllm variant in the catalog fits 12288 MB",
			got.Engine, router.MinVLLMVRAMMB)
	}
	// And the fallback is worth something: the ollama view has models.
	byID := map[string]CatalogFamily{}
	for _, f := range got.Families {
		byID[f.ModelID] = f
	}
	if !byID["qwen3-8b-instruct"].Fits {
		t.Errorf("8B should fit the ollama view on a 32 GB host: %+v", byID["qwen3-8b-instruct"])
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
	// The engine is committed rather than auto-picked for the reason given
	// on TestInferenceCatalog_GPUHost_ShortVRAM — since #522 the auto-picker
	// will not name vllm on a host where no vllm variant fits, which is
	// exactly the over-capacity fixture this subtest needs.
	t.Run("vllm host over-capacity", func(t *testing.T) {
		inf := &fakeInference{
			hwProfile: hardware.Profile{
				OS:         "linux",
				RAMTotalGB: 32,
				GPUs:       []hardware.GPU{{Vendor: "nvidia", Model: "RTX 3060", VRAMTotalMB: 12288}},
				Engines:    hardware.InstalledEngines{VLLM: hardware.EngineInfo{Installed: true}},
			},
			canned: InferenceStatus{Active: &ActiveSelection{Runtime: catalog.RuntimeVLLM}},
		}
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

// PRODUCT CONTRACT (waired-agent#627): the catalog reports whether a
// PERSON at this machine answered the model question, separately from
// whether a preference exists. The install picker keys on it, and reading
// the preference's presence instead is how an instruction the setup path
// applied silently deleted the picker from a first install.
func TestInferenceCatalog_ModelQuestionAnsweredFollowsProvenance(t *testing.T) {
	for _, tc := range []struct {
		name string
		pref agentconfig.Preference
		want bool
	}{
		{"a person picked a model here", agentconfig.Preference{
			ModelID: "qwen3-8b-instruct", Source: agentconfig.PreferenceSourceOperator}, true},
		{"a person chose to run without one", agentconfig.Preference{
			None: true, Source: agentconfig.PreferenceSourceOperator}, true},
		{"the reconciler applied an instruction", agentconfig.Preference{
			ModelID: "qwen3-8b-instruct", Source: agentconfig.PreferenceSourceDesired}, false},
		{"a record from before provenance existed", agentconfig.Preference{
			ModelID: "qwen3-8b-instruct"}, false},
		{"the question expired unanswered", agentconfig.Preference{Unanswered: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prefDir := t.TempDir()
			if err := agentconfig.SavePreference(
				filepath.Join(prefDir, "preferred-model.json"), tc.pref); err != nil {
				t.Fatalf("save preference: %v", err)
			}
			s := newCatalogTestServer(t, &fakeInference{hwProfile: hardware.Profile{RAMTotalGB: 32}}, prefDir)
			w, got := doGet(t, s, "/waired/v1/inference/catalog")
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d", w.Code)
			}
			if got.ModelQuestionAnswered != tc.want {
				t.Errorf("model_question_answered = %v, want %v", got.ModelQuestionAnswered, tc.want)
			}
			// The two fields stay independent: a preference the host was
			// handed is still reported, it just is not an answer.
			if tc.pref.ModelID != "" && got.PreferredModelID != tc.pref.ModelID {
				t.Errorf("preferred_model_id = %q, want %q", got.PreferredModelID, tc.pref.ModelID)
			}
		})
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
// "no vLLM variant".
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
	if byID["qwen3-4b-instruct"].DeficitLabel != "no vLLM variant" {
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
	// than the wire's "no Ollama variant".
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

// fixtureVariantSHA is the measurement-ledger key for one catalogFixture
// family's ollama variant.
func fixtureVariantSHA(t *testing.T, modelID string) string {
	t.Helper()
	for _, m := range catalogFixture() {
		if m.ModelID != modelID {
			continue
		}
		for _, v := range m.Variants {
			for _, rt := range v.RuntimeSupport {
				if rt == catalog.RuntimeOllama {
					return catalog.VariantSHA(v)
				}
			}
		}
	}
	t.Fatalf("no ollama variant for %q in the fixture", modelID)
	return ""
}

func markedPick(t *testing.T, families []CatalogFamily) string {
	t.Helper()
	var marked []string
	for _, f := range families {
		if f.RecommendedPick {
			marked = append(marked, f.ModelID)
		}
	}
	if len(marked) != 1 {
		t.Fatalf("recommended rows: %v, want exactly 1", marked)
	}
	return marked[0]
}

// PRODUCT CONTRACT (waired-agent#784): the badge moves off a model this
// host has MEASURED below its own floor, and the next rung down takes
// it.
//
// This is the rc9 defect end to end. On the reported Windows host the
// picker default was the 9B, the host benchmarked it at 11-12 tok/s, the
// step-down prompt offered the 4B — and this endpoint went on marking
// the 9B as the host's own pick, so every surface reading it kept
// pointing at the model the machine had just rejected.
func TestInferenceCatalog_MeasuredSlowMovesTheBadge(t *testing.T) {
	inf := &fakeInference{hwProfile: hardware.Profile{RAMTotalGB: 32}}
	_, before := doGet(t, newCatalogTestServer(t, inf, t.TempDir()), "/waired/v1/inference/catalog")
	if got := markedPick(t, before.Families); got != "qwen3-8b-instruct" {
		t.Fatalf("before measuring, badge = %q, want qwen3-8b-instruct", got)
	}

	inf.measuredRates = map[string]router.MeasuredRate{
		fixtureVariantSHA(t, "qwen3-8b-instruct"): {Tokps: 11},
	}
	inf.measuredFloor = 60

	_, after := doGet(t, newCatalogTestServer(t, inf, t.TempDir()), "/waired/v1/inference/catalog")
	if got := markedPick(t, after.Families); got != "qwen3-4b-instruct" {
		t.Errorf("after the 8B measured 11 tok/s, badge = %q, want qwen3-4b-instruct", got)
	}

	// The row that lost the badge has to say why. A badge that moves
	// with the figure visible nowhere is the same silence #784 reported,
	// arriving from the other direction.
	var said bool
	for _, f := range after.Families {
		if f.ModelID != "qwen3-8b-instruct" {
			continue
		}
		said = true
		if f.MeasuredTokps != 11 {
			t.Errorf("8B row reports %v tok/s, want 11", f.MeasuredTokps)
		}
		if f.RecommendedPick {
			t.Error("the measured-slow model is still marked as this host's pick")
		}
	}
	if !said {
		t.Error("the measured-slow model vanished from the catalog; it must stay offered")
	}
	for _, f := range after.Families {
		if f.ModelID != "qwen3-8b-instruct" && f.MeasuredTokps != 0 {
			t.Errorf("%s reports %v tok/s; nothing was measured for it",
				f.ModelID, f.MeasuredTokps)
		}
	}
}

// PRODUCT CONTRACT (waired-ai/waired#1056 decision 1): a host that has
// measured everything it can run as slow keeps its badge rather than
// being left with no local AI at all.
func TestInferenceCatalog_EverythingMeasuredSlowKeepsTheBadge(t *testing.T) {
	inf := &fakeInference{
		hwProfile: hardware.Profile{RAMTotalGB: 32},
		measuredRates: map[string]router.MeasuredRate{
			fixtureVariantSHA(t, "qwen3-8b-instruct"): {Tokps: 11},
			fixtureVariantSHA(t, "qwen3-4b-instruct"): {Tokps: 26},
		},
		measuredFloor: 60,
	}
	_, got := doGet(t, newCatalogTestServer(t, inf, t.TempDir()), "/waired/v1/inference/catalog")
	if marked := markedPick(t, got.Families); marked != "qwen3-8b-instruct" {
		t.Errorf("badge = %q, want qwen3-8b-instruct — the pass must stand down, not blank the badge",
			marked)
	}
}

// Product contract (waired-agent#625): one catalog response describes one
// machine. The host block reports the budget the fit rules judged this
// host on, not GPUs[0]'s raw figure.
//
// The two differ exactly where it mattered. The darwin detector reports
// VRAMTotalMB=0 on purpose — Apple Silicon has no separate pool to
// report — so reading GPUs[0] left vram_total_mb absent on every Mac
// while the deficit labels in the same document quoted 12288 MB.
//
// The profile below is the shape the REAL darwin detector produces:
// UnifiedMemory with UsableVRAMMB set and no VRAMTotalMB on the device.
// Fixtures elsewhere in this repo set a non-zero VRAMTotalMB on Apple
// GPUs, which is a machine that cannot exist, and that fake is why #662
// shipped green.
func TestInferenceCatalog_HostBlockMatchesTheBudgetTheFitUsed(t *testing.T) {
	inf := &fakeInference{
		hwProfile: hardware.Profile{
			RAMTotalGB:              16,
			RAMAvailableAtInstallGB: 6,
			UnifiedMemory:           true,
			UsableVRAMMB:            12288,
			GPUs:                    []hardware.GPU{{Vendor: "apple", Model: "Apple M4"}},
		},
	}
	s := newCatalogTestServer(t, inf, t.TempDir())

	w, got := doGet(t, s, "/waired/v1/inference/catalog")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got.Host.VRAMTotalMB != 12288 {
		t.Errorf("host.vram_total_mb = %d, want 12288 (EffectiveVRAMMB, not GPUs[0].VRAMTotalMB=0)",
			got.Host.VRAMTotalMB)
	}
	if !got.Host.UnifiedMemory {
		t.Error("host.unified_memory is false on a unified host; a surface would add the two figures")
	}
	// max(OSMemoryAllowanceGB, 16-6). The label says this host has 6 GB
	// allocatable, and this is the figure that makes that checkable.
	if got.Host.OSReservedGB != 10 {
		t.Errorf("host.os_reserved_gb = %d, want 10 (measured 16-6, #568)", got.Host.OSReservedGB)
	}
}

// A host whose RAM probe failed reports no reservation at all rather than
// zero: no machine reserves nothing, and a surface reading 0 as a
// measurement would print "0 GB is already in use".
func TestInferenceCatalog_NoRAMReadingReportsNoReservation(t *testing.T) {
	inf := &fakeInference{hwProfile: hardware.Profile{}}
	s := newCatalogTestServer(t, inf, t.TempDir())

	w, got := doGet(t, s, "/waired/v1/inference/catalog")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got.Host.OSReservedGB != 0 {
		t.Errorf("host.os_reserved_gb = %d on a host that reported no RAM, want absent", got.Host.OSReservedGB)
	}
}
