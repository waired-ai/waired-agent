package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
)

// PRODUCT CONTRACT (waired-agent#647, the wire-contract field table on
// the issue): the agent publishes a choice timestamp ONLY for an answer a
// person gave at this machine, and publishes nothing for everything else.
//
// The silent cases are the ones that matter. The control plane reads a
// timestamp as licence to move its own desired-model instruction, so a
// preference the instruction itself wrote — or one whose provenance is
// unknown because it predates the field — must not be able to confirm
// that instruction back to the sender.
func TestProviderLocalModelChoiceAt(t *testing.T) {
	chosen := time.Date(2026, 8, 10, 2, 31, 4, 512000000, time.UTC)

	for _, tc := range []struct {
		name      string
		write     *agentconfig.Preference // nil = no file at all
		wantModel string
		want      string
	}{
		{
			"a model a person chose here",
			&agentconfig.Preference{ModelID: "qwen3.5-2b", SetAt: chosen, Source: agentconfig.PreferenceSourceOperator},
			"qwen3.5-2b",
			"2026-08-10T02:31:04.512Z",
		},
		{
			// The probe compares the id with ActiveModel, which is always
			// the canonical model_id (waired-ai/waired#1454).
			"an alias comes back as the canonical id",
			&agentconfig.Preference{ModelID: "qwen2.5-coder-14b", SetAt: chosen, Source: agentconfig.PreferenceSourceOperator},
			"qwen2.5-coder-14b-instruct",
			"2026-08-10T02:31:04.512Z",
		},
		{
			"'run without a local model' is an answer too",
			&agentconfig.Preference{None: true, SetAt: chosen, Source: agentconfig.PreferenceSourceOperator},
			"",
			"2026-08-10T02:31:04.512Z",
		},
		{
			"an instruction the setup reconciler applied",
			&agentconfig.Preference{ModelID: "qwen3.5-4b", SetAt: chosen, Source: agentconfig.PreferenceSourceDesired},
			"",
			"",
		},
		{
			"a record written before provenance existed",
			&agentconfig.Preference{ModelID: "qwen3.5-4b", SetAt: chosen},
			"",
			"",
		},
		{
			"a question nobody answered",
			&agentconfig.Preference{Unanswered: true, SetAt: chosen, Source: agentconfig.PreferenceSourceOperator},
			"",
			"",
		},
		{"no preference file at all", nil, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "preferred-model.json")
			if tc.write != nil {
				if err := agentconfig.SavePreference(path, *tc.write); err != nil {
					t.Fatalf("save: %v", err)
				}
			}
			p := &agentInferenceProvider{preferencePath: path, manifests: canonicalTestManifests()}
			model, at := p.LocalModelChoiceAt()
			if model != tc.wantModel || at != tc.want {
				t.Errorf("LocalModelChoiceAt() = (%q, %q), want (%q, %q)", model, at, tc.wantModel, tc.want)
			}
		})
	}

	// A provider with no preference path configured — the same "nothing to
	// say" answer, reached without touching the filesystem.
	if model, at := (&agentInferenceProvider{}).LocalModelChoiceAt(); model != "" || at != "" {
		t.Errorf("an unconfigured provider claimed (%q, %q)", model, at)
	}
}

// The demotion is the flow the field exists for, so it is walked
// end to end at the file layer: the wizard's instruction lands first,
// then the person accepts the step down, and only the second one is
// publishable.
func TestProviderLocalModelChoiceAt_DemotionReplacesTheInstruction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "preferred-model.json")
	p := &agentInferenceProvider{preferencePath: path}

	// The control plane told this host to run the 4B.
	if err := agentconfig.SavePreference(path, agentconfig.Preference{
		ModelID: "qwen3.5-4b",
		Source:  agentconfig.PreferenceSourceDesired,
	}); err != nil {
		t.Fatalf("save instruction: %v", err)
	}
	if _, got := p.LocalModelChoiceAt(); got != "" {
		t.Fatalf("the instruction alone claimed a local choice: %q", got)
	}

	// It measured slow, and the operator accepted the lighter model. That
	// answer goes through the management endpoint, which marks it.
	if err := agentconfig.SavePreference(path, agentconfig.Preference{
		ModelID: "qwen3.5-2b",
		Source:  agentconfig.PreferenceSourceOperator,
	}); err != nil {
		t.Fatalf("save demotion: %v", err)
	}
	choseModel, got := p.LocalModelChoiceAt()
	if got == "" {
		t.Fatal("the accepted demotion published nothing")
	}
	if _, err := time.Parse(time.RFC3339Nano, got); err != nil {
		t.Errorf("published %q, which is not the RFC3339Nano the wire wants: %v", got, err)
	}
	if choseModel != "qwen3.5-2b" {
		t.Errorf("the accepted demotion named %q, want qwen3.5-2b", choseModel)
	}
}

// PRODUCT CONTRACT (waired-ai/waired#1454, found on hardware 2026-09-19):
// a local choice's time rides only while the push reports the chosen
// value as the one in force. The control plane moves its instruction
// onto the reported value when the time is newer, so a time paired with
// the value the person is leaving moved the instruction there — and the
// device applied it over the choice.
func TestLocalChoiceInForce(t *testing.T) {
	t.Run("model", func(t *testing.T) {
		for _, tc := range []struct {
			name, chosen, active string
			want                 bool
		}{
			{"the chosen model is served", "qwen3.6-35b-a3b", "qwen3.6-35b-a3b", true},
			{"the chosen model is still downloading", "qwen3.6-35b-a3b", "qwen3.5-122b-a10b", false},
			{"nothing is served yet", "qwen3.6-35b-a3b", "", false},
			{"'no local model' while nothing is served", "", "", true},
			{"'no local model' while a model is still served", "", "qwen3.5-9b", false},
		} {
			if got := localModelChoiceInForce(tc.chosen, tc.active); got != tc.want {
				t.Errorf("%s: localModelChoiceInForce(%q, %q) = %v, want %v", tc.name, tc.chosen, tc.active, got, tc.want)
			}
		}
	})
	t.Run("residency", func(t *testing.T) {
		for _, tc := range []struct {
			name, chosen, reported string
			want                   bool
		}{
			{"the chosen value is in force", "45m0s", "45m0s", true},
			{"two spellings of one value", "45m", "45m0s", true},
			{"held indefinitely, chosen and reported", "0s", "0s", true},
			// vLLM holds the model until the engine exits and reports 0s
			// whatever was chosen (waired-agent#943).
			{"a vLLM host reporting 0s", "45m0s", "0s", false},
			{"an env override after a restart", "45m0s", "10m0s", false},
			{"no recorded value", "", "45m0s", false},
			{"no reported value", "45m0s", "", false},
			{"an unparseable record", "soon", "45m0s", false},
		} {
			if got := residencyChoiceInForce(tc.chosen, tc.reported); got != tc.want {
				t.Errorf("%s: residencyChoiceInForce(%q, %q) = %v, want %v", tc.name, tc.chosen, tc.reported, got, tc.want)
			}
		}
	})
}
