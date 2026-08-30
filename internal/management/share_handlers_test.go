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
	current   state.SharingState
	desired   state.SharingState
	suspended bool
	err       error
	// suspends/unsuspends count the calls so a test can prove the
	// session override was driven, not just its side effect.
	suspends   int
	unsuspends int
	// The control plane's settings, which this controller only reports.
	mesh      state.MeshShareState
	public    state.SharingState
	publicMax int
}

func newFakeShareCtl(initial state.SharingState) *fakeShareCtl {
	return &fakeShareCtl{current: initial, desired: initial}
}

func (f *fakeShareCtl) Share(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = ctx
	if f.err != nil {
		return f.err
	}
	f.current = state.SharingOn
	f.desired = state.SharingOn
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
	f.current = state.SharingOff
	f.desired = state.SharingOff
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

// The console's settings, reported read-only so one status call can
// answer who this computer serves (waired#1297).
func (f *fakeShareCtl) MeshShare() state.MeshShareState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mesh
}

func (f *fakeShareCtl) PublicShare() state.SharingState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.public
}

func (f *fakeShareCtl) PublicMaxClients() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.publicMax
}

func (f *fakeShareCtl) State() (state.SharingState, state.SharingState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.suspended {
		return state.SharingOff, f.desired
	}
	return f.current, f.desired
}

func TestShareControlEndpointFlipsState(t *testing.T) {
	sc := newFakeShareCtl(state.SharingOn)
	srv := New(fakeStatus{}, fakePinger{}).WithShareControl(sc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/waired/v1/sharing/disable", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got ShareStateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.State != "off" || got.DesiredState != "off" {
		t.Fatalf("after disable: %+v", got)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/waired/v1/sharing/enable", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.State != "on" || got.DesiredState != "on" {
		t.Fatalf("after enable: %+v", got)
	}
}

// TestShareSuspendEndpointWithholdsWithoutChangingDesired pins the
// PRODUCT CONTRACT behind the tray's Quit path (#316): suspending stops
// peers from being served right now, while desired_state keeps recording
// what the operator actually chose. Reusing /sharing/disable here would
// persist "off" and silently revoke the preference for good.
func TestShareSuspendEndpointWithholdsWithoutChangingDesired(t *testing.T) {
	sc := newFakeShareCtl(state.SharingOn)
	srv := New(fakeStatus{}, fakePinger{}).WithShareControl(sc)

	got := postShare(t, srv, "suspend")
	if got.State != "off" {
		t.Errorf("state after suspend = %q, want off", got.State)
	}
	if got.DesiredState != "on" {
		t.Errorf("desired_state after suspend = %q, want the operator's choice to survive", got.DesiredState)
	}
	if !got.Suspended {
		t.Errorf("suspended flag not reported: %+v", got)
	}
	if sc.suspends != 1 {
		t.Errorf("Suspend calls = %d, want 1", sc.suspends)
	}

	got = postShare(t, srv, "unsuspend")
	if got.State != "on" || got.DesiredState != "on" {
		t.Errorf("after unsuspend: %+v, want sharing restored", got)
	}
	if got.Suspended {
		t.Errorf("suspended flag still set after unsuspend: %+v", got)
	}
}

// A suspension must not resurrect sharing the operator turned off: the
// override only ever withholds.
func TestShareUnsuspendKeepsOperatorOff(t *testing.T) {
	sc := newFakeShareCtl(state.SharingOff)
	srv := New(fakeStatus{}, fakePinger{}).WithShareControl(sc)

	_ = postShare(t, srv, "suspend")
	got := postShare(t, srv, "unsuspend")
	if got.State != "off" || got.DesiredState != "off" {
		t.Errorf("after unsuspend: %+v, want the persisted off to stand", got)
	}
}

func TestShareSuspendEndpointRejectsGET(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{}).WithShareControl(newFakeShareCtl(state.SharingOn))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/sharing/suspend", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestShareSuspendEndpointMissingControllerIs404(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{}) // no WithShareControl
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/waired/v1/sharing/suspend", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when no ShareController, got %d", rec.Code)
	}
}

func postShare(t *testing.T, srv *Server, verb string) ShareStateResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/waired/v1/sharing/"+verb, nil)
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
	srv := New(fakeStatus{}, fakePinger{}).WithShareControl(newFakeShareCtl(state.SharingOn))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/sharing/disable", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestShareControlEndpointMissingControllerIs404(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{}) // no WithShareControl
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/waired/v1/sharing/disable", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when no ShareController, got %d", rec.Code)
	}
}

func TestShareControlEndpointPropagatesError(t *testing.T) {
	sc := newFakeShareCtl(state.SharingOn)
	sc.err = errors.New("disk full")
	srv := New(fakeStatus{}, fakePinger{}).WithShareControl(sc)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/waired/v1/sharing/disable", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "disk full") {
		t.Errorf("expected error body to mention disk full, got %s", rec.Body.String())
	}
}

// TestSharingStatusReportsWhoThisComputerServes pins the read side
// (waired#1297): one GET answers whether this computer lends itself out
// AND what the console has it shared with, so a person diagnosing "why
// is nothing routed here" does not have to guess which of the two they
// are looking at.
func TestSharingStatusReportsWhoThisComputerServes(t *testing.T) {
	sc := newFakeShareCtl(state.SharingOn)
	sc.mesh = state.MeshShareOff
	sc.public = state.SharingOn
	sc.publicMax = 3
	srv := New(fakeStatus{}, fakePinger{}).WithShareControl(sc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/sharing", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got ShareStateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.State != "on" || got.MeshShare != "off" || got.PublicShare != "on" || got.PublicMaxClients != 3 {
		t.Fatalf("status did not carry the whole picture: %+v", got)
	}
}

// The status route is a read, and a daemon without a sharing controller
// answers 404 so the caller can hide the row rather than render an
// error — the same treatment the transition routes get.
func TestSharingStatusMissingControllerIs404(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/sharing", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d, want 404", rec.Code)
	}
}

// Empty is not "off". Before the first signed map of this run the
// console's settings are unknown, and reporting them as off would send a
// reader looking for a switch nobody moved.
func TestSharingStatusOmitsSettingsItHasNotHeard(t *testing.T) {
	sc := newFakeShareCtl(state.SharingOn)
	srv := New(fakeStatus{}, fakePinger{}).WithShareControl(sc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/sharing", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "mesh_share") || strings.Contains(rec.Body.String(), "public_share") {
		t.Fatalf("an unheard setting was reported: %s", rec.Body.String())
	}
}
