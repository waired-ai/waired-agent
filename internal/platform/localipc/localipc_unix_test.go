//go:build linux || darwin

package localipc

import (
	"errors"
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

// TestListenRefusesLiveEndpoint is the waired#81 guard: a second instance
// must NOT unlink a socket someone is still accepting on. Before the fix the
// unconditional os.Remove let it take the endpoint over, leaving the first
// listener alive but permanently unreachable — a silent split-brain.
func TestListenRefusesLiveEndpoint(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "mgmt.sock")

	first, err := Listen(sock)
	if err != nil {
		t.Fatalf("first Listen: %v", err)
	}
	defer first.Close()
	// Accept in the background: a socket with no accepter still completes
	// connect() from the backlog, but serving proves the whole path.
	go func() {
		for {
			c, aerr := first.Accept()
			if aerr != nil {
				return
			}
			_ = c.Close()
		}
	}()

	second, err := Listen(sock)
	if err == nil {
		_ = second.Close()
		t.Fatal("second Listen succeeded; it stole a live endpoint")
	}
	if !errors.Is(err, ErrEndpointInUse) {
		t.Fatalf("second Listen err = %v, want ErrEndpointInUse", err)
	}

	// The first listener must still be reachable — the point of the guard.
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("first listener unreachable after the refused takeover: %v", err)
	}
	_ = c.Close()
}
