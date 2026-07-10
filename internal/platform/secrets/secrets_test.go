package secrets_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/waired-ai/waired-agent/internal/platform/secrets"
)

func TestWriteSecret_AtomicAndReadable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key.bin")
	want := []byte("super-secret-payload")
	if err := secrets.WriteSecret(path, want); err != nil {
		t.Fatalf("WriteSecret: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("content roundtrip = %q, want %q", got, want)
	}
}

func TestWriteSecret_PermissionsOnUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows uses NTFS ACLs; mode bits are not exercised")
	}
	dir := t.TempDir()
	if err := secrets.SecureDir(dir); err != nil {
		t.Fatalf("SecureDir: %v", err)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if mode := di.Mode().Perm(); mode != 0o700 {
		t.Fatalf("SecureDir mode = %o, want 0700", mode)
	}

	path := filepath.Join(dir, "key.bin")
	if err := secrets.WriteSecret(path, []byte("x")); err != nil {
		t.Fatalf("WriteSecret: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := fi.Mode().Perm(); mode != 0o600 {
		t.Fatalf("WriteSecret mode = %o, want 0600", mode)
	}
}

func TestWriteFile_NonSecretPermissionsOnUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows uses NTFS ACLs; mode bits are not exercised")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "identity.json")
	if err := secrets.WriteFile(path, []byte("{}"), secrets.NonSecret); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// 0o644 modulo umask; we only require world-not-writable & owner-writable.
	if mode := fi.Mode().Perm(); mode&0o077 != 0o044 && mode&0o077 != 0o040 && mode&0o077 != 0 {
		t.Fatalf("WriteFile NonSecret mode = %o, want owner-only-write", mode)
	}
}

func TestWriteSecret_OverwritesAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rolled.bin")
	if err := secrets.WriteSecret(path, []byte("v1")); err != nil {
		t.Fatalf("write v1: %v", err)
	}
	if err := secrets.WriteSecret(path, []byte("v2-longer-payload")); err != nil {
		t.Fatalf("write v2: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "v2-longer-payload" {
		t.Fatalf("overwrite content = %q, want v2-longer-payload", got)
	}
	// Temp file must not have leaked.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() == filepath.Base(path) {
			continue
		}
		t.Fatalf("unexpected leftover %q in %s", e.Name(), dir)
	}
}
