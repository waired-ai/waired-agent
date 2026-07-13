package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
)

func newPreferredModelTestServer(t *testing.T, inf *fakeInference, prefDir string, restarts *int32) *Server {
	t.Helper()
	cfg := &CatalogConfig{
		PreferencePath: filepath.Join(prefDir, "preferred-model.json"),
		ManifestsFn:    func() ([]catalog.Manifest, error) { return catalogFixture(), nil },
		RestartScheduler: func() {
			atomic.AddInt32(restarts, 1)
		},
	}
	return New(stubStatus{}, stubPinger{}).WithInference(inf).WithCatalog(cfg)
}

func doPostJSON(t *testing.T, s *Server, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	buf := &bytes.Buffer{}
	if err := json.NewEncoder(buf).Encode(body); err != nil {
		t.Fatalf("encode body: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, path, buf)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestPreferredModel_KnownModelTriggersRestartAndPersists(t *testing.T) {
	prefDir := t.TempDir()
	var restarts int32
	inf := &fakeInference{
		models: []ModelEntry{{ModelID: "qwen3-8b-instruct", State: catalog.ModelStateReady}},
	}
	s := newPreferredModelTestServer(t, inf, prefDir, &restarts)

	w := doPostJSON(t, s, "/waired/v1/inference/preferred-model",
		PreferredModelRequest{ModelID: "qwen3-8b-instruct"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("status: want 202, got %d body=%s", w.Code, w.Body.String())
	}
	var got PreferredModelResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ModelID != "qwen3-8b-instruct" || !got.WillRestart {
		t.Errorf("response: %+v", got)
	}
	if got.Downloading {
		t.Errorf("Downloading should be false when the model is already ready")
	}

	pref, ok, err := agentconfig.LoadPreference(filepath.Join(prefDir, "preferred-model.json"))
	if err != nil || !ok {
		t.Fatalf("preference not persisted: ok=%v err=%v", ok, err)
	}
	if pref.ModelID != "qwen3-8b-instruct" {
		t.Errorf("preference: got %q", pref.ModelID)
	}

	// RestartScheduler runs in a goroutine; spin-wait briefly.
	for i := 0; i < 100 && atomic.LoadInt32(&restarts) == 0; i++ {
		// 10 ms × 100 = 1 s budget — plenty for a goroutine to dispatch.
		time.Sleep(10 * time.Millisecond)
	}
	if atomic.LoadInt32(&restarts) != 1 {
		t.Errorf("RestartScheduler should have fired exactly once, got %d", restarts)
	}
}

func TestPreferredModel_UnknownModelReturns404(t *testing.T) {
	prefDir := t.TempDir()
	var restarts int32
	s := newPreferredModelTestServer(t, &fakeInference{}, prefDir, &restarts)

	w := doPostJSON(t, s, "/waired/v1/inference/preferred-model",
		PreferredModelRequest{ModelID: "nonexistent-model"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d body=%s", w.Code, w.Body.String())
	}
	if atomic.LoadInt32(&restarts) != 0 {
		t.Errorf("unknown model must not trigger restart")
	}
	if _, ok, _ := agentconfig.LoadPreference(filepath.Join(prefDir, "preferred-model.json")); ok {
		t.Errorf("unknown model must not persist preference")
	}
}

func TestPreferredModel_UndownloadedReportsDownloadingWithoutPull(t *testing.T) {
	prefDir := t.TempDir()
	var restarts int32
	inf := &fakeInference{
		models: []ModelEntry{}, // 8B not yet downloaded
	}
	s := newPreferredModelTestServer(t, inf, prefDir, &restarts)

	w := doPostJSON(t, s, "/waired/v1/inference/preferred-model",
		PreferredModelRequest{ModelID: "qwen3-8b-instruct"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d body=%s", w.Code, w.Body.String())
	}
	var got PreferredModelResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if !got.Downloading {
		t.Errorf("Downloading should be true when model is missing")
	}
	// The handler must NOT dispatch a pre-restart pull: the imminent
	// restart would cancel it, and its failure path writes a transient
	// failed state a watching client could misread (waired#774). The
	// post-restart bootstrap owns the real pull.
	if inf.pulled != "" {
		t.Errorf("PullModel must not be called pre-restart, got %q", inf.pulled)
	}
}

func TestPreferredModel_BadRequest(t *testing.T) {
	prefDir := t.TempDir()
	var restarts int32
	s := newPreferredModelTestServer(t, &fakeInference{}, prefDir, &restarts)

	w := doPostJSON(t, s, "/waired/v1/inference/preferred-model",
		PreferredModelRequest{ModelID: ""})
	if w.Code != http.StatusBadRequest {
		t.Errorf("empty model_id: want 400, got %d", w.Code)
	}
}

func TestPreferredModel_MethodNotAllowed(t *testing.T) {
	prefDir := t.TempDir()
	var restarts int32
	s := newPreferredModelTestServer(t, &fakeInference{}, prefDir, &restarts)

	r := httptest.NewRequest(http.MethodGet, "/waired/v1/inference/preferred-model", nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: want 405, got %d", w.Code)
	}
}

func TestPreferredModel_NotConfiguredReturns404(t *testing.T) {
	// No WithCatalog → endpoint not mounted.
	s := New(stubStatus{}, stubPinger{}).WithInference(&fakeInference{})
	w := doPostJSON(t, s, "/waired/v1/inference/preferred-model",
		PreferredModelRequest{ModelID: "qwen3-4b-instruct"})
	if w.Code != http.StatusNotFound {
		t.Errorf("want 404 when catalog unconfigured, got %d", w.Code)
	}
}
