package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
)

// localGatewayBaseURL resolves the loopback base URL the local gateway
// serves, from agent.json over the defaults — the same shape claudeBaseURL
// uses for the Claude listener.
//
// Without this the CLI answered from a compiled-in constant, so a host that
// pinned inference.local_gateway_port got plugins pointing at 9473 anyway:
// written, audited as healthy, and unable to reach the gateway. The daemon
// derives its own expected_base_url from the same config, so both halves now
// agree and the drift check reports a real mismatch instead of agreeing with
// itself (waired-ai/waired-agent#999).
func localGatewayBaseURL(stateDir string) (string, int) {
	c := agentconfig.Defaults()
	_ = c.MergeJSON(agentconfig.JSONPathFor(stateDir))
	port := c.Inference.LocalGatewayPort
	return fmt.Sprintf("http://127.0.0.1:%d", port), port
}

// resolveGatewayBaseURL returns what the caller asked for when they passed
// --gateway-base-url, and this host's configured gateway otherwise.
//
// The flag's declared default is a constant because cobra fixes defaults at
// construction time, before --state-dir has been parsed. Consulting Changed
// is what keeps an explicit flag authoritative while an unset one follows
// the config.
func resolveGatewayBaseURL(cmd *cobra.Command, stateDir, current string) string {
	if cmd != nil && cmd.Flags().Changed("gateway-base-url") {
		return current
	}
	url, _ := localGatewayBaseURL(stateDir)
	return url
}
