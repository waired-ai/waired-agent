package tray

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
)

func TestClient_LoginStart_OK(t *testing.T) {
	var sawMethod, sawPath, sawControl string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawMethod, sawPath = r.Method, r.URL.Path
		var req management.LoginStartRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		sawControl = req.ControlURL
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(management.LoginStatus{
			SessionID: "sess-1",
			Phase:     management.LoginPhaseLoggingIn,
		})
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(srv.URL)
	st, err := c.LoginStart(context.Background(), management.LoginStartRequest{ControlURL: "https://cp.example"})
	if err != nil {
		t.Fatal(err)
	}
	if sawMethod != http.MethodPost || sawPath != "/waired/v1/login/start" {
		t.Errorf("server saw %s %s", sawMethod, sawPath)
	}
	if sawControl != "https://cp.example" {
		t.Errorf("control_url not sent: %q", sawControl)
	}
	if st.SessionID != "sess-1" || st.Phase != management.LoginPhaseLoggingIn {
		t.Errorf("unexpected status: %+v", st)
	}
}

func TestClient_LoginStatus_OK(t *testing.T) {
	var sawSession string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawSession = r.URL.Query().Get("session")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(management.LoginStatus{
			SessionID:    "sess-1",
			Phase:        management.LoginPhaseActive,
			AccountEmail: "user@example.com",
		})
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(srv.URL)
	st, err := c.LoginStatus(context.Background(), "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if sawSession != "sess-1" {
		t.Errorf("session query not sent: %q", sawSession)
	}
	if st.Phase != management.LoginPhaseActive || st.AccountEmail != "user@example.com" {
		t.Errorf("unexpected status: %+v", st)
	}
}

// A daemon predating the login API 404s both routes; the tray maps that
// to ErrLoginUnsupported so it can fall back to the legacy pkexec path.
func TestClient_Login_404IsUnsupported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(srv.URL)
	if _, err := c.LoginStart(context.Background(), management.LoginStartRequest{}); !errors.Is(err, ErrLoginUnsupported) {
		t.Errorf("LoginStart: expected ErrLoginUnsupported, got %v", err)
	}
	if _, err := c.LoginStatus(context.Background(), "x"); !errors.Is(err, ErrLoginUnsupported) {
		t.Errorf("LoginStatus: expected ErrLoginUnsupported, got %v", err)
	}
}
