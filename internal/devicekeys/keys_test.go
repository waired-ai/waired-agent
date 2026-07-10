package devicekeys

import (
	"bytes"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/waired-ai/waired-agent/internal/platform/securestore"
)

// useMemKeychain swaps in an in-memory Keychain so these tests never exec
// /usr/bin/security (and never trigger an auth prompt) on darwin.
func useMemKeychain(t *testing.T) {
	t.Helper()
	t.Cleanup(securestore.SwapStoreForTest(securestore.NewMemStore()))
}

func TestLoadOrCreateMachineKey_GeneratesAndPersists(t *testing.T) {
	useMemKeychain(t)
	path := filepath.Join(t.TempDir(), "machine.key")

	k1, err := LoadOrCreateMachineKey(path)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if len(k1.Private) != ed25519.PrivateKeySize {
		t.Fatalf("private key %d bytes, want %d", len(k1.Private), ed25519.PrivateKeySize)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("file mode %v, want 0600", fi.Mode().Perm())
		}
	}

	k2, err := LoadOrCreateMachineKey(path)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if !bytes.Equal(k1.Private, k2.Private) {
		t.Fatal("machine key not stable across loads")
	}
}

func TestLoadOrCreateMachineKey_KeychainSurvivesFileLoss(t *testing.T) {
	useMemKeychain(t)
	path := filepath.Join(t.TempDir(), "machine.key")

	k1, err := LoadOrCreateMachineKey(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Lose the on-disk file; the Keychain copy must still serve the key
	// (so an accidental secrets/ wipe does not force re-enrollment on macOS).
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	k2, err := LoadOrCreateMachineKey(path)
	if err != nil {
		t.Fatalf("reload after file loss: %v", err)
	}
	if !bytes.Equal(k1.Private, k2.Private) {
		t.Fatal("expected the same key from the keychain after file loss")
	}
}

func TestLoadOrCreateMachineKey_RejectsWrongSize(t *testing.T) {
	useMemKeychain(t)
	path := filepath.Join(t.TempDir(), "machine.key")
	if err := os.WriteFile(path, []byte("too-short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateMachineKey(path); err == nil {
		t.Fatal("expected error on wrong-size machine key")
	}
}

// NodeKey is intentionally NOT Keychain-backed this round (#261 scope); it
// stays file-only. Keep light coverage so the unchanged path is exercised.

func TestLoadOrCreateNodeKey_GeneratesAndStable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.key")
	k1, err := LoadOrCreateNodeKey(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(k1.Private) != 32 {
		t.Fatalf("node private %d bytes, want 32", len(k1.Private))
	}
	k2, err := LoadOrCreateNodeKey(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if k1.Private != k2.Private {
		t.Fatal("node key not stable across loads")
	}
}

func TestSaveNodeKey_NilErrors(t *testing.T) {
	if err := SaveNodeKey(filepath.Join(t.TempDir(), "n"), nil); err == nil {
		t.Fatal("expected error on nil node key")
	}
}
