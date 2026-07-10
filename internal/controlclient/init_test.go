package controlclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/devicekeys"
)

// TestRunInit_PollHeaderSwitchesByHTTPClient pins the contract that the
// poll request rides X-Waired-Poll-Token when the caller provides a
// custom HTTPClient (the bypass-mode path), and Authorization otherwise.
// Cloud Run IAM occupies Authorization, so multiplexing onto a custom
// header is the only way to keep both auth layers happy.
func TestRunInit_PollHeaderSwitchesByHTTPClient(t *testing.T) {
	cases := []struct {
		name           string
		useHTTPClient  bool
		wantHeader     string
		wantOtherEmpty string
	}{
		{
			name:           "default uses Authorization",
			useHTTPClient:  false,
			wantHeader:     "Authorization",
			wantOtherEmpty: "X-Waired-Poll-Token",
		},
		{
			name:           "HTTPClient override uses X-Waired-Poll-Token",
			useHTTPClient:  true,
			wantHeader:     "X-Waired-Poll-Token",
			wantOtherEmpty: "Authorization",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newCPMock(t)
			defer rig.srv.Close()

			mk, _ := devicekeys.NewMachineKey()
			nk, _ := devicekeys.NewNodeKey()

			params := InitParams{
				ControlURL:    rig.srv.URL,
				DeviceName:    "test-device",
				Platform:      "linux",
				Arch:          "amd64",
				ClientVersion: "0.1.0-test",
				Endpoint:      "udp4:127.0.0.1:51820",
				MachineKey:    mk,
				NodeKey:       nk,
				PollInterval:  10 * time.Millisecond,
				PollTimeout:   2 * time.Second,
			}
			if tc.useHTTPClient {
				params.HTTPClient = &http.Client{Timeout: 5 * time.Second}
			}

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			res, err := RunInit(ctx, params)
			if err != nil {
				t.Fatalf("RunInit: %v", err)
			}
			if res.DeviceID == "" {
				t.Fatalf("expected device_id")
			}
			if got := atomic.LoadInt32(&rig.pollCount); got == 0 {
				t.Fatalf("expected at least one poll, got 0")
			}
			if got := rig.lastPollHeader(tc.wantHeader); got == "" {
				t.Fatalf("expected non-empty %s on last poll", tc.wantHeader)
			}
			if other := rig.lastPollHeader(tc.wantOtherEmpty); other != "" {
				t.Fatalf("expected empty %s on last poll, got %q", tc.wantOtherEmpty, other)
			}
		})
	}
}

// cpMock is a minimal CP fake covering the three RunInit endpoints.
type cpMock struct {
	srv               *httptest.Server
	mu                sync.Mutex
	lastPollHeaderMap map[string]string

	pollCount int32
}

func (r *cpMock) lastPollHeader(name string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastPollHeaderMap[name]
}

func newCPMock(t *testing.T) *cpMock {
	t.Helper()
	rig := &cpMock{
		lastPollHeaderMap: map[string]string{},
	}
	mux := http.NewServeMux()
	const sessionID = "ls_test_0001"
	const pollToken = "waired_poll_test_0001"

	mux.HandleFunc("POST /v1/auth/login-sessions", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"login_session_id":      sessionID,
			"login_url":             "http://placeholder/login/" + sessionID,
			"user_code":             "TEST-CODE",
			"poll_token":            pollToken,
			"expires_at":            time.Now().Add(time.Minute).UTC().Format(time.RFC3339),
			"poll_interval_seconds": 1,
		})
	})

	mux.HandleFunc("GET /v1/auth/login-sessions/{id}", func(w http.ResponseWriter, req *http.Request) {
		atomic.AddInt32(&rig.pollCount, 1)
		rig.mu.Lock()
		rig.lastPollHeaderMap["Authorization"] = req.Header.Get("Authorization")
		rig.lastPollHeaderMap["X-Waired-Poll-Token"] = req.Header.Get("X-Waired-Poll-Token")
		rig.mu.Unlock()

		// Verify the poll token actually matches, regardless of which
		// header carried it; reject otherwise.
		got := req.Header.Get("X-Waired-Poll-Token")
		if got == "" {
			authz := req.Header.Get("Authorization")
			if strings.HasPrefix(authz, "Bearer ") {
				got = strings.TrimPrefix(authz, "Bearer ")
			}
		}
		if got != pollToken {
			http.Error(w, "bad poll token", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":              "authorized",
			"registration_ticket": "rt_test_0001",
			"account_email":       "alice@example.com",
			"network_id":          "nw_test",
			"network_name":        "alice",
		})
	})

	mux.HandleFunc("POST /v1/devices/enroll/complete", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_id":                      "dev_test_0001",
			"network_id":                     "nw_test",
			"account_id":                     "acct_test",
			"overlay_ip":                     "100.64.0.2",
			"device_certificate":             json.RawMessage(`{"placeholder":true}`),
			"device_access_token":            "dat_test_0001",
			"device_access_token_expires_at": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
			"device_auth_expires_at":         time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
			"control_signing_public_key":     "",
		})
	})

	rig.srv = httptest.NewServer(mux)
	return rig
}

// TestRunInit_NonJSONResponseFriendlyError pins the contract that pointing
// --control at a host that is not a Control Plane API — one that answers the
// login-session POST with an HTML page (even at HTTP 200, e.g. a web SPA's
// index.html catch-all or a load balancer) — surfaces a diagnosable error
// instead of the raw `invalid character '<'` JSON decode failure that bit a
// user who passed https://dev.waired.net instead of https://app.dev.waired.net.
func TestRunInit_NonJSONResponseFriendlyError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/auth/login-sessions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<!DOCTYPE html>\n<html><head><title>Waired</title></head><body>app</body></html>"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mk, _ := devicekeys.NewMachineKey()
	nk, _ := devicekeys.NewNodeKey()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := RunInit(ctx, InitParams{
		ControlURL:    srv.URL,
		DeviceName:    "test-device",
		Platform:      "linux",
		Arch:          "amd64",
		ClientVersion: "0.1.0-test",
		Endpoint:      "udp4:127.0.0.1:51820",
		MachineKey:    mk,
		NodeKey:       nk,
		PollInterval:  10 * time.Millisecond,
		PollTimeout:   2 * time.Second,
	})
	if err == nil {
		t.Fatal("expected an error when the Control Plane returns HTML, got nil")
	}
	msg := err.Error()
	if strings.Contains(msg, "invalid character") {
		t.Fatalf("error still leaks the raw JSON decode failure: %v", err)
	}
	for _, want := range []string{"non-JSON", "--control", "app.dev.waired.net"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q missing expected hint %q", msg, want)
		}
	}
}

func TestLooksLikeHTML(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		body        string
		want        bool
	}{
		{"json object", "application/json", `{"login_session_id":"x"}`, false},
		{"json array", "application/json", `[1,2,3]`, false},
		{"empty body", "application/json", ``, false},
		{"doctype html", "text/html; charset=utf-8", "<!DOCTYPE html><html></html>", true},
		{"leading whitespace then tag", "", "  \n\t<html>", true},
		{"html content-type only", "text/html", "anything", true},
		{"xhtml content-type", "application/xhtml+xml", "x", true},
		{"json content-type but html body", "application/json", "<html>oops</html>", true},
		{"plain text non-tag", "text/plain", "hello", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := looksLikeHTML(tc.contentType, []byte(tc.body)); got != tc.want {
				t.Fatalf("looksLikeHTML(%q, %q) = %v, want %v", tc.contentType, tc.body, got, tc.want)
			}
		})
	}
}
