package controlclient

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestPushPublicShareSignsBodyAndDecodesResult verifies the toggle
// push's wire-format obligations against an httptest mock CP: JSON
// envelope fields (device_id/issued_at/nonce/enabled/max_clients),
// base64 Ed25519 signature over the EXACT raw body bytes in
// X-Waired-Body-Signature, bearer auth, and result decoding. All IDs
// are synthetic (public repo).
func TestPushPublicShareSignsBodyAndDecodesResult(t *testing.T) {
	machinePub, machinePriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	var (
		gotPath  string
		gotAuth  string
		gotSigOK bool
		gotReq   publicSharePushRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		sig, _ := base64.StdEncoding.DecodeString(r.Header.Get("X-Waired-Body-Signature"))
		gotSigOK = ed25519.Verify(machinePub, body, sig)
		_ = json.Unmarshal(body, &gotReq)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","enabled":true,"max_clients":2,"revoked_grants":0}`))
	}))
	defer srv.Close()

	cli := New(srv.URL, "tok-deadbeef")
	res, err := cli.PushPublicShare(context.Background(), "device-abc", true, 3, machinePriv)
	if err != nil {
		t.Fatalf("PushPublicShare: %v", err)
	}
	if gotPath != "/v1/devices/self/public-share" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer tok-deadbeef" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if !gotSigOK {
		t.Errorf("signature did not verify against the supplied raw body")
	}
	if gotReq.DeviceID != "device-abc" || !gotReq.Enabled || gotReq.MaxClients != 3 {
		t.Errorf("request round-trip mismatch: %+v", gotReq)
	}
	if gotReq.IssuedAt == "" || gotReq.Nonce == "" {
		t.Errorf("envelope fields missing: %+v", gotReq)
	}
	if !res.Enabled || res.MaxClients != 2 {
		t.Errorf("result = %+v, want enabled max_clients=2", res)
	}
}

// TestPushPublicShareDisableCarriesRevokedGrants covers the OFF leg:
// enabled=false rides the body and the CP's revoked_grants count comes
// back for the UI.
func TestPushPublicShareDisableCarriesRevokedGrants(t *testing.T) {
	_, machinePriv, _ := ed25519.GenerateKey(rand.Reader)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req publicSharePushRequest
		_ = json.Unmarshal(body, &req)
		if req.Enabled {
			t.Errorf("expected enabled=false, got %+v", req)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","enabled":false,"max_clients":0,"revoked_grants":2}`))
	}))
	defer srv.Close()

	cli := New(srv.URL, "tok")
	res, err := cli.PushPublicShare(context.Background(), "device-abc", false, 0, machinePriv)
	if err != nil {
		t.Fatalf("PushPublicShare: %v", err)
	}
	if res.RevokedGrants != 2 {
		t.Errorf("RevokedGrants = %d, want 2", res.RevokedGrants)
	}
}

// TestPushPublicSharePropagatesNonOK: non-200 becomes an error carrying
// the response body (429 rate_limited, 401 replay, etc.).
func TestPushPublicSharePropagatesNonOK(t *testing.T) {
	_, machinePriv, _ := ed25519.GenerateKey(rand.Reader)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limited"}}`))
	}))
	defer srv.Close()

	cli := New(srv.URL, "tok")
	if _, err := cli.PushPublicShare(context.Background(), "d", true, 0, machinePriv); err == nil {
		t.Fatalf("expected error from 429 response")
	}
}

// TestPushPublicShareUsesCustomAuthHeader mirrors the bypass-CP-IAM
// deployment mode: the device bearer must ride X-Waired-Agent-Bearer.
func TestPushPublicShareUsesCustomAuthHeader(t *testing.T) {
	_, machinePriv, _ := ed25519.GenerateKey(rand.Reader)

	var gotCustom, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCustom = r.Header.Get("X-Waired-Agent-Bearer")
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	cli := New(srv.URL, "tok-custom")
	cli.UseCustomAuthHeader = true
	if _, err := cli.PushPublicShare(context.Background(), "d", true, 0, machinePriv); err != nil {
		t.Fatalf("PushPublicShare: %v", err)
	}
	if gotCustom != "tok-custom" {
		t.Errorf("X-Waired-Agent-Bearer = %q", gotCustom)
	}
	if gotAuth != "" {
		t.Errorf("Authorization should be empty in custom-auth mode, got %q", gotAuth)
	}
}
