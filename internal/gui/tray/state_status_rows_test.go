package tray

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// The top-level status block: three display-only rows that answer "can this
// computer answer", "can the other computers" and "is Claude Code pointed
// here" without opening a submenu (waired-agent#1032).

func statusSnapshot() Snapshot {
	return Snapshot{
		Health:   HealthOnline,
		Identity: &management.IdentityView{Enrolled: true, AccountEmail: "u@example.com", DeviceID: "dev-1"},
		Status:   &management.Status{Phase: "active"},
	}
}

func TestEngineStatusRow(t *testing.T) {
	catalog := &management.ModelCatalogResponse{
		Active: &management.CatalogActive{ModelID: "qwen3-8b-instruct", DisplayName: "Qwen3 8B Instruct"},
	}
	tests := []struct {
		name string
		inf  *management.InferenceStatus
		cat  *management.ModelCatalogResponse
		want string
	}{
		{
			name: "no daemon inference API says nothing",
			inf:  nil,
			want: "",
		},
		{
			// A daemon that declares no subsystem state has not made a
			// claim; inventing "ready" or "off" from its silence is the
			// class of thing waired-agent#1027 took out of the init box.
			name: "an unstated subsystem state says nothing",
			inf:  &management.InferenceStatus{},
			want: "",
		},
		{
			name: "ready names the model",
			inf:  &management.InferenceStatus{SubsystemState: signer.SubsystemStateReady},
			cat:  catalog,
			want: "● Engine: ready — Qwen3 8B Instruct",
		},
		{
			// waired-agent#879 at the top level. The glyph stays ● — the
			// engine really is ready, it is the weights that are cold —
			// and the suffix is the 17-56 s the next request will pay.
			name: "an idle-expired model is visible without opening a submenu",
			inf: &management.InferenceStatus{
				SubsystemState: signer.SubsystemStateReady,
				Active:         &management.ActiveSelection{Runtime: "ollama", ModelID: "qwen3-8b-instruct"},
				Runtimes:       map[string]management.RuntimeStatus{"ollama": {ModelResident: boolPtr(false)}},
			},
			cat:  catalog,
			want: "● Engine: ready — Qwen3 8B Instruct (not loaded)",
		},
		{
			name: "a resident model carries no suffix",
			inf: &management.InferenceStatus{
				SubsystemState: signer.SubsystemStateReady,
				Active:         &management.ActiveSelection{Runtime: "ollama", ModelID: "qwen3-8b-instruct"},
				Runtimes:       map[string]management.RuntimeStatus{"ollama": {ModelResident: boolPtr(true)}},
			},
			cat:  catalog,
			want: "● Engine: ready — Qwen3 8B Instruct",
		},
		{
			// An engine with no residency axis at all (vLLM holds the
			// model for the life of the process) reports nil, and a nil
			// must not be rendered as "not loaded" — that would state
			// something the daemon did not say.
			name: "an unobserved residency is not rendered",
			inf: &management.InferenceStatus{
				SubsystemState: signer.SubsystemStateReady,
				Active:         &management.ActiveSelection{Runtime: "vllm", ModelID: "gpt-oss-20b"},
				Runtimes:       map[string]management.RuntimeStatus{"vllm": {}},
			},
			cat:  catalog,
			want: "● Engine: ready — Qwen3 8B Instruct",
		},
		{
			// Reading runtimes["ollama"] instead of the runtime this host
			// serves with is the hardcoding waired-agent#1026 removed one
			// block higher; it left the suffix dead on a vLLM host.
			name: "residency follows the serving runtime, not ollama",
			inf: &management.InferenceStatus{
				SubsystemState: signer.SubsystemStateReady,
				Active:         &management.ActiveSelection{Runtime: "vllm", ModelID: "gpt-oss-20b"},
				Runtimes: map[string]management.RuntimeStatus{
					"vllm":   {ModelResident: boolPtr(false)},
					"ollama": {ModelResident: boolPtr(true)},
				},
			},
			cat:  catalog,
			want: "● Engine: ready — Qwen3 8B Instruct (not loaded)",
		},
		{
			// The one word the top level cannot borrow from the Inference
			// submenu: with no heading above it to say whose engine this
			// is, "disabled" has to say where.
			name: "local inference off says where it is off",
			inf:  &management.InferenceStatus{SubsystemState: signer.SubsystemStateDisabled},
			cat:  catalog,
			want: "○ Engine: off on this computer",
		},
		{
			// The model is a preference on a host with nothing to run it.
			name: "an engine-less host names no model",
			inf:  &management.InferenceStatus{SubsystemState: signer.SubsystemStateNoEngine},
			cat:  catalog,
			want: "○ Engine: none on this computer",
		},
		{
			name: "a stopped engine is idle, not a fault",
			inf:  &management.InferenceStatus{SubsystemState: signer.SubsystemStateStopped},
			cat:  catalog,
			want: "○ Engine: stopped (memory freed)",
		},
		{
			name: "a download in flight is in progress",
			inf:  &management.InferenceStatus{SubsystemState: signer.SubsystemStateLoading},
			cat:  catalog,
			want: "◐ Engine: downloading — Qwen3 8B Instruct",
		},
		{
			name: "a crashed engine is a fault and names no model",
			inf:  &management.InferenceStatus{SubsystemState: signer.SubsystemStateEngineFailed},
			cat:  catalog,
			want: "⚠ Engine: engine failed",
		},
		{
			name: "a failed pull is a fault and names what failed",
			inf:  &management.InferenceStatus{SubsystemState: signer.SubsystemStatePullFailed},
			cat:  catalog,
			want: "⚠ Engine: pull failed — Qwen3 8B Instruct",
		},
		{
			// No manifest row resolved: the raw model_id beats saying
			// nothing.
			name: "the raw model id is the fallback for a name",
			inf: &management.InferenceStatus{
				SubsystemState: signer.SubsystemStateReady,
				Active:         &management.ActiveSelection{Runtime: "ollama", ModelID: "qwen3-8b-instruct"},
			},
			want: "● Engine: ready — qwen3-8b-instruct",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := engineStatusRow(tc.inf, tc.cat); got != tc.want {
				t.Errorf("engineStatusRow = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPeersStatusRow(t *testing.T) {
	serving := func(name string) inferencemesh.PeerView {
		return inferencemesh.PeerView{
			DeviceID:   "dev_" + name,
			DeviceName: name,
			InferenceState: &signer.InferenceState{
				Reachable: true, Models: []string{"qwen3.6:35b"}, ActiveModel: "qwen3.6-35b-a3b",
			},
		}
	}
	idle := func(name string) inferencemesh.PeerView {
		return inferencemesh.PeerView{DeviceID: "dev_" + name, DeviceName: name}
	}
	tests := []struct {
		name string
		mesh *inferencemesh.Snapshot
		want string
	}{
		{name: "no mesh endpoint says nothing", mesh: nil, want: ""},
		{
			// "0 of 0 serving" is a fact about nothing.
			name: "a host with no peers says nothing",
			mesh: &inferencemesh.Snapshot{},
			want: "",
		},
		{
			name: "some serving",
			mesh: &inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{serving("a"), serving("b"), idle("c"), idle("d")}},
			want: "● Peers: 2 of 4 serving",
		},
		{
			name: "none serving is idle, not a fault",
			mesh: &inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{idle("a"), idle("b")}},
			want: "○ Peers: none of 2 serving",
		},
		{
			// The count must not disagree with what the router can find:
			// a peer with no advertised tag is not routable, whatever it
			// says about itself (waired#1064).
			name: "a peer with no advertised model is not counted",
			mesh: &inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{{
				DeviceID:       "dev_a",
				DeviceName:     "a",
				InferenceState: &signer.InferenceState{Reachable: true, SubsystemState: signer.SubsystemStateReady},
			}}},
			want: "○ Peers: none of 1 serving",
		},
		{
			name: "a stale peer is not counted",
			mesh: &inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{{
				DeviceID:       "dev_a",
				DeviceName:     "a",
				Stale:          true,
				InferenceState: &signer.InferenceState{Reachable: true, Models: []string{"qwen3.6:35b"}},
			}}},
			want: "○ Peers: none of 1 serving",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := peersStatusRow(tc.mesh); got != tc.want {
				t.Errorf("peersStatusRow = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClaudeStatusRow(t *testing.T) {
	tests := []struct {
		name string
		in   *management.ClaudeIntegrationStatus
		want string
	}{
		{name: "no endpoint says nothing", in: nil, want: ""},
		{
			// The row waired-agent#1032 is about. The daemon reporting
			// that nothing local is serving must not change this answer:
			// routing is about where Claude Code sends its requests, and
			// that is settled by the managed-settings file alone.
			name: "routed stays routed while nothing local is serving",
			in: &management.ClaudeIntegrationStatus{
				Wrapper: management.ClaudeWrapperView{Reachable: false, Reason: "inference-unavailable"},
				ManagedSettings: management.ClaudeManagedSettingsView{
					Supported: true, Present: true, Configured: true,
					BaseURL: "http://127.0.0.1:9472", ExpectedBaseURL: "http://127.0.0.1:9472",
				},
			},
			want: "● Claude Code: routed through Waired",
		},
		{
			name: "never enabled is idle, not a fault",
			in: &management.ClaudeIntegrationStatus{
				ManagedSettings: management.ClaudeManagedSettingsView{Supported: true},
			},
			want: "○ Claude Code: not routed through Waired",
		},
		{
			// Someone else's proxy owns the variable: acting on that
			// needs a decision, so it is a fault and it names the value.
			name: "pointed somewhere else is a fault and says where",
			in: &management.ClaudeIntegrationStatus{
				ManagedSettings: management.ClaudeManagedSettingsView{
					Supported: true, Present: true, BaseURL: "http://127.0.0.1:1234",
					ExpectedBaseURL: "http://127.0.0.1:9472",
				},
			},
			want: "⚠ Claude Code: routed elsewhere (http://127.0.0.1:1234)",
		},
		{
			name: "an OS with no managed-settings path is idle",
			in:   &management.ClaudeIntegrationStatus{},
			want: "○ Claude Code: not available on this computer",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := claudeStatusRow(tc.in); got != tc.want {
				t.Errorf("claudeStatusRow = %q, want %q", got, tc.want)
			}
		})
	}
}

// End-to-end: the host waired-agent#1032 was reported on. No local engine, a
// peer answering, Claude Code routed. Every row must say so, and the header
// must not be a warning.
func TestUpdate_StatusRows_EnginelessHostServedByAPeer(t *testing.T) {
	snap := statusSnapshot()
	snap.Inference = &management.InferenceStatus{SubsystemState: signer.SubsystemStateNoEngine}
	snap.Mesh = &inferencemesh.Snapshot{
		Reachable: true,
		Peers: []inferencemesh.PeerView{{
			DeviceID:   "dev_b",
			DeviceName: "sv-evox2",
			InferenceState: &signer.InferenceState{
				Reachable: true, Models: []string{"qwen3.6:35b"}, ActiveModel: "qwen3.6-35b-a3b",
			},
		}},
	}
	snap.Claude = &management.ClaudeIntegrationStatus{
		Wrapper: management.ClaudeWrapperView{Reachable: true},
		ManagedSettings: management.ClaudeManagedSettingsView{
			Supported: true, Present: true, Configured: true,
			BaseURL: "http://127.0.0.1:9472", ExpectedBaseURL: "http://127.0.0.1:9472",
		},
	}
	got := Update(snap)

	if got.StatusEngineLabel != "○ Engine: none on this computer" {
		t.Errorf("StatusEngineLabel = %q", got.StatusEngineLabel)
	}
	if got.StatusPeersLabel != "● Peers: 1 of 1 serving" {
		t.Errorf("StatusPeersLabel = %q", got.StatusPeersLabel)
	}
	if got.StatusClaudeLabel != "● Claude Code: routed through Waired" {
		t.Errorf("StatusClaudeLabel = %q", got.StatusClaudeLabel)
	}
	if got.Icon != IconConnected {
		t.Errorf("Icon = %d, want IconConnected — an engine-less host being served is not degraded", got.Icon)
	}
	if got.HeaderTitle != "● Connected" {
		t.Errorf("HeaderTitle = %q, want the plain connected header", got.HeaderTitle)
	}
	if want := "Waired: ● Connected"; trayTooltip(got) == want {
		t.Errorf("trayTooltip = %q — the glyph belongs to the icon, not the tooltip", trayTooltip(got))
	}
	if got := trayTooltip(got); got != "Waired: Connected" {
		t.Errorf("trayTooltip = %q, want %q", got, "Waired: Connected")
	}
}

func TestTrayTooltip(t *testing.T) {
	tests := []struct{ header, want string }{
		// The glyph belongs to the icon, which carries that distinction
		// visually and survives a screen reader; the tooltip does not.
		{"● Connected", "Waired: Connected"},
		{"⚠ No engine is answering", "Waired: No engine is answering"},
		{"◐ Connecting…", "Waired: Connecting…"},
		{"○ Not signed in", "Waired: Not signed in"},
		{"⚠ Background service is not running", "Waired: Background service is not running"},
		{"◐ Background service is starting…", "Waired: Background service is starting…"},
		{"", "Waired"},
	}
	for _, tc := range tests {
		if got := trayTooltip(MenuModel{HeaderTitle: tc.header}); got != tc.want {
			t.Errorf("trayTooltip(%q) = %q, want %q", tc.header, got, tc.want)
		}
	}
}

// Mid-transition the question has no stable answer, so the block says
// nothing rather than flickering — the same gating the Inference group uses.
func TestUpdate_StatusRows_HiddenWhileConnecting(t *testing.T) {
	snap := statusSnapshot()
	snap.Status = &management.Status{Phase: "starting"}
	snap.Inference = &management.InferenceStatus{SubsystemState: signer.SubsystemStateReady}
	got := Update(snap)
	if got.StatusEngineLabel != "" || got.StatusPeersLabel != "" || got.StatusClaudeLabel != "" {
		t.Errorf("status block should be empty mid-transition: %q / %q / %q",
			got.StatusEngineLabel, got.StatusPeersLabel, got.StatusClaudeLabel)
	}
}
