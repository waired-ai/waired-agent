package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/runtime"
)

func newTokenedGateway(t *testing.T, token string) *Server {
	t.Helper()
	reg := runtime.NewRegistry()
	return NewServer(ServerConfig{}, Deps{
		Selector:       &fakeSelector{},
		Runtimes:       reg,
		ListManifests:  func() []catalog.Manifest { return nil },
		HTTPClient:     http.DefaultClient,
		AllowOpenAI:    true,
		AllowAnthropic: true,
		AuthToken:      token,
	})
}

func TestAuth_Disabled_NoTokenRequired(t *testing.T) {
	gw := newTokenedGateway(t, "")
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)
	if w.Code == http.StatusUnauthorized {
		t.Fatalf("empty AuthToken should not enforce auth; got 401")
	}
}

func TestAuth_RejectsMissingHeader(t *testing.T) {
	gw := newTokenedGateway(t, strings.Repeat("a", 64))
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing Authorization → status = %d, want 401", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "authentication_error") {
		t.Errorf("error body missing type: %s", body)
	}
}

func TestAuth_RejectsWrongScheme(t *testing.T) {
	tok := strings.Repeat("a", 64)
	gw := newTokenedGateway(t, tok)
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("Authorization", "Basic "+tok)
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("Basic scheme not rejected; status = %d", w.Code)
	}
}

func TestAuth_RejectsWrongToken(t *testing.T) {
	tok := strings.Repeat("a", 64)
	gw := newTokenedGateway(t, tok)
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("Authorization", "Bearer "+strings.Repeat("b", 64))
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token not rejected; status = %d", w.Code)
	}
}

func TestAuth_AcceptsCorrectToken(t *testing.T) {
	tok := strings.Repeat("a", 64)
	gw := newTokenedGateway(t, tok)
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)
	if w.Code == http.StatusUnauthorized {
		t.Fatalf("correct token rejected; body: %s", w.Body.String())
	}
}

func TestAuth_LoopbackEnforcedFirst(t *testing.T) {
	// Even when the token is correct, a non-loopback remote address
	// must still be rejected (loopback wraps token in Server.Handler).
	tok := strings.Repeat("a", 64)
	gw := newTokenedGateway(t, tok)
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.RemoteAddr = "203.0.113.7:55555"
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-loopback got status %d, want 403", w.Code)
	}
}
