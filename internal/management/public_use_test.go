package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
)

func newPublicUseTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "inference", agentconfig.PublicUseFileName)
	s := New(stubStatus{}, stubPinger{}).WithPublicUse(&PublicUseConfig{Path: path})
	return s, path
}

func doGetRaw(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestPublicWarning_ServesVersionedText(t *testing.T) {
	s, _ := newPublicUseTestServer(t)
	w := doGetRaw(t, s, "/waired/v1/public/warning")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp PublicWarningResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Version != PublicShareWarningVersion {
		t.Fatalf("version = %d, want %d", resp.Version, PublicShareWarningVersion)
	}
	// The §14-approved text is served as data — pin load-bearing lines
	// so an accidental edit fails loudly (the exact wording carries the
	// consent's meaning).
	for _, want := range []string{
		"Public nodes are other people's computers.",
		"Do not send secrets or private data",
		"More: docs.waired.ai/public-share",
	} {
		if !strings.Contains(resp.Text, want) {
			t.Fatalf("warning text missing %q:\n%s", want, resp.Text)
		}
	}
	if resp.Title == "" || resp.AcceptLabel == "" || resp.CancelLabel == "" {
		t.Fatalf("title/labels must be non-empty: %+v", resp)
	}
	// POST is not allowed on the warning route.
	if w := doPostJSON(t, s, "/waired/v1/public/warning", map[string]any{}); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST warning status = %d, want 405", w.Code)
	}
}

func TestPublicUse_SettingsRequireConsent(t *testing.T) {
	s, _ := newPublicUseTestServer(t)

	// Fresh state: off, unconsented.
	w := doGetRaw(t, s, "/waired/v1/public/use")
	if w.Code != http.StatusOK {
		t.Fatalf("GET status = %d", w.Code)
	}
	var resp PublicUseResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Consented || resp.Mode != agentconfig.PublicUseModeOff || resp.EffectiveMode != agentconfig.PublicUseModeOff {
		t.Fatalf("fresh state = %+v, want off/unconsented", resp)
	}

	// Settings write before consent → 409, never a silent enable.
	mode := agentconfig.PublicUseModeExplicit
	w = doPostJSON(t, s, "/waired/v1/public/use", PublicUseUpdateRequest{Mode: &mode})
	if w.Code != http.StatusConflict {
		t.Fatalf("pre-consent POST status = %d, want 409 (body=%s)", w.Code, w.Body.String())
	}
}

func TestPublicConsent_FlowAndDefaults(t *testing.T) {
	s, path := newPublicUseTestServer(t)

	// Wrong version → 409 and nothing persisted.
	w := doPostJSON(t, s, "/waired/v1/public/consent", PublicConsentRequest{WarningVersion: PublicShareWarningVersion + 1})
	if w.Code != http.StatusConflict {
		t.Fatalf("stale-version consent status = %d, want 409", w.Code)
	}
	if _, ok, _ := agentconfig.LoadPublicUse(path); ok {
		t.Fatalf("failed consent must not create the state file")
	}

	// Correct version → consent recorded + §4.2 defaults applied
	// (auto, main+sub on, no tier threshold).
	w = doPostJSON(t, s, "/waired/v1/public/consent", PublicConsentRequest{WarningVersion: PublicShareWarningVersion})
	if w.Code != http.StatusOK {
		t.Fatalf("consent status = %d body=%s", w.Code, w.Body.String())
	}
	var resp PublicUseResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !resp.Consented || resp.Mode != agentconfig.PublicUseModeAuto ||
		resp.EffectiveMode != agentconfig.PublicUseModeAuto ||
		!resp.Main || !resp.Sub || resp.MinQualityTier != 0 {
		t.Fatalf("post-consent state = %+v, want auto defaults", resp)
	}

	// Now settings apply, partially (unset fields unchanged).
	tier := 4
	sub := false
	w = doPostJSON(t, s, "/waired/v1/public/use", PublicUseUpdateRequest{MinQualityTier: &tier, Sub: &sub})
	if w.Code != http.StatusOK {
		t.Fatalf("settings status = %d body=%s", w.Code, w.Body.String())
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Mode != agentconfig.PublicUseModeAuto || resp.MinQualityTier != 4 || !resp.Main || resp.Sub {
		t.Fatalf("partial update = %+v", resp)
	}

	// Invalid mode value → 400.
	bad := "always"
	if w := doPostJSON(t, s, "/waired/v1/public/use", PublicUseUpdateRequest{Mode: &bad}); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid mode status = %d, want 400", w.Code)
	}

	// Re-consent (same version, e.g. re-running the flow) keeps the
	// user's settings — defaults are first-consent only.
	w = doPostJSON(t, s, "/waired/v1/public/consent", PublicConsentRequest{WarningVersion: PublicShareWarningVersion})
	if w.Code != http.StatusOK {
		t.Fatalf("re-consent status = %d", w.Code)
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.MinQualityTier != 4 || resp.Sub {
		t.Fatalf("re-consent must keep settings, got %+v", resp)
	}
}

func TestPublicUse_UnconfiguredRoutesAbsent(t *testing.T) {
	s := New(stubStatus{}, stubPinger{}) // no WithPublicUse
	if w := doGetRaw(t, s, "/waired/v1/public/use"); w.Code != http.StatusNotFound {
		t.Fatalf("unconfigured GET status = %d, want 404", w.Code)
	}
}

// The OnChange hook is the router's cache-invalidation signal
// (waired#827): it must fire exactly once per successful write of
// public_use.json, and never on a rejected request — a stale-but-valid
// cached policy is better than one refreshed from a write that did not
// happen.
func TestPublicUse_OnChangeFiresOnlyOnSuccessfulWrite(t *testing.T) {
	for _, tc := range []struct {
		name       string
		preConsent bool
		path       string
		body       string
		wantStatus int
		wantCalls  int
	}{
		{
			name: "settings update", preConsent: true, path: "/waired/v1/public/use",
			body: `{"mode":"off"}`, wantStatus: http.StatusOK, wantCalls: 1,
		},
		{
			name: "settings update rejected by validation", preConsent: true, path: "/waired/v1/public/use",
			body: `{"mode":"nonsense"}`, wantStatus: http.StatusBadRequest, wantCalls: 0,
		},
		{
			name: "settings update before consent", path: "/waired/v1/public/use",
			body: `{"mode":"auto"}`, wantStatus: http.StatusConflict, wantCalls: 0,
		},
		{
			name: "consent accepted", path: "/waired/v1/public/consent",
			body: `{"warning_version":1}`, wantStatus: http.StatusOK, wantCalls: 1,
		},
		{
			name: "consent version mismatch", path: "/waired/v1/public/consent",
			body: `{"warning_version":99}`, wantStatus: http.StatusConflict, wantCalls: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := New(stubStatus{}, stubPinger{}).WithPublicUse(&PublicUseConfig{
				Path:     filepath.Join(t.TempDir(), "inference", agentconfig.PublicUseFileName),
				OnChange: func() { calls++ },
			})
			if tc.preConsent {
				// The settings route is gated on consent; accept it first
				// and discount that write from the assertion below.
				cw := httptest.NewRecorder()
				cr := httptest.NewRequest(http.MethodPost, "/waired/v1/public/consent",
					strings.NewReader(`{"warning_version":1}`))
				cr.RemoteAddr = "127.0.0.1:1"
				srv.Handler().ServeHTTP(cw, cr)
				if cw.Code != http.StatusOK {
					t.Fatalf("consent setup failed: %d %s", cw.Code, cw.Body.String())
				}
				calls = 0
			}
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			r.RemoteAddr = "127.0.0.1:1"
			srv.Handler().ServeHTTP(w, r)

			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", w.Code, tc.wantStatus, w.Body.String())
			}
			if calls != tc.wantCalls {
				t.Fatalf("OnChange calls = %d, want %d", calls, tc.wantCalls)
			}
		})
	}
}
