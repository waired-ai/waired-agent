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
