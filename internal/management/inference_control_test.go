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

type fakeInferenceCtl struct {
	mu      sync.Mutex
	current state.InferenceState
	desired state.InferenceState
	err     error
}

func newFakeInferenceCtl(initial state.InferenceState) *fakeInferenceCtl {
	return &fakeInferenceCtl{current: initial, desired: initial}
}

func (f *fakeInferenceCtl) Enable(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.current = state.InferenceEnabled
	f.desired = state.InferenceEnabled
	return nil
}

func (f *fakeInferenceCtl) Disable(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.current = state.InferenceDisabled
	f.desired = state.InferenceDisabled
	return nil
}

func (f *fakeInferenceCtl) State() (state.InferenceState, state.InferenceState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current, f.desired
}

func TestInferenceControlEndpointFlipsState(t *testing.T) {
	ic := newFakeInferenceCtl(state.InferenceEnabled)
	srv := New(fakeStatus{}, fakePinger{}).WithInferenceControl(ic)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/waired/v1/inference/disable", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got InferenceStateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.State != "disabled" || got.DesiredState != "disabled" {
		t.Fatalf("after disable: %+v", got)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/waired/v1/inference/enable", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.State != "enabled" || got.DesiredState != "enabled" {
		t.Fatalf("after enable: %+v", got)
	}
}

func TestInferenceControlEndpointRejectsGET(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{}).WithInferenceControl(newFakeInferenceCtl(state.InferenceEnabled))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/inference/disable", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestInferenceControlEndpointMissingControllerIs404(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{}) // no WithInferenceControl
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/waired/v1/inference/disable", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when no InferenceController, got %d", rec.Code)
	}
}

func TestInferenceControlEndpointPropagatesError(t *testing.T) {
	ic := newFakeInferenceCtl(state.InferenceEnabled)
	ic.err = errors.New("disk full")
	srv := New(fakeStatus{}, fakePinger{}).WithInferenceControl(ic)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/waired/v1/inference/disable", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "disk full") {
		t.Errorf("expected error body to mention disk full, got %s", rec.Body.String())
	}
}
