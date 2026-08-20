package controlclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/devicekeys"
)

// flakyCP is an auth-key control plane whose enrol endpoint fails a set
// number of times before it works, so a test can say what the host saw.
type flakyCP struct {
	srv *httptest.Server
	// enrollFailures is decremented on each enrol call while > 0; those
	// calls answer with enrollStatus instead of enrolling.
	enrollFailures atomic.Int32
	enrollStatus   int
	enrollBody     string
	retryAfter     string
	// hangUpFirst, when > 0, makes that many enrol calls close the
	// connection without a response at all.
	hangUpFirst atomic.Int32
	enrollCalls atomic.Int32
	createCalls atomic.Int32
	createFails atomic.Int32
}

func newFlakyCP(t *testing.T) *flakyCP {
	t.Helper()
	cp := &flakyCP{enrollStatus: http.StatusServiceUnavailable, enrollBody: "Service Unavailable"}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/auth/login-sessions", func(w http.ResponseWriter, _ *http.Request) {
		cp.createCalls.Add(1)
		if cp.createFails.Load() > 0 {
			cp.createFails.Add(-1)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("Service Unavailable"))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"login_session_id":    "ls_flaky",
			"poll_token":          "waired_poll_flaky",
			"expires_at":          time.Now().Add(time.Minute).UTC().Format(time.RFC3339),
			"registration_ticket": "waired_reg_flaky",
			"account_email":       "ops@example.com",
			"network_id":          "nw_flaky",
			"network_name":        "ops",
		})
	})

	mux.HandleFunc("POST /v1/devices/enroll/complete", func(w http.ResponseWriter, r *http.Request) {
		cp.enrollCalls.Add(1)
		if cp.hangUpFirst.Load() > 0 {
			cp.hangUpFirst.Add(-1)
			// Take the connection down mid-request, the way an instance
			// killed while serving does.
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					_ = conn.Close()
					return
				}
			}
			panic("ResponseWriter is not a Hijacker")
		}
		if cp.enrollFailures.Load() > 0 {
			cp.enrollFailures.Add(-1)
			if cp.retryAfter != "" {
				w.Header().Set("Retry-After", cp.retryAfter)
			}
			w.WriteHeader(cp.enrollStatus)
			_, _ = w.Write([]byte(cp.enrollBody))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_id":                      "dev_flaky",
			"network_id":                     "nw_flaky",
			"account_id":                     "acct_flaky",
			"overlay_ip":                     "100.96.0.9",
			"device_certificate":             map[string]any{},
			"device_access_token":            "waired_dat_x",
			"device_access_token_expires_at": time.Now().Add(15 * time.Minute).UTC().Format(time.RFC3339),
			"device_refresh_token":           "waired_drt_x",
			"device_auth_expires_at":         time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
			"node_key_expires_at":            time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
			"control_signing_public_key":     "",
		})
	})

	cp.srv = httptest.NewServer(mux)
	t.Cleanup(cp.srv.Close)
	return cp
}

// params returns an auth-key enrolment wired to this CP. The backoff is
// sub-millisecond so the retry tests do not wait on wall clock; production
// takes DefaultEnrollRetryBackoff.
func (cp *flakyCP) params(t *testing.T) InitParams {
	t.Helper()
	mk, _ := devicekeys.NewMachineKey()
	nk, _ := devicekeys.NewNodeKey()
	return InitParams{
		ControlURL:    cp.srv.URL,
		DeviceName:    "flaky-1",
		Platform:      "linux",
		Arch:          "amd64",
		ClientVersion: "0.1.0-test",
		Endpoint:      "udp4:127.0.0.1:51820",
		MachineKey:    mk,
		NodeKey:       nk,
		AuthKey:       "waired_ak_valid",
		PollInterval:  time.Millisecond,
		PollTimeout:   2 * time.Second,
		RetryBackoff:  []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond},
	}
}

// A control-plane instance that dies mid-request answers every caller with
// 503 and is replaced within seconds (waired#1237). One of those must not
// end an enrolment: before this, a single 503 failed `waired init` outright
// on all three install-test legs.
func TestRunInit_RetriesTransientEnrollFailures(t *testing.T) {
	cp := newFlakyCP(t)
	cp.enrollFailures.Store(2)

	res, err := RunInit(context.Background(), cp.params(t))
	if err != nil {
		t.Fatalf("RunInit: %v", err)
	}
	if res.DeviceID != "dev_flaky" {
		t.Fatalf("device_id = %q, want dev_flaky", res.DeviceID)
	}
	if got := cp.enrollCalls.Load(); got != 3 {
		t.Fatalf("enrol attempts = %d, want 3 (two 503s then the one that worked)", got)
	}
}

// The same for the call that mints the session — it is the other one
// standing between a host and an enrolled device.
func TestRunInit_RetriesTransientCreateFailures(t *testing.T) {
	cp := newFlakyCP(t)
	cp.createFails.Store(2)

	if _, err := RunInit(context.Background(), cp.params(t)); err != nil {
		t.Fatalf("RunInit: %v", err)
	}
	if got := cp.createCalls.Load(); got != 3 {
		t.Fatalf("create attempts = %d, want 3", got)
	}
}

// An instance killed while serving does not always get as far as a status
// line: the connection just ends. That is the same event and gets the same
// treatment.
func TestRunInit_RetriesADroppedConnection(t *testing.T) {
	cp := newFlakyCP(t)
	cp.hangUpFirst.Store(1)

	if _, err := RunInit(context.Background(), cp.params(t)); err != nil {
		t.Fatalf("RunInit: %v", err)
	}
	if got := cp.enrollCalls.Load(); got != 2 {
		t.Fatalf("enrol attempts = %d, want 2", got)
	}
}

// A verdict is not a transient. Retrying one delays the same answer and
// hides it behind a longer wait, so each of these must cost one attempt.
func TestRunInit_DoesNotRetryVerdicts(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		// The control plane consumes the registration ticket inside the
		// enrolment transaction, so a replay of a committed enrol lands
		// here. It has to stop the retry, not restart it.
		{"ticket already consumed", http.StatusGone, "registration_ticket_consumed"},
		{"bad request", http.StatusBadRequest, "missing_endpoint"},
		{"machine signature invalid", http.StatusUnauthorized, "machine_signature_invalid"},
		// 500 means the request was handled and failed deterministically.
		{"internal error", http.StatusInternalServerError, "internal_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cp := newFlakyCP(t)
			cp.enrollStatus, cp.enrollBody = tc.status, tc.body
			cp.enrollFailures.Store(99)

			_, err := RunInit(context.Background(), cp.params(t))
			if err == nil {
				t.Fatal("RunInit succeeded, want the control plane's verdict")
			}
			if got := cp.enrollCalls.Load(); got != 1 {
				t.Fatalf("enrol attempts = %d, want 1", got)
			}
			// The rendered message is the one operators and the install
			// test logs have always seen.
			want := "enroll: status " + strconv.Itoa(tc.status) + ": " + tc.body
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), want)
			}
		})
	}
}

// A URL that is not a control-plane API is a configuration mistake. No
// number of attempts fixes it, and retrying only delays the message that
// says so.
func TestRunInit_DoesNotRetryANonAPIEndpoint(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<!doctype html><html><body>hello</body></html>"))
	}))
	t.Cleanup(srv.Close)

	mk, _ := devicekeys.NewMachineKey()
	nk, _ := devicekeys.NewNodeKey()
	_, err := RunInit(context.Background(), InitParams{
		ControlURL:   srv.URL,
		Endpoint:     "udp4:127.0.0.1:51820",
		MachineKey:   mk,
		NodeKey:      nk,
		RetryBackoff: []time.Duration{time.Millisecond, time.Millisecond},
	})
	if err == nil {
		t.Fatal("RunInit succeeded against an HTML endpoint")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1", got)
	}
	if !strings.Contains(err.Error(), "not a Waired Control Plane API endpoint") {
		t.Fatalf("error = %q, want the misconfigured-URL message", err.Error())
	}
}

// An empty (non-nil) backoff turns retrying off, which is what the
// single-attempt assertions above rely on being distinguishable from the
// default.
func TestRunInit_EmptyBackoffMakesOneAttempt(t *testing.T) {
	cp := newFlakyCP(t)
	cp.enrollFailures.Store(1)
	p := cp.params(t)
	p.RetryBackoff = []time.Duration{}

	if _, err := RunInit(context.Background(), p); err == nil {
		t.Fatal("RunInit succeeded, want the single 503 to end it")
	}
	if got := cp.enrollCalls.Load(); got != 1 {
		t.Fatalf("enrol attempts = %d, want 1", got)
	}
}

// A cancelled caller is the caller's decision, not the server's.
func TestRunInit_StopsOnContextCancel(t *testing.T) {
	cp := newFlakyCP(t)
	cp.enrollFailures.Store(99)
	p := cp.params(t)
	p.RetryBackoff = []time.Duration{time.Hour, time.Hour}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for cp.enrollCalls.Load() == 0 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	start := time.Now()
	if _, err := RunInit(ctx, p); err == nil {
		t.Fatal("RunInit succeeded, want it to end with the context")
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("RunInit waited %s past the cancel", elapsed)
	}
	if got := cp.enrollCalls.Load(); got != 1 {
		t.Fatalf("enrol attempts = %d, want 1", got)
	}
}

func TestParseRetryAfterAndDelay(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"3", 3 * time.Second},
		{"0", 0},
		{"-5", 0},
		{"Wed, 21 Oct 2026 07:28:00 GMT", 0}, // HTTP-date form is not parsed
		{"not-a-number", 0},
	} {
		if got := parseRetryAfter(tc.in); got != tc.want {
			t.Errorf("parseRetryAfter(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}

	fallback := 7 * time.Second
	if got := retryDelay(&httpStatusError{StatusCode: 503}, fallback); got != fallback {
		t.Errorf("no Retry-After: delay = %s, want the configured %s", got, fallback)
	}
	if got := retryDelay(&httpStatusError{StatusCode: 503, RetryAfter: 2 * time.Second}, fallback); got != 2*time.Second {
		t.Errorf("with Retry-After: delay = %s, want 2s", got)
	}
	// An unhealthy server's advice must not be able to hang the installer.
	if got := retryDelay(&httpStatusError{StatusCode: 503, RetryAfter: time.Hour}, fallback); got != maxRetryAfter {
		t.Errorf("oversized Retry-After: delay = %s, want the %s cap", got, maxRetryAfter)
	}
}
