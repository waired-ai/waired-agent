//go:build darwin

package keychain

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"testing"
)

// TestRealKeychainRoundTrip exercises the actual macOS Keychain on
// the developer machine. Gated by WAIRED_KEYCHAIN_REALHOST=1 so a
// normal `go test ./...` does not mutate the user's login keychain
// or trigger an authorization dialog. Must be run from an Aqua-
// attached Terminal session (an SSH or background-task context
// returns "User interaction is not allowed" because the login
// keychain has no UI to display the unlock prompt against).
//
// The "binary key" sub-test is the decisive regression catcher for
// #512: a raw ed25519 private key (64 bytes, frequently containing NUL
// and high bytes) is exactly what `security -w` used to hex-mangle back
// to 128 bytes — and what exec argv could not carry at all. Only a real
// /usr/bin/security run proves the codec fixed it; the fake-backed unit
// tests round-trip bytes perfectly and cannot.
//
// To run manually:
//
//	WAIRED_KEYCHAIN_REALHOST=1 go test ./internal/platform/keychain/ -run RealKeychain -v
func TestRealKeychainRoundTrip(t *testing.T) {
	if os.Getenv("WAIRED_KEYCHAIN_REALHOST") == "" {
		t.Skip("set WAIRED_KEYCHAIN_REALHOST=1 to exercise the real Keychain")
	}

	// A real 64-byte ed25519 private key — the secret type #512 corrupted.
	_, edPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	// A deterministic all-256-byte blob, so every byte value (incl. 0x00)
	// crosses the boundary regardless of the random key's contents.
	allBytes := make([]byte, 256)
	for i := range allBytes {
		allBytes[i] = byte(i)
	}

	cases := []struct {
		name string
		val  []byte
	}{
		{"printable string", []byte("hello-from-darwin-roundtrip-12345")},
		{"binary ed25519 key (64B, NUL-prone)", []byte(edPriv)},
		{"all 256 byte values", allBytes},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := New()
			item := Item{
				Account: "waired-test-roundtrip",
				Service: "keychain-realhost-verify",
			}
			t.Cleanup(func() { _ = store.Delete(item) })

			if err := store.Set(item, tc.val); err != nil {
				t.Fatalf("Set: %v", err)
			}
			t.Logf("Set OK (%d bytes)", len(tc.val))

			exists, err := store.Exists(item)
			if err != nil {
				t.Fatalf("Exists: %v", err)
			}
			if !exists {
				t.Fatalf("Exists after Set = false; want true")
			}

			got, err := store.Get(item)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if !bytes.Equal(got, tc.val) {
				t.Fatalf("round-trip mismatch: got %d bytes (%x), want %d bytes (%x)",
					len(got), got, len(tc.val), tc.val)
			}
			t.Logf("Get round-tripped %d bytes intact", len(got))

			if err := store.Delete(item); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			if _, err := store.Get(item); !errors.Is(err, ErrNotFound) {
				t.Fatalf("Get after Delete: got %v, want ErrNotFound", err)
			}
		})
	}
}
