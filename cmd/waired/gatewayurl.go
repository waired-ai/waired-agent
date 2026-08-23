package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/management"
)

// gatewayExpectationTimeout bounds the daemon read below. Short on purpose:
// this runs inside `waired link` and `waired doctor`, where the daemon is
// normally up and answering in milliseconds, and where a daemon that is down
// must cost a beat rather than a stall. A miss is not fatal — the config and
// then the compiled default stand behind it.
const gatewayExpectationTimeout = 2 * time.Second

// localGatewayBaseURL resolves the loopback base URL the local gateway
// serves, in three steps, each a fallback for the one before:
//
//  1. Ask the daemon. It is the only party that knows for certain, because
//     it is the one that bound the port.
//  2. Read agent.json under the state dir this CLI resolved.
//  3. The compiled default.
//
// Step 1 exists because step 2 is not enough on a system-service install,
// which is the shape most Linux and Windows hosts have. There the daemon's
// config lives in a root-owned 0700 tree the desktop user cannot read, and
// the CLI's own state dir has no agent.json at all — so a host that pinned
// inference.local_gateway_port got plugins pointing at the default port
// anyway. Measured on a Linux service install: with the port moved to
// 19473 the daemon listened on 19473 and `waired link` still wrote 9473,
// while `GET /waired/v1/integration/openclaw` over the management socket
// answered 19473 correctly (waired-ai/waired-agent#999).
//
// Reading the daemon's answer rather than its config file is also what
// docs/decisions/20260822/1742 settled: the daemon answers the expectation,
// and the surfaces running as the desktop user do the observing and the
// repairing.
func localGatewayBaseURL(ctx context.Context, mgmt, stateDir string) (string, int) {
	if url, ok := gatewayBaseURLFromDaemon(ctx, mgmt); ok {
		if port, ok := portOf(url); ok {
			return url, port
		}
	}
	c := agentconfig.Defaults()
	_ = c.MergeJSON(agentconfig.JSONPathFor(stateDir))
	port := c.Inference.LocalGatewayPort
	return fmt.Sprintf("http://127.0.0.1:%d", port), port
}

// gatewayBaseURLFromDaemon asks the daemon which port its own gateway is
// listening on, via the integration expectation it already publishes for
// the tray. The answer carries the "/v1" suffix the plugins need, which is
// trimmed here so callers hold a base URL.
//
// The read goes through mgmtReadRoute because the route is socket-only:
// over plain TCP the daemon answers 403 (it serves only the compatibility
// reads there while the socket is up), the same trap #785 hit.
func gatewayBaseURLFromDaemon(ctx context.Context, mgmt string) (string, bool) {
	target, cl, err := mgmtReadRoute(strings.TrimRight(mgmt, "/")+"/waired/v1/integration/openclaw", gatewayExpectationTimeout)
	if err != nil {
		return "", false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", false
	}
	resp, err := cl.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	var e management.IntegrationExpectation
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		return "", false
	}
	base := strings.TrimSuffix(strings.TrimRight(e.ExpectedBaseURL, "/"), "/v1")
	if base == "" {
		return "", false
	}
	return base, true
}

// portOf extracts the port from a base URL so callers that want the number
// (the doctor's probe line) do not re-parse it.
func portOf(baseURL string) (int, bool) {
	i := strings.LastIndex(baseURL, ":")
	if i < 0 {
		return 0, false
	}
	var port int
	if _, err := fmt.Sscanf(baseURL[i+1:], "%d", &port); err != nil || port <= 0 {
		return 0, false
	}
	return port, true
}

// resolveGatewayBaseURL returns what the caller asked for when they passed
// --gateway-base-url, and this host's real gateway otherwise.
//
// The flag's declared default is a constant because cobra fixes defaults at
// construction time, before --state-dir has been parsed. Consulting Changed
// is what keeps an explicit flag authoritative while an unset one follows
// the daemon.
func resolveGatewayBaseURL(cmd *cobra.Command, mgmt, stateDir, current string) string {
	if cmd != nil && cmd.Flags().Changed("gateway-base-url") {
		return current
	}
	ctx := context.Background()
	if cmd != nil && cmd.Context() != nil {
		ctx = cmd.Context()
	}
	url, _ := localGatewayBaseURL(ctx, mgmt, stateDir)
	return url
}
