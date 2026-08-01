package controlclient

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/devicekeys"
)

// fakeMachineKey wraps a freshly-generated ed25519 keypair for tests.
func fakeMachineKey(t *testing.T) (*devicekeys.MachineKey, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519: %v", err)
	}
	return &devicekeys.MachineKey{Public: pub, Private: priv}, pub
}

func TestRefreshDeviceTokenHappyPath(t *testing.T) {
	mk, machinePub := fakeMachineKey(t)
	const (
		deviceID     = "dev_TEST"
		networkID    = "wn_TEST"
		refreshToken = "waired_drt_initial"
	)
	var capturedSig []byte
	var capturedTranscriptHash string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth/device-token/refresh" {
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			DeviceID         string `json:"device_id"`
			NetworkID        string `json:"network_id"`
			RefreshToken     string `json:"refresh_token"`
			MachineSignature string `json:"machine_signature"`
			ClientNonce      string `json:"client_nonce"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("decode body: %v", err)
			http.Error(w, err.Error(), 400)
			return
		}
		if req.DeviceID != deviceID || req.NetworkID != networkID || req.RefreshToken != refreshToken {
			t.Errorf("body mismatch: %+v", req)
		}
		if req.ClientNonce == "" {
			t.Error("missing client_nonce")
		}
		sig, err := base64.StdEncoding.DecodeString(req.MachineSignature)
		if err != nil {
			t.Errorf("decode sig: %v", err)
		}
		capturedSig = sig

		// Recompute transcript server-side and verify the signature.
		tokenHash := sha256.Sum256([]byte(refreshToken))
		capturedTranscriptHash = hex.EncodeToString(tokenHash[:])
		var b strings.Builder
		b.WriteString("WAIRED-MACHINE-SIGNATURE-V1\n")
		b.WriteString("purpose=device-token-refresh\n")
		b.WriteString("device_id=" + req.DeviceID + "\n")
		b.WriteString("network_id=" + req.NetworkID + "\n")
		b.WriteString("refresh_token_hash=" + capturedTranscriptHash + "\n")
		b.WriteString("client_nonce=" + req.ClientNonce + "\n")
		if !ed25519.Verify(machinePub, []byte(b.String()), sig) {
			t.Errorf("transcript signature failed to verify")
		}

		// Echo back a valid response.
		w.Header().Set("Content-Type", "application/json")
		now := time.Date(2026, 5, 19, 3, 0, 0, 0, time.UTC)
		json.NewEncoder(w).Encode(map[string]any{
			"device_access_token":            "waired_dat_new",
			"device_access_token_expires_at": now.Add(15 * time.Minute).Format(time.RFC3339),
			"device_refresh_token":           "waired_drt_new",
			"device_auth_expires_at":         now.Add(180 * 24 * time.Hour).Format(time.RFC3339),
			"device_certificate":             map[string]any{"version": 1},
		})
	}))
	defer srv.Close()

	res, err := RefreshDeviceToken(context.Background(), RefreshParams{
		ControlURL:   srv.URL,
		DeviceID:     deviceID,
		NetworkID:    networkID,
		RefreshToken: refreshToken,
		MachineKey:   mk,
	})
	if err != nil {
		t.Fatalf("RefreshDeviceToken: %v", err)
	}
	if res.DeviceAccessToken != "waired_dat_new" {
		t.Errorf("access token = %q", res.DeviceAccessToken)
	}
	if res.DeviceRefreshToken != "waired_drt_new" {
		t.Errorf("refresh token = %q", res.DeviceRefreshToken)
	}
	if len(capturedSig) != ed25519.SignatureSize {
		t.Errorf("sig len = %d", len(capturedSig))
	}
}

func TestRefreshDeviceTokenClassifiesReuse(t *testing.T) {
	mk, _ := fakeMachineKey(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{
				"type":    "refresh_token_reuse_detected",
				"message": "leaked refresh token replayed",
				"hint":    "run waired init",
			},
		})
	}))
	defer srv.Close()

	_, err := RefreshDeviceToken(context.Background(), RefreshParams{
		ControlURL:   srv.URL,
		DeviceID:     "dev",
		NetworkID:    "net",
		RefreshToken: "waired_drt_stolen",
		MachineKey:   mk,
	})
	if !errors.Is(err, ErrRefreshReuseDetected) {
		t.Fatalf("expected ErrRefreshReuseDetected, got %v", err)
	}
}

func TestRefreshDeviceTokenClassifiesExpiredAndUnknown(t *testing.T) {
	cases := []struct {
		serverType string
		want       error
	}{
		{"expired_refresh_token", ErrRefreshExpired},
		{"invalid_refresh_token", ErrRefreshInvalid},
		{"device_not_approved", ErrDeviceNotApproved},
		{"device_suspended", ErrDeviceSuspended},
		{"machine_signature_invalid", ErrMachineSigInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.serverType, func(t *testing.T) {
			mk, _ := fakeMachineKey(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]string{"type": tc.serverType},
				})
			}))
			defer srv.Close()
			_, err := RefreshDeviceToken(context.Background(), RefreshParams{
				ControlURL:   srv.URL,
				DeviceID:     "d",
				NetworkID:    "n",
				RefreshToken: "r",
				MachineKey:   mk,
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			// Product contract (waired-agent#318): a classified answer
			// is a verdict, so it must NOT also read as "outcome
			// unknown" — that is what tells the refresher to stop
			// replaying and drop the pending rotation.
			if errors.Is(err, ErrRefreshOutcomeUnknown) {
				t.Fatalf("a classified 401 must not be outcome-unknown: %v", err)
			}
		})
	}
}

// TestRefreshDeviceTokenClientNonceIsHonoured pins the retry contract
// (waired-agent#318): supplying ClientNonce must make the request
// byte-identical to the earlier attempt — same nonce AND same machine
// signature — because that is the only thing letting the Control Plane
// recognise a replay of a rotation whose response it lost, rather than
// reading it as a stolen token.
func TestRefreshDeviceTokenClientNonceIsHonoured(t *testing.T) {
	mk, machinePub := fakeMachineKey(t)
	const (
		deviceID     = "dev_TEST"
		networkID    = "wn_TEST"
		refreshToken = "waired_drt_initial"
		nonce        = "cGlubmVkLW5vbmNlLTAwMDA="
	)
	var gotNonce, gotSig string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			MachineSignature string `json:"machine_signature"`
			ClientNonce      string `json:"client_nonce"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("decode body: %v", err)
		}
		gotNonce, gotSig = req.ClientNonce, req.MachineSignature
		json.NewEncoder(w).Encode(map[string]any{
			"device_access_token":  "waired_dat_new",
			"device_refresh_token": "waired_drt_new",
		})
	}))
	defer srv.Close()

	for i := range 2 {
		if _, err := RefreshDeviceToken(context.Background(), RefreshParams{
			ControlURL:   srv.URL,
			DeviceID:     deviceID,
			NetworkID:    networkID,
			RefreshToken: refreshToken,
			MachineKey:   mk,
			ClientNonce:  nonce,
		}); err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		if gotNonce != nonce {
			t.Fatalf("attempt %d: client_nonce = %q, want the supplied %q", i, gotNonce, nonce)
		}
		tokenHash := sha256.Sum256([]byte(refreshToken))
		transcript := refreshTranscript(deviceID, networkID, hex.EncodeToString(tokenHash[:]), nonce)
		sig, err := base64.StdEncoding.DecodeString(gotSig)
		if err != nil {
			t.Fatalf("attempt %d: decode sig: %v", i, err)
		}
		if !ed25519.Verify(machinePub, transcript, sig) {
			t.Fatalf("attempt %d: signature does not verify against the pinned transcript", i)
		}
	}
}

// TestRefreshDeviceTokenOutcomeUnknown pins the other half of that
// contract: when no usable response comes back, the error must carry
// ErrRefreshOutcomeUnknown so the refresher replays instead of minting a
// fresh rotation the CP would read as theft.
func TestRefreshDeviceTokenOutcomeUnknown(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			// No response at all: the server hangs up mid-request.
			name: "connection closed",
			handler: func(w http.ResponseWriter, r *http.Request) {
				hj, ok := w.(http.Hijacker)
				if !ok {
					t.Fatal("ResponseWriter is not a Hijacker")
				}
				conn, _, err := hj.Hijack()
				if err != nil {
					t.Fatalf("hijack: %v", err)
				}
				conn.Close()
			},
		},
		{
			// 200 whose body we cannot decode: the CP rotated and we
			// lost the new credentials — the same predicament.
			name: "undecodable 200",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, "{not json")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mk, _ := fakeMachineKey(t)
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			_, err := RefreshDeviceToken(context.Background(), RefreshParams{
				ControlURL:   srv.URL,
				DeviceID:     "d",
				NetworkID:    "n",
				RefreshToken: "r",
				MachineKey:   mk,
			})
			if !errors.Is(err, ErrRefreshOutcomeUnknown) {
				t.Fatalf("got %v, want ErrRefreshOutcomeUnknown", err)
			}
		})
	}
}
