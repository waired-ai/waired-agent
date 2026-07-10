package signedreq_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/proto/signedreq"
)

func mustKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519 keygen: %v", err)
	}
	return pub, priv
}

func mustNonce(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("nonce: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func TestSignAndVerify_RoundTrip(t *testing.T) {
	pub, priv := mustKey(t)
	body := []byte(`{"hello":"world"}`)
	h := http.Header{}
	signedreq.SetSignature(h, priv, body)

	if err := signedreq.Verify(h, body, pub); err != nil {
		t.Fatalf("Verify(unmodified): %v", err)
	}
}

func TestVerify_MissingHeader(t *testing.T) {
	pub, _ := mustKey(t)
	if err := signedreq.Verify(http.Header{}, []byte("x"), pub); !errors.Is(err, signedreq.ErrMissingSignature) {
		t.Fatalf("want ErrMissingSignature, got %v", err)
	}
}

func TestVerify_BadEncoding(t *testing.T) {
	pub, _ := mustKey(t)
	h := http.Header{}
	h.Set(signedreq.HeaderBodySignature, "not-base64!!!")
	err := signedreq.Verify(h, []byte("x"), pub)
	if !errors.Is(err, signedreq.ErrSignatureEncoding) {
		t.Fatalf("want ErrSignatureEncoding, got %v", err)
	}
}

func TestVerify_WrongSize(t *testing.T) {
	pub, _ := mustKey(t)
	// 32 random bytes — valid base64, wrong length.
	short := base64.StdEncoding.EncodeToString(make([]byte, 32))
	h := http.Header{}
	h.Set(signedreq.HeaderBodySignature, short)
	err := signedreq.Verify(h, []byte("x"), pub)
	if !errors.Is(err, signedreq.ErrSignatureEncoding) {
		t.Fatalf("want ErrSignatureEncoding, got %v", err)
	}
}

func TestVerify_BodyTampered(t *testing.T) {
	pub, priv := mustKey(t)
	body := []byte(`{"hello":"world"}`)
	h := http.Header{}
	signedreq.SetSignature(h, priv, body)

	// Mutate body after signing.
	tampered := append([]byte{}, body...)
	tampered[1] ^= 0x01
	if err := signedreq.Verify(h, tampered, pub); !errors.Is(err, signedreq.ErrSignatureMismatch) {
		t.Fatalf("want ErrSignatureMismatch, got %v", err)
	}
}

func TestVerify_WrongKey(t *testing.T) {
	_, priv1 := mustKey(t)
	pub2, _ := mustKey(t)
	body := []byte(`{}`)
	h := http.Header{}
	signedreq.SetSignature(h, priv1, body)
	if err := signedreq.Verify(h, body, pub2); !errors.Is(err, signedreq.ErrSignatureMismatch) {
		t.Fatalf("want ErrSignatureMismatch with mismatched key, got %v", err)
	}
}

func TestReadBody_Limit(t *testing.T) {
	body, err := signedreq.ReadBody(bytes.NewBufferString("12345"), 10)
	if err != nil || string(body) != "12345" {
		t.Fatalf("happy path: body=%q err=%v", body, err)
	}

	body, err = signedreq.ReadBody(bytes.NewBufferString("12345"), 5)
	if err != nil || string(body) != "12345" {
		t.Fatalf("at-limit: body=%q err=%v", body, err)
	}

	_, err = signedreq.ReadBody(bytes.NewBufferString("123456"), 5)
	if !errors.Is(err, signedreq.ErrBodyTooLarge) {
		t.Fatalf("over-limit: want ErrBodyTooLarge, got %v", err)
	}
}

func TestReadBody_NonPositiveMax(t *testing.T) {
	if _, err := signedreq.ReadBody(bytes.NewBufferString("x"), 0); !errors.Is(err, signedreq.ErrBodyRead) {
		t.Fatalf("want ErrBodyRead for max=0, got %v", err)
	}
}

func TestVerifyEnvelope_HappyPath(t *testing.T) {
	now := time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC)
	env := signedreq.Envelope{
		DeviceID: "dev-123",
		IssuedAt: now.Format(time.RFC3339),
		Nonce:    mustNonce(t, 16),
	}
	nonce, err := signedreq.VerifyEnvelope(env, "dev-123", now, 60*time.Second)
	if err != nil {
		t.Fatalf("happy path: %v", err)
	}
	if len(nonce) < 12 {
		t.Fatalf("nonce length: %d", len(nonce))
	}
}

func TestVerifyEnvelope_DeviceMismatch(t *testing.T) {
	now := time.Now().UTC()
	env := signedreq.Envelope{
		DeviceID: "dev-A",
		IssuedAt: now.Format(time.RFC3339),
		Nonce:    mustNonce(t, 12),
	}
	_, err := signedreq.VerifyEnvelope(env, "dev-B", now, time.Minute)
	if !errors.Is(err, signedreq.ErrDeviceMismatch) {
		t.Fatalf("want ErrDeviceMismatch, got %v", err)
	}
}

func TestVerifyEnvelope_BadIssuedAt(t *testing.T) {
	now := time.Now().UTC()
	env := signedreq.Envelope{
		DeviceID: "dev",
		IssuedAt: "not-a-time",
		Nonce:    mustNonce(t, 12),
	}
	_, err := signedreq.VerifyEnvelope(env, "dev", now, time.Minute)
	if !errors.Is(err, signedreq.ErrBadIssuedAt) {
		t.Fatalf("want ErrBadIssuedAt, got %v", err)
	}
}

func TestVerifyEnvelope_OutOfWindow(t *testing.T) {
	now := time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC)
	stale := now.Add(-2 * time.Minute) // outside ±60s
	env := signedreq.Envelope{
		DeviceID: "dev",
		IssuedAt: stale.Format(time.RFC3339),
		Nonce:    mustNonce(t, 12),
	}
	_, err := signedreq.VerifyEnvelope(env, "dev", now, time.Minute)
	if !errors.Is(err, signedreq.ErrIssuedAtOutOfRange) {
		t.Fatalf("want ErrIssuedAtOutOfRange (past), got %v", err)
	}
	future := now.Add(2 * time.Minute)
	env.IssuedAt = future.Format(time.RFC3339)
	_, err = signedreq.VerifyEnvelope(env, "dev", now, time.Minute)
	if !errors.Is(err, signedreq.ErrIssuedAtOutOfRange) {
		t.Fatalf("want ErrIssuedAtOutOfRange (future), got %v", err)
	}
}

func TestVerifyEnvelope_NonceTooShort(t *testing.T) {
	now := time.Now().UTC()
	tooShort := base64.StdEncoding.EncodeToString(make([]byte, 8))
	env := signedreq.Envelope{
		DeviceID: "dev",
		IssuedAt: now.Format(time.RFC3339),
		Nonce:    tooShort,
	}
	_, err := signedreq.VerifyEnvelope(env, "dev", now, time.Minute)
	if !errors.Is(err, signedreq.ErrBadNonce) {
		t.Fatalf("want ErrBadNonce, got %v", err)
	}
}

func TestVerifyEnvelope_NonceMalformed(t *testing.T) {
	now := time.Now().UTC()
	env := signedreq.Envelope{
		DeviceID: "dev",
		IssuedAt: now.Format(time.RFC3339),
		Nonce:    "@@@-not-base64-@@@",
	}
	_, err := signedreq.VerifyEnvelope(env, "dev", now, time.Minute)
	if !errors.Is(err, signedreq.ErrBadNonce) {
		t.Fatalf("want ErrBadNonce, got %v", err)
	}
}

func TestConsumeNonce_Replay(t *testing.T) {
	cache := signedreq.NewMemoryNonceCache()
	now := time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC)
	nonce := []byte("0123456789ab")
	if err := signedreq.ConsumeNonce(cache, "dev", nonce, now, 5*time.Minute); err != nil {
		t.Fatalf("first consume: %v", err)
	}
	// Same nonce within TTL → replay.
	if err := signedreq.ConsumeNonce(cache, "dev", nonce, now.Add(10*time.Second), 5*time.Minute); !errors.Is(err, signedreq.ErrReplayDetected) {
		t.Fatalf("want ErrReplayDetected, got %v", err)
	}
	// After TTL the same nonce can be reused.
	if err := signedreq.ConsumeNonce(cache, "dev", nonce, now.Add(6*time.Minute), 5*time.Minute); err != nil {
		t.Fatalf("after TTL: %v", err)
	}
}

func TestConsumeNonce_NilCache(t *testing.T) {
	if err := signedreq.ConsumeNonce(nil, "dev", []byte("abc"), time.Now(), time.Minute); err != nil {
		t.Fatalf("nil cache must be a no-op, got %v", err)
	}
}

func TestParseEnvelope(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"device_id": "abc",
		"issued_at": "2026-05-09T18:00:00Z",
		"nonce":     "AAAA",
		"extra":     "ignored",
	})
	env, err := signedreq.ParseEnvelope(body)
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	if env.DeviceID != "abc" || env.IssuedAt == "" || env.Nonce == "" {
		t.Fatalf("got: %+v", env)
	}
}

func TestParseEnvelope_BadJSON(t *testing.T) {
	if _, err := signedreq.ParseEnvelope([]byte("not-json")); err == nil {
		t.Fatalf("want JSON parse error")
	}
}

func TestDecodeFlexBase64_AllVariants(t *testing.T) {
	raw := []byte{0x01, 0x02, 0x03, 0x04, 0xff}
	cases := []string{
		base64.StdEncoding.EncodeToString(raw),
		base64.URLEncoding.EncodeToString(raw),
		base64.RawStdEncoding.EncodeToString(raw),
		base64.RawURLEncoding.EncodeToString(raw),
	}
	for _, in := range cases {
		out, err := signedreq.DecodeFlexBase64(in)
		if err != nil {
			t.Fatalf("variant %q: %v", in, err)
		}
		if !bytes.Equal(out, raw) {
			t.Fatalf("variant %q: got %x want %x", in, out, raw)
		}
	}
}

func TestDecodeFlexBase64_Invalid(t *testing.T) {
	if _, err := signedreq.DecodeFlexBase64("@@@"); err == nil {
		t.Fatalf("want error for non-base64 input")
	}
}

func TestHTTPStatus(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, http.StatusOK},
		{signedreq.ErrBodyTooLarge, http.StatusRequestEntityTooLarge},
		{signedreq.ErrMissingSignature, http.StatusUnauthorized},
		{signedreq.ErrSignatureEncoding, http.StatusUnauthorized},
		{signedreq.ErrSignatureMismatch, http.StatusUnauthorized},
		{signedreq.ErrIssuedAtOutOfRange, http.StatusUnauthorized},
		{signedreq.ErrReplayDetected, http.StatusUnauthorized},
		{signedreq.ErrBadIssuedAt, http.StatusBadRequest},
		{signedreq.ErrBadNonce, http.StatusBadRequest},
		{signedreq.ErrDeviceMismatch, http.StatusBadRequest},
		{signedreq.ErrBodyRead, http.StatusBadRequest},
		{errors.New("unmapped"), http.StatusInternalServerError},
	}
	for _, c := range cases {
		if got := signedreq.HTTPStatus(c.err); got != c.want {
			t.Errorf("HTTPStatus(%v) = %d, want %d", c.err, got, c.want)
		}
	}
}

func TestMemoryNonceCache_Concurrent(t *testing.T) {
	cache := signedreq.NewMemoryNonceCache()
	now := time.Now().UTC()
	const N = 100
	done := make(chan bool, N)
	first := 0
	for i := 0; i < N; i++ {
		go func(idx int) {
			ok := cache.Consume("dev", "same-nonce", now, time.Minute)
			done <- ok
		}(i)
	}
	for i := 0; i < N; i++ {
		if <-done {
			first++
		}
	}
	if first != 1 {
		t.Fatalf("exactly one Consume should succeed; got %d", first)
	}
}

func TestSetAndVerifyHeaderEnvelope_RoundTrip(t *testing.T) {
	pub, priv := mustKey(t)
	now := time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC)
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	h := http.Header{}
	signedreq.SetHeaderEnvelope(h, priv, "dev-A", now.Format(time.RFC3339), mustNonce(t, 16), body)

	env, nonce, err := signedreq.VerifyHeaderEnvelope(h, body, pub, "dev-A", now, time.Minute)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if env.DeviceID != "dev-A" || len(nonce) < 12 {
		t.Fatalf("got env=%+v nonce=%dbytes", env, len(nonce))
	}
}

func TestVerifyHeaderEnvelope_BodyTampered(t *testing.T) {
	pub, priv := mustKey(t)
	now := time.Now().UTC()
	body := []byte(`original`)
	h := http.Header{}
	signedreq.SetHeaderEnvelope(h, priv, "dev", now.Format(time.RFC3339), mustNonce(t, 12), body)

	tampered := []byte(`tampered`)
	_, _, err := signedreq.VerifyHeaderEnvelope(h, tampered, pub, "dev", now, time.Minute)
	if !errors.Is(err, signedreq.ErrSignatureMismatch) {
		t.Fatalf("want ErrSignatureMismatch on body tamper, got %v", err)
	}
}

func TestVerifyHeaderEnvelope_HeaderTampered(t *testing.T) {
	pub, priv := mustKey(t)
	now := time.Now().UTC()
	body := []byte(`x`)
	h := http.Header{}
	signedreq.SetHeaderEnvelope(h, priv, "dev", now.Format(time.RFC3339), mustNonce(t, 12), body)

	// Tampering with the device_id header invalidates the canonical hash.
	h.Set(signedreq.HeaderDeviceID, "evil")
	_, _, err := signedreq.VerifyHeaderEnvelope(h, body, pub, "evil", now, time.Minute)
	if !errors.Is(err, signedreq.ErrSignatureMismatch) {
		t.Fatalf("want ErrSignatureMismatch on header tamper, got %v", err)
	}
}

func TestVerifyHeaderEnvelope_DeviceMismatch(t *testing.T) {
	pub, priv := mustKey(t)
	now := time.Now().UTC()
	body := []byte(`x`)
	h := http.Header{}
	signedreq.SetHeaderEnvelope(h, priv, "dev-A", now.Format(time.RFC3339), mustNonce(t, 12), body)

	// Headers signed for dev-A but caller resolved peer to dev-B.
	_, _, err := signedreq.VerifyHeaderEnvelope(h, body, pub, "dev-B", now, time.Minute)
	if !errors.Is(err, signedreq.ErrDeviceMismatch) {
		t.Fatalf("want ErrDeviceMismatch, got %v", err)
	}
}

func TestVerifyHeaderEnvelope_OutOfWindow(t *testing.T) {
	pub, priv := mustKey(t)
	now := time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC)
	body := []byte(`x`)
	h := http.Header{}
	signedreq.SetHeaderEnvelope(h, priv, "dev", now.Add(-2*time.Minute).Format(time.RFC3339), mustNonce(t, 12), body)

	_, _, err := signedreq.VerifyHeaderEnvelope(h, body, pub, "dev", now, time.Minute)
	if !errors.Is(err, signedreq.ErrIssuedAtOutOfRange) {
		t.Fatalf("want ErrIssuedAtOutOfRange, got %v", err)
	}
}

func TestCanonicalBytes_Stable(t *testing.T) {
	body := []byte("hello world")
	a := signedreq.CanonicalBytes("d", "t", "n", body)
	b := signedreq.CanonicalBytes("d", "t", "n", body)
	if string(a) != string(b) {
		t.Fatalf("canonical bytes are not stable: %q vs %q", a, b)
	}
	c := signedreq.CanonicalBytes("d2", "t", "n", body)
	if string(a) == string(c) {
		t.Fatalf("device_id change must produce different canonical bytes")
	}
	d := signedreq.CanonicalBytes("d", "t", "n", []byte("different body"))
	if string(a) == string(d) {
		t.Fatalf("body change must produce different canonical bytes")
	}
}

// Sign output should be a parseable base64 string of exactly the
// signature size — test guards against accidental encoding swaps.
func TestSign_Encoding(t *testing.T) {
	_, priv := mustKey(t)
	got := signedreq.Sign(priv, []byte("body"))
	if !strings.ContainsAny(got, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/=") {
		t.Fatalf("expected base64 output, got %q", got)
	}
	dec, err := signedreq.DecodeFlexBase64(got)
	if err != nil {
		t.Fatalf("decode Sign() output: %v", err)
	}
	if len(dec) != signedreq.SignatureSize {
		t.Fatalf("len = %d, want %d", len(dec), signedreq.SignatureSize)
	}
}
