//go:build linux || darwin

package ipcclient

import (
	"context"
	"net"
	"os"

	"github.com/waired-ai/waired-agent/internal/platform/paths"
)

// resolveEndpoint picks the unix-domain socket path to dial.
//
// $WAIRED_MGMT_SOCKET wins (paths.MgmtEndpoint honours it), matching the
// daemon. Otherwise we must not key on our OWN euid: a system install is
// the default on every OS, and a non-root CLI or the desktop-user tray
// still talks to a System daemon whose socket lives in the System runtime
// dir. So probe the System socket first and fall back to the per-user one
// (a dev / interactive daemon).
func resolveEndpoint() string {
	if v := os.Getenv(paths.MgmtSocketEnvOverride); v != "" {
		return v
	}
	if sys := paths.MgmtEndpoint(paths.System); sys != "" {
		if _, err := os.Stat(sys); err == nil {
			return sys
		}
	}
	return paths.MgmtEndpoint(paths.Interactive)
}

func dial(ctx context.Context, endpoint string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", endpoint)
}
