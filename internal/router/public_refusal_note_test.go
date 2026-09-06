package router

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// A refused "Waired public share" turn says WHICH switch declined it
// (waired-agent#1201).
//
// PIN: product contract. Before this, publicGateFor collapsed four causes
// into one zero value, and the refusal reported the operator's own posture
// as "no public machine is reachable right now" — a statement that is not
// merely vague but false, since the grant acquirer releases every held
// grant while the posture is off.

func TestPublicGateFor_NamesTheSwitchThatRefused(t *testing.T) {
	consented := func(m PublicMode, main, sub bool) PublicPolicy {
		return PublicPolicy{Mode: m, Consented: true, Main: main, Sub: sub}
	}
	for _, tc := range []struct {
		name       string
		policy     PublicPolicy
		unwired    bool
		class      string
		wantAdmit  bool
		wantDenial publicDenial
	}{
		{
			name: "nothing wired the policy in", unwired: true,
			wantAdmit: false, wantDenial: publicDenialUnrecorded,
		},
		{
			// Consent is tested BEFORE mode. EffectiveMode already folds
			// "never consented" into "off" before the policy reaches the
			// router, so a mode-first order would leave this unnameable.
			name:      "never consented, even with a mode set",
			policy:    PublicPolicy{Mode: PublicModeAuto, Main: true, Sub: true},
			class:     state.ClaudeClassMain,
			wantAdmit: false, wantDenial: publicDenialNotConsented,
		},
		{
			name:   "consented and then switched off",
			policy: consented(PublicModeOff, true, true), class: state.ClaudeClassMain,
			wantAdmit: false, wantDenial: publicDenialModeOff,
		},
		{
			name:   "main-agent turns are switched off",
			policy: consented(PublicModeExplicit, false, true), class: state.ClaudeClassMain,
			wantAdmit: false, wantDenial: publicDenialMainOff,
		},
		{
			name:   "sub-agent turns are switched off",
			policy: consented(PublicModeExplicit, true, false), class: state.ClaudeClassSub,
			wantAdmit: false, wantDenial: publicDenialSubOff,
		},
		{
			// An empty class is admitted on EITHER toggle, so reaching a
			// denial means both are off.
			name:   "both toggles off, general inference",
			policy: consented(PublicModeExplicit, false, false), class: "",
			wantAdmit: false, wantDenial: publicDenialBothOff,
		},
		{
			name:   "admitted",
			policy: allowAll(), class: state.ClaudeClassMain,
			wantAdmit: true, wantDenial: publicDenialNone,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := publicSelector(t, tc.policy)
			if tc.unwired {
				s.in.PublicPolicyFn = nil
			}
			got := s.publicGateFor(tc.class)
			if got.admit != tc.wantAdmit {
				t.Errorf("admit = %v, want %v", got.admit, tc.wantAdmit)
			}
			if got.denial != tc.wantDenial {
				t.Errorf("denial = %d, want %d", got.denial, tc.wantDenial)
			}
		})
	}
}

// Every reason has its own sentence, and the settings arms come first.
//
// PIN: product contract for the arms that name a switch (waired-agent#1201).
// The three world-state arms at the bottom are a record of today's wording;
// two of them had no test at all before this.
func TestPublicShareDeclineReason_EveryReasonHasItsOwnSentence(t *testing.T) {
	// A map that carries a provider grant, so the "nobody is lending" arm
	// is false and the arms below it are reachable.
	snapshotWithPublicProvider := func() inferencemesh.Snapshot {
		return inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{
			mkPublicPeer(publicPeerDeviceID, publicPeerAlias, "qwen3:8b-q4_K_M"),
		}}
	}
	lending := publicShortfall{
		hit:  true,
		snap: snapshotWithPublicProvider(),
		gate: publicGate{admit: true, denial: publicDenialNone},
	}
	withGate := func(g publicGate) publicShortfall {
		s := lending
		s.gate = g
		return s
	}
	for _, tc := range []struct {
		name  string
		short publicShortfall
		want  string
	}{
		{
			name:  "nothing was recorded",
			short: publicShortfall{},
			want:  "",
		},
		{
			name:  "the policy was never wired in",
			short: withGate(publicGate{denial: publicDenialUnrecorded}),
			want:  "",
		},
		{
			name:  "never consented",
			short: withGate(publicGate{denial: publicDenialNotConsented}),
			want:  "security and privacy warning has not been accepted",
		},
		{
			name:  "switched off",
			short: withGate(publicGate{denial: publicDenialModeOff}),
			want:  "set not to use other people's public machines",
		},
		{
			name:  "main-agent turns off",
			short: withGate(publicGate{denial: publicDenialMainOff}),
			want:  "turned off for main-agent turns",
		},
		{
			name:  "sub-agent turns off",
			short: withGate(publicGate{denial: publicDenialSubOff}),
			want:  "turned off for sub-agent turns",
		},
		{
			name:  "both classes off",
			short: withGate(publicGate{denial: publicDenialBothOff}),
			want:  "both main-agent and sub-agent turns",
		},
		{
			name: "everything lent is below the Public Share floor",
			short: func() publicShortfall {
				s := withGate(publicGate{admit: true, denial: publicDenialNone, minSize: hostfit.ModelSizeLarge})
				s.belowPublicFloor = 1
				return s
			}(),
			want: "no public machine runs a large model, which is the smallest you accept",
		},
		{
			name: "nobody is lending",
			short: publicShortfall{
				hit:  true,
				gate: publicGate{admit: true, denial: publicDenialNone},
			},
			want: "no public machine is reachable right now",
		},
		{
			name:  "lending, but auto says none is better",
			short: withGate(publicGate{admit: true, denial: publicDenialNone, auto: true}),
			want:  "only when it beats this one",
		},
		{
			name:  "lending, and none of them fits",
			short: lending,
			want:  "no public machine can serve this request",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := publicShareDeclineReason(tc.short)
			if tc.want == "" {
				if got != "" {
					t.Fatalf("reason = %q, want none — an attempt that learned nothing must claim nothing", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("reason = %q, want it to contain %q", got, tc.want)
			}
		})
	}

	// The property the arms exist for: told apart, not merely worded.
	t.Run("no two causes produce the same sentence", func(t *testing.T) {
		seen := map[string]string{}
		for _, d := range []publicDenial{
			publicDenialNotConsented, publicDenialModeOff,
			publicDenialMainOff, publicDenialSubOff, publicDenialBothOff,
		} {
			got := publicShareDeclineReason(withGate(publicGate{denial: d}))
			if prev, dup := seen[got]; dup {
				t.Errorf("denial %d and %s produce the same sentence %q", d, prev, got)
			}
			seen[got] = "another"
		}
	})
}

// The settings arms must outrank the reachability arm. This is the defect
// itself: with the posture off the acquirer has released every grant, so
// the map holds no provider and the old order reported the operator's own
// switch as somebody else's outage.
//
// PIN: product contract (waired-agent#1201).
func TestPublicShareRefusal_DoesNotBlameTheWorldForTheOperatorsSwitch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy PublicPolicy
		class  string
		want   string
	}{
		{
			name:   "never consented",
			policy: PublicPolicy{Mode: PublicModeExplicit, Main: true, Sub: true},
			want:   "security and privacy warning has not been accepted",
		},
		{
			name:   "consented and switched off",
			policy: PublicPolicy{Mode: PublicModeOff, Consented: true, Main: true, Sub: true},
			want:   "set not to use other people's public machines",
		},
		{
			name:   "sub-agent turns switched off",
			policy: PublicPolicy{Mode: PublicModeExplicit, Consented: true, Main: true},
			class:  state.ClaudeClassSub,
			want:   "turned off for sub-agent turns",
		},
	} {
		// Two maps, because the posture and the map move at different
		// speeds. "released" is what a host with the posture off actually
		// looks like — the grant acquirer drops every held grant while the
		// posture is off — and it is the shape that produced the false
		// sentence this test exists for. "still held" is the propagation
		// window just after the switch.
		for _, world := range []struct {
			name  string
			peers []inferencemesh.PeerView
		}{
			{name: "grants released", peers: []inferencemesh.PeerView{
				mkPeer("dev_own00000001", "qwen3:8b-q4_K_M", true, false)}},
			{name: "a grant still held", peers: []inferencemesh.PeerView{
				mkPublicPeer(publicPeerDeviceID, publicPeerAlias, "qwen3:8b-q4_K_M")}},
		} {
			t.Run(tc.name+", "+world.name, func(t *testing.T) {
				s, _, _ := publicSelector(t, tc.policy, world.peers...)
				s.in.PublicOnly = true
				s.in.RoutingMode = state.RoutingModePeerOnly

				_, err := s.SelectK(t.Context(), Request{Model: "waired/default", Class: tc.class}, 5)
				if err == nil {
					t.Fatal("a posture that admits nothing must refuse")
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Errorf("err = %v, want it to contain %q", err, tc.want)
				}
				if strings.Contains(err.Error(), "reachable") {
					t.Errorf("err = %v, must not report the operator's own switch as unreachability", err)
				}
				// The row the operator picked leads the sentence, not the mesh.
				if !strings.HasPrefix(err.Error(), "router: Waired public share declined this turn") {
					t.Errorf("err = %v, want it to lead with the entry that declined", err)
				}
				if strings.Contains(err.Error(), "local state=") {
					t.Errorf("err = %v, must not report this host's state on a turn that never intended to run here", err)
				}
			})
		}
	}
}

// The consumer's Public Share floor is its own shortfall, with its own
// command — and must NOT reach the SizeFloorError wrapper, which names the
// operator's `waired worker set --min-model-size`.
//
// PIN: product contract (waired-agent#1201; the second half cites
// waired-agent#1128's ruling that the floor names the switch that was set).
func TestPublicMinModelSize_IsCountedAsItsOwnShortfall(t *testing.T) {
	policy := allowAll()
	policy.MinModelSize = hostfit.ModelSizeLarge
	// 4.7 GB of weights prices as "small", so the large floor excludes it.
	s, _, _ := publicSelectorWith(t, policy, qwenSized(50, 4.7),
		mkPublicPeer(publicPeerDeviceID, publicPeerAlias, "qwen3:8b-q4_K_M"))
	s.in.PublicOnly = true
	s.in.RoutingMode = state.RoutingModePeerOnly

	_, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 5)
	if err == nil {
		t.Fatal("a floor above every lent machine must refuse")
	}
	if !strings.Contains(err.Error(), "which is the smallest you accept") {
		t.Errorf("err = %v, want it to name the Public Share floor", err)
	}
	if !strings.Contains(err.Error(), "waired public use --min-model-size") {
		t.Errorf("err = %v, want it to name the command that changes THIS floor", err)
	}
	if BelowModelSizeFloor(err) {
		t.Errorf("err = %v, must not be reported as the operator's routing floor", err)
	}
}

// The operator's routing floor does not get to answer for a turn that was
// never going to run on this computer's engine.
//
// PIN: product contract — owner ruling 2026-09-06, narrowing
// waired-agent#1128's floor-first order by this one case. See
// docs/decisions/20260906/0410-the-public-entry-answers-for-its-own-refusal.md.
func TestPublicShareRefusal_OutranksTheOperatorFloor(t *testing.T) {
	// The operator's floor, not the consumer's: it drops the lent machine
	// and would otherwise wrap the miss as "no computer runs a large model".
	s, _, _ := publicSelectorWith(t, allowAll(), qwenSized(50, 4.7),
		mkPublicPeer(publicPeerDeviceID, publicPeerAlias, "qwen3:8b-q4_K_M"))
	s.in.MinModelSize = hostfit.ModelSizeLarge
	s.in.PublicOnly = true
	s.in.RoutingMode = state.RoutingModePeerOnly

	_, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 5)
	if err == nil {
		t.Fatal("nothing was admitted, so the turn must be refused")
	}
	if BelowModelSizeFloor(err) {
		t.Errorf("err = %v, want the public entry to answer for its own refusal", err)
	}
	if strings.Contains(err.Error(), "waired worker set --min-model-size") {
		t.Errorf("err = %v, must not send the operator to a switch this turn never used", err)
	}
	if !strings.HasPrefix(err.Error(), "router: Waired public share declined this turn") {
		t.Errorf("err = %v, want the public headline", err)
	}
}
