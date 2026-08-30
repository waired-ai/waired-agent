package controlclient

import (
	"os"
	"regexp"
	"testing"
)

// Every capability constant proto/signer publishes is either declared by
// this build or listed below with a reason.
//
// The hand-written expectations in network_map_capability_test.go say
// what a declaration SHOULD contain; they cannot say that a constant was
// forgotten, because a constant nobody mentions is absent from both the
// production list and the test's. That is exactly how
// signer.CapabilityMeshShareV1 shipped undeclared (waired#1297): the
// control plane then never folded InferenceState.DesiredShare, so the
// console's mesh-sharing switch stored a value the device never heard —
// and the device kept reporting the setting "on" from its own boot
// default, which is indistinguishable from having been told so. It was
// found on real hardware.
//
// So this reads the constants from the source instead. A new capability
// fails here until someone decides, in writing, which side of the line
// it is on.
var capabilityNotDeclared = map[string]string{
	// The onboarding quartet is declared all-or-none and only by an
	// agent that has a setup reconciler, so it is not in the
	// unconditional list — declareCapabilities appends it when
	// OnboardingCapable. network_map_capability_test.go covers both
	// rows.
	"CapabilityOnboardingV1": "conditional: appended when OnboardingCapable",
	"CapabilityOnboardingV2": "conditional: appended when OnboardingCapable",
	"CapabilityOnboardingV3": "conditional: appended when OnboardingCapable",
	"CapabilityOnboardingV4": "conditional: appended when OnboardingCapable",
}

func TestEveryProtoCapabilityIsDecided(t *testing.T) {
	const capabilitySrc = "../../proto/signer/capability.go"
	src, err := os.ReadFile(capabilitySrc)
	if err != nil {
		t.Fatalf("read %s: %v", capabilitySrc, err)
	}
	names := regexp.MustCompile(`(?m)^\s*(Capability\w+)\s*=\s*"`).FindAllStringSubmatch(string(src), -1)

	// A floor, not a count. Without it a moved file or a changed spelling
	// would leave the regex matching nothing and this test passing
	// vacuously — quiet in exactly the situation it exists for.
	if len(names) < 8 {
		t.Fatalf("found %d capability constants in %s, want at least 8 — has the file moved?",
			len(names), capabilitySrc)
	}

	decl, err := os.ReadFile("network_map.go")
	if err != nil {
		t.Fatalf("read network_map.go: %v", err)
	}
	for _, m := range names {
		name := m[1]
		if reason, ok := capabilityNotDeclared[name]; ok {
			if reason == "" {
				t.Errorf("%s is excluded with no reason", name)
			}
			continue
		}
		if !regexp.MustCompile(`signer\.` + name + `\b`).Match(decl) {
			t.Errorf("%s is published by proto/signer but this build neither declares it "+
				"nor lists it in capabilityNotDeclared. An undeclared capability means the "+
				"control plane never sends the field it gates, silently.", name)
		}
	}

	// And the other direction: an exclusion that matches no constant is a
	// stale claim, and nothing was reading it.
	found := map[string]bool{}
	for _, m := range names {
		found[m[1]] = true
	}
	for name := range capabilityNotDeclared {
		if !found[name] {
			t.Errorf("capabilityNotDeclared lists %s, which proto/signer no longer publishes", name)
		}
	}
}
