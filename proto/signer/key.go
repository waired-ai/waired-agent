package signer

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// Key is a single Ed25519 keypair used by the CP to sign documents.
// Key *storage* (0600 file + macOS Keychain via securestore) is a
// control-plane concern and lives in the private waired repo; this
// package carries only the pure signing surface that both sides of the
// protocol must byte-agree on.
type Key struct {
	Private ed25519.PrivateKey
	Public  ed25519.PublicKey
}

// Generate returns a fresh random Key. Production key persistence is
// CP-side (LoadOrGenerate in the waired repo); Generate exists so tests
// on either side can mint a key without OS keystore dependencies.
func Generate() (*Key, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("signer: generate: %w", err)
	}
	return &Key{Private: priv, Public: pub}, nil
}

// PublicKeyBase64 returns the public key as standard base64 - the format
// used in the .well-known endpoint and the TOFU agent cache.
func (k *Key) PublicKeyBase64() string {
	return base64.StdEncoding.EncodeToString(k.Public)
}

// Sign returns an Ed25519 signature over msg. Use CanonicalJSON to derive
// msg from a struct in a way that a verifier can reproduce.
func (k *Key) Sign(msg []byte) []byte {
	return ed25519.Sign(k.Private, msg)
}

// VerifyWithKey checks an Ed25519 signature against the supplied public key.
func VerifyWithKey(pub ed25519.PublicKey, msg, sig []byte) bool {
	return ed25519.Verify(pub, msg, sig)
}
