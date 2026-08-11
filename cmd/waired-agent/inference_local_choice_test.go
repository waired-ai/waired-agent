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
		name  string
		write *agentconfig.Preference // nil = no file at all
		want  string
	}{
		{
			"a model a person chose here",
			&agentconfig.Preference{ModelID: "qwen3.5-2b", SetAt: chosen, Source: agentconfig.PreferenceSourceOperator},
			"2026-08-10T02:31:04.512Z",
		},
		{
			"'run without a local model' is an answer too",
			&agentconfig.Preference{None: true, SetAt: chosen, Source: agentconfig.PreferenceSourceOperator},
			"2026-08-10T02:31:04.512Z",
		},
		{
			"an instruction the setup reconciler applied",
			&agentconfig.Preference{ModelID: "qwen3.5-4b", SetAt: chosen, Source: agentconfig.PreferenceSourceDesired},
			"",
		},
		{
			"a record written before provenance existed",
			&agentconfig.Preference{ModelID: "qwen3.5-4b", SetAt: chosen},
			"",
		},
		{
			"a question nobody answered",
			&agentconfig.Preference{Unanswered: true, SetAt: chosen, Source: agentconfig.PreferenceSourceOperator},
			"",
		},
		{"no preference file at all", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "preferred-model.json")
			if tc.write != nil {
				if err := agentconfig.SavePreference(path, *tc.write); err != nil {
					t.Fatalf("save: %v", err)
				}
			}
			p := &agentInferenceProvider{preferencePath: path}
			if got := p.LocalModelChoiceAt(); got != tc.want {
				t.Errorf("LocalModelChoiceAt() = %q, want %q", got, tc.want)
			}
		})
	}

	// A provider with no preference path configured — the same "nothing to
	// say" answer, reached without touching the filesystem.
	if got := (&agentInferenceProvider{}).LocalModelChoiceAt(); got != "" {
		t.Errorf("an unconfigured provider claimed %q", got)
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
	if got := p.LocalModelChoiceAt(); got != "" {
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
	got := p.LocalModelChoiceAt()
	if got == "" {
		t.Fatal("the accepted demotion published nothing")
	}
	if _, err := time.Parse(time.RFC3339Nano, got); err != nil {
		t.Errorf("published %q, which is not the RFC3339Nano the wire wants: %v", got, err)
	}
}
