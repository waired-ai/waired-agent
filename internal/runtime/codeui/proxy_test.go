package codeui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// backendStub stands in for `opencode serve` with OPENCODE_SERVER_PASSWORD set:
// it 401s anything without the right Basic creds and records what the proxy
// forwarded.
type backendStub struct {
	user, pass string
	gotAuthOK  bool
	gotCookie  string
}

func (b *backendStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.gotCookie = r.Header.Get("Cookie")
		u, p, ok := r.BasicAuth()
		if !ok || u != b.user || p != b.pass {
			w.Header().Set("WWW-Authenticate", `Basic realm="Secure Area"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		b.gotAuthOK = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

func newProxyTestServer(t *testing.T, auth Authenticator) (*httptest.Server, *backendStub) {
	t.Helper()
	be := &backendStub{user: OpenCodeServerUser, pass: "backend-pw"}
	backend := httptest.NewServer(be.handler())
	t.Cleanup(backend.Close)
	addr := strings.TrimPrefix(backend.URL, "http://")
	front := httptest.NewServer(ProxyHandler(addr, be.pass, auth))
	t.Cleanup(front.Close)
	return front, be
}

// noRedirectClient returns a client that does not follow redirects so the test
// can inspect the 302 + Set-Cookie from the capability-token redemption.
func noRedirectClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

func TestProxy_Token_NoCredentials_401(t *testing.T) {
	front, be := newProxyTestServer(t, NewTokenAuth("cap-secret"))
	resp, err := http.Get(front.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if be.gotAuthOK {
		t.Fatal("backend was reached without a valid proxy session")
	}
	// A token-mode 401 must NOT send a Basic challenge (no password prompt).
	if resp.Header.Get("WWW-Authenticate") != "" {
		t.Errorf("token mode must not send WWW-Authenticate, got %q", resp.Header.Get("WWW-Authenticate"))
	}
}

func TestProxy_Token_WrongToken_401(t *testing.T) {
	front, _ := newProxyTestServer(t, NewTokenAuth("cap-secret"))
	resp, err := noRedirectClient().Get(front.URL + "/?wt=not-the-secret")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestProxy_Token_CapabilityLink_SetsCookieAndStripsToken(t *testing.T) {
	front, _ := newProxyTestServer(t, NewTokenAuth("cap-secret"))
	resp, err := noRedirectClient().Get(front.URL + "/?wt=cap-secret&foo=bar")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if strings.Contains(loc, "wt=") {
		t.Errorf("redirect Location still carries the token: %q", loc)
	}
	if !strings.Contains(loc, "foo=bar") {
		t.Errorf("redirect dropped other query params: %q", loc)
	}
	var set string
	for _, c := range resp.Cookies() {
		if c.Name == CookieName {
			set = c.Value
			if !c.HttpOnly {
				t.Error("session cookie must be HttpOnly")
			}
			if c.SameSite != http.SameSiteStrictMode {
				t.Error("session cookie must be SameSite=Strict")
			}
		}
	}
	if set != "cap-secret" {
		t.Fatalf("session cookie = %q, want the secret", set)
	}
}

func TestProxy_Token_WithCookie_ProxiesAndInjectsBackendAuth(t *testing.T) {
	front, be := newProxyTestServer(t, NewTokenAuth("cap-secret"))
	req, _ := http.NewRequest(http.MethodGet, front.URL+"/", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: "cap-secret"})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !be.gotAuthOK {
		t.Fatal("proxy did not inject backend Basic credentials")
	}
	// The proxy must not leak the session cookie to the opencode process.
	if strings.Contains(be.gotCookie, CookieName) {
		t.Errorf("session cookie leaked to backend: %q", be.gotCookie)
	}
}

func TestProxy_Basic_ChallengeAndAccept(t *testing.T) {
	front, be := newProxyTestServer(t, NewBasicAuth("alice", "hunter2"))

	// No creds → 401 + challenge.
	resp, err := http.Get(front.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Basic ") {
		t.Errorf("missing Basic challenge: %q", resp.Header.Get("WWW-Authenticate"))
	}

	// Correct creds → 200, and the backend got opencode's creds (not alice's).
	req, _ := http.NewRequest(http.MethodGet, front.URL+"/", nil)
	req.SetBasicAuth("alice", "hunter2")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp2.StatusCode)
	}
	if !be.gotAuthOK {
		t.Fatal("backend did not receive injected opencode credentials")
	}

	// Wrong password → 401, backend never reached.
	be.gotAuthOK = false
	req3, _ := http.NewRequest(http.MethodGet, front.URL+"/", nil)
	req3.SetBasicAuth("alice", "wrong")
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp3.StatusCode)
	}
	if be.gotAuthOK {
		t.Fatal("backend reached with wrong proxy credentials")
	}
}

func TestGenerateToken_DistinctHex(t *testing.T) {
	a, err := GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("tokens must be unique")
	}
	if len(a) != 64 {
		t.Fatalf("token len = %d, want 64 hex chars", len(a))
	}
	if _, err := url.QueryUnescape(a); err != nil {
		t.Errorf("token not URL-safe: %v", err)
	}
}
