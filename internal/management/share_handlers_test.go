package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

type fakeShareCtl struct {
	mu      sync.Mutex
	current state.ShareMeshState
	desired state.ShareMeshState
	err     error
}

func newFakeShareCtl(initial state.ShareMeshState) *fakeShareCtl {
	return &fakeShareCtl{current: initial, desired: initial}
}

func (f *fakeShareCtl) Share(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = ctx
	if f.err != nil {
		return f.err
	}
	f.current = state.ShareMeshShared
	f.desired = state.ShareMeshShared
	return nil
}

func (f *fakeShareCtl) Unshare(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = ctx
	if f.err != nil {
		return f.err
	}
	f.current = state.ShareMeshNotShared
	f.desired = state.ShareMeshNotShared
	return nil
}

func (f *fakeShareCtl) State() (state.ShareMeshState, state.ShareMeshState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current, f.desired
}

func TestShareControlEndpointFlipsState(t *testing.T) {
	sc := newFakeShareCtl(state.ShareMeshShared)
	srv := New(fakeStatus{}, fakePinger{}).WithShareControl(sc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/waired/v1/inference/share/disable", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got ShareStateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.State != "not_shared" || got.DesiredState != "not_shared" {
		t.Fatalf("after disable: %+v", got)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/waired/v1/inference/share/enable", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.State != "shared" || got.DesiredState != "shared" {
		t.Fatalf("after enable: %+v", got)
	}
}

func TestShareControlEndpointRejectsGET(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{}).WithShareControl(newFakeShareCtl(state.ShareMeshShared))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/inference/share/disable", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestShareControlEndpointMissingControllerIs404(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{}) // no WithShareControl
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/waired/v1/inference/share/disable", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when no ShareController, got %d", rec.Code)
	}
}

func TestShareControlEndpointPropagatesError(t *testing.T) {
	sc := newFakeShareCtl(state.ShareMeshShared)
	sc.err = errors.New("disk full")
	srv := New(fakeStatus{}, fakePinger{}).WithShareControl(sc)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/waired/v1/inference/share/disable", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "disk full") {
		t.Errorf("expected error body to mention disk full, got %s", rec.Body.String())
	}
}

// InferenceStatus.ShareWithMesh must be populated by the management
// Server.handleInferenceStatus from the ShareController when wired,
// independently of the InferenceProvider. The tray relies on this to
// render the share-toggle alongside engine state without needing two
// round-trips.
func TestInferenceStatusSurfacesShareWithMesh(t *testing.T) {
	inf := &fakeInference{canned: InferenceStatus{SubsystemState: "ready"}}
	sc := newFakeShareCtl(state.ShareMeshNotShared)
	srv := New(fakeStatus{}, fakePinger{}).WithInference(inf).WithShareControl(sc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/inference/status", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got InferenceStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ShareWithMesh != "not_shared" {
		t.Errorf("ShareWithMesh = %q, want not_shared", got.ShareWithMesh)
	}
}

// When no ShareController is wired (e.g., older daemons or
// agents booted with Inference.Enabled=false), ShareWithMesh must
// stay empty so the tray can distinguish "no daemon-side support"
// from an explicit value.
func TestInferenceStatusOmitsShareWithMeshWhenNoController(t *testing.T) {
	inf := &fakeInference{canned: InferenceStatus{SubsystemState: "ready"}}
	srv := New(fakeStatus{}, fakePinger{}).WithInference(inf) // no WithShareControl

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/inference/status", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"share_with_mesh"`) {
		t.Errorf("share_with_mesh should be omitted when no ShareController, body=%s", rec.Body.String())
	}
}
