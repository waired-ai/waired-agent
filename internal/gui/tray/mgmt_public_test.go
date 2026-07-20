package tray

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
)

// capture records what the fake daemon saw so assertions can inspect the
// request line and body the Client actually sent.
type capture struct {
	method string
	path   string
	body   string
}

// publicTestServer spins up an httptest server whose handler runs fn and
// records the first request's method/path/body. The returned Client has
// both its read and write paths pointed at the server.
func publicTestServer(t *testing.T, fn http.HandlerFunc) (*Client, *capture) {
	t.Helper()
	cap := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		cap.method = r.Method
		cap.path = r.URL.Path
		cap.body = string(b)
		fn(w, r)
	}))
	t.Cleanup(srv.Close)
	return newTestClient(srv.URL), cap
}

func TestPublicShareStatus_OK(t *testing.T) {
	c, cap := publicTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSONResp(w, http.StatusOK, management.PublicShareStateResponse{
			State:        string("public"),
			DesiredState: string("public"),
		})
	})
	resp, err := c.PublicShareStatus(context.Background())
	if err != nil {
		t.Fatalf("PublicShareStatus: %v", err)
	}
	if cap.method != http.MethodGet || cap.path != "/waired/v1/public/share" {
		t.Errorf("server saw %s %s, want GET /waired/v1/public/share", cap.method, cap.path)
	}
	if resp.State != "public" || resp.DesiredState != "public" {
		t.Errorf("decoded state=%q desired=%q, want public/public", resp.State, resp.DesiredState)
	}
}

func TestEnablePublicShare_SendsMaxClients(t *testing.T) {
	c, cap := publicTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSONResp(w, http.StatusOK, management.PublicShareStateResponse{State: "public", DesiredState: "public"})
	})
	if _, err := c.EnablePublicShare(context.Background(), 4); err != nil {
		t.Fatalf("EnablePublicShare: %v", err)
	}
	if cap.method != http.MethodPost || cap.path != "/waired/v1/public/share/enable" {
		t.Errorf("server saw %s %s, want POST /waired/v1/public/share/enable", cap.method, cap.path)
	}
	var got struct {
		MaxClients int `json:"max_clients"`
	}
	if err := json.Unmarshal([]byte(cap.body), &got); err != nil {
		t.Fatalf("decode body %q: %v", cap.body, err)
	}
	if got.MaxClients != 4 {
		t.Errorf("max_clients=%d, want 4", got.MaxClients)
	}
}

func TestEnablePublicShare_ZeroSendsZero(t *testing.T) {
	c, cap := publicTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSONResp(w, http.StatusOK, management.PublicShareStateResponse{State: "public", DesiredState: "public"})
	})
	if _, err := c.EnablePublicShare(context.Background(), 0); err != nil {
		t.Fatalf("EnablePublicShare: %v", err)
	}
	// max_clients has no omitempty on the wire struct, so 0 must be sent
	// explicitly (the server reads 0 as "keep CP default").
	if !strings.Contains(cap.body, `"max_clients":0`) {
		t.Errorf("body %q missing explicit max_clients:0", cap.body)
	}
}

func TestDisablePublicShare_OK(t *testing.T) {
	c, cap := publicTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSONResp(w, http.StatusOK, management.PublicShareStateResponse{
			State:        "not_public",
			DesiredState: "not_public",
			Note:         management.PublicShareDisableNote,
		})
	})
	resp, err := c.DisablePublicShare(context.Background())
	if err != nil {
		t.Fatalf("DisablePublicShare: %v", err)
	}
	if cap.method != http.MethodPost || cap.path != "/waired/v1/public/share/disable" {
		t.Errorf("server saw %s %s, want POST /waired/v1/public/share/disable", cap.method, cap.path)
	}
	if resp.Note != management.PublicShareDisableNote {
		t.Errorf("Note=%q, want it to round-trip the disable note", resp.Note)
	}
}

func TestPublicShare_404IsUnsupported(t *testing.T) {
	c, _ := publicTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	})
	if _, err := c.PublicShareStatus(context.Background()); err == nil || err != ErrPublicShareUnsupported {
		t.Errorf("PublicShareStatus 404 → %v, want ErrPublicShareUnsupported", err)
	}
	if _, err := c.EnablePublicShare(context.Background(), 0); err == nil || err != ErrPublicShareUnsupported {
		t.Errorf("EnablePublicShare 404 → %v, want ErrPublicShareUnsupported", err)
	}
	if _, err := c.DisablePublicShare(context.Background()); err == nil || err != ErrPublicShareUnsupported {
		t.Errorf("DisablePublicShare 404 → %v, want ErrPublicShareUnsupported", err)
	}
}

func TestPublicUse_OK(t *testing.T) {
	c, cap := publicTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSONResp(w, http.StatusOK, management.PublicUseResponse{
			Mode:          "auto",
			EffectiveMode: "auto",
			Consented:     true,
		})
	})
	resp, err := c.PublicUse(context.Background())
	if err != nil {
		t.Fatalf("PublicUse: %v", err)
	}
	if cap.method != http.MethodGet || cap.path != "/waired/v1/public/use" {
		t.Errorf("server saw %s %s, want GET /waired/v1/public/use", cap.method, cap.path)
	}
	if resp.Mode != "auto" || !resp.Consented {
		t.Errorf("decoded mode=%q consented=%v, want auto/true", resp.Mode, resp.Consented)
	}
}

func TestSetPublicUse_SendsPointerFields(t *testing.T) {
	c, cap := publicTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSONResp(w, http.StatusOK, management.PublicUseResponse{Mode: "auto"})
	})
	mode := "auto"
	if _, err := c.SetPublicUse(context.Background(), management.PublicUseUpdateRequest{Mode: &mode}); err != nil {
		t.Fatalf("SetPublicUse: %v", err)
	}
	if cap.method != http.MethodPost || cap.path != "/waired/v1/public/use" {
		t.Errorf("server saw %s %s, want POST /waired/v1/public/use", cap.method, cap.path)
	}
	if !strings.Contains(cap.body, `"mode":"auto"`) {
		t.Errorf("body %q missing mode:auto", cap.body)
	}
	if strings.Contains(cap.body, "min_quality_tier") {
		t.Errorf("body %q should omit nil min_quality_tier", cap.body)
	}
}

func TestSetPublicUse_409IsConsentRequired(t *testing.T) {
	c, _ := publicTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSONResp(w, http.StatusConflict, map[string]string{
			"error_code": "consent_required",
			"message":    "accept the current warning first",
		})
	})
	if _, err := c.SetPublicUse(context.Background(), management.PublicUseUpdateRequest{}); err != ErrPublicConsentRequired {
		t.Errorf("SetPublicUse 409 → %v, want ErrPublicConsentRequired", err)
	}
}

func TestPublicWarning_OK(t *testing.T) {
	c, cap := publicTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSONResp(w, http.StatusOK, management.PublicWarningResponse{
			Version:     1,
			Title:       "Use public shared nodes?",
			Text:        "warning body",
			AcceptLabel: "OK — share my machine and start",
			CancelLabel: "Cancel",
		})
	})
	resp, err := c.PublicWarning(context.Background())
	if err != nil {
		t.Fatalf("PublicWarning: %v", err)
	}
	if cap.method != http.MethodGet || cap.path != "/waired/v1/public/warning" {
		t.Errorf("server saw %s %s, want GET /waired/v1/public/warning", cap.method, cap.path)
	}
	if resp.Version != 1 || resp.Title == "" || resp.Text == "" || resp.AcceptLabel == "" || resp.CancelLabel == "" {
		t.Errorf("decoded warning missing a field: %+v", resp)
	}
}

func TestAcceptPublicConsent_SendsVersion(t *testing.T) {
	c, cap := publicTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSONResp(w, http.StatusOK, management.PublicUseResponse{Mode: "auto", Consented: true})
	})
	if _, err := c.AcceptPublicConsent(context.Background(), 1); err != nil {
		t.Fatalf("AcceptPublicConsent: %v", err)
	}
	if cap.method != http.MethodPost || cap.path != "/waired/v1/public/consent" {
		t.Errorf("server saw %s %s, want POST /waired/v1/public/consent", cap.method, cap.path)
	}
	if !strings.Contains(cap.body, `"warning_version":1`) {
		t.Errorf("body %q missing warning_version:1", cap.body)
	}
}

func TestAcceptPublicConsent_409IsVersionMismatch(t *testing.T) {
	c, _ := publicTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSONResp(w, http.StatusConflict, map[string]string{
			"error_code": "warning_version_mismatch",
			"message":    "re-fetch the current text",
		})
	})
	if _, err := c.AcceptPublicConsent(context.Background(), 1); err != ErrPublicWarningVersionMismatch {
		t.Errorf("AcceptPublicConsent 409 → %v, want ErrPublicWarningVersionMismatch", err)
	}
}

func TestPublicUse_404IsUnsupported(t *testing.T) {
	c, _ := publicTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	})
	if _, err := c.PublicUse(context.Background()); err != ErrPublicUseUnsupported {
		t.Errorf("PublicUse 404 → %v, want ErrPublicUseUnsupported", err)
	}
	if _, err := c.PublicWarning(context.Background()); err != ErrPublicUseUnsupported {
		t.Errorf("PublicWarning 404 → %v, want ErrPublicUseUnsupported", err)
	}
	if _, err := c.SetPublicUse(context.Background(), management.PublicUseUpdateRequest{}); err != ErrPublicUseUnsupported {
		t.Errorf("SetPublicUse 404 → %v, want ErrPublicUseUnsupported", err)
	}
	if _, err := c.AcceptPublicConsent(context.Background(), 1); err != ErrPublicUseUnsupported {
		t.Errorf("AcceptPublicConsent 404 → %v, want ErrPublicUseUnsupported", err)
	}
}

// writeJSONResp is a tiny test-local JSON responder (the production
// writeJSON lives in another package).
func writeJSONResp(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
