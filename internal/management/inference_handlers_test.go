package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/router"
)

type stubStatus struct{}

func (stubStatus) Status() Status { return Status{DeviceName: "test"} }

type stubPinger struct{}

func (stubPinger) PingPeer(context.Context, string) (PingResult, error) {
	return PingResult{}, nil
}

// fakeInference satisfies InferenceProvider for tests.
type fakeInference struct {
	mu         sync.Mutex
	pulled     string
	deleted    string
	deleteErr  error
	pullErr    error
	selectErr  error
	canned     InferenceStatus
	hwProfile  hardware.Profile
	runtimes   []RuntimeStatus
	models     []ModelEntry
	selectResp router.Selection

	benchOut      BenchmarkOutcome
	benchOK       bool
	benchErr      error
	benchStatus   BenchmarkStatusResponse
	dismissedFrom string
	dismissedTo   string
	dismissErr    error
}

func (f *fakeInference) Status(context.Context) InferenceStatus    { return f.canned }
func (f *fakeInference) Hardware(context.Context) hardware.Profile { return f.hwProfile }
func (f *fakeInference) Runtimes(context.Context) []RuntimeStatus  { return f.runtimes }
func (f *fakeInference) ListModels(context.Context) []ModelEntry   { return f.models }
func (f *fakeInference) PullModel(_ context.Context, m string) (PullJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pullErr != nil {
		return PullJob{}, f.pullErr
	}
	f.pulled = m
	return PullJob{JobID: "job_1", ModelID: m, Status: "queued"}, nil
}
func (f *fakeInference) DeleteModel(_ context.Context, m string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = m
	return f.deleteErr
}
func (f *fakeInference) Select(_ context.Context, req router.Request) (router.Selection, error) {
	if f.selectErr != nil {
		return router.Selection{}, f.selectErr
	}
	out := f.selectResp
	if out.ModelID == "" {
		out.ModelID = req.Model
	}
	return out, nil
}
func (f *fakeInference) RunBenchmark(context.Context) (BenchmarkOutcome, bool, error) {
	return f.benchOut, f.benchOK, f.benchErr
}
func (f *fakeInference) BenchmarkStatus() BenchmarkStatusResponse {
	if f.benchStatus.State == "" {
		return BenchmarkStatusResponse{State: BenchmarkStateIdle}
	}
	return f.benchStatus
}
func (f *fakeInference) DismissRecommendation(from, to string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dismissedFrom = from
	f.dismissedTo = to
	return f.dismissErr
}

func newServerWithInference(inf InferenceProvider) *Server {
	return New(stubStatus{}, stubPinger{}).WithInference(inf)
}

func TestInferenceStatus(t *testing.T) {
	inf := &fakeInference{canned: InferenceStatus{
		SubsystemState: "ready",
		Runtimes:       map[string]RuntimeStatus{"ollama": {Installed: true, Version: "0.22.1", State: "ready"}},
		Models:         ModelsSnapshot{Ready: []string{"qwen3-8b-instruct"}},
	}}
	s := newServerWithInference(inf)

	r := httptest.NewRequest(http.MethodGet, "/waired/v1/inference/status", nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var got InferenceStatus
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SubsystemState != "ready" || got.Runtimes["ollama"].Version != "0.22.1" {
		t.Errorf("got = %+v", got)
	}
}

func TestInferenceHardware(t *testing.T) {
	inf := &fakeInference{hwProfile: hardware.Profile{OS: "linux", Arch: "x86_64", RAMTotalGB: 64}}
	s := newServerWithInference(inf)
	r := httptest.NewRequest(http.MethodGet, "/waired/v1/inference/hardware", nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"ram_total_gb":64`) {
		t.Errorf("body = %s", w.Body.String())
	}
}

func TestInferenceRuntimes(t *testing.T) {
	inf := &fakeInference{runtimes: []RuntimeStatus{{Installed: true, Version: "0.22.1", State: "ready"}}}
	s := newServerWithInference(inf)
	r := httptest.NewRequest(http.MethodGet, "/waired/v1/inference/runtimes", nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "0.22.1") {
		t.Errorf("body = %s", w.Body.String())
	}
}

func TestInferenceSelect_OK(t *testing.T) {
	inf := &fakeInference{selectResp: router.Selection{
		EndpointID: "ep_local_ollama_qwen3", Runtime: "ollama", ExecutionMode: "local",
	}}
	s := newServerWithInference(inf)
	body := `{"model":"waired/default"}`
	r := httptest.NewRequest(http.MethodPost, "/waired/v1/inference/select", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
}

func TestInferenceSelect_RouterErrorMaps404(t *testing.T) {
	inf := &fakeInference{selectErr: wrapErr(router.ErrModelNotFound, "alias not found")}
	s := newServerWithInference(inf)
	body := `{"model":"x"}`
	r := httptest.NewRequest(http.MethodPost, "/waired/v1/inference/select", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestModelsList(t *testing.T) {
	inf := &fakeInference{models: []ModelEntry{
		{ModelID: "qwen3-8b-instruct", Aliases: []string{"waired/default"}, State: "ready", SizeBytes: 5e9},
	}}
	s := newServerWithInference(inf)
	r := httptest.NewRequest(http.MethodGet, "/waired/v1/models", nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "qwen3-8b-instruct") {
		t.Errorf("body = %s", w.Body.String())
	}
}

func TestModelsPull_Accepted(t *testing.T) {
	inf := &fakeInference{}
	s := newServerWithInference(inf)
	r := httptest.NewRequest(http.MethodPost, "/waired/v1/models/pull", bytes.NewBufferString(`{"model":"waired/default"}`))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if inf.pulled != "waired/default" {
		t.Errorf("pulled = %q, want waired/default", inf.pulled)
	}
}

func TestModelsPull_BadRequest(t *testing.T) {
	s := newServerWithInference(&fakeInference{})
	r := httptest.NewRequest(http.MethodPost, "/waired/v1/models/pull", bytes.NewBufferString(`{}`))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestModelsDelete(t *testing.T) {
	inf := &fakeInference{}
	s := newServerWithInference(inf)
	r := httptest.NewRequest(http.MethodDelete, "/waired/v1/models/qwen3-8b-instruct", nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d body=%s", w.Code, w.Body.String())
	}
	if inf.deleted != "qwen3-8b-instruct" {
		t.Errorf("deleted = %q", inf.deleted)
	}
}

func TestInferenceRoutesDisabledWhenNotWired(t *testing.T) {
	s := New(stubStatus{}, stubPinger{}) // no WithInference
	r := httptest.NewRequest(http.MethodGet, "/waired/v1/inference/status", nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when InferenceProvider is nil", w.Code)
	}
}

func TestExistingStatusStillWorks(t *testing.T) {
	// The pre-existing /waired/v1/status route must keep working
	// alongside the new inference routes.
	s := newServerWithInference(&fakeInference{})
	r := httptest.NewRequest(http.MethodGet, "/waired/v1/status", nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
}

func wrapErr(target error, msg string) error { return wrappedErr{target: target, msg: msg} }

type wrappedErr struct {
	target error
	msg    string
}

func (e wrappedErr) Error() string { return e.msg + ": " + e.target.Error() }
func (e wrappedErr) Unwrap() error { return e.target }

// unused so the test file compiles cleanly even if errors.Is isn't
// used elsewhere here.
var _ = errors.Is
