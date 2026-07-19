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
// daemon. Then, if $WAIRED_STATE_DIR names a NON-DEFAULT state dir, the
// instance socket derived from it (waired#81) — the daemon derives the same
// path from the same variable, so a dev instance that exports it to both
// sides is self-consistent and never collides with another instance.
//
// That step is a probe, not a commitment: it is taken only when a socket is
// actually bound there, so a client that sets $WAIRED_STATE_DIR while talking
// to the ordinary service daemon (or to one predating #81) still reaches the
// System socket below. When nothing is bound anywhere, the final fallback
// resolves back to the instance path — which is the right thing to NAME in
// the "is waired-agent running?" error, since it is the dir the operator chose.
//
// Otherwise we must not key on our OWN euid: a system install is the default
// on every OS, and a non-root CLI or the desktop-user tray still talks to a
// System daemon whose socket lives in the System runtime dir. So probe the
// System socket first and fall back to the per-user one (a dev /
// interactive daemon).
func resolveEndpoint() string {
	if v := os.Getenv(paths.MgmtSocketEnvOverride); v != "" {
		return v
	}
	if inst := paths.InstanceMgmtEndpoint(os.Getenv(paths.EnvOverride)); inst != "" {
		if _, err := os.Stat(inst); err == nil {
			return inst
		}
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
