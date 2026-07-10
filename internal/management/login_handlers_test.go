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

type fakeLoginCtl struct {
	mu        sync.Mutex
	startReq  LoginStartRequest
	statusID  string
	startResp LoginStatus
	statusRes LoginStatus
	err       error
}

func (f *fakeLoginCtl) Start(ctx context.Context, req LoginStartRequest) (LoginStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startReq = req
	if f.err != nil {
		return LoginStatus{}, f.err
	}
	return f.startResp, nil
}

func (f *fakeLoginCtl) Status(ctx context.Context, sessionID string) (LoginStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusID = sessionID
	if f.err != nil {
		return LoginStatus{}, f.err
	}
	return f.statusRes, nil
}

func TestLoginStartReturnsStatus(t *testing.T) {
	lc := &fakeLoginCtl{startResp: LoginStatus{SessionID: "sess-1", Phase: LoginPhaseLoggingIn}}
	srv := New(fakeStatus{}, fakePinger{}).WithLogin(lc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/waired/v1/login/start",
		strings.NewReader(`{"control_url":"https://cp.example"}`))
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("start: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got LoginStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.SessionID != "sess-1" || got.Phase != LoginPhaseLoggingIn {
		t.Fatalf("unexpected status: %+v", got)
	}
	if lc.startReq.ControlURL != "https://cp.example" {
		t.Fatalf("control_url not plumbed: %+v", lc.startReq)
	}
}

func TestLoginStartToleratesEmptyBody(t *testing.T) {
	lc := &fakeLoginCtl{startResp: LoginStatus{SessionID: "sess-2", Phase: LoginPhaseLoggingIn}}
	srv := New(fakeStatus{}, fakePinger{}).WithLogin(lc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/waired/v1/login/start", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("empty-body start: code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestLoginStatusPlumbsSessionQuery(t *testing.T) {
	lc := &fakeLoginCtl{statusRes: LoginStatus{
		SessionID:    "sess-3",
		Phase:        LoginPhaseActive,
		AccountEmail: "user@example.com",
	}}
	srv := New(fakeStatus{}, fakePinger{}).WithLogin(lc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/login/status?session=sess-3", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if lc.statusID != "sess-3" {
		t.Fatalf("session query not plumbed: %q", lc.statusID)
	}
	var got LoginStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Phase != LoginPhaseActive || got.AccountEmail != "user@example.com" {
		t.Fatalf("unexpected status: %+v", got)
	}
}

func TestLoginStartRejectsGET(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{}).WithLogin(&fakeLoginCtl{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/login/start", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestLoginStatusRejectsPOST(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{}).WithLogin(&fakeLoginCtl{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/waired/v1/login/status", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestLoginEndpointsMissingControllerAre404(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{}) // no WithLogin
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodPost, "/waired/v1/login/start"},
		{http.MethodGet, "/waired/v1/login/status"},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.RemoteAddr = "127.0.0.1:1"
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s: expected 404 without WithLogin, got %d", tc.method, tc.path, rec.Code)
		}
	}
}

func TestLoginStartPropagatesError(t *testing.T) {
	lc := &fakeLoginCtl{err: errors.New("control plane unreachable")}
	srv := New(fakeStatus{}, fakePinger{}).WithLogin(lc)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/waired/v1/login/start", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "control plane unreachable") {
		t.Errorf("expected error body, got %s", rec.Body.String())
	}
}
