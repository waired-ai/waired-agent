//go:build windows

package ipcclient

import (
	"context"
	"net"

	"github.com/Microsoft/go-winio"

	"github.com/waired-ai/waired-agent/internal/platform/paths"
)

// resolveEndpoint returns the named-pipe name. The pipe namespace is
// machine-global — there is no per-user variant to probe for and
// $WAIRED_MGMT_SOCKET does not apply on Windows — so the Mode is
// immaterial and every client resolves the same name as the daemon.
func resolveEndpoint() string {
	return paths.MgmtEndpoint(paths.System)
}

func dial(ctx context.Context, endpoint string) (net.Conn, error) {
	return winio.DialPipeContext(ctx, endpoint)
}
