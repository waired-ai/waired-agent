package main

import (
	"strings"
	"testing"
)

// `waired runtimes upgrade vllm` used to be a hard error — "not
// supported yet" — which is what #843 was filed against: the verb the
// installer scripts call during an update refused for one of the two
// engines, so a venv never left the release it was set up with.
func TestRuntimesUpgrade_KnowsBothEngines(t *testing.T) {
	if err := runRuntimesUpgradeBody("vllm", t.TempDir(), true); err != nil {
		t.Fatalf("runtimes upgrade vllm on a host with no venv: %v", err)
	}
}

// And it still refuses an engine it does not manage, naming both of the
// ones it does — the message is what a mistyped engine name gets.
func TestRuntimesUpgrade_UnknownEngineNamesBoth(t *testing.T) {
	err := runRuntimesUpgradeBody("llamacpp", t.TempDir(), true)
	if err == nil {
		t.Fatal("an unknown engine was accepted")
	}
	for _, want := range []string{"ollama", "vllm"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q as supported", err, want)
		}
	}
}
