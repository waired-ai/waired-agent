package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/runtime"
)

// newPlainGateway is the production shape with the browser guard off, which
// is what ServerConfig{} means for this package's own tests: httptest builds
// requests with Host "example.com", and every route test would 403 on the
// Host check otherwise.
func newPlainGateway(t *testing.T) *Server {
	t.Helper()
	return NewServer(ServerConfig{}, Deps{
		Selector:       &fakeSelector{},
		Runtimes:       runtime.NewRegistry(),
		ListManifests:  func() []catalog.Manifest { return nil },
		HTTPClient:     http.DefaultClient,
		AllowOpenAI:    true,
		AllowAnthropic: true,
	})
}

// TestGateway_ServesWithoutAnyCredential replaces the five TestAuth_* cases
// this file's predecessor carried. The listener used to answer 401 to a
// request with no Authorization header, and the whole surface of that
// behaviour — the header, the Bearer scheme, the constant-time compare — is
// gone with waired-ai/waired#1277.
//
// Sending an Authorization header must not change the answer either: a
// client that still has one configured from an older install is not an
// error, it is just a header nothing reads.
func TestGateway_ServesWithoutAnyCredential(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
	}{
		{"no header", ""},
		{"a bearer nothing issued", "Bearer whatever-a-stale-config-still-has"},
		{"a malformed scheme", "Basic Zm9vOmJhcg=="},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw := newPlainGateway(t)
			r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			r.RemoteAddr = "127.0.0.1:1"
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			w := httptest.NewRecorder()
			gw.Handler().ServeHTTP(w, r)
			if w.Code == http.StatusUnauthorized {
				t.Fatalf("got 401 — the gateway must carry no credential; body=%s", w.Body.String())
			}
			if w.Code != http.StatusOK {
				t.Fatalf("got %d, want 200; body=%s", w.Code, w.Body.String())
			}
		})
	}
}
