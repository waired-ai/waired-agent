package agentconfig

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

func TestDefaultsRouting(t *testing.T) {
	cfg := Defaults()
	if cfg.Routing.Mode != state.RoutingModeAuto {
		t.Errorf("Routing.Mode default = %q, want %q", cfg.Routing.Mode, state.RoutingModeAuto)
	}
	if cfg.Routing.PinnedPeerDeviceID != "" {
		t.Errorf("Routing.PinnedPeerDeviceID default = %q, want empty", cfg.Routing.PinnedPeerDeviceID)
	}
}

func TestRoutingConfigAsPreference(t *testing.T) {
	cases := []struct {
		name string
		in   RoutingConfig
		want state.RoutingPreference
	}{
		{"empty-mode-becomes-auto", RoutingConfig{}, state.RoutingPreference{Mode: state.RoutingModeAuto}},
		{"auto", RoutingConfig{Mode: state.RoutingModeAuto}, state.RoutingPreference{Mode: state.RoutingModeAuto}},
		{"local-only", RoutingConfig{Mode: state.RoutingModeLocalOnly}, state.RoutingPreference{Mode: state.RoutingModeLocalOnly}},
		{"peer-preferred", RoutingConfig{Mode: state.RoutingModePeerPreferred}, state.RoutingPreference{Mode: state.RoutingModePeerPreferred}},
		{
			"pinned-with-peer",
			RoutingConfig{Mode: state.RoutingModePinned, PinnedPeerDeviceID: "dev_abc"},
			state.RoutingPreference{Mode: state.RoutingModePinned, PinnedPeerDeviceID: "dev_abc"},
		},
		{
			"stray-pin-on-auto-is-stripped",
			RoutingConfig{Mode: state.RoutingModeAuto, PinnedPeerDeviceID: "stray"},
			state.RoutingPreference{Mode: state.RoutingModeAuto},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.in.AsPreference()
			if got != c.want {
				t.Errorf("AsPreference = %#v, want %#v", got, c.want)
			}
		})
	}
}

func TestValidateRoutingRejectsPinnedWithoutPeer(t *testing.T) {
	cfg := Defaults()
	cfg.Routing = RoutingConfig{Mode: state.RoutingModePinned}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for pinned without peer")
	}
	if !strings.Contains(err.Error(), "requires pinned_peer_device_id") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateRoutingRejectsAutoWithPeer(t *testing.T) {
	cfg := Defaults()
	cfg.Routing = RoutingConfig{Mode: state.RoutingModeAuto, PinnedPeerDeviceID: "stray"}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for auto with stray pin")
	}
	if !strings.Contains(err.Error(), "must not carry pinned_peer_device_id") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateRoutingRejectsUnknownMode(t *testing.T) {
	cfg := Defaults()
	cfg.Routing = RoutingConfig{Mode: state.RoutingMode("bogus")}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for unknown mode")
	}
}
