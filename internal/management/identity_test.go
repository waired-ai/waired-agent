package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeIdentity struct{ v IdentityView }

func (f fakeIdentity) Identity() IdentityView { return f.v }

func TestIdentityEndpoint_Enrolled(t *testing.T) {
	want := IdentityView{
		Enrolled:     true,
		AccountEmail: "alice@example.com",
		NetworkName:  "alice-net",
		NetworkID:    "net-1",
		DeviceID:     "dev-1",
		DeviceName:   "alice-laptop",
		OverlayIP:    "100.96.0.10",
		ControlURL:   "https://control.example.com",
	}
	srv := New(fakeStatus{}, fakePinger{}).WithIdentity(fakeIdentity{v: want})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/identity", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got IdentityView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != want {
		t.Fatalf("identity mismatch:\n got=%+v\nwant=%+v", got, want)
	}
}

func TestIdentityEndpoint_NotEnrolled(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{}).WithIdentity(fakeIdentity{v: IdentityView{Enrolled: false}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/identity", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got IdentityView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Enrolled {
		t.Fatalf("expected Enrolled=false, got %+v", got)
	}
	if got.AccountEmail != "" || got.DeviceID != "" {
		t.Fatalf("not-enrolled view should leave identity fields empty, got %+v", got)
	}
}

func TestIdentityEndpoint_DisabledByDefault(t *testing.T) {
	// Without WithIdentity, the route must not be registered (404).
	srv := New(fakeStatus{}, fakePinger{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/identity", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when identity provider not wired, got %d", rec.Code)
	}
}

func TestIdentityEndpoint_RejectsNonGET(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{}).WithIdentity(fakeIdentity{v: IdentityView{Enrolled: true}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/waired/v1/identity", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestIdentityEndpoint_RejectsNonLoopback(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{}).WithIdentity(fakeIdentity{v: IdentityView{Enrolled: true}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/identity", nil)
	req.RemoteAddr = "192.0.2.1:54321"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-loopback, got %d", rec.Code)
	}
}
