package devicekeys

import (
	"bytes"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLoadOrCreateMachineKey_GeneratesAndPersists(t *testing.T) {
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

// The file is the whole of the machine key, on every OS. Losing it
// means losing the device identity and enrolling fresh — which is what
// `uninstall.sh --clean` promises and what Linux and Windows always did.
// macOS used to be the exception: the key was mirrored into the System
// keychain and read back first, so a wiped state dir still proved it was
// the same device and the control plane re-enrolled it onto its old row
// (#680, waired#1136). This asserts the exception is gone.
func TestLoadOrCreateMachineKey_FileLossLosesTheKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "machine.key")

	k1, err := LoadOrCreateMachineKey(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	k2, err := LoadOrCreateMachineKey(path)
	if err != nil {
		t.Fatalf("reload after file loss: %v", err)
	}
	if bytes.Equal(k1.Private, k2.Private) {
		t.Fatal("the same key came back after the file was deleted; something outlives the state dir")
	}
}

func TestLoadOrCreateMachineKey_RejectsWrongSize(t *testing.T) {
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
