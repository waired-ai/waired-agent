package integration

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/waired-ai/waired-agent/internal/platform/securestore"
)

// useMemKeychain swaps in a FRESH in-memory Keychain per test. Fresh
// matters: every gateway token shares one (account, service) item, so a
// store shared across tests would leak one test's token into another. It
// also stops `go test` execing /usr/bin/security on darwin.
func useMemKeychain(t *testing.T) {
	t.Helper()
	t.Cleanup(securestore.SwapStoreForTest(securestore.NewMemStore()))
}

func TestLoadOrCreateGatewayToken_GeneratesOnFirstCall(t *testing.T) {
	useMemKeychain(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway-token")

	tok, err := LoadOrCreateGatewayToken(path)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if !validGatewayToken(tok) {
		t.Fatalf("generated token %q is not a valid 32-byte hex", tok)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Permission semantics differ on Windows; the test suite is
	// POSIX-targeted (see plan Q11) so 0600 must hold here.
	if runtime.GOOS != "windows" {
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("token mode = %o, want 0600", info.Mode().Perm())
		}
	}
}

func TestLoadOrCreateGatewayToken_StableAcrossCalls(t *testing.T) {
	useMemKeychain(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway-token")

	first, err := LoadOrCreateGatewayToken(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreateGatewayToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("token mutated across calls: %q vs %q", first, second)
	}
}

func TestLoadOrCreateGatewayToken_FixesLoosePermsOnExisting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only permission semantics")
	}
	useMemKeychain(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway-token")
	// Pre-seed with a valid token written at 0644.
	tok, err := generateGatewayToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(tok), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := LoadOrCreateGatewayToken(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != tok {
		t.Fatalf("load returned different token: %q vs %q", got, tok)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("perms after load = %o, want 0600 (defensive reperm)", info.Mode().Perm())
	}
}

func TestLoadOrCreateGatewayToken_RejectsCorruptFile(t *testing.T) {
	useMemKeychain(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway-token")
	if err := os.WriteFile(path, []byte("not-a-hex-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateGatewayToken(path); err == nil {
		t.Fatal("expected error for malformed token, got nil")
	}
}

func TestRotateGatewayToken_Differs(t *testing.T) {
	useMemKeychain(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway-token")
	first, err := LoadOrCreateGatewayToken(path)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := RotateGatewayToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if rotated == first {
		t.Fatal("RotateGatewayToken returned the same value (statistically impossible)")
	}
	loaded, err := LoadOrCreateGatewayToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded != rotated {
		t.Fatalf("after rotate, load = %q, want %q", loaded, rotated)
	}
}

func TestLoadOrCreateGatewayToken_EmptyPathErrors(t *testing.T) {
	if _, err := LoadOrCreateGatewayToken(""); err == nil {
		t.Fatal("expected error for empty path")
	}
}

func TestLoadOrCreateGatewayToken_KeychainBacked(t *testing.T) {
	useMemKeychain(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway-token")

	tok, err := LoadOrCreateGatewayToken(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Drop the file; the Keychain copy must still serve the same token.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	got, err := LoadOrCreateGatewayToken(path)
	if err != nil {
		t.Fatalf("reload after file loss: %v", err)
	}
	if got != tok {
		t.Fatalf("got %q, want %q (from keychain)", got, tok)
	}
}
