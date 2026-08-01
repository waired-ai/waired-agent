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

// fakeShareCtl mirrors the agent's shareController closely enough that
// the handler tests exercise the real semantics: desired is the persisted
// operator choice, suspended is the live-only session override, and
// current is derived from both (#316).
type fakeShareCtl struct {
	mu        sync.Mutex
	current   state.ShareMeshState
	desired   state.ShareMeshState
	suspended bool
	err       error
	// suspends/unsuspends count the calls so a test can prove the
	// session override was driven, not just its side effect.
	suspends   int
	unsuspends int
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
	f.suspended = false // an explicit choice clears the session override
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
	f.suspended = false // an explicit choice clears the session override
	return nil
}

func (f *fakeShareCtl) Suspend(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = ctx
	if f.err != nil {
		return f.err
	}
	f.suspended = true
	f.suspends++
	return nil
}

func (f *fakeShareCtl) Unsuspend(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = ctx
	if f.err != nil {
		return f.err
	}
	f.suspended = false
	f.unsuspends++
	return nil
}

func (f *fakeShareCtl) IsSuspended() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.suspended
}

func (f *fakeShareCtl) State() (state.ShareMeshState, state.ShareMeshState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.suspended {
		return state.ShareMeshNotShared, f.desired
	}
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

// TestShareSuspendEndpointWithholdsWithoutChangingDesired pins the
// PRODUCT CONTRACT behind the tray's Quit path (#316): suspending stops
// peers from being served right now, while desired_state keeps recording
// what the operator actually chose. Reusing /share/disable here would
// persist "not_shared" and silently revoke the preference for good.
func TestShareSuspendEndpointWithholdsWithoutChangingDesired(t *testing.T) {
	sc := newFakeShareCtl(state.ShareMeshShared)
	srv := New(fakeStatus{}, fakePinger{}).WithShareControl(sc)

	got := postShare(t, srv, "suspend")
	if got.State != "not_shared" {
		t.Errorf("state after suspend = %q, want not_shared", got.State)
	}
	if got.DesiredState != "shared" {
		t.Errorf("desired_state after suspend = %q, want the operator's choice to survive", got.DesiredState)
	}
	if !got.Suspended {
		t.Errorf("suspended flag not reported: %+v", got)
	}
	if sc.suspends != 1 {
		t.Errorf("Suspend calls = %d, want 1", sc.suspends)
	}

	got = postShare(t, srv, "unsuspend")
	if got.State != "shared" || got.DesiredState != "shared" {
		t.Errorf("after unsuspend: %+v, want sharing restored", got)
	}
	if got.Suspended {
		t.Errorf("suspended flag still set after unsuspend: %+v", got)
	}
}

// A suspension must not resurrect sharing the operator turned off: the
// override only ever withholds.
func TestShareUnsuspendKeepsOperatorOff(t *testing.T) {
	sc := newFakeShareCtl(state.ShareMeshNotShared)
	srv := New(fakeStatus{}, fakePinger{}).WithShareControl(sc)

	_ = postShare(t, srv, "suspend")
	got := postShare(t, srv, "unsuspend")
	if got.State != "not_shared" || got.DesiredState != "not_shared" {
		t.Errorf("after unsuspend: %+v, want the persisted not_shared to stand", got)
	}
}

func TestShareSuspendEndpointRejectsGET(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{}).WithShareControl(newFakeShareCtl(state.ShareMeshShared))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/inference/share/suspend", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestShareSuspendEndpointMissingControllerIs404(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{}) // no WithShareControl
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/waired/v1/inference/share/suspend", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when no ShareController, got %d", rec.Code)
	}
}

// TestInferenceStatusSurfacesShareSuspended: the tray cannot tell a
// suspension from an operator opt-out through share_with_mesh alone,
// because that field deliberately keeps reporting the persisted choice.
func TestInferenceStatusSurfacesShareSuspended(t *testing.T) {
	inf := &fakeInference{canned: InferenceStatus{SubsystemState: "ready"}}
	sc := newFakeShareCtl(state.ShareMeshShared)
	srv := New(fakeStatus{}, fakePinger{}).WithInference(inf).WithShareControl(sc)
	_ = postShare(t, srv, "suspend")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/inference/status", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	var got InferenceStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.ShareSuspended {
		t.Errorf("share_suspended not surfaced: %+v", got)
	}
	if got.ShareWithMesh != "shared" {
		t.Errorf("share_with_mesh = %q, want the persisted choice (shared)", got.ShareWithMesh)
	}
}

// postShare POSTs one share verb and decodes the response body.
func postShare(t *testing.T, srv *Server, verb string) ShareStateResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/waired/v1/inference/share/"+verb, nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: code=%d body=%s", verb, rec.Code, rec.Body.String())
	}
	var got ShareStateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("%s: %v", verb, err)
	}
	return got
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
