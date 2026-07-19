// Package localipc provides the OS-native local IPC listener the Local
// Management API serves mutating (write) requests on: a unix-domain
// socket on Linux/macOS and a named pipe on Windows.
//
// It exists so that writes are reachable only by local processes.
// Browsers, DNS-rebinding pages, and network peers cannot open a unix
// socket or a named pipe, which structurally closes the cross-site write
// surface that the loopback TCP port (internal/management) only guards
// heuristically via Host/Origin/Content-Type checks (waired#836). See
// waired#838 for the threat analysis.
//
// The listener is intentionally connectable by any local user (unix
// socket mode 0666; the named-pipe DACL grants local interactive users):
// the daemon runs as a service user (Linux waired, macOS root, Windows
// LocalSystem) while the tray and CLI run as the desktop user, and on a
// shared machine every logged-in user legitimately drives their own tray.
// It performs NO peer authentication — the transport is the boundary, and
// a same-user local process is deliberately out of scope, because it can
// already read anything the tray can (a bearer token or a uid check would
// not stop it, and a uid allow-list would wrongly reject other users'
// trays on a system-wide install).
package localipc

import (
	"errors"
	"net"
)

// ErrEndpointInUse reports that another process is already serving the
// endpoint. Listen never takes an endpoint away from a live listener: on
// Linux/macOS a stale socket node has to be removed before bind, and doing
// that blindly would let a second agent instance silently steal a running
// one's socket, leaving the first with a listener no client can reach
// (waired#81). Callers treat this like any other bind failure — the daemon
// logs it and keeps running with no write endpoint, which is safe because
// the management server's writeGuard fails open.
var ErrEndpointInUse = errors.New("endpoint is already served by another instance")

// Listen creates the local IPC listener for endpoint.
//
// On Linux/macOS endpoint is a unix-domain socket path: its parent
// runtime dir is created 0755, any stale socket node from an unclean
// prior exit is removed, and the new socket is chmod'd 0666 so a
// desktop-user client (a different uid than the service user on a system
// install) can connect.
//
// On Windows endpoint is a named-pipe name (e.g. \\.\pipe\waired-mgmt);
// the pipe is created with a security descriptor granting connect to
// SYSTEM, Administrators, and local interactive users only (no network
// logons, so it is unreachable over SMB).
func Listen(endpoint string) (net.Listener, error) {
	return listen(endpoint)
}
