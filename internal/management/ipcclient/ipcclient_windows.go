//go:build windows

package ipcclient

import (
	"context"
	"net"
	"os"

	"github.com/Microsoft/go-winio"

	"github.com/waired-ai/waired-agent/internal/platform/paths"
)

// resolveEndpoint returns the named-pipe name. The pipe namespace is
// machine-global — there is no per-user variant to probe for and
// $WAIRED_MGMT_SOCKET does not apply on Windows — so the Mode is
// immaterial and every client resolves the same name as the daemon.
//
// The one exception is a NON-DEFAULT $WAIRED_STATE_DIR: a dev/test instance
// gets its own pipe name derived from that dir (waired#81), and the daemon
// derives the same name from the same variable. Unlike the unix side there
// is nothing cheap to stat, so the derived name is returned outright — which
// is correct, because a client that sets $WAIRED_STATE_DIR to a non-default
// dir is by definition addressing that instance and not the service one.
func resolveEndpoint() string {
	if inst := paths.InstanceMgmtEndpoint(os.Getenv(paths.EnvOverride)); inst != "" {
		return inst
	}
	return paths.MgmtEndpoint(paths.System)
}

func dial(ctx context.Context, endpoint string) (net.Conn, error) {
	return winio.DialPipeContext(ctx, endpoint)
}
