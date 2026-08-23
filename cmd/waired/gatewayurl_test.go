package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

// writePinnedPort drops an agent.json that moves the gateway off its
// default, the way an operator would when 9473 is already taken.
func writePinnedPort(t *testing.T, port int) string {
	t.Helper()
	dir := t.TempDir()
	body := []byte(`{"inference":{"local_gateway_port":` + itoa(port) + `}}`)
	if err := os.WriteFile(filepath.Join(dir, "agent.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestLocalGatewayBaseURL_FollowsAgentJSON is the regression guard for
// waired-ai/waired-agent#999.
//
// The CLI used to answer this question from a compiled-in constant, so on a
// host that pinned inference.local_gateway_port the plugins were written
// pointing at the default port, Audit compared them against the same wrong
// constant, and the tray row, `waired doctor` and the audit all reported a
// healthy integration whose requests went to a port nothing was listening
// on. Reading agent.json is what makes the two halves able to disagree.
func TestLocalGatewayBaseURL_FollowsAgentJSON(t *testing.T) {
	dir := writePinnedPort(t, 19473)
	got, port := localGatewayBaseURL(dir)
	if want := "http://127.0.0.1:19473"; got != want {
		t.Errorf("localGatewayBaseURL = %q, want %q — a pinned port must reach the plugins", got, want)
	}
	if port != 19473 {
		t.Errorf("port = %d, want 19473", port)
	}
}

// TestLocalGatewayBaseURL_DefaultsWhenUnset keeps the guard above from
// passing by always echoing whatever it read: with no agent.json the
// compiled default still stands.
func TestLocalGatewayBaseURL_DefaultsWhenUnset(t *testing.T) {
	got, port := localGatewayBaseURL(t.TempDir())
	if got != defaultGatewayURL {
		t.Errorf("localGatewayBaseURL = %q, want the default %q", got, defaultGatewayURL)
	}
	if port != 9473 {
		t.Errorf("port = %d, want 9473", port)
	}
}

// TestResolveGatewayBaseURL_ExplicitFlagWins pins the precedence: someone
// who typed --gateway-base-url means it, even on a host whose agent.json
// says otherwise. Without this, pointing the CLI at a tunnel or a second
// daemon would silently be overridden by the local config.
func TestResolveGatewayBaseURL_ExplicitFlagWins(t *testing.T) {
	dir := writePinnedPort(t, 19473)

	cmd := &cobra.Command{Use: "x"}
	var flagVal string
	cmd.Flags().StringVar(&flagVal, "gateway-base-url", defaultGatewayURL, "")

	// Not given: follow the host's config.
	if got := resolveGatewayBaseURL(cmd, dir, flagVal); got != "http://127.0.0.1:19473" {
		t.Errorf("unset flag: got %q, want the configured port", got)
	}

	// Given: honour it verbatim.
	if err := cmd.Flags().Set("gateway-base-url", "http://127.0.0.1:1234"); err != nil {
		t.Fatal(err)
	}
	if got := resolveGatewayBaseURL(cmd, dir, flagVal); got != "http://127.0.0.1:1234" {
		t.Errorf("explicit flag: got %q, want it honoured verbatim", got)
	}
}
