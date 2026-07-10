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
)

type fakeUpdateCtl struct {
	mu          sync.Mutex
	checkReq    UpdateCheckRequest
	checkResp   UpdateStatus
	statusRes   UpdateStatus
	err         error
	notifyCalls []bool
}

func (f *fakeUpdateCtl) Check(_ context.Context, req UpdateCheckRequest) (UpdateStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkReq = req
	if f.err != nil {
		return UpdateStatus{}, f.err
	}
	return f.checkResp, nil
}

func (f *fakeUpdateCtl) Status(_ context.Context) (UpdateStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return UpdateStatus{}, f.err
	}
	return f.statusRes, nil
}

func (f *fakeUpdateCtl) SetNotify(_ context.Context, enabled bool) (UpdateStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notifyCalls = append(f.notifyCalls, enabled)
	if f.err != nil {
		return UpdateStatus{}, f.err
	}
	st := f.statusRes
	st.NotifyEnabled = enabled
	return st, nil
}

func TestUpdateCheckReturnsStatus(t *testing.T) {
	uc := &fakeUpdateCtl{checkResp: UpdateStatus{
		Phase:          UpdatePhaseAvailable,
		Available:      true,
		CurrentVersion: "1.2.3",
		LatestVersion:  "1.3.0",
		ApplyMethod:    "apt",
	}}
	srv := New(fakeStatus{}, fakePinger{}).WithUpdateController(uc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/waired/v1/update/check",
		strings.NewReader(`{"force":true}`))
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("check: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got UpdateStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Available || got.LatestVersion != "1.3.0" || got.ApplyMethod != "apt" {
		t.Fatalf("unexpected status: %+v", got)
	}
	if !uc.checkReq.Force {
		t.Fatalf("force flag not plumbed: %+v", uc.checkReq)
	}
}

func TestUpdateCheckToleratesEmptyBody(t *testing.T) {
	uc := &fakeUpdateCtl{checkResp: UpdateStatus{Phase: UpdatePhaseIdle, CurrentVersion: "1.2.3"}}
	srv := New(fakeStatus{}, fakePinger{}).WithUpdateController(uc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/waired/v1/update/check", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("empty-body check: code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestUpdateStatusReturnsCachedResult(t *testing.T) {
	uc := &fakeUpdateCtl{statusRes: UpdateStatus{
		Phase:          UpdatePhaseIdle,
		Available:      false,
		CurrentVersion: "1.3.0",
		LatestVersion:  "1.3.0",
	}}
	srv := New(fakeStatus{}, fakePinger{}).WithUpdateController(uc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/update/status", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got UpdateStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Available || got.CurrentVersion != "1.3.0" {
		t.Fatalf("unexpected status: %+v", got)
	}
}

func TestUpdateSettingsPersistsNotify(t *testing.T) {
	uc := &fakeUpdateCtl{statusRes: UpdateStatus{
		Phase:          UpdatePhaseAvailable,
		Available:      true,
		CurrentVersion: "1.2.3",
		LatestVersion:  "1.3.0",
		NotifyEnabled:  true,
	}}
	srv := New(fakeStatus{}, fakePinger{}).WithUpdateController(uc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/waired/v1/update/settings",
		strings.NewReader(`{"notify":false}`))
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("settings: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got UpdateStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.NotifyEnabled {
		t.Errorf("expected notify_enabled=false in response, got %+v", got)
	}
	if len(uc.notifyCalls) != 1 || uc.notifyCalls[0] != false {
		t.Errorf("SetNotify not plumbed: calls=%v", uc.notifyCalls)
	}
}

func TestUpdateSettingsRejectsGET(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{}).WithUpdateController(&fakeUpdateCtl{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/update/settings", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestUpdateSettingsMissingControllerIs404(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{}) // no WithUpdateController
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/waired/v1/update/settings",
		strings.NewReader(`{"notify":true}`))
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 without controller, got %d", rec.Code)
	}
}

func TestUpdateCheckRejectsGET(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{}).WithUpdateController(&fakeUpdateCtl{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/update/check", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestUpdateStatusRejectsPOST(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{}).WithUpdateController(&fakeUpdateCtl{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/waired/v1/update/status", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestUpdateEndpointsMissingControllerAre404(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{}) // no WithUpdateController
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodPost, "/waired/v1/update/check"},
		{http.MethodGet, "/waired/v1/update/status"},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.RemoteAddr = "127.0.0.1:1"
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s: expected 404 without controller, got %d", tc.method, tc.path, rec.Code)
		}
	}
}

func TestUpdateCheckPropagatesError(t *testing.T) {
	uc := &fakeUpdateCtl{err: errors.New("github unreachable")}
	srv := New(fakeStatus{}, fakePinger{}).WithUpdateController(uc)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/waired/v1/update/check", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "github unreachable") {
		t.Errorf("expected error body, got %s", rec.Body.String())
	}
}
