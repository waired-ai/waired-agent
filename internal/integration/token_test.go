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

// TestLoadOrCreateGatewayToken_KeychainHitRestoresTheFile pins #654: a
// Keychain hit used to return without writing the file, so the 0600 file
// this function's own doc comment promises to always keep (#261) was
// absent.
//
// It is a macOS defect with a plain cause. securestore.Read is
// Keychain-first there, and a Keychain item outlives the state dir —
// logout wiped machine-key, access-token and refresh-token but not this
// one. So a `--clean` reinstall hit the Keychain and never created the
// file, which is why the observed host had access_token, node.key and
// refresh_token in secrets/ (all written through the dual-writing
// securestore.Write) and no gateway-token at all. env.sh `cat`s that path
// and `waired doctor` stats it, so both were left pointing at a file that
// would never appear.
//
// Product contract from #654, not a record of today's behaviour.
func TestLoadOrCreateGatewayToken_KeychainHitRestoresTheFile(t *testing.T) {
	useMemKeychain(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway-token")

	tok, err := LoadOrCreateGatewayToken(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// The shape of a wiped state dir on a host whose Keychain still holds
	// the item.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	got, err := LoadOrCreateGatewayToken(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got != tok {
		t.Fatalf("token = %q, want %q", got, tok)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the file was not restored after a keychain hit: %v", err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != tok {
		t.Errorf("file holds %q, want the token %q verbatim (env.sh cats it)", body, tok)
	}
}
