package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// #812: with ApplyModelSwitch wired the switch applies in process, so the
// response reports WillRestart:false and RestartScheduler never fires.
func TestPreferredModel_InProcessSwapNoRestart(t *testing.T) {
	prefDir := t.TempDir()
	var restarts int32
	var swapCalls int
	var swapModel string
	inf := &fakeInference{
		models: []ModelEntry{{ModelID: "qwen3-8b-instruct", State: catalog.ModelStateReady}},
	}
	cfg := &CatalogConfig{
		PreferencePath:   filepath.Join(prefDir, "preferred-model.json"),
		ManifestsFn:      func() ([]catalog.Manifest, error) { return catalogFixture(), nil },
		RestartScheduler: func() { atomic.AddInt32(&restarts, 1) },
		ApplyModelSwitch: func(_ context.Context, modelID string) (bool, error) {
			// Called synchronously by the handler, so plain vars are race-free.
			swapCalls++
			swapModel = modelID
			return true, nil // report a background pull started
		},
	}
	s := New(stubStatus{}, stubPinger{}).WithInference(inf).WithCatalog(cfg)

	w := doPostJSON(t, s, "/waired/v1/inference/preferred-model",
		PreferredModelRequest{ModelID: "qwen3-8b-instruct"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("status: want 202, got %d body=%s", w.Code, w.Body.String())
	}
	var got PreferredModelResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.WillRestart {
		t.Errorf("in-process swap must report WillRestart=false, got %+v", got)
	}
	if !got.Downloading {
		t.Errorf("Downloading should mirror the swap hook's return (true)")
	}
	if swapCalls != 1 || swapModel != "qwen3-8b-instruct" {
		t.Errorf("ApplyModelSwitch: calls=%d model=%q, want 1 / qwen3-8b-instruct", swapCalls, swapModel)
	}
	// The whole point of #812: no restart on the common path.
	if atomic.LoadInt32(&restarts) != 0 {
		t.Errorf("in-process swap must not restart; restarts=%d", restarts)
	}
	if pref, ok, err := agentconfig.LoadPreference(filepath.Join(prefDir, "preferred-model.json")); err != nil || !ok || pref.ModelID != "qwen3-8b-instruct" {
		t.Errorf("preference not persisted: %+v ok=%v err=%v", pref, ok, err)
	}
}

// #812: when the in-process swap can't apply (here a stubbed error, standing in
// for a cross-engine target / wedged setup) the handler falls back to the
// supervised restart and reports WillRestart:true.
func TestPreferredModel_SwapErrorFallsBackToRestart(t *testing.T) {
	prefDir := t.TempDir()
	var restarts int32
	var swapCalls int
	inf := &fakeInference{
		models: []ModelEntry{{ModelID: "qwen3-8b-instruct", State: catalog.ModelStateReady}},
	}
	cfg := &CatalogConfig{
		PreferencePath:   filepath.Join(prefDir, "preferred-model.json"),
		ManifestsFn:      func() ([]catalog.Manifest, error) { return catalogFixture(), nil },
		RestartScheduler: func() { atomic.AddInt32(&restarts, 1) },
		ApplyModelSwitch: func(_ context.Context, _ string) (bool, error) {
			swapCalls++
			return false, errors.New("cross-engine target needs restart")
		},
	}
	s := New(stubStatus{}, stubPinger{}).WithInference(inf).WithCatalog(cfg)

	w := doPostJSON(t, s, "/waired/v1/inference/preferred-model",
		PreferredModelRequest{ModelID: "qwen3-8b-instruct"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d body=%s", w.Code, w.Body.String())
	}
	var got PreferredModelResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if !got.WillRestart {
		t.Errorf("swap-error path must fall back to restart (WillRestart=true), got %+v", got)
	}
	if swapCalls != 1 {
		t.Errorf("ApplyModelSwitch should have been attempted once, got %d", swapCalls)
	}
	for i := 0; i < 100 && atomic.LoadInt32(&restarts) == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if atomic.LoadInt32(&restarts) != 1 {
		t.Errorf("RestartScheduler should fire once on swap error, got %d", restarts)
	}
}

// TestPreferredModel_UnavailableSwitchIsReportedNotRestarted is
// waired-agent#257's other half: the restart fallback above is the right
// answer for "this daemon cannot apply it in process" and the WRONG one
// for "this host will not fetch the weights at all" — restarting bounces
// the management API, gateway and mesh to re-run a bootstrap that fails
// for the same reason. The sentinel is how the handler tells them apart;
// the test above (a plain error) pins that the fallback still happens for
// everything else.
func TestPreferredModel_UnavailableSwitchIsReportedNotRestarted(t *testing.T) {
	prefDir := t.TempDir()
	var restarts int32
	var swapCalls int
	inf := &fakeInference{
		models: []ModelEntry{{ModelID: "qwen3-8b-instruct", State: catalog.ModelStateReady}},
	}
	cfg := &CatalogConfig{
		PreferencePath:   filepath.Join(prefDir, "preferred-model.json"),
		ManifestsFn:      func() ([]catalog.Manifest, error) { return catalogFixture(), nil },
		RestartScheduler: func() { atomic.AddInt32(&restarts, 1) },
		ApplyModelSwitch: func(_ context.Context, _ string) (bool, error) {
			swapCalls++
			return false, fmt.Errorf("start the download: %w: %w",
				errors.New("pulls are disabled by config (allow_pull=false)"),
				ErrModelSwitchUnavailable)
		},
	}
	s := New(stubStatus{}, stubPinger{}).WithInference(inf).WithCatalog(cfg)

	w := doPostJSON(t, s, "/waired/v1/inference/preferred-model",
		PreferredModelRequest{ModelID: "qwen3-8b-instruct"})
	if w.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d body=%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("allow_pull=false")) {
		t.Errorf("body = %s, want the cause carried through to the caller", w.Body.String())
	}
	if swapCalls != 1 {
		t.Errorf("ApplyModelSwitch calls = %d, want 1", swapCalls)
	}
	// Give a stray restart time to land before declaring there was none.
	time.Sleep(50 * time.Millisecond)
	if n := atomic.LoadInt32(&restarts); n != 0 {
		t.Errorf("restarts = %d, want 0 — a restart cannot make this switch work", n)
	}
	// The preference is deliberately left saved: it is the operator's
	// stated choice, and it applies by itself once pulls are possible.
	if pref, ok, err := agentconfig.LoadPreference(filepath.Join(prefDir, "preferred-model.json")); err != nil || !ok || pref.ModelID != "qwen3-8b-instruct" {
		t.Errorf("preference = %+v ok=%v err=%v, want the choice kept", pref, ok, err)
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

// PRODUCT CONTRACT (waired-ai/waired-agent#200): a name we WITHDREW gets
// a named answer, not "never heard of it". 409, not 404 — the request is
// well-formed and names something real; the world moved.
//
// This is the one place the retirement map does NOT substitute. The
// handler echoes model_id back and persists it, so substituting would
// write a pin the operator never chose. The asymmetry is deliberate and
// mirrors signer.IsRetiredIntegrationTarget: a NEW instruction naming a
// withdrawn value is refused, a STORED one is migrated.
//
// All three names the deleted manifest answered to, because findManifest
// matches model_id exactly and the aliases died with the file.
func TestPreferredModel_RetiredModelReturns409NamingTheSuccessor(t *testing.T) {
	for _, name := range []string{
		"qwen2.5-coder-0.5b-instruct",
		"qwen2.5-coder-0.5b",
		"Qwen/Qwen2.5-Coder-0.5B-Instruct",
	} {
		t.Run(name, func(t *testing.T) {
			prefDir := t.TempDir()
			var restarts int32
			inf := &fakeInference{}
			s := newPreferredModelTestServer(t, inf, prefDir, &restarts)
			var swaps int32
			s.catalog.ApplyModelSwitch = func(context.Context, string) (bool, error) {
				atomic.AddInt32(&swaps, 1)
				return false, nil
			}

			w := doPostJSON(t, s, "/waired/v1/inference/preferred-model",
				PreferredModelRequest{ModelID: name})
			if w.Code != http.StatusConflict {
				t.Fatalf("want 409, got %d body=%s", w.Code, w.Body.String())
			}
			// The message has to carry the way forward: the operator cannot
			// read the retirement table, and "model_retired" alone leaves
			// them guessing at a replacement.
			var body struct {
				ErrorCode string `json:"error_code"`
				Message   string `json:"message"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode error body: %v (%s)", err, w.Body.String())
			}
			if body.ErrorCode != "model_retired" {
				t.Errorf("error_code = %q, want model_retired", body.ErrorCode)
			}
			if !bytes.Contains(w.Body.Bytes(), []byte("qwen3.5-0.8b")) {
				t.Errorf("message does not name the successor: %s", body.Message)
			}

			// Nothing may happen as a side effect of a refusal.
			if _, ok, _ := agentconfig.LoadPreference(filepath.Join(prefDir, "preferred-model.json")); ok {
				t.Error("a retired model must not persist a preference")
			}
			if n := atomic.LoadInt32(&swaps); n != 0 {
				t.Errorf("ApplyModelSwitch called %d times for a refused request", n)
			}
			if atomic.LoadInt32(&restarts) != 0 {
				t.Error("a retired model must not trigger a restart")
			}
		})
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

// PRODUCT CONTRACT (waired-agent#586; owner-ruled 2026-08-08,
// waired-ai/waired#1067): {"none":true} is the install flow's "don't
// download a model now". It persists Preference.None, applies in process
// through the hook, restarts nothing, and downloads nothing.
func TestPreferredModel_NoneChoicePersistsAndAppliesInProcess(t *testing.T) {
	prefDir := t.TempDir()
	var restarts int32
	var noneApplied int
	inf := &fakeInference{}
	cfg := &CatalogConfig{
		PreferencePath:       filepath.Join(prefDir, "preferred-model.json"),
		ManifestsFn:          func() ([]catalog.Manifest, error) { return catalogFixture(), nil },
		RestartScheduler:     func() { atomic.AddInt32(&restarts, 1) },
		ApplyNoModelSelected: func() { noneApplied++ },
	}
	s := New(stubStatus{}, stubPinger{}).WithInference(inf).WithCatalog(cfg)

	w := doPostJSON(t, s, "/waired/v1/inference/preferred-model",
		PreferredModelRequest{None: true})
	if w.Code != http.StatusAccepted {
		t.Fatalf("status: want 202, got %d body=%s", w.Code, w.Body.String())
	}
	var got PreferredModelResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ModelID != "" || got.WillRestart || got.Downloading {
		t.Errorf("none must name no model, restart nothing and download nothing: %+v", got)
	}
	if noneApplied != 1 {
		t.Errorf("ApplyNoModelSelected calls = %d, want 1", noneApplied)
	}
	pref, ok, err := agentconfig.LoadPreference(filepath.Join(prefDir, "preferred-model.json"))
	if err != nil || !ok {
		t.Fatalf("none preference not persisted: ok=%v err=%v", ok, err)
	}
	if !pref.None || pref.ModelID != "" {
		t.Errorf("persisted preference: %+v, want None with no model", pref)
	}
	if inf.pulled != "" {
		t.Errorf("PullModel must not run on a none choice, got %q", inf.pulled)
	}
	time.Sleep(20 * time.Millisecond)
	if atomic.LoadInt32(&restarts) != 0 {
		t.Errorf("RestartScheduler must not fire on a none choice, fired %d times", restarts)
	}
}

// A body that both names a model and claims there is none is
// contradictory, and a later model choice must overwrite a stored none —
// LoadPreference reporting ok=false for it would re-arm the fallback the
// record stands down (#586).
func TestPreferredModel_NoneWithModelIDIsRefused(t *testing.T) {
	prefDir := t.TempDir()
	var restarts int32
	s := newPreferredModelTestServer(t, &fakeInference{}, prefDir, &restarts)

	w := doPostJSON(t, s, "/waired/v1/inference/preferred-model",
		PreferredModelRequest{ModelID: "qwen3-8b-instruct", None: true})
	if w.Code != http.StatusBadRequest {
		t.Errorf("none+model_id: want 400, got %d body=%s", w.Code, w.Body.String())
	}
}

// The nil-hook wiring (an older main.go, or --disable-inference builds)
// still persists the choice: the next boot reads the file, which is the
// half the hook only accelerates.
func TestPreferredModel_NoneWithoutHookStillPersists(t *testing.T) {
	prefDir := t.TempDir()
	var restarts int32
	s := newPreferredModelTestServer(t, &fakeInference{}, prefDir, &restarts)

	w := doPostJSON(t, s, "/waired/v1/inference/preferred-model",
		PreferredModelRequest{None: true})
	if w.Code != http.StatusAccepted {
		t.Fatalf("status: want 202, got %d body=%s", w.Code, w.Body.String())
	}
	pref, ok, err := agentconfig.LoadPreference(filepath.Join(prefDir, "preferred-model.json"))
	if err != nil || !ok || !pref.None {
		t.Fatalf("none preference not persisted without the hook: pref=%+v ok=%v err=%v", pref, ok, err)
	}
}

// POST /inference/model-choice-pending relays the claim to the provider
// hook verbatim — both directions — and answers 404 when no hook is
// wired, which the CLI reads as best-effort (#586).
func TestModelChoicePending_RelaysTheClaim(t *testing.T) {
	prefDir := t.TempDir()
	var restarts int32
	var claims []bool
	inf := &fakeInference{}
	cfg := &CatalogConfig{
		PreferencePath:         filepath.Join(prefDir, "preferred-model.json"),
		ManifestsFn:            func() ([]catalog.Manifest, error) { return catalogFixture(), nil },
		RestartScheduler:       func() { atomic.AddInt32(&restarts, 1) },
		NoteModelChoicePending: func(pending bool) { claims = append(claims, pending) },
	}
	s := New(stubStatus{}, stubPinger{}).WithInference(inf).WithCatalog(cfg)

	if w := doPostJSON(t, s, "/waired/v1/inference/model-choice-pending",
		ModelChoicePendingRequest{Pending: true}); w.Code != http.StatusNoContent {
		t.Fatalf("pending=true: want 204, got %d body=%s", w.Code, w.Body.String())
	}
	if w := doPostJSON(t, s, "/waired/v1/inference/model-choice-pending",
		ModelChoicePendingRequest{Pending: false}); w.Code != http.StatusNoContent {
		t.Fatalf("pending=false: want 204, got %d body=%s", w.Code, w.Body.String())
	}
	if len(claims) != 2 || claims[0] != true || claims[1] != false {
		t.Errorf("claims relayed = %v, want [true false]", claims)
	}
}

func TestModelChoicePending_NoHookAnswers404(t *testing.T) {
	prefDir := t.TempDir()
	var restarts int32
	s := newPreferredModelTestServer(t, &fakeInference{}, prefDir, &restarts)

	if w := doPostJSON(t, s, "/waired/v1/inference/model-choice-pending",
		ModelChoicePendingRequest{Pending: true}); w.Code != http.StatusNotFound {
		t.Errorf("no hook: want 404, got %d", w.Code)
	}
}
