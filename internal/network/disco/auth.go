package disco

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"time"

	wireframe "github.com/waired-ai/waired-agent/proto/disco"
)

// signFrameHMAC encodes f without an auth tag, computes HMAC-SHA256
// over the prefix with key, sets f.HMACTag to the result, and re-encodes.
// Returns the final wire bytes. Used for STUN frames against the relay's
// UDP echo service — peer↔peer probe/pong/cmm have moved to AEAD (see
// wireframe.EncodeSealed / DecodeSealed in internal/disco/sealed.go).
func signFrameHMAC(f *wireframe.Frame, key []byte) ([]byte, error) {
	f.HMACTag = nil
	bare, err := f.Encode()
	if err != nil {
		return nil, err
	}
	h := hmac.New(sha256.New, key)
	h.Write(bare)
	f.HMACTag = h.Sum(nil)
	return f.Encode()
}

// verifyFrameHMAC re-derives the HMAC over the byte-prefix preceding
// the trailing auth TLV and compares constant-time.
func verifyFrameHMAC(raw []byte, key []byte, tag []byte) error {
	if len(tag) != wireframe.HMACTagSize {
		return errors.New("disco/auth: hmac tag wrong size")
	}
	prefix, err := wireframe.SignedPrefix(raw)
	if err != nil {
		return err
	}
	h := hmac.New(sha256.New, key)
	h.Write(prefix)
	expected := h.Sum(nil)
	if !hmac.Equal(expected, tag) {
		return errors.New("disco/auth: hmac mismatch")
	}
	return nil
}

// timestampInWindow returns true when ts is within ±window of now.
func timestampInWindow(now, ts time.Time, window time.Duration) bool {
	d := now.Sub(ts)
	if d < 0 {
		d = -d
	}
	return d <= window
}
