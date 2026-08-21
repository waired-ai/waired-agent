package integration

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLoadOrCreateGatewayToken_GeneratesOnFirstCall(t *testing.T) {
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

// The file is the token's only home, and this function's own doc
// comment promises it always exists afterwards — env.sh `cat`s that
// path and `waired doctor` stats it, so both point at nothing if it
// does not.
//
// #654 is why that is pinned rather than assumed. On darwin the token
// used to be mirrored into a keychain and read from there first, and a
// keychain item outlived the state dir: a `--clean` reinstall hit the
// mirror, returned early, and never created the file. Nothing outlives
// the state dir any more, so a wiped file means a new token — which is
// the same thing Linux and Windows always did.
//
// Product contract from #654, not a record of today's behaviour.
func TestLoadOrCreateGatewayToken_AlwaysLeavesTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway-token")

	tok, err := LoadOrCreateGatewayToken(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// The shape of a wiped state dir.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	got, err := LoadOrCreateGatewayToken(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got == tok {
		t.Errorf("the previous token came back after the file was deleted; something outlives the state dir")
	}
	if !validGatewayToken(got) {
		t.Errorf("token = %q, want a fresh valid one", got)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the file was not written: %v", err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != got {
		t.Errorf("file holds %q, want the token %q verbatim (env.sh cats it)", body, got)
	}
}
