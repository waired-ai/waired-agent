// Package signedreq is the shared Ed25519 raw-body signed-request
// helper used by:
//
//   - CP endpoints advertise (POST /v1/devices/self/endpoints)
//   - CP inference-status push (POST /v1/devices/self/inference-status)
//   - Peer-to-peer overlay inference (POST 9474:/anthropic/v1/messages, …)
//
// All three follow the same wire shape:
//
//   - The HTTP body is a JSON object whose top-level keys include a
//     `device_id`, an RFC3339 `issued_at`, and a base64 `nonce`
//     (≥12 bytes when decoded).
//   - The header `X-Waired-Body-Signature` carries a base64-encoded
//     Ed25519 signature over the raw body bytes (no canonical-JSON
//     transform — agents send what they signed).
//   - The receiver looks up the public key from a side channel
//     (CP: device row; overlay: NetworkMap peer entry) keyed by the
//     body's device_id, then verifies, then opens the envelope.
//
// This package owns Sign / Verify, envelope parsing + window/nonce
// checks, and an in-memory NonceCache. Rate-limit token buckets are
// out of scope (kept at call sites because the rate parameters differ
// by endpoint).
package signedreq

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// HeaderBodySignature is the canonical HTTP header that carries the
// base64-encoded Ed25519 signature.
//
// For CP signed-write endpoints (endpoints advertise, inference-status
// push), the signature covers the raw request body bytes directly —
// the envelope (device_id / issued_at / nonce) lives inside that body
// as JSON fields, so the body bytes already encode it.
//
// For the Phase 4 peer-to-peer overlay inference path, the request
// body is an opaque LLM payload (Anthropic Messages, OpenAI chat,
// possibly multi-MB streamed). Wrapping it in another envelope JSON
// would double the wire bytes, so the envelope rides in HTTP headers
// (HeaderDeviceID / HeaderIssuedAt / HeaderNonce) and the signature
// covers a canonical string that pins the headers to the body via
// CanonicalBytes.
const HeaderBodySignature = "X-Waired-Body-Signature"

// HeaderDeviceID / HeaderIssuedAt / HeaderNonce carry the envelope on
// the peer-overlay path. Receivers reconstruct the canonical string
// via CanonicalBytes and verify it with Verify (treating the
// canonical bytes as the signed payload).
const (
	HeaderDeviceID = "X-Waired-Device"
	HeaderIssuedAt = "X-Waired-Issued-At"
	HeaderNonce    = "X-Waired-Nonce"
)

// SignatureSize is ed25519.SignatureSize, re-exported so callers
// don't have to import crypto/ed25519 just for this constant.
const SignatureSize = ed25519.SignatureSize

// NonceMinBytes is the minimum decoded length the receiver requires
// for the `nonce` envelope field.
const NonceMinBytes = 12

// Envelope is the always-present subset of a signed request body.
// Endpoint-specific request types are expected to embed Envelope (or
// declare matching json tags) so the helper can parse it without
// knowing the full body shape.
type Envelope struct {
	DeviceID string `json:"device_id"`
	IssuedAt string `json:"issued_at"`
	Nonce    string `json:"nonce"`
}

// Sentinel errors. All Verify / VerifyEnvelope failures wrap one of
// these so callers can map to HTTP status codes consistently.
var (
	ErrMissingSignature   = errors.New("signedreq: X-Waired-Body-Signature header is missing")
	ErrSignatureEncoding  = errors.New("signedreq: signature header is not a valid base64 ed25519 signature")
	ErrSignatureMismatch  = errors.New("signedreq: signature does not match body")
	ErrBodyTooLarge       = errors.New("signedreq: body exceeds size limit")
	ErrBodyRead           = errors.New("signedreq: failed to read body")
	ErrBadIssuedAt        = errors.New("signedreq: issued_at is missing or unparseable")
	ErrIssuedAtOutOfRange = errors.New("signedreq: issued_at outside server-clock window")
	ErrBadNonce           = errors.New("signedreq: nonce missing, malformed, or shorter than minimum")
	ErrDeviceMismatch     = errors.New("signedreq: body.device_id does not match expected device")
	ErrReplayDetected     = errors.New("signedreq: nonce already seen within TTL")
)

// Sign returns the base64 (StdEncoding) Ed25519 signature over body
// bytes. Callers typically write the result into the
// X-Waired-Body-Signature header via SetSignature.
func Sign(priv ed25519.PrivateKey, body []byte) string {
	sig := ed25519.Sign(priv, body)
	return base64.StdEncoding.EncodeToString(sig)
}

// SetSignature stores Sign(priv, body) into h under
// HeaderBodySignature. The body argument MUST be the exact bytes that
// will be transmitted on the wire — any later mutation invalidates the
// signature.
func SetSignature(h http.Header, priv ed25519.PrivateKey, body []byte) {
	h.Set(HeaderBodySignature, Sign(priv, body))
}

// CanonicalBytes formats the bytes that get signed on the
// header-envelope path. The canonical form is:
//
//	device_id "\n" issued_at "\n" nonce "\n" sha256_hex(body)
//
// Hashing the body lets the canonical string stay tiny regardless of
// payload size, while still binding the envelope to the exact bytes
// that will travel on the wire — a tampered body produces a different
// hash and therefore a signature mismatch.
func CanonicalBytes(deviceID, issuedAt, nonce string, body []byte) []byte {
	sum := sha256.Sum256(body)
	canonical := deviceID + "\n" + issuedAt + "\n" + nonce + "\n" + hex.EncodeToString(sum[:])
	return []byte(canonical)
}

// SetHeaderEnvelope writes the X-Waired-Device / X-Waired-Issued-At /
// X-Waired-Nonce / X-Waired-Body-Signature headers onto h. body must
// be the exact bytes that will be transmitted (otherwise the
// canonical-string hash will not match on the receiver). Returns the
// signature string so the caller can log or attach it elsewhere.
func SetHeaderEnvelope(h http.Header, priv ed25519.PrivateKey, deviceID, issuedAt, nonce string, body []byte) string {
	canonical := CanonicalBytes(deviceID, issuedAt, nonce, body)
	sig := Sign(priv, canonical)
	h.Set(HeaderDeviceID, deviceID)
	h.Set(HeaderIssuedAt, issuedAt)
	h.Set(HeaderNonce, nonce)
	h.Set(HeaderBodySignature, sig)
	return sig
}

// VerifyHeaderEnvelope is the receiver-side counterpart to
// SetHeaderEnvelope: it pulls the four headers from h, reconstructs
// CanonicalBytes(headers, body), verifies it against pub, then runs
// VerifyEnvelope on the headers (window check + nonce length).
//
// Returns the parsed Envelope (so callers can plug it into
// ConsumeNonce or audit logs) plus the decoded nonce bytes.
//
// Caller responsibilities:
//   - body must already be read from r.Body (signedreq.ReadBody is the
//     usual source);
//   - peerDeviceID is the DeviceID the caller has resolved from the
//     WG source IP via NetworkMap lookup. If headers' device_id
//     disagrees, ErrDeviceMismatch is returned.
func VerifyHeaderEnvelope(h http.Header, body []byte, pub ed25519.PublicKey, peerDeviceID string, now time.Time, window time.Duration) (Envelope, []byte, error) {
	env := Envelope{
		DeviceID: h.Get(HeaderDeviceID),
		IssuedAt: h.Get(HeaderIssuedAt),
		Nonce:    h.Get(HeaderNonce),
	}
	canonical := CanonicalBytes(env.DeviceID, env.IssuedAt, env.Nonce, body)
	if err := Verify(h, canonical, pub); err != nil {
		return env, nil, err
	}
	nonce, err := VerifyEnvelope(env, peerDeviceID, now, window)
	if err != nil {
		return env, nonce, err
	}
	return env, nonce, nil
}

// ReadBody reads up to maxBytes+1 from r. Returning more than maxBytes
// indicates the caller should reject with 413; ReadBody itself returns
// the read bytes plus ErrBodyTooLarge so the caller can attach the
// limit to a structured error response. r is not closed.
func ReadBody(r io.Reader, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("%w: maxBytes must be positive", ErrBodyRead)
	}
	b, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBodyRead, err)
	}
	if int64(len(b)) > maxBytes {
		return b[:maxBytes], fmt.Errorf("%w: max %d bytes", ErrBodyTooLarge, maxBytes)
	}
	return b, nil
}

// Verify checks the X-Waired-Body-Signature header against body using
// pub. Returns ErrMissingSignature / ErrSignatureEncoding /
// ErrSignatureMismatch on failure.
func Verify(h http.Header, body []byte, pub ed25519.PublicKey) error {
	sigB64 := h.Get(HeaderBodySignature)
	if sigB64 == "" {
		return ErrMissingSignature
	}
	sig, err := DecodeFlexBase64(sigB64)
	if err != nil || len(sig) != SignatureSize {
		return fmt.Errorf("%w: got %d bytes", ErrSignatureEncoding, len(sig))
	}
	if !ed25519.Verify(pub, body, sig) {
		return ErrSignatureMismatch
	}
	return nil
}

// VerifyEnvelope validates the device_id / issued_at / nonce shape of
// env and returns the decoded nonce bytes. expectedDeviceID is what
// the caller has already determined out-of-band (CP: bearer-token
// owner; overlay: WG-source-IP → NetworkMap lookup). window is the
// allowed clock-skew on issued_at (typically ±60s).
//
// VerifyEnvelope intentionally does NOT touch the NonceCache. Callers
// run any payload-specific validation first, then call
// ConsumeNonce (or NonceCache.Consume directly) so a request that is
// going to be rejected anyway doesn't burn the agent's nonce.
func VerifyEnvelope(env Envelope, expectedDeviceID string, now time.Time, window time.Duration) ([]byte, error) {
	if env.DeviceID != expectedDeviceID {
		return nil, fmt.Errorf("%w: body=%q expected=%q", ErrDeviceMismatch, env.DeviceID, expectedDeviceID)
	}
	issuedAt, err := time.Parse(time.RFC3339, env.IssuedAt)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadIssuedAt, err)
	}
	if d := now.Sub(issuedAt); d > window || d < -window {
		return nil, fmt.Errorf("%w: server=%s body=%s drift=%s",
			ErrIssuedAtOutOfRange, now.Format(time.RFC3339), env.IssuedAt, d)
	}
	nonce, err := DecodeFlexBase64(env.Nonce)
	if err != nil || len(nonce) < NonceMinBytes {
		return nil, fmt.Errorf("%w: got %d bytes", ErrBadNonce, len(nonce))
	}
	return nonce, nil
}

// ConsumeNonce records the (deviceID, nonce) pair against cache and
// returns ErrReplayDetected if the nonce was already seen within ttl.
// nonce is the decoded byte slice returned from VerifyEnvelope.
func ConsumeNonce(cache NonceCache, deviceID string, nonce []byte, now time.Time, ttl time.Duration) error {
	if cache == nil {
		return nil
	}
	if !cache.Consume(deviceID, string(nonce), now, ttl) {
		return ErrReplayDetected
	}
	return nil
}

// ParseEnvelope decodes just the envelope fields out of body. Callers
// that need the full request type can json.Unmarshal twice (once into
// Envelope, once into their wider type) or embed Envelope into their
// type — both work because Verify operates on raw body bytes.
func ParseEnvelope(body []byte) (Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return Envelope{}, err
	}
	return env, nil
}

// DecodeFlexBase64 accepts standard or URL base64, padded or raw.
// Returned for convenience because every signed-request consumer
// needs it (signatures, nonces, device fingerprints).
func DecodeFlexBase64(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding,
		base64.URLEncoding,
		base64.RawStdEncoding,
		base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, errors.New("signedreq: not valid base64")
}

// NonceCache is the replay-detection interface. Implementations must
// be safe for concurrent use and must opportunistically GC entries
// older than the supplied ttl.
type NonceCache interface {
	// Consume returns true on first observation of (deviceID, nonce)
	// within ttl; false if the nonce was already seen for this device
	// within ttl (= replay).
	Consume(deviceID, nonce string, now time.Time, ttl time.Duration) bool
}

// MemoryNonceCache is the default in-process NonceCache. State is
// keyed by deviceID so two devices reusing the same nonce string
// (rare but legal) don't collide. Spanner-backed implementation is
// deferred to Phase 10 (todo.md "Phase 10: agent → CP push の認証強度").
type MemoryNonceCache struct {
	mu      sync.Mutex
	buckets map[string]map[string]time.Time
}

// NewMemoryNonceCache returns an empty in-process cache.
func NewMemoryNonceCache() *MemoryNonceCache {
	return &MemoryNonceCache{buckets: map[string]map[string]time.Time{}}
}

// Consume implements NonceCache.
func (c *MemoryNonceCache) Consume(deviceID, nonce string, now time.Time, ttl time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	bucket, ok := c.buckets[deviceID]
	if !ok {
		bucket = map[string]time.Time{}
		c.buckets[deviceID] = bucket
	}
	if seen, ok := bucket[nonce]; ok && now.Sub(seen) <= ttl {
		return false
	}
	bucket[nonce] = now
	for k, t := range bucket {
		if now.Sub(t) > ttl {
			delete(bucket, k)
		}
	}
	return true
}

// HTTPStatus maps a signedreq sentinel error to a recommended HTTP
// status code. Endpoint handlers should still own their own JSON error
// body shape (CP and overlay use different envelopes), but the status
// code mapping is uniform.
func HTTPStatus(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, ErrBodyTooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, ErrBodyRead),
		errors.Is(err, ErrBadIssuedAt),
		errors.Is(err, ErrBadNonce),
		errors.Is(err, ErrDeviceMismatch):
		return http.StatusBadRequest
	case errors.Is(err, ErrMissingSignature),
		errors.Is(err, ErrSignatureEncoding),
		errors.Is(err, ErrSignatureMismatch),
		errors.Is(err, ErrIssuedAtOutOfRange),
		errors.Is(err, ErrReplayDetected):
		return http.StatusUnauthorized
	default:
		return http.StatusInternalServerError
	}
}
