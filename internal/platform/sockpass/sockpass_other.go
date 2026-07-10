//go:build !darwin

package sockpass

import (
	"errors"
	"net"
)

// errUnsupported guards the broker path on platforms that do not use fd
// passing for the proxy: Linux gets its listening socket from systemd
// (internal/platform/sdsocket) and Windows binds it directly as LocalSystem.
var errUnsupported = errors.New("sockpass: listening-fd passing is only implemented on darwin")

// Receive returns (nil, nil) off darwin so the agent's listener acquisition
// treats "no broker" as "proxy not active" rather than a hard error.
func Receive(string) (net.Listener, error) { return nil, nil }

// Send is unsupported off darwin — only the macOS root broker ever calls it.
func Send(*net.UnixConn, net.Listener) error { return errUnsupported }
