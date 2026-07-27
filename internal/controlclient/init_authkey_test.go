package controlclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/devicekeys"
)

// authKeyCP is a Control Plane that authorizes the session inside the
// create call whenever a valid auth key is presented (waired#976), and
// records what it was asked.
type authKeyCP struct {
	srv        *httptest.Server
	validKey   string
	sawAuthKey atomic.Value // string
	sawField   atomic.Bool  // the request carried an auth_key field at all
	pollCount  atomic.Int32
	enrolled   atomic.Int32
	lastTicket atomic.Value // string
}

func newAuthKeyCP(t *testing.T, validKey string) *authKeyCP {
	t.Helper()
	cp := &authKeyCP{validKey: validKey}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/auth/login-sessions", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		key, present := body["auth_key"].(string)
		cp.sawField.Store(present)
		cp.sawAuthKey.Store(key)

		resp := map[string]any{
			"login_session_id":      "ls_authkey",
			"login_url":             "http://placeholder/login/ls_authkey",
			"user_code":             "TEST-CODE",
			"poll_token":            "waired_poll_authkey",
			"expires_at":            time.Now().Add(time.Minute).UTC().Format(time.RFC3339),
			"poll_interval_seconds": 1,
		}
		if present {
			if key != cp.validKey {
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]string{"type": "auth_key_invalid", "message": "nope"},
				})
				return
			}
			resp["registration_ticket"] = "waired_reg_authkey"
			resp["account_email"] = "ops@example.com"
			resp["network_id"] = "nw_authkey"
			resp["network_name"] = "ops"
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("GET /v1/auth/login-sessions/{id}", func(w http.ResponseWriter, _ *http.Request) {
		cp.pollCount.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":              "authorized",
			"registration_ticket": "waired_reg_from_poll",
			"account_email":       "browser@example.com",
			"network_id":          "nw_browser",
			"network_name":        "browser",
		})
	})

	mux.HandleFunc("POST /v1/devices/enroll/complete", func(w http.ResponseWriter, r *http.Request) {
		cp.enrolled.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		ticket, _ := body["registration_ticket"].(string)
		cp.lastTicket.Store(ticket)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_id":                      "dev_authkey",
			"network_id":                     "nw_authkey",
			"account_id":                     "acct_authkey",
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

func (cp *authKeyCP) params(t *testing.T) InitParams {
	t.Helper()
	mk, _ := devicekeys.NewMachineKey()
	nk, _ := devicekeys.NewNodeKey()
	return InitParams{
		ControlURL:    cp.srv.URL,
		DeviceName:    "headless-1",
		Platform:      "linux",
		Arch:          "amd64",
		ClientVersion: "0.1.0-test",
		Endpoint:      "udp4:127.0.0.1:51820",
		MachineKey:    mk,
		NodeKey:       nk,
		PollInterval:  10 * time.Millisecond,
		PollTimeout:   2 * time.Second,
	}
}

// PRODUCT CONTRACT (#175, waired#976): an auth-key enrollment is
// authorized inside the create call, so RunInit must take the ticket from
// there and NEVER poll. Polling would reintroduce the browser flow's
// dependency on the CP's in-process ticket rendezvous, which is
// instance-affine — the reason the ticket is returned inline at all.
func TestRunInit_AuthKeySkipsThePollLoop(t *testing.T) {
	const key = "waired_ak_valid"
	cp := newAuthKeyCP(t, key)

	p := cp.params(t)
	p.AuthKey = key
	var loginURLCalls int32
	p.OnLoginURL = func(string, string) { atomic.AddInt32(&loginURLCalls, 1) }
	var completedEmail, completedNetwork string
	p.OnLoginComplete = func(email, network string) { completedEmail, completedNetwork = email, network }

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := RunInit(ctx, p)
	if err != nil {
		t.Fatalf("RunInit: %v", err)
	}

	if got := cp.pollCount.Load(); got != 0 {
		t.Errorf("polled %d times; an auth-key enrollment must never poll", got)
	}
	// No URL exists for anyone to open, so offering one is worse than
	// useless on a headless host: it is a prompt nobody can answer.
	if loginURLCalls != 0 {
		t.Errorf("OnLoginURL called %d times; want 0", loginURLCalls)
	}
	if cp.enrolled.Load() != 1 {
		t.Fatalf("enroll/complete called %d times, want 1", cp.enrolled.Load())
	}
	if got, _ := cp.lastTicket.Load().(string); got != "waired_reg_authkey" {
		t.Errorf("enrolled with ticket %q, want the one from the create response", got)
	}

	// The account/network the CP reported alongside the ticket must reach
	// both the callback and the result — nothing else knows them, since
	// there was no poll response to carry them.
	if completedEmail != "ops@example.com" || completedNetwork != "ops" {
		t.Errorf("OnLoginComplete(%q, %q), want (ops@example.com, ops)", completedEmail, completedNetwork)
	}
	if res.AccountEmail != "ops@example.com" || res.NetworkName != "ops" {
		t.Errorf("result account=%q network=%q", res.AccountEmail, res.NetworkName)
	}
	if res.DeviceID != "dev_authkey" {
		t.Errorf("device id = %q", res.DeviceID)
	}
}

// PRODUCT CONTRACT: the field is omitted entirely when there is no key.
// A control plane predating auth keys decodes with DisallowUnknownFields,
// so sending `"auth_key": ""` would 400 every interactive enrollment.
func TestRunInit_NoAuthKeyOmitsTheField(t *testing.T) {
	cp := newAuthKeyCP(t, "waired_ak_valid")

	p := cp.params(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := RunInit(ctx, p); err != nil {
		t.Fatalf("RunInit: %v", err)
	}

	if cp.sawField.Load() {
		t.Error("interactive enrollment sent an auth_key field; it must be omitted")
	}
	// ...and it still went through the browser flow.
	if cp.pollCount.Load() == 0 {
		t.Error("interactive enrollment did not poll")
	}
	if got, _ := cp.lastTicket.Load().(string); got != "waired_reg_from_poll" {
		t.Errorf("enrolled with ticket %q, want the polled one", got)
	}
}

// A refused key must fail the enrollment outright rather than silently
// continuing into the browser flow, which on a headless host would hang
// until the poll timeout.
func TestRunInit_RejectedAuthKeyFailsFast(t *testing.T) {
	cp := newAuthKeyCP(t, "waired_ak_valid")

	p := cp.params(t)
	p.AuthKey = "waired_ak_wrong"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := RunInit(ctx, p); err == nil {
		t.Fatal("want an error for a refused auth key")
	}
	if cp.pollCount.Load() != 0 {
		t.Errorf("polled %d times after a refused key; want 0", cp.pollCount.Load())
	}
	if cp.enrolled.Load() != 0 {
		t.Errorf("enrolled %d times after a refused key; want 0", cp.enrolled.Load())
	}
}
