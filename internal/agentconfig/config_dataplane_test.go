package agentconfig

import (
	"os"
	"path/filepath"
	"testing"
)

// The data-plane listener was named OpenCodeGatewayPort until
// waired-agent#333 renamed it. The port itself did NOT move — OpenClaw's
// plugin has always pointed at it, and its literal 9479 is compiled into
// plugins already written to users' home directories.
//
// PRODUCT CONTRACT: an operator who pinned a non-default port before the
// rename keeps that port. A rename that silently reverts them to 9479
// moves a listener out from under a working plugin, and nothing would
// report it — the agent would come up "fine" on the wrong port.
func TestDataPlaneGatewayPort_LegacyJSONKeyStillHonoured(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{
			name: "legacy key alone is honoured",
			body: `{"inference":{"opencode_gateway_port":9599}}`,
			want: 9599,
		},
		{
			name: "current key alone is honoured",
			body: `{"inference":{"data_plane_gateway_port":9588}}`,
			want: 9588,
		},
		{
			// The current spelling wins: an operator who wrote it meant
			// it, and letting the old key override would turn the rename
			// into a setting that silently does nothing.
			name: "both present — the current key wins",
			body: `{"inference":{"data_plane_gateway_port":9588,"opencode_gateway_port":9599}}`,
			want: 9588,
		},
		{
			name: "neither present — the default stands",
			body: `{"inference":{"local_gateway_port":9473}}`,
			want: 9479,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agent.json")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := Defaults()
			if err := cfg.MergeJSON(path); err != nil {
				t.Fatalf("MergeJSON: %v", err)
			}
			if got := cfg.Inference.DataPlaneGatewayPort; got != tc.want {
				t.Errorf("DataPlaneGatewayPort = %d, want %d", got, tc.want)
			}
			// The legacy field is a read-only inlet: once folded it must
			// not survive on the in-memory config, or a later Marshal
			// would write the retired key back out.
			if cfg.Inference.LegacyOpenCodeGatewayPort != 0 {
				t.Errorf("LegacyOpenCodeGatewayPort = %d, want 0 after folding",
					cfg.Inference.LegacyOpenCodeGatewayPort)
			}
		})
	}
}

// The env override follows the same rule as the JSON key: both spellings
// are accepted for one release so an operator's existing unit file or
// container env keeps working across the rename.
func TestDataPlaneGatewayPort_LegacyEnvKeyStillHonoured(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  []string
		want int
	}{
		{"legacy env", []string{"WAIRED_INFERENCE_OPENCODE_GATEWAY_PORT=9599"}, 9599},
		{"current env", []string{"WAIRED_INFERENCE_DATA_PLANE_GATEWAY_PORT=9588"}, 9588},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Defaults()
			if err := cfg.MergeEnv(tc.env); err != nil {
				t.Fatalf("MergeEnv: %v", err)
			}
			if got := cfg.Inference.DataPlaneGatewayPort; got != tc.want {
				t.Errorf("DataPlaneGatewayPort = %d, want %d", got, tc.want)
			}
		})
	}
}
