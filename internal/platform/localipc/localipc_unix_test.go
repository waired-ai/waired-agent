//go:build linux || darwin

package localipc

import (
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// TestListenCreatesDirAndMode checks that Listen creates a missing parent
// runtime dir and opens the socket 0666 so a different-uid desktop client
// can connect (waired#838).
func TestListenCreatesDirAndMode(t *testing.T) {
	// A missing subdir exercises the MkdirAll path.
	sock := filepath.Join(t.TempDir(), "run", "mgmt.sock")
	ln, err := Listen(sock)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if fi.Mode()&fs.ModeSocket == 0 {
		t.Fatalf("path is not a socket: mode=%v", fi.Mode())
	}
	if perm := fi.Mode().Perm(); perm != 0o666 {
		t.Fatalf("socket perm = %o, want 0666", perm)
	}
}

// TestListenRemovesStaleNode confirms Listen clears a leftover node from an
// unclean prior exit (otherwise bind fails with EADDRINUSE).
func TestListenRemovesStaleNode(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "mgmt.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := Listen(sock)
	if err != nil {
		t.Fatalf("Listen over stale node: %v", err)
	}
	defer ln.Close()

	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial fresh socket: %v", err)
	}
	_ = c.Close()
}
