package controlclient

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// Synthetic identifiers only — this repo is public.
const (
	usageDeviceID = "dev_test00000001"
	usageGrantID  = "grant_test0001"
)

func sampleReport() signer.PublicUsageReport {
	return signer.PublicUsageReport{Entries: []signer.PublicUsageEntry{{
		GrantID:      usageGrantID,
		ModelID:      "qwen3:8b-q4_K_M",
		Class:        "main",
		Requests:     3,
		InputTokens:  120,
		OutputTokens: 45,
		InferenceMS:  900,
		WindowStart:  "2026-07-20T08:00:00Z",
		WindowEnd:    "2026-07-20T08:01:00Z",
	}}}
}

// The CP verifies an Ed25519 signature over the EXACT bytes it received,
// with the envelope fields flat alongside the report. Getting either
// wrong fails closed at the CP, so both are pinned here.
func TestPushPublicUsage_SignsRawBodyWithFlatEnvelope(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}

	var gotBody []byte
	var gotSig, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotSig = r.Header.Get("X-Waired-Body-Signature")
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/v1/devices/self/public-usage" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","inserted":1}`))
	}))
	defer srv.Close()

	c := NewWithBearer(srv.URL, func() string { return "test-bearer" })
	res, err := c.PushPublicUsage(context.Background(), usageDeviceID, sampleReport(), priv)
	if err != nil {
		t.Fatalf("PushPublicUsage: %v", err)
	}
	if res.Inserted != 1 || res.Status != "ok" {
		t.Errorf("result = %+v", res)
	}

	sig, err := base64.StdEncoding.DecodeString(gotSig)
	if err != nil {
		t.Fatalf("signature header not base64: %v", err)
	}
	if !ed25519.Verify(pub, gotBody, sig) {
		t.Fatal("signature does not verify over the raw body the server received")
	}
	if gotAuth != "Bearer test-bearer" {
		t.Errorf("Authorization = %q", gotAuth)
	}

	// Envelope fields must be siblings of "entries", not nested.
	var flat map[string]json.RawMessage
	if err := json.Unmarshal(gotBody, &flat); err != nil {
		t.Fatalf("body not an object: %v", err)
	}
	for _, k := range []string{"device_id", "issued_at", "nonce", "entries"} {
		if _, ok := flat[k]; !ok {
			t.Errorf("body is missing top-level %q: %s", k, gotBody)
		}
	}

	// §15-10: nothing but counters crosses this boundary.
	for _, banned := range []string{"messages", "content", "prompt", "role"} {
		if strings.Contains(string(gotBody), `"`+banned+`"`) {
			t.Errorf("usage body carries a %q field: %s", banned, gotBody)
		}
	}
}

func TestPushPublicUsage_ClassifiesRejections(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		retryable bool
	}{
		{"rate limited", http.StatusTooManyRequests,
			`{"error":{"type":"rate_limited","message":"slow down"}}`, true},
		{"clock skew", http.StatusUnauthorized,
			`{"error":{"type":"issued_at_out_of_window","message":"skew"}}`, true},
		{"bad signature", http.StatusUnauthorized,
			`{"error":{"type":"body_signature_mismatch","message":"nope"}}`, false},
		{"grant not owned", http.StatusForbidden,
			`{"error":{"type":"grant_not_owned","message":"nope"}}`, false},
		{"server error", http.StatusInternalServerError,
			`{"error":{"type":"internal_error","message":"boom"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			_, priv, _ := ed25519.GenerateKey(nil)
			c := NewWithBearer(srv.URL, func() string { return "b" })
			_, err := c.PushPublicUsage(context.Background(), usageDeviceID, sampleReport(), priv)

			var ue *PublicUsageError
			if !asPublicUsageError(err, &ue) {
				t.Fatalf("err = %v, want *PublicUsageError", err)
			}
			if ue.Status != tc.status {
				t.Errorf("Status = %d, want %d", ue.Status, tc.status)
			}
			if got := ue.Retryable(); got != tc.retryable {
				t.Errorf("Retryable() = %v, want %v (code=%q)", got, tc.retryable, ue.Code)
			}
		})
	}
}

func TestPushPublicUsage_RejectsBadInput(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	c := NewWithBearer("http://127.0.0.1:1", func() string { return "b" })

	if _, err := c.PushPublicUsage(context.Background(), usageDeviceID,
		signer.PublicUsageReport{}, priv); err == nil {
		t.Error("an empty report was accepted")
	}
	if _, err := c.PushPublicUsage(context.Background(), usageDeviceID,
		sampleReport(), ed25519.PrivateKey("short")); err == nil {
		t.Error("a malformed machine key was accepted")
	}
}

func asPublicUsageError(err error, target **PublicUsageError) bool {
	e, ok := err.(*PublicUsageError)
	if ok {
		*target = e
	}
	return ok
}
