package inferencemesh

import (
	"testing"

	"github.com/waired-ai/waired-agent/proto/signer"
)

func peer(mut func(*PeerView)) PeerView {
	p := PeerView{
		DeviceID:   "dev_peer",
		DeviceName: "linux-gpu",
		InferenceState: &signer.InferenceState{
			Reachable:      true,
			Type:           signer.InferenceTypeOllama,
			Models:         []string{"qwen3:8b-q4_K_M"},
			ActiveModel:    "qwen3-8b-instruct",
			SubsystemState: signer.SubsystemStateReady,
		},
	}
	if mut != nil {
		mut(&p)
	}
	return p
}

// PRODUCT CONTRACT (public share spec §8.5, via waired-agent#888): a
// prose surface may name one of your own machines, and may name a public
// machine only by its grant pseudonym. The control plane substitutes the
// pseudonym into DeviceName at injection time, so the grant-peer rows
// here are the second lock — they fail only if this function starts
// trusting that substitution.
func TestPeerDisplayName(t *testing.T) {
	grant := func(pseudonym string) func(*PeerView) {
		return func(p *PeerView) {
			p.DeviceName = "stranger-workstation"
			p.Grant = &signer.PeerGrant{ID: "grant_x", Kind: "public", Role: "provider", Pseudonym: pseudonym}
		}
	}
	tests := []struct {
		name   string
		p      PeerView
		want   string
		wantOK bool
	}{
		{"your own machine is named", peer(nil), "linux-gpu", true},
		{"an own peer that reported no name falls back to its id",
			peer(func(p *PeerView) { p.DeviceName = "" }), "dev_peer", true},
		{"a public machine is its pseudonym, never its name",
			peer(grant("guest-a7f3")), "guest-a7f3", true},
		{"a public machine with no pseudonym has no name to show",
			peer(grant("")), "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := PeerDisplayName(tt.p)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("PeerDisplayName = (%q, %v), want (%q, %v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

// PRODUCT CONTRACT (waired#1064): one model has to read as one model no
// matter which engine — and therefore which OS — the peer runs. The same
// catalog entry is an ollama tag on macOS/Windows and an HF repo id under
// vLLM, which is Linux-only, so preferring the engine tag would spell one
// model three ways in a picker listing a mixed fleet.
func TestPeerModel_PrefersTheCatalogIDOverTheEngineTag(t *testing.T) {
	ollamaHost := peer(func(p *PeerView) {
		p.InferenceState.Models = []string{"hf.co/unsloth/Qwen3-Coder-Next-GGUF:Q4_K_M"}
		p.InferenceState.ActiveModel = "qwen3-coder-next-80b-a3b-instruct"
	})
	vllmHost := peer(func(p *PeerView) {
		p.InferenceState.Type = signer.InferenceTypeVLLM
		p.InferenceState.Models = []string{"Qwen/Qwen3-Next-80B-A3B-Instruct"}
		p.InferenceState.ActiveModel = "qwen3-coder-next-80b-a3b-instruct"
	})
	if a, b := PeerModel(ollamaHost), PeerModel(vllmHost); a != b {
		t.Errorf("the same model read as %q and %q depending on the engine", a, b)
	}
}

func TestPeerModel(t *testing.T) {
	tests := []struct {
		name string
		p    PeerView
		want string
	}{
		{"catalog id wins", peer(nil), "qwen3-8b-instruct"},
		// An agent predating the field sends no active_model, and the row
		// must keep saying what it said before rather than going blank.
		{"older agent falls back to the engine tag", peer(func(p *PeerView) {
			p.InferenceState.ActiveModel = ""
		}), "qwen3:8b-q4_K_M"},
		{"no engine state at all", peer(func(p *PeerView) {
			p.InferenceState = nil
		}), ""},
		{"names nothing", peer(func(p *PeerView) {
			p.InferenceState.ActiveModel = ""
			p.InferenceState.Models = nil
		}), ""},
		// The model survives the advertisement being withdrawn — that is
		// the case the field exists for.
		{"mid-pull, tag withdrawn", peer(func(p *PeerView) {
			p.InferenceState.Models = nil
			p.InferenceState.SubsystemState = signer.SubsystemStateLoading
		}), "qwen3-8b-instruct"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := PeerModel(tc.p); got != tc.want {
				t.Errorf("PeerModel() = %q, want %q", got, tc.want)
			}
		})
	}
}

// PeerServing stays keyed on the engine tags because those are what a
// request is matched against. A record of today's behaviour, unchanged by
// waired#1064 — the new fields explain a peer, they do not make one
// routable.
func TestPeerServing(t *testing.T) {
	tests := []struct {
		name string
		p    PeerView
		want bool
	}{
		{"serving", peer(nil), true},
		{"stale", peer(func(p *PeerView) { p.Stale = true }), false},
		{"unreachable", peer(func(p *PeerView) { p.InferenceState.Reachable = false }), false},
		{"no engine state", peer(func(p *PeerView) { p.InferenceState = nil }), false},
		{"no advertised tag", peer(func(p *PeerView) { p.InferenceState.Models = nil }), false},
		// waired#729: a disco-silent peer is still selectable.
		{"silent", peer(func(p *PeerView) { p.Silent = true }), true},
		// A reported state that disagrees with a live advertisement does
		// not make the peer unroutable — the tag is the routing fact.
		{"serving despite a loading state", peer(func(p *PeerView) {
			p.InferenceState.SubsystemState = signer.SubsystemStateLoading
		}), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := PeerServing(tc.p); got != tc.want {
				t.Errorf("PeerServing() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPeerCondition(t *testing.T) {
	tests := []struct {
		name string
		p    PeerView
		want string
	}{
		{"serving", peer(nil), signer.SubsystemStateReady},

		// The peer's own reason wins over the viewer's coarser reading:
		// each of these is also "unreachable" from here, and each is
		// something an operator can act on.
		{"downloading its model", peer(func(p *PeerView) {
			p.InferenceState.Models = nil
			p.InferenceState.SubsystemState = signer.SubsystemStateLoading
		}), signer.SubsystemStateLoading},
		{"download failed", peer(func(p *PeerView) {
			p.InferenceState.Models = nil
			p.InferenceState.SubsystemState = signer.SubsystemStatePullFailed
		}), signer.SubsystemStatePullFailed},
		{"engine down", peer(func(p *PeerView) {
			p.InferenceState.Reachable = false
			p.InferenceState.Models = nil
			p.InferenceState.SubsystemState = signer.SubsystemStateEngineFailed
		}), signer.SubsystemStateEngineFailed},
		{"operator stopped the engine", peer(func(p *PeerView) {
			p.InferenceState.Reachable = false
			p.InferenceState.Models = nil
			p.InferenceState.SubsystemState = signer.SubsystemStateStopped
		}), signer.SubsystemStateStopped},

		// The viewer's own facts win over anything the peer said: a claim
		// we have not heard repeated is not evidence about now.
		{"stale outranks a reported ready", peer(func(p *PeerView) {
			p.Stale = true
		}), ConditionStale},
		{"stale outranks a reported failure", peer(func(p *PeerView) {
			p.Stale = true
			p.InferenceState.SubsystemState = signer.SubsystemStatePullFailed
		}), ConditionStale},

		// No reason offered — an agent that predates the field.
		{"older agent, engine down", peer(func(p *PeerView) {
			p.InferenceState.Reachable = false
			p.InferenceState.Models = nil
			p.InferenceState.SubsystemState = ""
		}), ConditionUnreachable},
		{"older agent, reachable but no tag", peer(func(p *PeerView) {
			p.InferenceState.Models = nil
			p.InferenceState.SubsystemState = ""
		}), ConditionUnavailable},
		// A "ready" contradicted by the missing tag is no reason at all.
		{"claims ready but advertises nothing", peer(func(p *PeerView) {
			p.InferenceState.Models = nil
		}), ConditionUnavailable},

		{"never reported an engine", peer(func(p *PeerView) {
			p.InferenceState = nil
		}), signer.SubsystemStateNoEngine},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := PeerCondition(tc.p); got != tc.want {
				t.Errorf("PeerCondition() = %q, want %q", got, tc.want)
			}
		})
	}
}

// PRODUCT CONTRACT (waired#1064): the three conditions that carry no
// reason keep reading as the published word. docs-site defines
// "(unavailable)" for the pin row and waired-agent#729's contract is
// written in terms of it, so waired#1064 adds specificity strictly where
// the peer volunteered a reason.
func TestConditionLabel_NoReasonStaysThePublishedWord(t *testing.T) {
	for _, c := range []string{ConditionStale, ConditionUnreachable, ConditionUnavailable} {
		if got := ConditionLabel(c); got != "unavailable" {
			t.Errorf("ConditionLabel(%q) = %q, want %q", c, got, "unavailable")
		}
	}
}

func TestConditionLabel(t *testing.T) {
	cases := map[string]string{
		signer.SubsystemStateReady:         "ready",
		signer.SubsystemStateLoading:       "loading",
		signer.SubsystemStatePullFailed:    "pull failed",
		signer.SubsystemStateEngineFailed:  "engine failed",
		signer.SubsystemStateNoEngine:      "no engine",
		signer.SubsystemStateAwaitingModel: "awaiting model",
		signer.SubsystemStateStopped:       "stopped",
		signer.SubsystemStateStarting:      "starting",
		signer.SubsystemStateDisabled:      "disabled",
		signer.SubsystemStateDegraded:      "degraded",
		// Unmapped values pass through rather than being dropped: the
		// control plane validates the set, so an unknown one means this
		// table is behind the wire — and hiding it would hide the reason
		// a node is not serving.
		"brand_new": "brand_new",
	}
	for in, want := range cases {
		if got := ConditionLabel(in); got != want {
			t.Errorf("ConditionLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// A stale peer's last-known model is a claim about the past. Rendering it
// beside a live-looking row would state it as the present.
func TestConditionHasFreshModel(t *testing.T) {
	if ConditionHasFreshModel(ConditionStale) {
		t.Error("a stale peer's model must not be shown as current")
	}
	for _, c := range []string{
		signer.SubsystemStateReady,
		signer.SubsystemStateLoading,
		signer.SubsystemStatePullFailed,
		ConditionUnreachable,
		ConditionUnavailable,
	} {
		if !ConditionHasFreshModel(c) {
			t.Errorf("ConditionHasFreshModel(%q) = false; the report is current", c)
		}
	}
}

// PRODUCT CONTRACT (public share spec §8.5, as quoted in
// internal/gateway/probe.go's peerDisplayID doc: "Real foreign device
// identifiers must never reach a header, an event, a log line or a CLI
// surface"). A public machine is named by its owner account's grant
// pseudonym, never by its real device id.
//
// The synthetic constants are deliberately unmistakable so a leak
// anywhere shows up as a substring hit; this repository is public and no
// real device identifier may appear in a fixture.
func TestPeerDisplayID(t *testing.T) {
	const (
		foreignDeviceID = "dev_foreign00000001"
		foreignAlias    = "guest-a7f3"
	)
	cases := []struct {
		name  string
		peer  PeerView
		want  string
		wantK bool
	}{
		{
			name:  "own-network peer keeps its device id",
			peer:  peer(nil),
			want:  "dev_peer",
			wantK: true,
		},
		{
			name: "public machine is named by its pseudonym",
			peer: peer(func(p *PeerView) {
				p.DeviceID = foreignDeviceID
				p.Grant = &signer.PeerGrant{Kind: "public", Role: "provider", Pseudonym: foreignAlias}
			}),
			want:  foreignAlias,
			wantK: true,
		},
		{
			// Falling back to the DeviceID here is the leak itself, so
			// there is nothing to show. The control plane skips injecting
			// such a peer, so this is the second lock on that door.
			name: "public machine with no pseudonym has no identifier to show",
			peer: peer(func(p *PeerView) {
				p.DeviceID = foreignDeviceID
				p.Grant = &signer.PeerGrant{Kind: "public", Role: "provider"}
			}),
			want:  "",
			wantK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := PeerDisplayID(tc.peer)
			if got != tc.want || ok != tc.wantK {
				t.Errorf("PeerDisplayID() = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.wantK)
			}
			if got == foreignDeviceID {
				t.Errorf("the public machine's real device id reached a display surface: %q", got)
			}
		})
	}
}
