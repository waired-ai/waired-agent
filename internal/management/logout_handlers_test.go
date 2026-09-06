package management

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeLogoutController struct {
	req  LogoutRequest
	resp LogoutResponse
	err  error
}

func (f *fakeLogoutController) Logout(_ context.Context, req LogoutRequest) (LogoutResponse, error) {
	f.req = req
	return f.resp, f.err
}

func postLogout(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(http.MethodPost, "/waired/v1/logout", nil)
	} else {
		r = httptest.NewRequest(http.MethodPost, "/waired/v1/logout", strings.NewReader(body))
	}
	// loopbackOnly wraps the mux; httptest's default RemoteAddr is not a
	// loopback address, so without this every case would assert a 403 it did
	// not mean.
	r.RemoteAddr = "127.0.0.1:55555"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	return w
}

// A daemon with no logout controller answers 404 — the same answer an agent
// that predates the route gives. Clients key their fallback on it, so it has
// to stay a 404 and not become a 501 or a 200 with an empty body.
func TestLogoutIsNotFoundWithoutAController(t *testing.T) {
	srv := newServer(Status{}, fakePinger{})
	if got := postLogout(t, srv, "").Code; got != http.StatusNotFound {
		t.Errorf("POST /logout with no controller = %d, want 404", got)
	}
}

func TestLogoutRejectsNonPost(t *testing.T) {
	srv := newServer(Status{}, fakePinger{}).WithLogout(&fakeLogoutController{})
	r := httptest.NewRequest(http.MethodGet, "/waired/v1/logout", nil)
	r.RemoteAddr = "127.0.0.1:55555"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /logout = %d, want 405", w.Code)
	}
}

// An empty body means an ordinary, recoverable sign-out — the app sends one.
func TestLogoutAcceptsAnEmptyBody(t *testing.T) {
	f := &fakeLogoutController{resp: LogoutResponse{Deauthed: true}}
	srv := newServer(Status{}, fakePinger{}).WithLogout(f)

	w := postLogout(t, srv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("POST /logout = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if f.req.Revoke {
		t.Error("an empty body was decoded as a revoke; a person signing out of the app must not lose the device row")
	}
	var got LogoutResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Enrolled {
		t.Error("response says still enrolled after a completed sign-out")
	}
	if !got.Deauthed {
		t.Error("Deauthed did not survive the round trip")
	}
}

func TestLogoutCarriesRevokeThrough(t *testing.T) {
	f := &fakeLogoutController{}
	srv := newServer(Status{}, fakePinger{}).WithLogout(f)
	if got := postLogout(t, srv, `{"revoke":true}`).Code; got != http.StatusOK {
		t.Fatalf("POST /logout = %d, want 200", got)
	}
	if !f.req.Revoke {
		t.Error("revoke:true did not reach the controller")
	}
}

// A sign-in in flight is a 409, not a 500: the caller is being told to wait,
// not that something broke.
func TestLogoutMapsLoginInFlightTo409(t *testing.T) {
	srv := newServer(Status{}, fakePinger{}).
		WithLogout(&fakeLogoutController{err: ErrLoginInFlight})
	if got := postLogout(t, srv, "").Code; got != http.StatusConflict {
		t.Errorf("POST /logout while signing in = %d, want 409", got)
	}
}

func TestLogoutMapsOtherErrorsTo500(t *testing.T) {
	srv := newServer(Status{}, fakePinger{}).
		WithLogout(&fakeLogoutController{err: errors.New("disk on fire")})
	if got := postLogout(t, srv, "").Code; got != http.StatusInternalServerError {
		t.Errorf("POST /logout with a failing controller = %d, want 500", got)
	}
}

// TestLogoutIsRefusedOverLoopbackTCP is the one that matters for who may sign
// this machine out.
//
// The route removes credentials, so it must be reachable only over the local
// IPC socket — never from the loopback TCP port, which anything on the host
// (including a browser) can reach. writeGuard already does that for every
// mutating verb; this pins that /logout is not somehow exempted, because the
// cost of finding out later is a device signed out by a web page.
func TestLogoutIsRefusedOverLoopbackTCP(t *testing.T) {
	srv := newServer(Status{DeviceName: "alice"}, fakePinger{}).
		WithLogout(&fakeLogoutController{}).
		WithSocketWritesOnly(true)
	srv.socketUp.Store(true)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, addr) }()

	base := "http://" + addr
	client := &http.Client{Timeout: 5 * time.Second}
	waitForServer(t, client, base+"/waired/v1/status")

	resp, err := client.Post(base+"/waired/v1/logout", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /logout: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST /logout over TCP: got %d want 403 — the sign-out route must be socket-only", resp.StatusCode)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve returned %v", err)
	}
}
