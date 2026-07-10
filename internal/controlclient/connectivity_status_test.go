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
	"time"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// TestPushConnectivityStatusSignsBodyAndDecodesContentChanged mirrors the
// inference-status wire-format test (#252): JSON body, base64 Ed25519
// signature over the exact raw bytes, bearer auth, and the
// content_changed boolean propagated back.
func TestPushConnectivityStatusSignsBodyAndDecodesContentChanged(t *testing.T) {
	machinePub, machinePriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	var (
		gotPath   string
		gotAuth   string
		gotSigOK  bool
		gotState  signer.ConnectivityState
		gotDevice string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		sig, _ := base64.StdEncoding.DecodeString(r.Header.Get("X-Waired-Body-Signature"))
		gotSigOK = ed25519.Verify(ed25519.PublicKey(machinePub), body, sig)
		var req upsertConnectivityStatusRequest
		_ = json.Unmarshal(body, &req)
		gotState = req.State
		gotDevice = req.DeviceID
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","content_changed":true}`))
	}))
	defer srv.Close()

	cli := New(srv.URL, "tok-deadbeef")
	state := signer.ConnectivityState{
		DirectPeers: 2,
		RelayPeers:  1,
		TotalPeers:  3,
		LastCheck:   time.Now().UTC().Format(time.RFC3339Nano),
	}
	changed, err := cli.PushConnectivityStatus(context.Background(), "device-abc", state, machinePriv)
	if err != nil {
		t.Fatalf("PushConnectivityStatus: %v", err)
	}
	if !changed {
		t.Errorf("expected ContentChanged=true (mock says yes)")
	}
	if gotPath != "/v1/devices/self/connectivity-status" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer tok-deadbeef" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if !gotSigOK {
		t.Errorf("signature did not verify against the supplied raw body")
	}
	if gotDevice != "device-abc" {
		t.Errorf("device_id = %q", gotDevice)
	}
	if gotState.DirectPeers != 2 || gotState.RelayPeers != 1 || gotState.TotalPeers != 3 {
		t.Errorf("state round-trip mismatch: %+v", gotState)
	}
}

func TestPushConnectivityStatusPropagatesNonOK(t *testing.T) {
	_, machinePriv, _ := ed25519.GenerateKey(rand.Reader)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limited"}}`))
	}))
	defer srv.Close()

	cli := New(srv.URL, "tok")
	_, err := cli.PushConnectivityStatus(context.Background(), "d", signer.ConnectivityState{
		TotalPeers: 0,
		LastCheck:  time.Now().UTC().Format(time.RFC3339Nano),
	}, machinePriv)
	if err == nil {
		t.Fatalf("expected error from 429 response")
	}
}
