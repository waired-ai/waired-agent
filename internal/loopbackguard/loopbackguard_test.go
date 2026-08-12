package loopbackguard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// okHandler records that the request reached the other side of the guard.
func okHandler(reached *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reached != nil {
			*reached = true
		}
		w.WriteHeader(http.StatusOK)
	})
}

// recordingReject captures what the guard asked the caller to render, so the
// tests can pin the arguments and not just the status code.
type rejection struct {
	status  int
	code    string
	message string
}

func recordingReject(got *[]rejection) Reject {
	return func(w http.ResponseWriter, _ *http.Request, status int, code, message string) {
		*got = append(*got, rejection{status, code, message})
		http.Error(w, message, status)
	}
}

func loopbackRequest(method, target string, body string) *http.Request {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	req.RemoteAddr = "127.0.0.1:1"
	req.Host = "127.0.0.1:9476"
	return req
}

// TestBrowserDisabledReturnsNextUnchanged pins the property the management
// package's whole test suite rests on: with the guard off, a request a browser
// could forge still reaches the handler. Product contract — the config-gate
// shape is the ruling of waired-ai/waired#836
// (docs/decisions/20260719/0710-mgmt-api-9476-browser-hardening.md in the
// private monorepo, "既存テストは無改修").
func TestBrowserDisabledReturnsNextUnchanged(t *testing.T) {
	var reached bool
	next := okHandler(&reached)
	got := Browser(next, false, Options{RequireJSONContentType: true})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("hi"))
	req.Host = "evil.com:9476"
	req.Header.Set("Origin", "http://evil.com")
	got.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !reached {
		t.Fatalf("disabled guard must pass through; got %d reached=%v", rec.Code, reached)
	}
}

// TestBrowserHost is the Host allow-list. Product contract (waired#836,
// extended to :9472/:9479 by waired#1195): a DNS-rebinding page must put the
// name it rebound into Host, so a loopback allow-list defeats it.
func TestBrowserHost(t *testing.T) {
	cases := []struct {
		host string
		want int
	}{
		{"127.0.0.1:9476", http.StatusOK},
		{"127.0.0.1", http.StatusOK},
		{"localhost:9476", http.StatusOK},
		{"localhost", http.StatusOK},
		{"LOCALHOST:9476", http.StatusOK}, // case-insensitive
		{"[::1]:9476", http.StatusOK},
		{"[::1]", http.StatusOK}, // IPv6 literal, no port
		{"evil.com:9476", http.StatusForbidden},
		{"evil.com", http.StatusForbidden},
		{"169.254.169.254", http.StatusForbidden}, // link-local metadata IP, not loopback
		{"", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.host, func(t *testing.T) {
			var got []rejection
			h := Browser(okHandler(nil), true, Options{Reject: recordingReject(&got)})
			rec := httptest.NewRecorder()
			req := loopbackRequest(http.MethodGet, "/x", "")
			req.Host = tc.host
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("Host %q: got %d want %d", tc.host, rec.Code, tc.want)
			}
			if tc.want == http.StatusForbidden {
				want := rejection{http.StatusForbidden, "forbidden", "invalid Host header"}
				if len(got) != 1 || got[0] != want {
					t.Fatalf("Host %q: Reject called with %+v, want exactly one %+v", tc.host, got, want)
				}
			}
		})
	}
}

// TestBrowserOrigin is the Origin allow-list. An absent Origin is allowed on
// purpose: the CLI, curl, Claude Code and the testnet fallback scripts send
// none (waired#836).
func TestBrowserOrigin(t *testing.T) {
	cases := []struct {
		origin string
		want   int
	}{
		{"", http.StatusOK}, // absent → allowed
		{"http://127.0.0.1:9476", http.StatusOK},
		{"http://localhost:9476", http.StatusOK},
		{"https://127.0.0.1", http.StatusOK},
		{"http://evil.com", http.StatusForbidden},
		{"http://evil.com:9476", http.StatusForbidden},
		{"null", http.StatusForbidden},      // opaque origin
		{"file:///x", http.StatusForbidden}, // non-http scheme
	}
	for _, tc := range cases {
		name := tc.origin
		if name == "" {
			name = "absent"
		}
		t.Run(name, func(t *testing.T) {
			var got []rejection
			h := Browser(okHandler(nil), true, Options{Reject: recordingReject(&got)})
			rec := httptest.NewRecorder()
			req := loopbackRequest(http.MethodGet, "/x", "")
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("Origin %q: got %d want %d", tc.origin, rec.Code, tc.want)
			}
			if tc.want == http.StatusForbidden {
				want := rejection{http.StatusForbidden, "forbidden", "cross-origin request rejected"}
				if len(got) != 1 || got[0] != want {
					t.Fatalf("Origin %q: Reject called with %+v, want exactly one %+v", tc.origin, got, want)
				}
			}
		})
	}
}

// TestBrowserContentTypeWhenRequired covers RequireJSONContentType=true, the
// setting the Local Management API runs with.
func TestBrowserContentTypeWhenRequired(t *testing.T) {
	cases := []struct {
		name string
		ct   string
		want int
	}{
		{"json", "application/json", http.StatusOK},
		{"json-charset", "application/json; charset=utf-8", http.StatusOK},
		{"text-plain", "text/plain", http.StatusUnsupportedMediaType},
		{"form", "application/x-www-form-urlencoded", http.StatusUnsupportedMediaType},
		{"absent", "", http.StatusUnsupportedMediaType},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []rejection
			h := Browser(okHandler(nil), true, Options{
				RequireJSONContentType: true,
				Reject:                 recordingReject(&got),
			})
			rec := httptest.NewRecorder()
			req := loopbackRequest(http.MethodPost, "/x", `{"peer":"bob"}`)
			if tc.ct != "" {
				req.Header.Set("Content-Type", tc.ct)
			}
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("Content-Type %q: got %d want %d", tc.ct, rec.Code, tc.want)
			}
			if tc.want == http.StatusUnsupportedMediaType {
				want := rejection{http.StatusUnsupportedMediaType,
					"unsupported_media_type", "Content-Type: application/json required"}
				if len(got) != 1 || got[0] != want {
					t.Fatalf("Content-Type %q: Reject called with %+v, want exactly one %+v", tc.ct, got, want)
				}
			}
		})
	}
}

// TestBrowserContentTypeMethodScope confirms the Content-Type check only
// applies to bodied write verbs: GET is never checked, and DELETE (the bodyless
// model-delete) is exempt and relies on the Host/Origin checks instead.
func TestBrowserContentTypeMethodScope(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodDelete, http.MethodOptions} {
		t.Run(method, func(t *testing.T) {
			h := Browser(okHandler(nil), true, Options{RequireJSONContentType: true})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, loopbackRequest(method, "/x", ""))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s should bypass the Content-Type check; got %d", method, rec.Code)
			}
		})
	}
}

// TestBrowserContentTypeNotRequiredPassesAnyBody is why the option exists:
// :9472's catch-all reverse-proxies arbitrary Anthropic API calls, multipart
// uploads included, so a JSON requirement there would break them
// (waired-ai/waired#1195). Host and Origin still apply.
func TestBrowserContentTypeNotRequiredPassesAnyBody(t *testing.T) {
	for _, ct := range []string{
		"multipart/form-data; boundary=abc",
		"application/octet-stream",
		"text/plain",
		"",
	} {
		name := ct
		if name == "" {
			name = "absent"
		}
		t.Run(name, func(t *testing.T) {
			var reached bool
			h := Browser(okHandler(&reached), true, Options{}) // RequireJSONContentType off
			rec := httptest.NewRecorder()
			req := loopbackRequest(http.MethodPost, "/v1/files", "binary-ish")
			if ct != "" {
				req.Header.Set("Content-Type", ct)
			}
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK || !reached {
				t.Fatalf("Content-Type %q: got %d reached=%v, want it to pass", ct, rec.Code, reached)
			}
		})
	}
}

// TestBrowserFirstFailureWins pins the order Host → Origin → Content-Type: a
// request that fails several checks is answered for the first one, so the
// response never says "cross-origin" about a rebound Host.
func TestBrowserFirstFailureWins(t *testing.T) {
	var got []rejection
	h := Browser(okHandler(nil), true, Options{
		RequireJSONContentType: true,
		Reject:                 recordingReject(&got),
	})
	rec := httptest.NewRecorder()
	req := loopbackRequest(http.MethodPost, "/x", "hi")
	req.Host = "evil.com"
	req.Header.Set("Origin", "http://evil.com")
	req.Header.Set("Content-Type", "text/plain")
	h.ServeHTTP(rec, req)

	want := rejection{http.StatusForbidden, "forbidden", "invalid Host header"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("Reject called with %+v, want exactly one %+v", got, want)
	}
}

// TestBrowserNilRejectDoesNotPanic covers the fallback renderer: a caller that
// forgets Reject must still get a rejection, not a nil-call panic.
func TestBrowserNilRejectDoesNotPanic(t *testing.T) {
	h := Browser(okHandler(nil), true, Options{})
	rec := httptest.NewRecorder()
	req := loopbackRequest(http.MethodGet, "/x", "")
	req.Host = "evil.com"
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid Host header") {
		t.Fatalf("fallback body = %q, want it to carry the message", rec.Body.String())
	}
}

// TestPeer is the transport-peer check. An unparseable RemoteAddr is rejected
// — record of today's behaviour, and deliberately the opposite of
// internal/gateway's own loopbackOnly, which passes one through.
func TestPeer(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string
		want       int
	}{
		{"ipv4-loopback", "127.0.0.1:1234", http.StatusOK},
		{"ipv4-loopback-other", "127.9.9.9:1234", http.StatusOK},
		{"ipv6-loopback", "[::1]:1234", http.StatusOK},
		{"public", "203.0.113.7:1234", http.StatusForbidden},
		{"private-lan", "192.168.1.5:1234", http.StatusForbidden},
		{"no-port", "127.0.0.1", http.StatusForbidden},
		{"hostname", "localhost:1234", http.StatusForbidden},
		{"empty", "", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []rejection
			var reached bool
			h := Peer(okHandler(&reached), recordingReject(&got))
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.RemoteAddr = tc.remoteAddr
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("RemoteAddr %q: got %d want %d", tc.remoteAddr, rec.Code, tc.want)
			}
			if tc.want == http.StatusForbidden {
				want := rejection{http.StatusForbidden, "forbidden", "loopback only"}
				if len(got) != 1 || got[0] != want {
					t.Fatalf("RemoteAddr %q: Reject called with %+v, want exactly one %+v", tc.remoteAddr, got, want)
				}
				if reached {
					t.Fatalf("RemoteAddr %q: handler was reached despite the rejection", tc.remoteAddr)
				}
			}
		})
	}
}

// TestPeerNilRejectDoesNotPanic mirrors TestBrowserNilRejectDoesNotPanic.
func TestPeerNilRejectDoesNotPanic(t *testing.T) {
	h := Peer(okHandler(nil), nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "203.0.113.7:1234"
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d want 403", rec.Code)
	}
}

// TestGuardsCompose checks the shape the listeners actually mount: Peer
// outermost, so a non-loopback peer is answered before the Host check runs.
func TestGuardsCompose(t *testing.T) {
	var got []rejection
	reject := recordingReject(&got)
	h := Peer(Browser(okHandler(nil), true, Options{Reject: reject}), reject)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "203.0.113.7:1234"
	req.Host = "evil.com"
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d want 403", rec.Code)
	}
	want := rejection{http.StatusForbidden, "forbidden", "loopback only"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("Reject called with %+v, want exactly one %+v", got, want)
	}
}
