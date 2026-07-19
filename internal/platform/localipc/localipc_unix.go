//go:build linux || darwin

package localipc

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// listen binds a unix-domain socket at socketPath. It creates the parent
// runtime dir (0755, traversable by the desktop user), removes any stale
// node, and opens the socket 0666 so a client running as a different uid
// than the service-user daemon can connect. Authorization is the
// transport itself (a browser/network peer cannot open a unix socket),
// not a uid check — see the package doc and waired#838.
func listen(socketPath string) (net.Listener, error) {
	dir := filepath.Dir(socketPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("localipc: create runtime dir %s: %w", dir, err)
	}
	// A previous unclean exit can leave a stale socket node that would make
	// bind fail with EADDRINUSE. systemd's RuntimeDirectory clears
	// /run/waired on stop, but a raw-binary or crashed run may not — so
	// remove it defensively.
	//
	// "Stale" must be proven, not assumed: an unconditional remove lets a
	// second agent instance steal a RUNNING instance's socket, leaving the
	// first with a listener no client can ever reach again (waired#81).
	// A successful dial means someone is accepting there, so refuse instead.
	// The probe/bind window is a benign TOCTOU: losing it just degrades to
	// the pre-#81 behaviour, and the daemon fails open either way.
	if c, derr := net.DialTimeout("unix", socketPath, 250*time.Millisecond); derr == nil {
		_ = c.Close()
		return nil, fmt.Errorf("localipc: %s: %w", socketPath, ErrEndpointInUse)
	}
	// A genuine ENOENT is fine.
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("localipc: remove stale socket %s: %w", socketPath, err)
	}
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("localipc: listen unix %s: %w", socketPath, err)
	}
	if err := os.Chmod(socketPath, 0o666); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("localipc: chmod socket %s: %w", socketPath, err)
	}
	return ln, nil
}
