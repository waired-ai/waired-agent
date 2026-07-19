package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/waired-ai/waired-agent/internal/platform/keychain"
	"github.com/waired-ai/waired-agent/internal/platform/securestore"
)

// TestMain swaps an in-memory Keychain for the whole cmd/waired test
// binary so logout's securestore.Remove never deletes the developer's real
// Keychain items when `go test` runs on darwin. On Linux/CI the keychain
// stub already returns ErrUnsupported, but this keeps darwin dev runs safe.
//
// It also clears mgmtWriteBase for the whole binary: since waired#838
// management writes travel over a local IPC socket, which httptest cannot
// serve, so these tests address their httptest TCP servers verbatim. They
// cover command logic and endpoint semantics, both transport-independent;
// the socket routing itself is asserted in main_ipcwrite_unix_test.go and
// the transport in internal/management/ipcclient.
func TestMain(m *testing.M) {
	restore := securestore.SwapStoreForTest(securestore.NewMemStore())
	mgmtWriteBase = ""
	code := m.Run()
	restore()
	os.Exit(code)
}

// seedEnrolled writes a minimal enrolled state dir (identity with a CP URL
// + an access token) so logout's server-deauth path has something to call.
func seedEnrolled(t *testing.T, dir, controlURL string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf(`{"device_id":"dev_1","network_id":"net_1","control_url":%q}`, controlURL)
	if err := os.WriteFile(filepath.Join(dir, "identity.json"), []byte(id), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secrets", "access_token"), []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRunLogout_CallsServerDeauthThenWipes(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/devices/self/logout" {
			atomic.AddInt32(&hits, 1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dir := t.TempDir()
	seedEnrolled(t, dir, srv.URL)

	if err := runLogout([]string{"--state-dir", dir, "--yes"}); err != nil {
		t.Fatalf("runLogout: %v", err)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Errorf("expected 1 server-deauth call, got %d", hits)
	}
	if _, err := os.Stat(filepath.Join(dir, "secrets", "access_token")); !os.IsNotExist(err) {
		t.Errorf("access_token should be wiped after logout, err=%v", err)
	}
}

func TestRunLogout_LocalFlagSkipsServerCall(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dir := t.TempDir()
	seedEnrolled(t, dir, srv.URL)

	if err := runLogout([]string{"--state-dir", dir, "--yes", "--local"}); err != nil {
		t.Fatalf("runLogout --local: %v", err)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Errorf("--local must not contact the control plane, got %d calls", hits)
	}
	if _, err := os.Stat(filepath.Join(dir, "identity.json")); !os.IsNotExist(err) {
		t.Errorf("identity.json should be wiped even with --local, err=%v", err)
	}
}

// A server-deauth failure must NOT block the local wipe (logout always
// clears local state; the user is warned instead).
func TestRunLogout_ServerErrorStillWipesLocal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	dir := t.TempDir()
	seedEnrolled(t, dir, srv.URL)

	if err := runLogout([]string{"--state-dir", dir, "--yes"}); err != nil {
		t.Fatalf("runLogout must succeed despite server error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "identity.json")); !os.IsNotExist(err) {
		t.Errorf("identity.json should be wiped despite server error, err=%v", err)
	}
}

func TestRunLogout_DeletesIdentityAndSecretsKeepsCache(t *testing.T) {
	dir := t.TempDir()

	// Pre-populate the state dir as `waired init` would.
	mustWrite := func(p string, body []byte, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, body, mode); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(filepath.Join(dir, "identity.json"), []byte(`{"device_id":"x"}`), 0o644)
	mustWrite(filepath.Join(dir, "secrets", "machine.key"), []byte("mk"), 0o600)
	mustWrite(filepath.Join(dir, "secrets", "node.key"), []byte("nk"), 0o600)
	mustWrite(filepath.Join(dir, "secrets", "access_token"), []byte("tok"), 0o600)
	mustWrite(filepath.Join(dir, "cache", "network_map.json"), []byte("nm"), 0o644)

	if err := runLogout([]string{"--state-dir", dir, "--yes"}); err != nil {
		t.Fatalf("runLogout: %v", err)
	}

	gone := []string{
		filepath.Join(dir, "identity.json"),
		filepath.Join(dir, "secrets", "machine.key"),
		filepath.Join(dir, "secrets", "node.key"),
		filepath.Join(dir, "secrets", "access_token"),
	}
	for _, p := range gone {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("expected %s to be removed, got err=%v", p, err)
		}
	}

	// cache/ stays — Network Map and signing key cache are recoverable from CP,
	// no harm leaving them; they'll be re-fetched on next enrollment.
	if _, err := os.Stat(filepath.Join(dir, "cache", "network_map.json")); err != nil {
		t.Errorf("cache/network_map.json should remain after logout, got err=%v", err)
	}
}

// TestRunLogout_DeletesKeychainItems verifies logout clears the
// Keychain-backed secrets, not just their files — otherwise a stale
// Keychain entry would resurrect a logged-out credential (#261).
func TestRunLogout_DeletesKeychainItems(t *testing.T) {
	fake := securestore.NewMemStore()
	t.Cleanup(securestore.SwapStoreForTest(fake))

	dir := t.TempDir()
	seedEnrolled(t, dir, "") // empty control URL + --local => no server deauth

	items := []keychain.Item{
		{Account: securestore.Account, Service: securestore.ServiceMachineKey},
		{Account: securestore.Account, Service: securestore.ServiceAccessToken},
		{Account: securestore.Account, Service: securestore.ServiceRefreshToken},
	}
	for _, it := range items {
		if err := fake.Set(it, []byte("secret")); err != nil {
			t.Fatal(err)
		}
	}

	if err := runLogout([]string{"--state-dir", dir, "--yes", "--local"}); err != nil {
		t.Fatalf("runLogout: %v", err)
	}
	for _, it := range items {
		if ok, _ := fake.Exists(it); ok {
			t.Errorf("keychain item %q should be deleted after logout", it.Service)
		}
	}
}

// --server-only contacts the CP to deregister but must NOT delete local
// identity/secrets (on the .deb path dpkg/purge owns local deletion).
// --revoke selects the terminal self-revoke endpoint.
func TestRunLogout_ServerOnlyRevokeKeepsLocal(t *testing.T) {
	var gotPath string
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dir := t.TempDir()
	seedEnrolled(t, dir, srv.URL)

	if err := runLogout([]string{"--state-dir", dir, "--yes", "--server-only", "--revoke"}); err != nil {
		t.Fatalf("runLogout --server-only --revoke: %v", err)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Errorf("expected 1 CP call, got %d", hits)
	}
	if gotPath != "/v1/devices/self/revoke" {
		t.Errorf("path=%q want /v1/devices/self/revoke", gotPath)
	}
	// --server-only keeps local state (dpkg owns deletion).
	for _, p := range []string{
		filepath.Join(dir, "identity.json"),
		filepath.Join(dir, "secrets", "access_token"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("--server-only must keep %s, err=%v", p, err)
		}
	}
}

// --revoke (without --server-only) still wipes local state but targets the
// terminal self-revoke endpoint rather than self-logout.
func TestRunLogout_RevokeUsesRevokeEndpoint(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dir := t.TempDir()
	seedEnrolled(t, dir, srv.URL)

	if err := runLogout([]string{"--state-dir", dir, "--yes", "--revoke"}); err != nil {
		t.Fatalf("runLogout --revoke: %v", err)
	}
	if gotPath != "/v1/devices/self/revoke" {
		t.Errorf("path=%q want /v1/devices/self/revoke", gotPath)
	}
	if _, err := os.Stat(filepath.Join(dir, "identity.json")); !os.IsNotExist(err) {
		t.Errorf("identity.json should be wiped after --revoke, err=%v", err)
	}
}

func TestRunLogout_MissingStateDirIsNoop(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	if err := runLogout([]string{"--state-dir", dir, "--yes"}); err != nil {
		t.Fatalf("logout on missing state dir should be a no-op, got: %v", err)
	}
}

func TestRunLogout_NotEnrolled(t *testing.T) {
	// State dir exists but has no identity.json — earlier setup attempt or
	// daemon-started-without-init scenario. Logout should still succeed
	// (idempotent) and not error.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := runLogout([]string{"--state-dir", dir, "--yes"}); err != nil {
		t.Fatalf("logout on not-enrolled state dir should succeed, got: %v", err)
	}
}
