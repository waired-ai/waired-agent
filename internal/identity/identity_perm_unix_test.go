//go:build linux || darwin

package identity

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestLoadPermissionDenied pins the error contract the CLI's sudo-hint
// classification depends on: an unreadable state dir must yield an
// error satisfying errors.Is(err, fs.ErrPermission), not be misread as
// "not enrolled" (nil, nil) or fail on an unrelated chmod.
func TestLoadPermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission checks")
	}
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "identity.json"),
		[]byte(`{"device_id":"did_x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	id, err := Load(dir)
	if id != nil {
		t.Fatalf("Load = %+v, want nil identity", id)
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("Load error = %v, want fs.ErrPermission in the chain", err)
	}
}
